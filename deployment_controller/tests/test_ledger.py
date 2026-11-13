"""Real PostgreSQL lease, recovery and region-isolation tests; _test DB only."""

import os
import unittest
from urllib.parse import urlparse
from pathlib import Path
import threading
import psycopg
from deployment_controller.ledger import Ledger, Conflict
from deployment_controller.tests.test_controller import plan


@unittest.skipUnless(os.environ.get("TEST_DATABASE_URL"), "TEST_DATABASE_URL required")
class LedgerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dsn = os.environ["TEST_DATABASE_URL"]
        if not urlparse(cls.dsn).path.endswith("_test"):
            raise RuntimeError("Use an isolated _test database")
        with psycopg.connect(cls.dsn) as c:
            c.execute("CREATE SCHEMA IF NOT EXISTS model_gateway")
            # Gateway fixtures create the instance and request tables first.
            c.execute(Path("deploy/migrations/deployment-lifecycle.sql").read_text())

    def setUp(self):
        with psycopg.connect(self.dsn) as c:
            c.execute(
                "TRUNCATE model_gateway.deployment_leases,model_gateway.deployment_operations,model_gateway.deployment_domain"
            )
            c.execute(
                "INSERT INTO model_gateway.deployment_domain(id,region) VALUES(1,'cn/dev')"
            )

    def test_only_one_controller_claims_a_worker(self):
        barrier = threading.Barrier(2)
        results = []

        def claim():
            ledger = Ledger(self.dsn, "cn/dev")
            barrier.wait()
            try:
                ledger.acquire("same", plan())
                results.append("acquired")
            except Conflict:
                results.append("blocked")

        threads = [threading.Thread(target=claim) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertCountEqual(results, ["acquired", "blocked"])

    def test_takeover_fences_old_controller_and_preserves_phase(self):
        a = Ledger(self.dsn, "cn/dev")
        a.acquire("same", plan())
        a.advance("drained", snapshot={"proof": "saved"})
        with psycopg.connect(self.dsn) as c:
            c.execute("UPDATE model_gateway.deployment_leases SET lease_until='epoch'")
        b = Ledger(self.dsn, "cn/dev")
        row = b.acquire("same", plan())
        self.assertEqual(row["phase"], "drained")
        self.assertGreater(b.epoch, a.epoch)
        with self.assertRaises(Conflict):
            a.advance("serving")
        with self.assertRaises(Conflict):
            a.renew()
        b.advance("complete")
        self.assertEqual(b.status("same")["snapshot"], {"proof": "saved"})

    def test_expiration_never_allows_a_different_unfinished_plan(self):
        a = Ledger(self.dsn, "cn/dev")
        a.acquire("same", plan())
        a.release()
        with self.assertRaises(Conflict):
            Ledger(self.dsn, "cn/dev").acquire("other", plan())
        changed = plan()
        changed["action"] = "rollback"
        with self.assertRaises(Conflict):
            Ledger(self.dsn, "cn/dev").acquire("same", changed)

    def test_region_is_checked_against_database_not_hostname(self):
        with self.assertRaises(Conflict):
            Ledger(self.dsn, "us/prod").acquire("same", plan())

    def test_regions_and_gpus_are_not_implicitly_interchangeable(self):
        a = Ledger(self.dsn, "cn/dev")
        a.acquire("first", plan())
        a.advance("complete")
        changed = plan()
        changed["worker_id"] = "another-worker"
        with self.assertRaises(Conflict):
            Ledger(self.dsn, "cn/dev").acquire("second", changed)
