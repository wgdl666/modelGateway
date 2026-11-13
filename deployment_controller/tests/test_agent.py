import json
import tempfile
from pathlib import Path
import sys
import unittest
from deployment_controller.agent import execute


class AgentTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.counter = self.root / "count"
        code = (
            "from pathlib import Path;import json;p=Path("
            + repr(str(self.counter))
            + ');p.write_text(str(int(p.read_text())+1) if p.exists() else "1");print(json.dumps({"ok":True}))'
        )
        self.config = {
            "region": "cn/dev",
            "worker_id": "gpu1",
            "plans": {
                "sha": {
                    "activate": {"argv": [sys.executable, "-c", code]},
                    "stop": {"argv": [sys.executable, "-c", code]},
                }
            },
        }
        self.message = {
            "region": "cn/dev",
            "worker_id": "gpu1",
            "manifest_sha256": "sha",
            "operation_id": "op",
            "epoch": 1,
            "action": "activate",
        }

    def test_recovery_replays_completed_mutation_across_controller_epochs(self):
        first = execute(self.config, self.root, self.message)
        second = execute(self.config, self.root, {**self.message, "epoch": 2})
        self.assertEqual(first, second)
        self.assertEqual(self.counter.read_text(), "1")
        with self.assertRaisesRegex(ValueError, "Stale"):
            execute(self.config, self.root, self.message)

    def test_unknown_execution_is_never_replayed(self):
        cfg = json.loads(json.dumps(self.config))
        cfg["plans"]["sha"]["activate"]["argv"] = [
            sys.executable,
            "-c",
            "raise SystemExit(1)",
        ]
        with self.assertRaises(RuntimeError):
            execute(cfg, self.root, self.message)
        with self.assertRaisesRegex(RuntimeError, "unknown"):
            execute(self.config, self.root, {**self.message, "epoch": 2})
        self.assertFalse(self.counter.exists())

    def test_wrong_region_and_uninstalled_plan_have_no_side_effect(self):
        for changes in [
            {"region": "us/prod"},
            {"worker_id": "gpu3"},
            {"manifest_sha256": "other"},
        ]:
            with self.assertRaises(ValueError):
                execute(self.config, self.root, {**self.message, **changes})
        self.assertFalse(self.counter.exists())

    def test_modified_hook_file_cannot_execute_or_replay(self):
        import hashlib

        script = self.root / "hook.py"
        script.write_text("print('{\"ok\": true}')")
        self.config["plans"]["sha"]["activate"] = {
            "argv": [sys.executable, str(script)]
        }
        with self.assertRaisesRegex(ValueError, "not bound"):
            execute(self.config, self.root, self.message)
        definition = self.config["plans"]["sha"]["activate"]
        definition["files"] = {
            str(script): hashlib.sha256(script.read_bytes()).hexdigest()
        }
        execute(self.config, self.root, self.message)
        script.write_text("raise SystemExit(1)")
        with self.assertRaisesRegex(ValueError, "changed"):
            execute(self.config, self.root, {**self.message, "epoch": 2})

    def test_completed_action_is_bound_to_its_original_definition(self):
        execute(self.config, self.root, self.message)
        self.config["plans"]["sha"]["activate"]["timeout"] = 60
        with self.assertRaisesRegex(ValueError, "definition changed"):
            execute(self.config, self.root, {**self.message, "epoch": 2})
        self.assertEqual(self.counter.read_text(), "1")
