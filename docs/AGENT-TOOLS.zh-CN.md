# AgentsPool 通用 Agent 工具

AgentsPool 不适配具体 Agent 运行时。Codex、Claude、OMP、Pi 或其他 Agent 都调用同一个：

```text
agentspool tool <tool-name> --input '<json>'
```

项目的 `.agents/skills/agentspool-compute/SKILL.md` 是标准发现入口。Agent 自己保留 Provider、模型、Token、Prompt、上下文、Session 和工具循环；AgentsPool 不启动 Agent、不解析运行时事件，也不接收这些数据。支持其他 Skill 目录的运行时可以安装同一份 Skill，不需要编写品牌适配器。

Agent 加载 Skill 后先在本地发现目录，不需要连接控制面或提供令牌：

```sh
agentspool tool list --json
agentspool tool list --category sandboxes --json
agentspool tool describe sandboxes.exec --json
```

目录带 `catalog_version`、权限 alternatives、是否修改状态、输入 JSON Schema 和示例。工具执行与目录共用同一份注册表；目录是当前二进制的机器可读权威，本文只解释稳定的对象关系。

## 对象与责任

| 对象 | 谁决定 | AgentsPool 做什么 |
| --- | --- | --- |
| Device | Worker 上报事实；人和 Agent 分层补充描述 | 发布实时资源、能力、带来源的用途说明和单调递增 revision |
| Placement | Agent | 根据任务和 Device 快照选择 `node_id` |
| ResourceLease | Agent 提需求 | 用 `expected_revision` 做并发准入，拒绝过期快照和超售 |
| Sandbox | Agent 请求 | 在所选节点创建持久 ext4 卷、cgroup 和 bwrap 执行边界 |
| Data | Agent 选择 | 提供显式 push/pull 与内容寻址 Artifact，不猜同步策略 |

GPU 只存在于 Node 遥测。资源请求没有 GPU 字段，沙盒 helper 不挂载 GPU 设备。

## 工具清单

- `devices.list`、`devices.get`、`devices.watch`、`devices.annotate`、`devices.unannotate`
- `leases.acquire`、`leases.list`、`leases.get`、`leases.renew`、`leases.resize`、`leases.release`
- `sandboxes.ensure`、`sandboxes.list`、`sandboxes.get`、`sandboxes.exec`
- `sandboxes.resize`、`sandboxes.rebind`、`sandboxes.start`、`sandboxes.stop`、`sandboxes.destroy`
- `services.list`
- `operations.get`、`operations.logs`、`operations.cancel`、`operations.wait`
- `files.push`、`files.pull`
- `artifacts.list`、`artifacts.delete`
- `maintenance.prune`

`devices.watch` 是最多等待 30 秒的 long-poll；节点注册、上下线、租约、描述或沙盒状态等 placement 语义变化会产生新 revision。高频 heartbeat 的 CPU/内存/磁盘观察值保持实时但不会制造 revision 或落盘；`devices.list/get` 总会读取当前内存遥测，资源准入也以当前状态为准。Agent 应把 placement 和 `leases.acquire.expected_revision` 绑定到同一快照。冲突后必须重新观察并重新决策。

### 人与 Agent 共同描述机器

`capabilities`、`observed` 和 `available` 是机器实时上报或控制面计算的事实；`descriptions` 是持久保存的用途说明。每条 description 都有 `source`、`author`、`basis_revision` 和更新时间，并可记录：

- `availability`：`unknown`、`always_on`、`scheduled`、`on_demand`；
- `service_policy`：`unknown`、`preferred`、`allowed`、`avoid`、`forbidden`；
- `preferred_workloads`、`avoid_workloads`、`constraints` 和自然语言说明。

`source=human` 只能来自人的明确指示，是 placement 的权威意图；`source=agent` 是有署名的建议，不能覆盖 human 条目。Worker 心跳不会覆盖任何 description。不同人或 Agent 的贡献分别保存，`devices.unannotate` 只删除匹配的 `source + author`。

这些描述不让 Server 变成隐藏调度器。当人明确说“任务必须在某台机器跑”时，Agent 必须从当前快照解析该机器的 `node_id`，并把它传给 `leases.acquire`；资源或能力不满足时应报告冲突，不能私自换机器。没有明确指定时，Agent 才结合 human 描述、实时资源和自己的建议选择节点。

## 权限

- allocator token：供普通 Agent 读取 Device/Service、维护自己的 Agent description，并首次申请资源；不能管理节点、Task、Artifact 或 human description。
- admin token：仅供所有者管理节点、Task、Artifact、human description 和显式历史清理；普通 Agent 不需要。
- resource lease token：只能查看、续租、调整或释放对应资源租约，并创建其沙盒。
- sandbox token：只能操作对应沙盒、等待其 Operation、push/pull 其文件。
- node token：只给 Worker，Agent 不应看到。
- root：只存在于本机 `agentspool-sandboxd`，经 `root:agentspool 0660` Unix socket 接受严格协议。

令牌优先通过文件 locator 传入，不放进命令参数、命令输出、仓库或沙盒文件：

- `AGENTSPOOL_ADMIN_TOKEN_FILE` / `--admin-token-file`；管理子命令使用 `--token-file`；
- `AGENTSPOOL_ALLOCATOR_TOKEN_FILE` / `--allocator-token-file`；
- `AGENTSPOOL_RESOURCE_LEASE_TOKEN_FILE` / `--resource-lease-token-file`；
- `AGENTSPOOL_SANDBOX_TOKEN_FILE` / `--sandbox-token-file`；
- Server 和 Worker 还支持 `AGENTSPOOL_JOIN_TOKEN_FILE` / `--join-token-file`。

文件必须是普通文件，只允许 owner 访问，或只读地共享给受控 group；CLI 接受 `0600`、`0400`、`0640`，拒绝 group 写入/执行和任何 other 权限。一个令牌同时配置明文值和文件 locator 时会直接报错，防止来源不明确。

当命令提供 resource lease token 或 sandbox token 时，CLI 会主动停止发送 admin Bearer；即使父进程仍设置了 `AGENTSPOOL_ADMIN_TOKEN`，一次调用也只使用对应的最小权限令牌。不要把 AgentsPool 令牌注入沙盒、日志或 Artifact。

CLI 默认不设置整次调用截止时间，`operations.wait`、`files.push` 和 `files.pull` 会等待真实结果。只有调用方显式传入正数 `--timeout` 时才启用总时限；`--timeout 0` 同样表示无限等待。

## 沙盒边界

每次 exec 都由 root helper 创建新的 mount/PID/IPC/UTS/cgroup namespace，随后在命名空间内降权为 `agentspool` 用户并清空 capabilities。`systemd-run` 施加 `CPUQuota`、`MemoryMax` 和 `TasksMax`；只有调用方显式给出正数 `timeout_seconds` 时才增加运行截止时间。持久 ext4 镜像施加 `disk_mb` 硬上限。默认只有最小 `/dev`，没有 NVIDIA/DRM 设备、宿主 home、SSH 配置或 `/etc/shadow`。

持久性指 `/workspace` 与 `/home/agent` 的文件跨 exec、Worker 重启和控制面重启保留。省略或传入 `0` 的 `ttl_seconds`/`timeout_seconds` 不设置人为截止时间。控制面重启时正在运行的 exec 短暂进入 `orphaned`；Worker 会读取真实 systemd unit，仍活动就恢复 `running` 并继续接受原 Operation Token 的终态回报，已消失则明确失败并撤回端点。结果回报会在控制面短暂不可用时重试。正常停止 Worker 会先停止领取并无限等待活动 Operation，不会私自添加 drain 截止；需要升级时应先显式取消长期服务。强制杀死 Worker/Helper 不承诺保住进程，但持久卷仍保留。`sandboxes.stop` 后当前租约有效时可用 `sandboxes.start` 恢复。有限租约到期后数据同样保留，Agent 可在同一节点申请新租约并 `sandboxes.rebind`。

网络权限是显式的：`network:false` 使用隔离网络命名空间；`network:true` 允许沙盒使用节点网络。它不等于自动获得任何服务凭据。

长期服务在 `sandboxes.exec.endpoints` 声明 `name/protocol/port/url/health_url`，健康地址必须与公开 URL 使用同一 host/port。Operation 进入 `running` 才发布到 `services.list`；查询时会实际探测声明的健康地址并返回 `healthy/unhealthy/unknown`，完成或取消自动撤回。同一节点的活动 Operation 不能声明相同端口。`operations.cancel` 取消单个 Operation，`sandboxes.stop/destroy` 会先请求取消活动 exec，再排队生命周期操作。

控制面不会按年龄或条数自动删除租约、沙盒或 Operation 历史。管理员可先调用 `maintenance.prune`（默认 `execute:false`）查看精确 ID，再显式执行；它可压缩活动 Sandbox 的终态 Operation、已失去主体或只由待清理 Operation 引用的 Artifact、已 destroy 的沙盒历史和旧观察噪声，非终态服务 Operation 始终保留。

## 数据连接

`files.push` 先把本地文件上传为内容寻址 Artifact，目标 Worker 校验下载后经固定 transfer root 写入持久卷。`files.pull` 反向导出为 Artifact，再由 sandbox token 下载。两条路径都拒绝 `..` 和符号链接穿越。`artifacts.list/delete` 让同一个 sandbox token 管理自己的桥接副本；destroy 自动清理无外部引用副本，共享内容仍有逻辑引用时保留。

多节点不会同时读写一个共享 POSIX 目录。Agent 负责选择 Git、Artifact push/pull 或网络服务，并负责冲突策略。

## alpha.4 生命周期与日志

同一 Sandbox 的执行、文件操作和扩缩容串行进行；运行中需要停止服务时调用 stop/cancel，等待返回 Operation 的终态后再继续。运行中 resize 返回冲突，不会提前释放账面资源。空闲 resize 成功后才提交 CPU/内存新额度；磁盘只增不减，失败的部分增长仍保守占用磁盘额度。

`stop` 成功表示进程已经退出并卸载磁盘，CPU/内存预留释放，磁盘仍保留；`start` 重新做资源准入，可能因为容量不足返回冲突。有限租约 `renew` 不带 `ttl_seconds` 时按原 TTL 从当前时间续期；显式 0 改为无限期。Worker 持续读取控制面的最新期限；网络中断时只信任最后确认的有限期限。

```sh
agentspool tool operations.logs --input '{"operation_id":"sbxop_example","stream":"stdout","after":0,"limit":65536}'
```

运行中也可读取日志。`after` 是字节偏移，下页使用 `next_after`；`data_base64` 保留原始字节，`data` 用于显示。`truncated=true` 表示启用了轮转后该游标以前的数据已丢弃，应从返回的 offset 继续。stdout/stderr 分别维护游标。Operation 的 stdout/stderr 只内联最后最多 64 KiB，并带 `output_truncated`；完整输出保存于独立日志文件，默认不自动删除。管理员可配置 Server `AGENTSPOOL_SANDBOX_LOG_RETAIN_BYTES` 和 helper `--log-retain-bytes` 显式轮转；两端各自生效，中央日志与 Worker 本地日志各占一份磁盘。中央日志随 maintenance.prune 删除，本地日志随 Sandbox destroy 删除。
