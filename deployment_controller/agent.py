"""Host-local fenced executor. Invoke over SSH; no gateway credentials on workers."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def atomic(path, value):
    fd, name = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(value, f, sort_keys=True)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def execute(config, root, message):
    if (
        message["region"] != config["region"]
        or message["worker_id"] != config["worker_id"]
    ):
        raise ValueError("Agent belongs to another worker or regional control domain")
    plan = config["plans"].get(message["manifest_sha256"])
    if plan is None:
        raise ValueError("Plan was not installed on this agent")
    action = message["action"]
    epoch = message["epoch"]
    operation = message["operation_id"]
    if type(epoch) is not int or epoch < 1:
        raise ValueError("Invalid fencing epoch")
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root / "agent.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        statepath = root / "state.json"
        state = (
            json.loads(statepath.read_text())
            if statepath.exists()
            else {"epoch": 0, "operation_id": "", "actions": {}}
        )
        if epoch < state["epoch"] or (
            epoch == state["epoch"] and operation != state["operation_id"]
        ):
            raise ValueError("Stale deployment controller")
        if epoch > state["epoch"]:
            state.update(epoch=epoch, operation_id=operation)
            atomic(statepath, state)
        if action == "fence":
            return {"status": "fenced", "epoch": epoch}
        if action not in ["prepare", "idle", "activate", "verify", "stop"]:
            raise ValueError("Unknown action")
        definition = plan[action]
        command = definition["argv"]
        timeout = definition.get("timeout", 600)
        if not command or not all(isinstance(v, str) for v in command):
            raise ValueError("Hook requires an argv array")
        key = operation + ":" + message["manifest_sha256"] + ":" + action
        mutation = action in ["activate", "stop"]
        previous = state["actions"].get(key)
        if mutation and previous:
            if previous["status"] != "completed":
                raise RuntimeError(
                    "Hook outcome is unknown; inspect the host before recovery"
                )
            return previous["receipt"]
        if mutation:
            state["actions"][key] = {
                "status": "running",
                "command_sha256": hashlib.sha256(
                    json.dumps(command).encode()
                ).hexdigest(),
            }
            atomic(statepath, state)
        # Hooks are trusted host-installed argv, never shell text supplied over the wire.
        proc = subprocess.run(
            command,
            cwd=definition.get("cwd"),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            text=True,
        )
        log = root / (hashlib.sha256(key.encode()).hexdigest() + ".log")
        log.write_text(proc.stdout + "\n" + proc.stderr)
        log.chmod(0o600)
        if proc.returncode:
            raise RuntimeError(
                "Agent hook failed: "
                + action
                + " (exit "
                + str(proc.returncode)
                + "); inspect host logs"
            )
        result = json.loads(proc.stdout)
        if not isinstance(result, dict) or result.get("ok") is not True:
            raise RuntimeError("Hook did not confirm completion: " + action)
        result.update(
            operation_id=operation,
            worker_id=config["worker_id"],
            region=config["region"],
            action=action,
            manifest_sha256=message["manifest_sha256"],
        )
        if mutation:
            state["actions"][key] = {"status": "completed", "receipt": result}
            atomic(statepath, state)
        return result


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--config", required=True)
    p.add_argument("--state", required=True)
    args = p.parse_args()
    config = Path(args.config)
    if config.stat().st_mode & 0o022:
        raise ValueError("Agent config must not be writable by group or others")
    message = json.load(sys.stdin)
    result = execute(json.loads(config.read_text()), args.state, message)
    print(json.dumps(result))


if __name__ == "__main__":
    main()
