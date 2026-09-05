# AgentsPool Python SDK

A synchronous, dependency-free Python 3.10+ client for the compatibility
AgentsPool Admin and Task APIs. It uses only the standard library. New Agent
integrations should use the `agentspool-compute` Skill and `agentspool tool`
instead of treating this SDK as a runtime adapter.

## Install

From the repository:

```bash
python -m pip install ./sdk/python
```

Or run directly without installation:

```bash
PYTHONPATH=./sdk/python python examples/python/submit_task.py
```

## Environment

```bash
export AGENTSPOOL_SERVER=http://router.lan:8090
export AGENTSPOOL_ADMIN_TOKEN='replace-me'
```

## Submit and follow logs

```python
from agentspool_sdk import AgentsPoolClient

client = AgentsPoolClient()
response = client.submit_task({
    "name": "sdk-demo",
    "executor": {
        "kind": "shell",
        "script": "echo hello from $(hostname); mkdir -p out; date -Iseconds > out/time.txt",
    },
    "outputs": [{"pattern": "out/**", "required": True}],
})

def print_log(event):
    print(f"[{event.get('stream', 'system')}] {event.get('message', '')}")

detail = client.wait_task(response["task"]["id"], on_log=print_log)
print(detail["task"]["result"])
```

`wait_task` raises `TaskTerminalError` for `failed`, `cancelled`,
`dead_letter`, or `needs_review` by default. Pass `raise_on_failure=False` when
you want to inspect those terminal states directly.

Downloads are written to a temporary sibling file, flushed, and atomically
renamed into place. Existing destinations are protected unless
`overwrite=True` is explicit.

Terminal Task history and logs can be removed with `delete_task()`. Artifact
deletion refuses live metadata references unless `force=True`; orphan cleanup
is a dry run through `gc_artifacts()` unless `delete=True` is explicit.
