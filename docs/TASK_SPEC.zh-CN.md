# Task Spec 参考

Task API 接收一个 JSON `TaskSpec`。服务端拒绝未知字段，并填充默认值。

## 完整结构

```json
{
  "name": "string, required",
  "queue": "default",
  "priority": 0,
  "max_attempts": 1,
  "timeout_seconds": 0,
  "retry_backoff_seconds": 5,
  "retry_on_failure": false,
  "side_effect_mode": "none",
  "idempotency_key": "optional",
  "requirements": {
    "capabilities": [],
    "os": "",
    "arch": "",
    "min_cpu_threads": 0,
    "min_memory_mb": 0,
    "min_disk_mb": 0,
    "labels": {},
    "allowed_node_ids": [],
    "excluded_node_ids": [],
    "preferred_node_ids": []
  },
  "executor": {
    "kind": "shell | process | docker",
    "script": "shell only",
    "command": ["process executable or docker command", "arg"],
    "image": "docker only",
    "env": {"KEY": "value or ${secret:NAME}"},
    "work_dir": "relative/path",
    "network": "none | bridge | host",
    "cpu_limit": 0,
    "memory_mb": 0,
    "user": "",
    "read_only_root": false
  },
  "inputs": [
    {"artifact_id": "art_xxx", "path": "inputs/file.bin", "executable": false}
  ],
  "outputs": [
    {"pattern": "outputs/**/*", "required": false, "media_type": "application/octet-stream"}
  ],
  "labels": {},
  "metadata": {}
}
```

## 顶层字段

### `name`

必填，最多 200 字符。用于 UI、日志和审计，不参与调度。

### `queue`

默认 `default`，最多 64 字符。Worker 用 `--queues` 声明自己从哪些业务队列领取；队列不由 GPU 检测结果自动创建或选择。

### `priority`

范围 `-1000..1000`。优先级在 placement score 之前，因此高优先级任务不会因为另一任务的数据已经缓存而被越过。

### `max_attempts`

默认 1，必须为正整数，没有人为上限。每次成功 claim 就消耗一次 attempt，包括在真正 start 前 Worker 失联的情况。

### `timeout_seconds`

默认 `0`，表示不设置执行截止时间。只有调用方显式给出正数时，Worker 才在对应秒数后终止进程组并以失败上报；正数没有人为上限，最终受节点硬件和操作系统约束。

### `retry_backoff_seconds`

默认 5，必须为正整数，没有人为上限。第 N 次失败后的延迟近似：

```text
base * 2^(attempt-1)
```

实现只在 Go `time.Duration` 的数值表示边界处做饱和保护，不设置人为的小时级封顶。

### `retry_on_failure`

控制命令明确返回失败时是否自动重试。Lease 丢失是基础设施失败，只要安全且 attempt 预算未耗尽，即使该字段为 false 也会重试。

### `side_effect_mode`

- `none`：默认。
- `idempotent`：允许自动重放。
- `non_idempotent`：开始执行后失联不自动重放；明确返回失败时同样不自动重试。

### `idempotency_key`

最多 256 字符。相同 key 再次提交会返回原 Task，而不是创建新 Task。当前 key 在整个控制面命名空间全局唯一且不自动过期，因此建议加入上层业务域：

```text
content-publish:x:post-2026-08-30-001
```

它只防重复提交，不保证外部副作用 exactly-once。

### `labels` 与 `metadata`

各最多 64 项。Task labels 用于上层分类；metadata 用于传递可审计上下文。当前调度使用的是 `requirements.labels`，不是 Task 自身 labels。

## Requirements

### `capabilities`

必须全部存在于 Worker 离散能力中。内置常见值：

```text
executor.shell
executor.process
executor.docker
runtime.python
runtime.node
tool.git
tool.ffmpeg
gui.session
```

Worker 可用 `--capabilities browser.playwright,office.onlyoffice` 发布自定义能力。

服务端会根据 executor 自动补充对应 `executor.*` capability。GPU 型号、驱动、显存和可用量只在 Worker 明确启用检测时记录到 Node 遥测字段，不进入 capability 集合。

### `os` / `arch`

与 Go 平台名比较，不区分大小写，例如 `linux`、`windows`、`amd64`、`arm64`。

### 数量资源

- `min_cpu_threads` 与节点静态逻辑线程数比较。
- `min_memory_mb` 与 claim 时可用内存比较。
- `min_disk_mb` 与 Worker 数据盘可用空间比较。

这是 admission-time 快照，不是硬资源预留。要限制实际消耗，Docker 任务同时设置 `cpu_limit` 和 `memory_mb`；原生任务需使用 systemd/cgroup 或独立 Worker 账号进一步限制。

GPU 遥测不参与硬过滤、评分、队列选择或 Executor 参数生成。API 会拒绝旧版 `gpu_count`、`min_gpu_vram_mb`、`preserve_gpu_nodes` 和 `executor.gpu` 字段。

### `labels`

精确匹配节点标签，例如：

```json
{"room": "office", "trusted": "true"}
```

### 节点名单

- `allowed_node_ids` 非空时，只允许其中节点。
- `excluded_node_ids` 始终排除。
- `preferred_node_ids` 只是显著加分，节点不满足硬要求时不会强行选择。

## Executor

### Shell

```json
{
  "kind": "shell",
  "script": "set -eu\npython3 main.py"
}
```

通过 Worker 配置的 shell 运行，Linux 默认 `/bin/sh -lc`。脚本最多 1 MiB。

### Process

```json
{
  "kind": "process",
  "command": ["python3", "main.py", "--input", "inputs/data.json"]
}
```

不经过 shell 展开，参数边界稳定，优先用于结构化程序调用。

### Docker

```json
{
  "kind": "docker",
  "image": "python:3.13-slim",
  "command": ["python", "/workspace/main.py"],
  "network": "none",
  "cpu_limit": 2,
  "memory_mb": 4096,
  "read_only_root": true
}
```

工作区以读写方式挂载到 `/workspace`。镜像拉取由 Docker CLI 自己完成，其输出进入执行日志。可选字段：

- `network` 默认 `none`。`bridge` 需要 Worker `--allow-network=true`；`host` 还需要 `--allow-host-network=true`。
- `user` 传给 `docker run --user`。
- `read_only_root` 增加 `--read-only`，并提供受限 `/tmp` tmpfs。

## Environment 与 Secrets

普通值直接传递：

```json
{"env": {"MODE": "batch"}}
```

秘密引用：

```json
{"env": {"INTERNAL_SERVICE_TOKEN": "${secret:INTERNAL_SERVICE_TOKEN}"}}
```

Secret 只在执行 Worker 本地解析。Docker 执行器不会把值拼进命令行，而是使用执行期间存在的私有 env-file；Docker 环境值不能包含换行或 NUL，多行凭据应编码或改用 Artifact。调度器目前不知道哪台 Worker 拥有哪些 Secret；需要用节点 label 或自定义 capability 保证放置，例如：

```text
--labels secrets=internal-service
```

再声明：

```json
{"requirements": {"labels": {"secrets": "internal-service"}}}
```

## Inputs

每个 input 引用已经存在的 Artifact。`path` 必须是工作区内相对路径，父目录自动创建。`executable=true` 将物化文件权限设为 `0700`，否则 `0600`。

输入会从 Worker cache **复制**，不是 hard link，因此任务修改输入不会污染缓存。

## Outputs

`pattern` 支持：

- `*`：同一级任意字符，不跨 `/`。
- `?`：同一级一个字符。
- `**`：可跨目录。
- `**/`：零层或多层目录。

示例：

```text
result.json             只匹配根文件
outputs/*.txt           匹配 outputs 直接子文件
outputs/**/*.txt        匹配 outputs 下任意深度 txt
**/*                    匹配所有普通文件
```

符号链接和非普通文件会被跳过。`required=true` 且没有匹配时，任务被判定失败。Worker 和服务端默认不添加 output/Artifact 字节上限，只受真实硬件容量约束；管理员仍可显式配置正数防御性上限。

输出 Artifact metadata 中保存原始 `relative_path`，下载后可由上层恢复目录结构。

## 推荐任务模板

### 纯计算

```json
{
  "max_attempts": 3,
  "retry_on_failure": true,
  "side_effect_mode": "idempotent"
}
```

### 现实副作用

```json
{
  "max_attempts": 1,
  "retry_on_failure": false,
  "side_effect_mode": "non_idempotent",
  "idempotency_key": "publish:platform:business-object-id"
}
```

任务本身仍应调用支持幂等键的外部 API，并把远端 receipt 保存为输出 Artifact。
