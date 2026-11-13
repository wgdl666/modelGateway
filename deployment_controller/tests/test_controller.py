import copy
import unittest
from deployment_controller.controller import Controller, validate
from deployment_controller.ledger import Conflict


def plan(action="upgrade"):
    return {
        "schema_version": 1,
        "region": "cn/dev",
        "worker_id": "gpu1",
        "action": action,
        "resource_groups": ["cn/gpu1"],
        "instances": (
            []
            if action == "offline"
            else [
                dict(
                    instance_id="new",
                    model_key="body",
                    protocol="body_model",
                    endpoint="http://worker",
                    health_url="http://worker/health",
                    resource_group="cn/gpu1",
                    machine_id="node",
                    gpu_devices=[1],
                    capacity_units=1,
                    deployment_id="release-2",
                    artifact_digest="sha256-" + "a" * 64,
                )
            ]
        ),
    }


class Ledger:
    region = "cn/dev"
    epoch = 1
    ttl = 300

    def __init__(self):
        self.row = {"phase": "planned", "snapshot": {}, "receipts": {}}

    def acquire(self, *args):
        return copy.deepcopy(self.row)

    def renew(self):
        pass

    def release(self):
        pass

    def advance(self, phase, **kw):
        self.row.update(phase=phase)
        if kw.get("snapshot") is not None:
            self.row["snapshot"] = copy.deepcopy(kw["snapshot"])
        self.row["receipts"].update(kw.get("receipt", {}))

    def status(self, *args):
        return self.row


class Gateway:
    def __init__(self):
        self.rows = [
            dict(
                id="old",
                model_key="body",
                endpoint="http://worker",
                resource_group="cn/gpu1",
                machine_id="node",
                gpu_devices=[1],
                draining=False,
                reserved_requests=0,
                reserved_units=0,
            )
        ]
        self.events = []
        self.probe_ready = True

    def instances(self):
        return copy.deepcopy(self.rows)

    def drain(self, id):
        self.events.append(("drain", id))
        next(r for r in self.rows if r["id"] == id).update(
            draining=True, eligible=False, clearable=True
        )

    def resume(self, id):
        self.events.append(("resume", id))
        next(r for r in self.rows if r["id"] == id).update(
            draining=False, eligible=True
        )

    def probe(self, row):
        return {"ready": self.probe_ready}

    def call(self, method, path, body=None):
        if path == "/v1/resource-groups":
            return [{"id": "cn/gpu1", "machine_id": "node", "gpu_device": 1}]
        if path == "/v1/deployments/domain":
            return {"region": "cn/dev", "lifecycle_protocol": 1}
        if method == "DELETE":
            self.events.append(("delete", path))
            self.rows = [r for r in self.rows if r["id"] != path.rsplit("/", 1)[1]]
            return {}
        if path == "/v1/instances/register":
            assert body["initially_draining"] is True
            self.events.append(("register", body["instance_id"]))
            if not any(r["id"] == body["instance_id"] for r in self.rows):
                self.rows.append(
                    {
                        **body,
                        "id": body["instance_id"],
                        "draining": True,
                        "eligible": False,
                        "reserved_requests": 0,
                        "reserved_units": 0,
                    }
                )
            return {}
        raise AssertionError(path)


class FakeController(Controller):
    def __init__(self, *a, **kw):
        super().__init__(*a, **kw)
        self.actions = []
        self.idle = True

    def agent(self, action):
        self.actions.append(action)
        if action == "idle":
            return {
                "ok": True,
                "idle": self.idle,
                "active_requests": 0,
                "uncertain_executions": 0,
            }
        if action == "verify":
            return {
                "ok": True,
                "stopped": True,
                "smoke_passed": True,
                "boot_id": "boot",
                "bindings": self.manifest["instances"],
            }
        return {"ok": True}


class ControllerTest(unittest.TestCase):
    def controller(self):
        ledger = Ledger()
        gateway = Gateway()
        return (
            FakeController(ledger, gateway, [], timeout=0.01, interval=0.001),
            ledger,
            gateway,
        )

    def test_online_order_and_idempotent_completion(self):
        c, l, g = self.controller()
        self.assertEqual(c.run("op", plan())["phase"], "complete")
        self.assertEqual(
            [x[0] for x in g.events], ["drain", "register", "resume", "delete"]
        )
        self.assertLess(c.actions.index("idle"), c.actions.index("activate"))
        count = len(g.events)
        self.assertEqual(c.run("op", plan())["phase"], "complete")
        self.assertEqual(len(g.events), count)

    def test_busy_worker_stays_drained(self):
        c, l, g = self.controller()
        c.idle = False
        with self.assertRaises(Conflict):
            c.run("op", plan())
        self.assertTrue(g.rows[0]["draining"])
        self.assertNotIn("activate", c.actions)

    def test_missing_shared_binding_blocks_before_drain(self):
        c, l, g = self.controller()
        g.rows.append({**g.rows[0], "id": "vton", "model_key": "vton"})
        with self.assertRaises(Conflict):
            c.run("op", plan())
        self.assertFalse(g.events)

    def test_bad_probe_never_enables_new_instance(self):
        c, l, g = self.controller()
        g.probe_ready = False
        with self.assertRaises(TimeoutError):
            c.run("op", plan())
        self.assertFalse(any(x[0] == "resume" for x in g.events))
        self.assertEqual(l.row["phase"], "verified")

    def test_preexisting_pause_preserved(self):
        c, l, g = self.controller()
        g.rows[0]["draining"] = True
        c.run("op", plan())
        self.assertFalse(any(x[0] == "resume" for x in g.events))
        self.assertTrue(g.rows[0]["draining"])

    def test_offline_stops_before_retiring(self):
        c, l, g = self.controller()
        c.run("op", plan("offline"))
        self.assertIn("stop", c.actions)
        self.assertNotIn("activate", c.actions)
        self.assertFalse(g.rows)

    def test_unknown_region_rejected(self):
        with self.assertRaises(ValueError):
            validate(plan(), "us/prod")
