"""PostgreSQL operation journal and fencing shared by all regional controllers."""

from contextlib import contextmanager
import hashlib
import json
import uuid
import psycopg
from psycopg.rows import dict_row
from psycopg.types.json import Jsonb


class Conflict(RuntimeError):
    pass


def digest(value):
    return hashlib.sha256(
        json.dumps(
            value, sort_keys=True, separators=(",", ":"), allow_nan=False
        ).encode()
    ).hexdigest()


class Ledger:
    def __init__(self, dsn, region, ttl=90):
        self.dsn, self.region, self.ttl = dsn, region, ttl
        self.owner, self.epoch = uuid.uuid4().hex, None
        self.operation = None
        self.groups = []

    @contextmanager
    def connection(self):
        with psycopg.connect(
            self.dsn, row_factory=dict_row, connect_timeout=10, client_encoding="utf8"
        ) as conn:
            conn.execute("SET search_path TO model_gateway, public")
            row = conn.execute(
                "SELECT region FROM deployment_domain WHERE id=1"
            ).fetchone()
            if not row or row["region"] != self.region:
                raise Conflict(
                    "Database deployment region does not match the selected region"
                )
            yield conn

    def acquire(self, operation, manifest):
        groups = sorted(manifest["resource_groups"])
        with self.connection() as c:
            c.execute(
                "INSERT INTO deployment_operations(operation_id,region,worker_id,manifest,manifest_sha256) VALUES(%s,%s,%s,%s,%s) ON CONFLICT DO NOTHING",
                (
                    operation,
                    self.region,
                    manifest["worker_id"],
                    Jsonb(manifest),
                    digest(manifest),
                ),
            )
            op = c.execute(
                "SELECT * FROM deployment_operations WHERE operation_id=%s FOR UPDATE",
                (operation,),
            ).fetchone()
            if op["region"] != self.region or op["manifest_sha256"] != digest(manifest):
                raise Conflict(
                    "Operation ID is already bound to another immutable plan"
                )
            if op["phase"] == "complete":
                return op
            rows = []
            for group in groups:
                c.execute(
                    "INSERT INTO deployment_leases(resource_group,region,worker_id,lease_until) VALUES(%s,%s,%s,'epoch') ON CONFLICT DO NOTHING",
                    (group, self.region, manifest["worker_id"]),
                )
                row = c.execute(
                    "SELECT *, lease_until > clock_timestamp() AS live FROM deployment_leases WHERE resource_group=%s FOR UPDATE",
                    (group,),
                ).fetchone()
                if (
                    row["region"] != self.region
                    or row["worker_id"] != manifest["worker_id"]
                ):
                    raise Conflict(
                        "GPU resource group belongs to another worker or region"
                    )
                if row["operation_id"] and row["operation_id"] != operation:
                    raise Conflict(
                        "Another unfinished operation owns this GPU; resume it first"
                    )
                if row["live"] and row["owner"] != self.owner:
                    raise Conflict("A controller is already executing this operation")
                rows.append(row)
            self.epoch = max(r["epoch"] for r in rows) + 1
            for group in groups:
                c.execute(
                    "UPDATE deployment_leases SET operation_id=%s,owner=%s,epoch=%s,lease_until=clock_timestamp()+(%s * interval '1 second') WHERE resource_group=%s",
                    (operation, self.owner, self.epoch, self.ttl, group),
                )
        self.operation, self.groups = operation, groups
        return op

    def headers(self):
        if self.epoch is None:
            raise Conflict("No active operation lease")
        return {
            "X-Deployment-Operation": self.operation,
            "X-Deployment-Owner": self.owner,
            "X-Deployment-Epoch": str(self.epoch),
        }

    def renew(self):
        with self.connection() as c:
            count = c.execute(
                "UPDATE deployment_leases SET lease_until=clock_timestamp()+(%s * interval '1 second') WHERE operation_id=%s AND owner=%s AND epoch=%s AND lease_until>clock_timestamp() AND resource_group=ANY(%s)",
                (self.ttl, self.operation, self.owner, self.epoch, self.groups),
            ).rowcount
            if count != len(self.groups):
                raise Conflict(
                    "Deployment lease was lost; no further actions are permitted"
                )

    def advance(self, phase, *, snapshot=None, receipt=None, error=""):
        with self.connection() as c:
            rows = c.execute(
                "SELECT *,lease_until>clock_timestamp() AS live FROM deployment_leases WHERE resource_group=ANY(%s) ORDER BY resource_group FOR UPDATE",
                (self.groups,),
            ).fetchall()
            if len(rows) != len(self.groups) or any(
                not r["live"]
                or r["owner"] != self.owner
                or r["epoch"] != self.epoch
                or r["operation_id"] != self.operation
                for r in rows
            ):
                raise Conflict("Deployment lease was lost")
            c.execute(
                "UPDATE deployment_operations SET phase=%s,snapshot=COALESCE(%s,snapshot),receipts=receipts || %s,error=%s,updated_at=clock_timestamp() WHERE operation_id=%s",
                (
                    phase,
                    Jsonb(snapshot) if snapshot is not None else None,
                    Jsonb(receipt or {}),
                    error,
                    self.operation,
                ),
            )
            if phase == "complete":
                c.execute(
                    "UPDATE deployment_leases SET operation_id='',owner='',lease_until='epoch' WHERE resource_group=ANY(%s)",
                    (self.groups,),
                )

    def release(self):
        # An unfinished operation remains the owner of the resource group. A later
        # controller may resume that operation, but cannot start a competing plan.
        with self.connection() as c:
            c.execute(
                "UPDATE deployment_leases SET lease_until='epoch' WHERE operation_id=%s AND owner=%s AND epoch=%s",
                (self.operation, self.owner, self.epoch),
            )

    def status(self, operation):
        with self.connection() as c:
            row = c.execute(
                "SELECT * FROM deployment_operations WHERE operation_id=%s AND region=%s",
                (operation, self.region),
            ).fetchone()
            if not row:
                raise Conflict("Unknown operation in this region")
            return row

    def pending(self, backoff=60):
        with self.connection() as c:
            return c.execute(
                """SELECT o.* FROM deployment_operations o
                WHERE o.region=%s AND o.phase<>'complete'
                AND o.updated_at < clock_timestamp()-(%s * interval '1 second')
                AND NOT EXISTS (SELECT 1 FROM deployment_leases l WHERE l.operation_id=o.operation_id AND l.lease_until>clock_timestamp())
                ORDER BY o.created_at LIMIT 32""",
                (self.region, backoff),
            ).fetchall()
