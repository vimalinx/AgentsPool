# AgentsPool 运维手册

## 1. 日常观察

### 池摘要

```bash
agentspool status
```

重点关注：

- online / draining / offline / disabled 节点数。
- queued / running / retry_wait / dead_letter / needs_review 任务数。
- Artifact 数量和存储字节。
- 控制面 uptime。

### 节点

```bash
agentspool nodes list
agentspool nodes get NODE_ID
```

`last_seen_at` 应按 Worker heartbeat 间隔持续刷新。`resources.observed_at` 明显滞后通常意味着 Worker 到控制面的网络、认证或服务进程有问题。

### 任务

```bash
agentspool tasks list --status queued
agentspool tasks list --status running
agentspool tasks get TASK_ID
agentspool tasks logs TASK_ID --follow
```

Task detail 同时返回所有 Execution 和活动 Lease。多次 Execution 说明发生过重试或人工重放。

### 事件

```bash
agentspool events --limit 200
agentspool events --type task.needs_review
```

事件默认不按条数自动截断，仍不替代长期日志归档。只有管理员显式配置正数 `AGENTSPOOL_EVENT_RETENTION` 才保留最近 N 条；控制面 stdout/journald 才是 HTTP 和内部错误的服务日志。

## 2. 节点生命周期

### 计划维护：drain

```bash
agentspool nodes drain NODE_ID
```

节点停止 claim，但当前 Execution 继续。等待活动作业清零后：

```bash
sudo systemctl stop agentspool-worker
```

维护结束：

```bash
sudo systemctl start agentspool-worker
agentspool nodes enable NODE_ID
```

Heartbeat 在 `draining` 时不会自动变回 online，必须管理员 enable。

### 临时断网：offline

在线节点超过 `node_timeout` 没有 heartbeat 会变为 offline。活动 Lease 由自己的 TTL 决定何时失效；两者是不同计时器。

网络恢复后，合法 Node Token 的 heartbeat 会把 offline 节点恢复 online。

### 安全封禁：disable

```bash
agentspool nodes disable NODE_ID
```

之后该 Node Token 的请求得到 403。运行中的本地进程会在续租被拒绝后终止。若该机器再次持有有效 Join Token，它仍可重新注册并轮换凭据，因此真正撤销时应同时轮换 Join Token、隔离机器并检查审计事件。

### 删除节点

```bash
agentspool nodes delete NODE_ID
```

只有没有活动 Lease 的节点才能删除。历史 Execution 仍保留 node_id 字符串作为证据。

## 3. 任务异常状态

### `retry_wait`

等待指数退避。到达 `next_eligible_at` 后 reaper 自动改回 queued。通常无需人工干预。

### `failed`

执行明确失败，且任务不允许自动重试。先查看日志和 `last_error`，修复输入或环境后：

```bash
agentspool tasks retry TASK_ID
```

若尝试数已达到上限：

```bash
agentspool tasks retry TASK_ID --reset-attempts
```

### `dead_letter`

自动重试预算耗尽或基础设施多次中断。不要机械 reset；先判断是否是某个节点反复崩溃、资源要求过低、镜像不可用或输出规则错误。

### `needs_review`

表示一个 `non_idempotent` Execution 已经开始，但控制面无法确定它是否完成外部副作用。

处理顺序：

1. 查看该 Execution 全部日志。
2. 检查外部系统是否已经产生邮件、帖子、订单、付款或删除。
3. 检查任务是否输出了远端 receipt、业务 idempotency key 或操作 ID。
4. 若外部动作未发生，显式重试：

```bash
agentspool tasks retry TASK_ID \
  --acknowledge-non-idempotent \
  --reset-attempts
```

5. 若动作已发生，不要重试；由上层记录为已完成或创建补偿任务。当前 Alpha 不提供“手工标记成功”API，保留 `needs_review` 是更诚实的审计结果。

## 4. 取消

排队任务取消立即生效。运行任务取消通过 Worker heartbeat 或 Lease renew 下发，延迟上限约为两者较短周期。Worker 向进程组发送终止信号，等待后确认。

如果 Worker 同时失联，控制面先记录 `cancel_requested`；Lease 到期时任务进入 cancelled，而不是重试。

Sandbox Operation 使用 `operations.cancel`。排队 Operation 立即变为 `cancelled`；运行中 Operation 由 heartbeat 下发并停止确定性 systemd unit。`sandboxes.stop` 或 `destroy` 会先请求取消同一 Sandbox 的活动 exec，再排队生命周期操作。控制面重启把活动 exec 暂时标为 `orphaned` 并保留 claim；Worker 下一次 heartbeat 会检查确定性 systemd unit，仍在运行就恢复为 `running`，已经消失就明确记为失败并撤回端点。短暂断线时终态回报会持续重试。

## 5. 调度排查

任务长期 queued 时逐项比较：

```bash
agentspool tasks get TASK_ID
agentspool nodes list
```

常见原因：

- Queue 不在任何 online Worker 的 `queues` 中。
- executor capability 不存在，例如 Worker 禁用了 Docker。
- label 精确值不匹配。
- `min_memory_mb` / `min_disk_mb` 高于当前可用量。
- allowed node 已 offline/draining/disabled。
- 所有匹配节点 active_jobs 达到 max_concurrency。

调度器目前没有“为什么没匹配”的 API；排查时使用 Task spec 和 Node detail 对照。未来可增加 dry-run placement explain，但不会改变现有调度语义。

## 6. 容量与磁盘

控制面目录：

```text
state.json                 Task/Node/Lease/Execution/Artifact/Event 元数据
logs/<execution>.jsonl     执行日志
artifacts/xx/<sha256>      Artifact 内容
```

Worker 目录：

```text
credentials.json
machine-id                 仅机器没有 /etc/machine-id 时
cache/art_*
jobs/<task>/<execution>/workspace
```

默认成功工作区删除，失败工作区保留。Artifact cache 默认不设人工容量上限。磁盘压力大时可由管理员显式设置：

```dotenv
AGENTSPOOL_WORKER_KEEP_FAILED_JOBS=false
AGENTSPOOL_WORKER_CACHE_MAX_BYTES=10737418240
```

Artifact 不自动定时 GC。普通 Agent 用 `artifacts.list` 查看本 Sandbox 的桥接副本，并在 Operation 终态后用 `artifacts.delete` 删除。Sandbox destroy 会清理没有外部引用的桥接 Artifact。管理员仍应观察全池 metadata 和数据目录用量；不要直接删除仍在 metadata 中的内容文件。先运行 `agentspool artifacts gc` 查看真正的孤儿内容，人工确认后再运行 `agentspool artifacts gc --execute`。管理端删除单个 Artifact 使用 `agentspool artifacts delete ARTIFACT_ID`；仅在明确接受历史引用被移除时使用 `--force`。

终态 Task 的定义、Execution metadata 和日志可用 `agentspool tasks delete TASK_ID` 清理；输出 Artifact 保持独立，需按业务保留策略另行删除。

大文件传输和普通控制请求使用不同上下文：

```dotenv
AGENTSPOOL_WORKER_REQUEST_TIMEOUT=30s
AGENTSPOOL_WORKER_TRANSFER_TIMEOUT=0
```

前者只用于 heartbeat、claim、renew 等短控制请求；后者用于输入下载和输出上传，`0` 表示不设置总截止。控制面和 Worker 默认也不设置 Artifact/output/cache 字节上限，实际受磁盘与文件系统容量约束。若部署面对不可信调用者，管理员可按风险显式配置正数上限。

终态 Sandbox 历史不会按时间或条数自动删除。先运行 `agentspool tool maintenance.prune --input '{"execute":false}'` 查看精确 Operation、Sandbox、Lease、Artifact 和旧观察事件，再由所有者确认后改为 `true`。它可以压缩仍在运行的长期 Sandbox 中已经终态的旧 Operation，但不会碰当前非终态服务；普通 Agent 不应索取 Admin Token 执行这一步。

## 7. 日志和指标

### systemd

```bash
journalctl -u agentspool-server --since today
journalctl -u agentspool-worker --since today
```

设置 JSON 服务日志：

```dotenv
AGENTSPOOL_LOG_FORMAT=json
AGENTSPOOL_LOG_LEVEL=info
```

### Prometheus

默认需要 Bearer Token：

```bash
curl -H "Authorization: Bearer $AGENTSPOOL_ADMIN_TOKEN" \
  http://router:8090/metrics
```

`AGENTSPOOL_PUBLIC_METRICS=true` 只应在指标端点已被网络层隔离时启用。

主要指标：

```text
agentspool_up
agentspool_nodes{status=...}
agentspool_tasks{status=...}
agentspool_http_requests_total
agentspool_claims_total
agentspool_completions_total
agentspool_failures_total
agentspool_lease_expirations_total
agentspool_artifacts_uploaded_total
agentspool_artifact_bytes_uploaded_total
agentspool_authentication_failures_total
```

## 8. 升级

1. 备份控制面数据目录。
2. `agentspool nodes drain` 需要维护的 Worker，或先停任务提交。
3. 替换控制面二进制并重启。
4. 控制面重启会结束兼容 Task Plane 的活动 Execution Lease；Sandbox exec 短暂进入 `orphaned`，随后由 Worker 根据真实 systemd unit 自动恢复 `running` 或明确失败。仍建议先等待普通 Task 结束。
5. 逐台替换 Worker。Node heartbeat 会更新 `agent_version`。
6. 运行 `agentspool status`、提交 hello task、验证 Artifact。

状态文件有 `schema_version`。程序拒绝读取比自身更新的 schema，避免旧二进制破坏新数据。降级前必须恢复相应版本备份。

## 9. 令牌轮换

### Admin Token

修改 server.env 并重启控制面。CLI 和监控立即改用新值；Worker 不受影响。

### Join Token

修改并重启控制面。已注册 Worker 不受影响。后续新节点使用新值。

### Allocator Token

修改 allocator token 文件并重启控制面，再原子替换各 Agent 可读的受保护 token 文件。已有 scoped ResourceLease/Sandbox Token 不受影响。

### 单个 Node Token

安全做法：

1. disable 节点。
2. 从 Worker 删除 `credentials.json`。
3. 确认 Worker 拥有当前 Join Token。
4. enable 或删除旧节点后重启 Worker；同 machine_id 注册会轮换 Node Token。
5. 从长期 Worker 环境移除 Join Token。

## 10. 服务级目标建议

家庭池可采用简单目标：

- 控制面开机自启，数据盘可用。
- Worker heartbeat 10 秒，Lease TTL 45 秒，Node timeout 35 秒。
- 长任务使用幂等输出和至少 2 次 attempt。
- 现实副作用任务一律明确 `non_idempotent`，并保存远端 receipt。
- 每日备份 state，按价值备份 Artifact；每周执行一条真实 E2E 任务。
