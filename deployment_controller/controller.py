"""One resumable operation per physical worker; distinct regions never share capacity."""

import copy
import json
import re
import subprocess
import threading
import time
from urllib.parse import urlsplit
from .ledger import Conflict, digest

KEY = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$")
SHA = re.compile(r"^sha256-[a-f0-9]{64}$")
PHASES = [
    "planned",
    "prepared",
    "draining",
    "drained",
    "applied",
    "verified",
    "registered",
    "serving",
    "complete",
]


def validate(manifest, region):
    if manifest.get("schema_version") != 1 or manifest.get("region") != region:
        raise ValueError("Unsupported schema or wrong regional environment")
    if manifest.get("action") not in ["online", "upgrade", "rollback", "offline"]:
        raise ValueError("Invalid lifecycle action")
    if not KEY.fullmatch(manifest.get("worker_id", "")):
        raise ValueError("Invalid worker identity")
    groups = manifest.get("resource_groups", [])
    if (
        not groups
        or len(groups) != len(set(groups))
        or not all(isinstance(g, str) and g for g in groups)
    ):
        raise ValueError("Explicit unique GPU resource groups required")
    rows = manifest.get("instances", [])
    if manifest["action"] == "offline":
        if rows:
            raise ValueError("Offline plans cannot introduce registrations")
    elif not rows:
        raise ValueError("Online plans require all model bindings on the worker")
    ids = set()
    bindings = set()
    for row in rows:
        allowed = {
            "instance_id",
            "model_key",
            "protocol",
            "endpoint",
            "health_url",
            "resource_group",
            "machine_id",
            "gpu_devices",
            "capacity_units",
            "deployment_id",
            "artifact_digest",
            "serve",
        }
        if set(row) - allowed:
            raise ValueError("Unknown registration fields in plan")
        for field in ["endpoint", "health_url"]:
            url = urlsplit(row.get(field, ""))
            if (
                url.scheme not in ["http", "https"]
                or not url.hostname
                or url.username
                or url.password
                or url.query
                or url.fragment
            ):
                raise ValueError(
                    "Invalid endpoint; credentials must not appear in the manifest"
                )
        if "serve" in row and type(row["serve"]) is not bool:
            raise ValueError("serve must be boolean")
        for key in ["instance_id", "model_key", "deployment_id"]:
            if not KEY.fullmatch(row.get(key, "")):
                raise ValueError("Missing immutable " + key)
        if not SHA.fullmatch(row.get("artifact_digest", "")):
            raise ValueError("Immutable release digest required")
        if row["instance_id"] in ids or (row["model_key"], row["endpoint"]) in bindings:
            raise ValueError("Duplicate model binding")
        ids.add(row["instance_id"])
        bindings.add((row["model_key"], row["endpoint"]))
        if row["resource_group"] not in groups:
            raise ValueError("Registration escapes the worker resource groups")
        if (
            not row.get("machine_id")
            or not row.get("gpu_devices")
            or row.get("capacity_units", 0) < 1
        ):
            raise ValueError("Physical GPU identity and capacity required")
    return manifest


class Controller:
    def __init__(self, ledger, gateway, agent_argv, timeout=1800, interval=2):
        self.ledger, self.gateway, self.agent_argv = ledger, gateway, agent_argv
        self.timeout, self.interval = timeout, interval
        self.lost = threading.Event()
        self.stop = threading.Event()

    def check(self):
        if self.lost.is_set():
            raise Conflict(
                "Lease renewal failed; operation must be resumed by a new controller"
            )
        self.ledger.renew()

    def heartbeat(self):
        while not self.stop.wait(max(1, self.ledger.ttl / 3)):
            try:
                self.ledger.renew()
            except Exception:
                self.lost.set()
                return

    def agent(self, action):
        self.check()
        message = {
            "region": self.manifest["region"],
            "worker_id": self.manifest["worker_id"],
            "operation_id": self.operation,
            "epoch": self.ledger.epoch,
            "manifest_sha256": digest(self.manifest),
            "action": action,
        }
        p = subprocess.run(
            self.agent_argv,
            input=json.dumps(message),
            text=True,
            capture_output=True,
            timeout=self.timeout,
        )
        self.check()
        if p.returncode:
            raise RuntimeError(
                "Deployment agent failed for "
                + action
                + "; inspect its restricted host logs"
            )
        result = json.loads(p.stdout)
        if action != "fence" and (
            result.get("ok") is not True
            or result.get("manifest_sha256") != digest(self.manifest)
        ):
            raise Conflict("Agent receipt does not match the immutable plan")
        return result

    def affected(self):
        return [
            x
            for x in self.gateway.instances()
            if x["resource_group"] in self.manifest["resource_groups"]
            or any(
                (x.get("machine_id"), gpu) in self.physical
                for gpu in x.get("gpu_devices", [])
            )
        ]

    def wait(self, predicate, label):
        deadline = time.monotonic() + self.timeout
        while True:
            self.check()
            value = predicate()
            if value:
                return value
            if time.monotonic() > deadline:
                raise TimeoutError(
                    "Timed out waiting for " + label + "; registrations remain drained"
                )
            time.sleep(self.interval)

    def confirm_idle(self):
        result = self.agent("idle")
        if (
            result.get("idle") is not True
            or result.get("active_requests") != 0
            or result.get("uncertain_executions") != 0
        ):
            raise Conflict("Worker has active, uncertain, or unaccounted executions")
        return result

    def phase(self, name, **kw):
        self.check()
        self.ledger.advance(name, **kw)
        self.row["phase"] = name
        if "snapshot" in kw:
            self.row["snapshot"] = kw["snapshot"]

    def run(self, operation, manifest):
        if not KEY.fullmatch(operation):
            raise ValueError("Invalid operation ID")
        self.manifest = validate(copy.deepcopy(manifest), self.ledger.region)
        self.operation = operation
        domain = self.gateway.call("GET", "/v1/deployments/domain")
        if (
            domain.get("region") != self.ledger.region
            or domain.get("lifecycle_protocol") != 1
        ):
            raise Conflict("Gateway and database deployment domains differ")
        self.row = self.ledger.acquire(operation, self.manifest)
        if self.row["phase"] == "complete":
            return self.row
        thread = threading.Thread(target=self.heartbeat, daemon=True)
        thread.start()
        try:
            groups = self.gateway.call("GET", "/v1/resource-groups")
            selected = [
                g for g in groups if g["id"] in self.manifest["resource_groups"]
            ]
            if len(selected) != len(self.manifest["resource_groups"]):
                raise Conflict("Create physical GPU groups before deploying a worker")
            self.physical = {(g["machine_id"], g["gpu_device"]) for g in selected}
            for row in self.affected() + self.manifest["instances"]:
                if any(
                    (row.get("machine_id"), gpu) not in self.physical
                    for gpu in row.get("gpu_devices", [])
                ):
                    raise Conflict(
                        "Plan omits a physical GPU used by an affected worker"
                    )
                group = next(
                    (g for g in groups if g["id"] == row["resource_group"]), None
                )
                if (
                    group is None
                    or group["machine_id"] != row.get("machine_id")
                    or group["gpu_device"] != row.get("gpu_devices", [None])[0]
                ):
                    raise Conflict("Binding does not match the physical resource group")
            self.agent("fence")
            self.reconcile()
            return self.ledger.status(operation)
        except Exception as e:
            # Keep the last completed phase; retry reconciles it against live state.
            if not self.lost.is_set():
                self.ledger.advance(
                    self.row["phase"], error=type(e).__name__ + ": " + str(e)
                )
            raise
        finally:
            self.stop.set()
            thread.join(timeout=10)
            if self.row["phase"] != "complete":
                self.ledger.release()

    def wants_serving(self, row):
        if "serve" in row:
            if type(row["serve"]) is not bool:
                raise ValueError("serve must be boolean")
            return row["serve"]
        old = next(
            (
                x
                for x in self.row["snapshot"]["instances"]
                if x["model_key"] == row["model_key"]
                and x["endpoint"] == row["endpoint"]
            ),
            None,
        )
        return old is None or old.get("draining", False) is False

    def reconcile(self):
        phase = self.row["phase"]
        offline = self.manifest["action"] == "offline"
        if phase == "planned":
            prepared = self.agent("prepare")
            rows = self.affected()
            # The manifest must enumerate every existing model binding sharing the
            # worker. Omissions would silently retire an unrelated capability.
            if not offline:
                wanted = {
                    (r["model_key"], r["endpoint"]) for r in self.manifest["instances"]
                }
                if any((r["model_key"], r["endpoint"]) not in wanted for r in rows):
                    raise Conflict("Plan omits an existing binding on a shared GPU")
            self.phase(
                "prepared", snapshot={"instances": rows}, receipt={"prepare": prepared}
            )
            phase = "prepared"
        if phase in ["prepared", "draining"]:
            if phase == "prepared":
                self.phase("draining")
            for row in self.affected():
                self.check()
                self.gateway.drain(row["id"])
            self.wait(
                lambda: all(
                    r.get("draining") is True
                    and r.get("reserved_requests") == 0
                    and r.get("reserved_units") == 0
                    for r in self.affected()
                ),
                "all regional reservations to drain",
            )
            idle = self.confirm_idle()
            self.phase("drained", receipt={"idle": idle})
            phase = "drained"
        if phase == "drained":
            # Recheck physical execution immediately before an idempotent mutation.
            self.confirm_idle()
            result = self.agent("stop" if offline else "activate")
            self.phase("applied", receipt={"apply": result})
            phase = "applied"
        if phase == "applied":
            result = self.agent("verify")
            if offline:
                if result.get("stopped") is not True:
                    raise Conflict("Stop not confirmed")
            else:
                expected = {
                    r["model_key"]: (r["deployment_id"], r["artifact_digest"])
                    for r in self.manifest["instances"]
                }
                actual = {
                    r["model_key"]: (r["deployment_id"], r["artifact_digest"])
                    for r in result.get("bindings", [])
                }
                if (
                    actual != expected
                    or not result.get("boot_id")
                    or result.get("smoke_passed") is not True
                ):
                    raise Conflict("Loaded deployment differs from the plan")
            self.phase("verified", receipt={"verify": result})
            phase = "verified"
        if phase == "verified":
            if not offline:
                for row in self.manifest["instances"]:
                    self.check()
                    self.gateway.call(
                        "POST",
                        "/v1/instances/register",
                        {
                            **{k: v for k, v in row.items() if k != "serve"},
                            "initially_draining": True,
                        },
                    )
                    self.wait(
                        lambda r=row: self.gateway.probe(r).get("ready") is True,
                        "identity-bound probe " + row["instance_id"],
                    )
            self.phase("registered")
            phase = "registered"
        if phase == "registered":
            if not offline:
                for row in self.manifest["instances"]:
                    self.wait(
                        lambda r=row: self.gateway.probe(r).get("ready") is True,
                        "loaded identity before resume",
                    )
                    if self.wants_serving(row):
                        self.check()
                        self.gateway.resume(row["instance_id"])
                expected = {
                    r["instance_id"]
                    for r in self.manifest["instances"]
                    if self.wants_serving(r)
                }
                self.wait(
                    lambda: expected
                    <= {r["id"] for r in self.affected() if r.get("eligible") is True},
                    "new registrations to become eligible",
                )
            self.phase("serving")
            phase = "serving"
        if phase == "serving":
            newids = {r["instance_id"] for r in self.manifest["instances"]}
            for old in self.row["snapshot"]["instances"]:
                if old["id"] in newids:
                    continue

                def clearable(id=old["id"]):
                    row = next(
                        (r for r in self.gateway.instances() if r["id"] == id), None
                    )
                    return row is None or row.get("clearable") is True

                self.wait(clearable, "old registration archival " + old["id"])
                if any(r["id"] == old["id"] for r in self.gateway.instances()):
                    self.check()
                    self.gateway.call("DELETE", "/v1/instances/" + old["id"])
            self.phase("complete", receipt={"final_instances": self.affected()})
