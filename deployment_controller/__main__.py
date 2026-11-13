"""python -m deployment_controller --config regions.json apply plan.json --operation ID"""

import argparse
import json
import os
from pathlib import Path
import sys
import time
from .controller import Controller, validate
from .gateway import Gateway
from .ledger import Ledger


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--config", required=True)
    sub = p.add_subparsers(dest="command", required=True)
    apply = sub.add_parser("apply")
    apply.add_argument("manifest")
    apply.add_argument("--operation", required=True)
    apply.add_argument("--region", action="append")
    status = sub.add_parser("status")
    status.add_argument("--operation", required=True)
    status.add_argument("--region", required=True)
    watch = sub.add_parser("watch")
    watch.add_argument("--region", required=True)
    watch.add_argument("--interval", type=float, default=10)
    args = p.parse_args()
    config = json.loads(Path(args.config).read_text())
    if args.command == "watch":
        cfg = config["regions"][args.region]
        while True:
            discovery = Ledger(os.environ[cfg["database_url_env"]], args.region)
            for row in discovery.pending():
                try:
                    ledger = Ledger(os.environ[cfg["database_url_env"]], args.region)
                    gateway = Gateway(
                        cfg["gateway_url"], os.environ[cfg["admin_token_env"]], ledger
                    )
                    worker = cfg["workers"][row["worker_id"]]
                    result = Controller(ledger, gateway, worker["agent_argv"]).run(
                        row["operation_id"], row["manifest"]
                    )
                    print(
                        json.dumps(
                            {
                                "operation_id": row["operation_id"],
                                "phase": result["phase"],
                                "region": args.region,
                            }
                        ),
                        flush=True,
                    )
                except Exception as exc:
                    print(
                        json.dumps(
                            {
                                "operation_id": row["operation_id"],
                                "status": "incomplete",
                                "error": type(exc).__name__ + ": " + str(exc),
                            }
                        ),
                        flush=True,
                    )
            time.sleep(max(1, args.interval))
    if args.command == "status":
        cfg = config["regions"][args.region]
        row = Ledger(os.environ[cfg["database_url_env"]], args.region).status(
            args.operation
        )
        print(json.dumps(row, default=str))
        return
    data = json.loads(Path(args.manifest).read_text())
    plans = data.get("targets", [data])
    failed = False
    if args.region:
        missing = set(args.region) - {v["region"] for v in plans}
        if missing:
            raise ValueError("Requested region is absent from plan")
        plans = [v for v in plans if v["region"] in args.region]
    # Validate every target before starting any mutation. Never infer a region
    # or use another region's credentials after connection failure.
    for plan in plans:
        validate(plan, plan["region"])
        cfg = config["regions"][plan["region"]]
        for name in ["admin_token_env", "database_url_env"]:
            if not os.environ.get(cfg[name]):
                raise ValueError("Missing credential environment variable " + cfg[name])
        if not cfg["workers"][plan["worker_id"]]["agent_argv"]:
            raise ValueError("Worker transport is not configured")
    for plan in plans:
        region = plan["region"]
        cfg = config["regions"][region]
        operation = (
            args.operation
            if "targets" not in data
            else args.operation
            + "-"
            + __import__("hashlib")
            .sha256((region + "\0" + plan["worker_id"]).encode())
            .hexdigest()[:12]
        )
        try:
            ledger = Ledger(os.environ[cfg["database_url_env"]], region)
            gateway = Gateway(
                cfg["gateway_url"], os.environ[cfg["admin_token_env"]], ledger
            )
            row = Controller(
                ledger, gateway, cfg["workers"][plan["worker_id"]]["agent_argv"]
            ).run(operation, plan)
            print(
                json.dumps(
                    {
                        "operation_id": operation,
                        "region": region,
                        "worker_id": plan["worker_id"],
                        "phase": row["phase"],
                    },
                    default=str,
                ),
                flush=True,
            )
        except Exception as exc:
            failed = True
            print(
                json.dumps(
                    {
                        "operation_id": operation,
                        "region": region,
                        "worker_id": plan["worker_id"],
                        "status": "incomplete",
                        "error": type(exc).__name__ + ": " + str(exc),
                    }
                ),
                flush=True,
            )
    if failed:
        sys.exit(1)


if __name__ == "__main__":
    main()
