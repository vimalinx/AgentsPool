# 兼容 Task API 的 Python SDK

仓库提供一个 Python 3.10+ 的兼容 Admin/Task API SDK，位于 `sdk/python/`。它只使用 Python 标准库，适合批处理控制脚本。新 Agent 集成不应把 SDK 当运行时适配器；Agent 应发现 `agentspool-compute` Skill 并调用 `agentspool tool`。

## 安装

```bash
python -m pip install ./sdk/python
```

不安装也能直接运行：

```bash
PYTHONPATH=./sdk/python python examples/python/submit_task.py
```

设置控制面：

```bash
export AGENTSPOOL_SERVER=http://router.lan:8090
export AGENTSPOOL_ADMIN_TOKEN='your-admin-token'
```

## 最小调用

```python
from agentspool_sdk import AgentsPoolClient

pool = AgentsPoolClient()
created = pool.submit_task({
    "name": "repo-analysis",
    "requirements": {
        "capabilities": ["runtime.python"],
        "min_memory_mb": 8192
    },
    "executor": {
        "kind": "shell",
        "script": "python3 analyze.py --input inputs/repo.tar.gz --out outputs/report.json"
    },
    "inputs": [
        {"artifact_id": "art_xxx", "path": "inputs/repo.tar.gz"}
    ],
    "outputs": [
        {"pattern": "outputs/**/*.json", "required": True, "media_type": "application/json"}
    ]
})

task_id = created["task"]["id"]
detail = pool.wait_task(task_id, timeout=1800)
print(detail["task"]["result"])
```

## 跟随日志

```python
def on_log(event):
    print(f"[{event.get('stream')}] {event.get('message')}")

pool.wait_task(task_id, on_log=on_log)
```

SDK 按 Execution 分别维护日志游标。任务重试形成新的 Execution 时，游标会重置，因此第二次尝试的日志不会因为第一次尝试的序号更大而被跳过。

## 上传与下载 Artifact

```python
artifact = pool.upload_artifact("dataset.zip")
artifact_id = artifact["id"]

pool.download_artifact(
    artifact_id,
    "downloads/dataset.zip",
    overwrite=False,
)
```

下载先写同目录临时文件，刷新后再原子替换目标路径。默认拒绝覆盖已有文件，以减少调用方拼错 Artifact ID 或路径时的数据损失。

## 失败处理

`wait_task` 默认只把 `succeeded` 视为成功。`failed`、`cancelled`、`dead_letter` 和 `needs_review` 会抛出 `TaskTerminalError`，异常对象的 `task` 字段包含完整任务状态。

```python
from agentspool_sdk import TaskTerminalError

try:
    detail = pool.wait_task(task_id)
except TaskTerminalError as exc:
    print(exc.task["status"], exc.task.get("last_error"))
```

对于 `non_idempotent` 任务，人工确认副作用未发生后才能显式重试：

```python
pool.retry_task(
    task_id,
    reset_attempts=True,
    acknowledge_non_idempotent=True,
)
```

这只是管理员确认机制，不会自动证明外部副作用是否发生；真实业务仍应使用外部系统的幂等键、事务或业务去重。

## API 方法

主要方法包括：

- `status()`、`list_nodes()`、`get_node()`、`drain_node()`、`disable_node()`；
- `submit_task()`、`submit_task_file()`、`list_tasks()`、`get_task()`；
- `task_logs()`、`execution_logs()`、`wait_task()`；
- `cancel_task()`、`retry_task()`；
- `delete_task()`；
- `upload_artifact()`、`list_artifacts()`、`get_artifact()`、`download_artifact()`；
- `delete_artifact(force=False)`、`gc_artifacts(delete=False)`；
- `events()`。

所有服务端错误都被还原为 `APIError(status_code, code, message)`；网络连接错误为 `TransportError`。完整底层合约见 [`../api/openapi.yaml`](../api/openapi.yaml)。
