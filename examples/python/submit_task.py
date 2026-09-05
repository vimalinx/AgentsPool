#!/usr/bin/env python3
"""Exercise the compatibility Task API through the dependency-free SDK."""

from __future__ import annotations

import argparse
from pathlib import Path

from agentspool_sdk import AgentsPoolClient, TaskTerminalError


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--server", help="defaults to AGENTSPOOL_SERVER")
    parser.add_argument("--token", help="defaults to AGENTSPOOL_ADMIN_TOKEN")
    parser.add_argument("--output-dir", default="./agentspool-results")
    args = parser.parse_args()

    client = AgentsPoolClient(args.server, args.token)
    response = client.submit_task(
        {
            "name": "python-sdk-task-demo",
            "idempotency_key": "python-sdk-task-demo-v1",
            "executor": {
                "kind": "shell",
                "script": (
                    "set -eu\n"
                    "echo \"worker=$(hostname)\"\n"
                    "mkdir -p result\n"
                    "printf 'AgentsPool SDK task completed on %s\\n' \"$(hostname)\" "
                    "> result/report.txt\n"
                ),
            },
            "outputs": [
                {
                    "pattern": "result/**",
                    "required": True,
                    "media_type": "text/plain; charset=utf-8",
                }
            ],
        }
    )
    task_id = response["task"]["id"]
    print(f"task={task_id} created={response['created']}")

    def on_log(event: dict[str, object]) -> None:
        stream = event.get("stream", "system")
        message = event.get("message", "")
        print(f"[{stream}] {message}")

    try:
        detail = client.wait_task(task_id, timeout=180, on_log=on_log)
    except TaskTerminalError as error:
        print(error)
        return 1

    result = detail["task"].get("result") or {}
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    for artifact_id in result.get("output_artifact_ids", []):
        artifact = client.get_artifact(artifact_id)
        destination = output_dir / artifact["name"]
        client.download_artifact(artifact_id, destination, overwrite=True)
        print(f"downloaded {artifact_id} -> {destination}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
