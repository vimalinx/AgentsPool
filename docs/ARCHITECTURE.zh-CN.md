# AgentsPool 架构说明

## 1. 核心定位

AgentsPool 是 Agent 可主动调用的计算协调底座，不是 Agent 宿主、Agent 网关或 Session 管理器。

```text
┌──────────────────────────────────────────────┐
│ 任意 Agent                                   │
│ Provider / Token / Prompt / Context / Session│
└──────────────────────┬───────────────────────┘
                       │ 发现 Skill，调用 JSON CLI
                       ▼
              agentspool tool <name>
                       │
                       ▼
┌──────────────────────────────────────────────┐
│ AgentsPool control plane                     │
│ Device revision / Admission / Lease / Op     │
└──────────────────────┬───────────────────────┘
                       │ Worker 主动心跳与领取
                       ▼
┌──────────────────────────────────────────────┐
│ Worker + sandboxd                            │
│ cgroup / namespace / persistent volume       │
└──────────────────────────────────────────────┘
```

核心链路只有：

```text
Device revision → ResourceLease → Persistent Sandbox → Operation → Artifact
```

这条链路不包含 Worksite、RuntimeProfile、AgentSession 或 AgentRun。Agent 何时调用、调用前后如何推理、是否继续 Session，都留在原运行时。

## 2. 不接管原则

AgentsPool 核心不得：

- 启动、停止或包装 OMP、Codex、Claude、Pi 等 Agent 进程；
- 读取或保存 Provider Token、Prompt、对话上下文和原生 Session；
- 拼接某个运行时的 CLI 参数或适配私有 RPC；
- 解析 Agent turn/tool/usage 事件并冒充运行时终态；
- 扫描用户 HOME、仓库或凭据目录以建立 Agent 索引；
- 为某个 Agent 单独增加常驻 Bridge。

如果一个功能需要知道“当前是什么 Agent”，它通常应位于 Agent 自己的 Skill 或上层工作流，而不是 AgentsPool 服务端。

## 3. 发现与接口

`.agents/skills/agentspool-compute/SKILL.md` 描述通用放置与生命周期协议。能识别项目 Skill 的 Agent 可直接发现它；其他运行时也可以安装同一份 Skill。它们最终都调用同一接口：

```text
agentspool tool <tool-name> --input <JSON>
```

CLI 的 JSON 输入输出是兼容边界。Skill 负责告诉 Agent 何时观察、如何避免 stale revision、何时续租和怎样清理；服务端不需要知道调用者品牌或 Session 格式。

## 4. Compute Tool 对象

### Device

Worker 上报节点 OS、架构、CPU、内存、磁盘、标签、能力和在线状态。控制面为整个 Device 视图维护单调递增 revision。

Device 还包含不会被心跳覆盖的分层 `descriptions`。`human` 条目保存人确认的可用时段、服务策略、优先/避免工作负载和硬约束，是权威意图；`agent` 条目保存带作者和 basis revision 的建议。两者不混写，也不自动变成服务端调度规则。

Agent 必须先观察 Device，再自行决定 placement。人明确指定设备时，Agent 使用该设备的 `node_id` 精确申请；未指定时才综合 human 描述、实时状态和 Agent 建议。服务端不替 Agent 猜任务需求。

### ResourceLease

Agent 使用选定 `node_id`、观察到的 `expected_revision` 以及 CPU/内存/磁盘需求申请租约。控制面只做原子准入：

- revision 已过期则拒绝，Agent 必须重新观察；
- 资源超售则拒绝，Agent 必须重新选点或降低需求；
- 成功后签发 scoped ResourceLease token；
- 租约可续期、调整或释放。

GPU 不属于 ResourceRequest，也不参与准入或 placement。

### Persistent Sandbox

一个 ResourceLease 最多绑定一个持久 Sandbox。Sandbox 固定在一个 Node，持有独立 ext4 镜像，并用 bwrap namespace、systemd cgroup、降权用户和 capability 清空限制每次 exec。

`/workspace` 和 `/home/agent` 跨 exec、Worker 重启和控制面重启保留。一个无截止时间的前台 exec 可以承载长期服务；正常 Worker drain 会等待它。控制面重启先把元数据标为 `orphaned`，Worker 随后读取确定性 systemd unit 的真实状态：仍活动则恢复 `running`，已消失则明确失败并撤回端点。强制终止 Worker 或 helper 不承诺保存进程。租约过期后数据仍在，但必须在同一 Node 获取新租约并 rebind 才能继续。

### Operation

ensure、exec、resize、rebind、start、stop、destroy 和文件传输都是显式 Operation。Agent 必须等待 Operation 终态，并检查 `status`、`exit_code`、`stdout`、`stderr` 和任务自己的结果；HTTP 成功不等于语义成功。

Operation 状态包含 `queued/claimed/running/orphaned/succeeded/failed/cancelled`。Agent 可显式取消；Worker 通过 heartbeat 接收请求并终止或检查确定性 systemd unit。exec 可声明服务端点，只有运行中或等待重连的 Operation 会出现在全局 Service 视图；`services.list` 按需探测显式声明的健康地址，不把陈旧 metadata 当作存活证明。

### Artifact

跨节点数据只通过显式 push/pull 或调用者选择的 Git/网络服务移动。AgentsPool 的 Artifact 内容寻址并校验 SHA-256，不猜测同步范围，也不提供跨节点共享 POSIX 文件系统。每个桥接 Artifact 记录 sandbox 所有权；Scoped token 可管理自己的副本，destroy 清理无外部引用副本，显式维护再压缩终态 Operation 和陈旧 metadata。

## 5. 控制面

`agentspool server` 包含：

- Device registry 与 revision 广播；
- ResourceLease 原子准入、可选期限与 scoped token；
- Sandbox/Operation 元数据与 Worker 代理；
- Artifact 存储；
- 兼容 Task Plane 的 Task、Execution、Lease、Event 状态机；
- HTTP API、结构化 CLI 和可发现 Skill。

当前使用单进程内存状态加原子 JSON 快照。关键修改写临时文件、fsync、rename；它适合可信家庭 LAN 的单控制面，不是 HA 数据库。

systemd 部署中的控制面使用独立 `agentspool-server` 用户；Worker 和 sandboxd 使用 `agentspool` 用户/组。两者不能合并，否则控制节点上的原生 Task 可能读取控制面状态、Artifact 或 Token 文件。

## 6. Worker 与 sandboxd

`agentspool worker` 以受限系统用户运行，主动注册、心跳、领取兼容 Task，并向本机 `sandboxd` 转发已授权 Operation。Worker Node Token 不交给 Agent。

`agentspool sandboxd` 是唯一 root 组件，只通过 `root:agentspool 0660` Unix socket 接受严格结构化协议。它不接收任意宿主路径，不挂载宿主 HOME、SSH 配置、NVIDIA/DRM 设备或 Provider 凭据。

`network:false` 创建隔离网络 namespace；`network:true` 使用节点网络，但不会自动注入任何服务 Token。

## 7. 权限分层

- Allocator token：读取 Device/Service、写 Agent description 并首次申请资源；
- Admin token：所有者管理节点、Task、Artifact、human description 和显式历史清理；
- ResourceLease token：只管理对应资源租约并创建其 Sandbox；
- Sandbox token：只管理对应 Sandbox、Operation 和文件；
- Node token：只属于 Worker；
- root：只存在于本机 sandboxd。

每次缩小权限后，Agent 应丢弃不再需要的上一级 Token。Provider Token 永远不应进入这些层级。

## 8. 兼容 Task Plane

旧 Task API 仍保留给确定性批处理：Worker claim、短期 Execution Lease、Shell/Process/Docker executor、日志、重试和 Artifact。它与 Compute Tool 共享 Device、Worker 和存储基础设施，但不是 Agent Session 管理器。

对不可逆副作用，Task Plane 的 `non_idempotent` 失联状态仍进入 `needs_review`，不会盲目重放。Agent 自己的外部副作用则由 Agent 自己治理。

## 9. 数据与恢复

- Sandbox 数据固定在创建 Node；控制面只保存元数据；
- stop 不删除数据，destroy 明确删除数据；
- 默认 ResourceLease 不到期；显式有限 Lease 到期不删除 Sandbox，rebind 需要同节点新租约；
- 控制面重启保留活动 exec claim 并等待原 Worker 回报；正常 Worker 重启先 drain，长期 Operation 需先显式取消；
- 多节点一致性由调用 Agent 选择 Git、Artifact 或网络服务实现。

## 10. 安全与范围

系统默认部署在可信 LAN/overlay。公网需要 TLS、反向代理和严格防火墙。bwrap/cgroup 是 Linux 内核隔离，不是硬件 VM 或敌对多租户边界。兼容 Task Plane 的 native executor 也不是沙箱。

GPU 仅可作为可选只读遥测：不做 GPU 资源申请、调度、执行、设备挂载、本地模型部署或模型分片。
