# AgentsPool 0.4.0-alpha.4 实现与验收状态

## 当前产品边界

0.4 的唯一 Agent 集成面是项目 Skill 加 `agentspool tool`。AgentsPool 不拥有 Provider Token、Prompt、上下文、Session 或 Agent 生命周期，也不主动启动任何 Agent 进程。

```text
任意 Agent → agentspool-compute Skill → agentspool tool
          → Device → ResourceLease → Sandbox → Operation → Artifact
```

## 已实现

- revisioned Device list/get/watch，包含 CPU、内存、磁盘、架构、标签、在线状态和分层 descriptions；
- `devices.annotate/unannotate`，分别保存人类权威意图和有署名、带 basis revision 的 Agent 建议，Worker 心跳不会覆盖；
- Agent 自选节点，并用同一 Device revision 做乐观并发资源准入；
- CPU、内存、磁盘 ResourceLease 的 acquire/list/get/renew/resize/release；
- 持久 Sandbox 的 ensure/list/get/exec/resize/rebind/start/stop/destroy；
- `running/orphaned/cancelled` Operation 生命周期、显式取消、控制面重启后按真实 systemd unit 自动协调和不同 Sandbox 并发执行；
- exec 声明 LAN endpoint 与带实时 `health_url` 探测的 `services.list` 服务发现；
- systemd cgroup、bwrap namespace、降权执行和独立 ext4 磁盘硬上限；
- scoped allocator、ResourceLease、Sandbox、Operation token 与所有者 Admin 边界；
- 内容寻址 Artifact、显式 files.push/files.pull 和 sandbox-scoped Artifact list/delete；
- 终态 Task/Execution/日志删除、引用安全 Artifact 删除和默认 dry-run 的孤儿 GC；
- 活动/已销毁 Sandbox 终态 Operation、陈旧桥接 Artifact 和旧观察事件的 dry-run-first `maintenance.prune`，不按时间或条数自动删除；
- 沙箱 destroy 自动清理无外部引用的桥接 Artifact；
- 空闲 heartbeat/claim 仅更新易失遥测或执行只读检查，不再改 revision 或整库落盘；
- 控制面 age 加密异机备份、独立 Worker ext4 加密备份/恢复与 systemd TLS 到期检查；
- 可被支持项目 Skill 的 Agent 自动发现的 `agentspool-compute` Skill；
- 与执行注册表同源的 `tool list/describe` 本地目录，包含权限 alternatives、输入 JSON Schema 和示例；
- CLI、Server、Worker 和 scoped token 的权限受限文件 locator；
- 安装到共享目录的通用 Skill、CLI 与只保存 locator 的私有 client profile；
- 兼容保留的 Task API、Shell/Process/Docker Worker 和 Artifact 工作流；
- 单一静态 `agentspool` 二进制与 amd64、arm64、armv7、mipsle 发行包。

GPU 字段仅允许作为可选只读遥测。资源申请、准入、调度、沙盒设备和发行流程都不使用 GPU。

## 已移除

0.4 从源码、API、测试、systemd 单元、示例和发行包中删除了：

- `agentspool-agentd` Agent 控制面；
- `agentspool-bridge` 用户态 Session Bridge；
- `agentspool-agentctl` 管理器；
- Worksite、RuntimeProfile、AgentSession、AgentRun 对象；
- Pi RPC 适配、运行时参数拼装、Prompt 投递和事件解析；
- Agent Plane OpenAPI 与 fake runtime E2E。

这不是暂时关闭的功能，而是明确的架构边界。AgentsPool 后续也不应为 OMP、Codex、Claude、Pi 或其他运行时增加专用接管层。

## 验证入口

| 验证 | 覆盖范围 |
|---|---|
| `make check` | 仓库一致性、OpenAPI/路由、Python SDK、Go test、go vet |
| `make race` | Go race tests |
| `sudo python3 scripts/e2e-sandboxes.py dist/agentspool` | 真实 systemd/bwrap 生命周期、运行中续期、实时日志、停止后资源释放、加密卷恢复 |
| `sudo python3 scripts/e2e-install.py dist/release/*-linux-amd64.tar.gz` | 解压安装、共享程序升级、失败回滚、含活动服务的卸载 |
| `make e2e` | 真实本地 server/worker Task Plane，不使用 runtime fixture |
| `scripts/e2e-lan-sandboxes.sh` | 两种架构真实持久沙盒生命周期、限额、网络、传输与清理 |
| `make release` | 四架构单二进制构建、SPDX 2.3 SBOM、归档反向打开、校验和与私有目录排除 |
| `make release-verify` | 两次发行物逐字节复现、源码 ZIP 脱离 Git 后独立 check/build/release |

正式发行默认拒绝 dirty worktree；只做本地打包测试时可显式使用 `AGENTSPOOL_ALLOW_DIRTY_RELEASE=1 make release`。源码 ZIP 离开 Git metadata 后仍能运行 check、build 和 release。Tag 工作流还会为归档生成 GitHub build provenance 与 SBOM attestation。

真实 Provider Token、模型用量和 Agent Session 需要在对应 Agent 运行时内单独验收。AgentsPool 的成功只证明计算协调与沙盒操作成功。

## 当前 LAN 状态边界

项目提供可参数化的三节点 LAN 验收脚本，用于检查 shell-only、并发 1、GPU 检测关闭、注册后移除 Join Token、重启恢复、跨节点并发 Task、持久 Sandbox 全生命周期和最终零活动租约/沙盒/挂载。公开仓库刻意不保存维护者的真实主机名、地址、节点 ID 或验收对象 ID；正式部署应在自己的受控环境重新执行并留存结果。

## Alpha 限制

- 单控制面 JSON 快照存储，不提供 HA 或多控制面事务；
- bwrap/cgroup 沙盒是 Linux 内核隔离，不是硬件 VM 或敌对多租户边界；
- 持久数据固定在创建节点，不提供跨节点共享 POSIX 卷；
- `network:true` 使用节点网络，不自动提供任何外部凭据；
- 旧 Task Plane 的 native executor 仅适用于可信任务；
- Task、Execution 和日志当前不按时间自动过期；Sandbox bridge Artifact 可由 scoped Agent 显式删除并在 destroy 时清理，跨对象历史仍由管理员 dry-run 后显式维护；
- Device description 是 placement 提示而非服务端隐藏调度规则；Agent 仍需把人指定的机器解析为明确 `node_id`；
- 不部署本地模型，不做 GPU 调度、GPU 执行或模型分片。
