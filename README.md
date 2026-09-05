# AgentsPool

AgentsPool 是一个面向任意 Agent 的轻量局域网计算协调层。它只提供标准 Skill、结构化 CLI 和受限执行环境，不启动、不托管、不接管任何 Agent 运行时。

```text
Codex / Claude / OMP / Pi / 其他 Agent
                  │ 自行发现 agentspool-compute Skill
                  ▼
          agentspool tool <name>
                  │
                  ▼
 Device revision → ResourceLease → Persistent Sandbox → Operation → Artifact
```

当前版本：**0.4.0-alpha.4**。

## 核心边界

| Agent 自己负责 | AgentsPool 负责 |
|---|---|
| Provider、模型、Token | 实时 Device 视图与 revision |
| Prompt、上下文、Session | CPU、内存、磁盘资源准入与租约 |
| Agent 工具循环与事件 | 持久沙盒、隔离执行与生命周期 |
| 何时调用、如何判断结果 | 显式文件传输与 Artifact |

AgentsPool 不包含 OMP、Codex、Claude、Pi 等运行时适配器，不保存 Prompt 或 Agent Session，不解析运行时事件，也不会用后台 Bridge 主动接管用户的仓库、HOME 或凭据。

旧版 `agentspool-agentd`、`agentspool-bridge`、`agentspool-agentctl` 及 Agent Plane API 已从 0.4 源码、构建和发行物中移除。升级前请阅读 [0.4 迁移说明](docs/MIGRATION-v0.4.zh-CN.md)。

## Agent 如何使用

项目自带可发现 Skill：`.agents/skills/agentspool-compute/SKILL.md`。支持项目 Skill 的 Agent 进入仓库后可自行加载；其他 Agent 只需获得同一份 Skill 和 `agentspool` CLI，无需写专用适配器。

要让同一用户下、任意工作目录中的 Agent 都能发现它，可安装共享 CLI、Skill 和统一 client profile：

```sh
./scripts/install-client.sh ./dist/agentspool https://pool.lan:8090 /run/secrets/agentspool-allocator
```

profile 默认位于 `$XDG_CONFIG_HOME/agentspool/client.json`，只保存 Server URL 和 Token 文件 locator，不保存 Token 内容。Agent 先用 allocator 凭据读取 Device 并申请 Lease，随后只使用返回的 scoped Lease/Sandbox Token。

```sh
agentspool tool list --json
agentspool tool describe leases.acquire --json

export AGENTSPOOL_ALLOCATOR_TOKEN_FILE=/run/secrets/agentspool-allocator
agentspool tool devices.list

agentspool tool leases.acquire \
  --input '{"node_id":"NODE_ID","owner":"agent-session","expected_revision":42,"resources":{"cpu_millis":500,"memory_mb":512,"disk_mb":256}}'
```

ResourceLease 与 sandbox exec 默认不设置时间截止；只有 Agent 显式传入正数 `ttl_seconds` 或 `timeout_seconds` 才启用期限。正数没有人为上限，实际资源仍由节点硬件容量和 cgroup 准入约束。

`tool list/describe` 完全在本机返回带权限和输入 JSON Schema 的版本化目录，不需要连接控制面。之后由 Agent 用返回的 scoped token 创建或复用沙盒、执行命令、移动文件、续租、扩容和清理。完整对象与命令见 [通用 Agent 工具](docs/AGENT-TOOLS.zh-CN.md)。

Device 快照同时公开机器自动探测事实和带来源的 `descriptions`：人可以规定开机规律、长期服务策略、优先/禁止工作负载和硬约束；Agent 只能另行添加有署名、带 revision 的建议，不能覆盖人的条目。人明确指定机器时，Agent 使用该机器的 `node_id` 精确申请资源；未指定时才自行综合描述和实时状态选机。

## 组件

- `agentspool server`：Device registry、资源准入、租约、沙盒 Operation、Artifact，以及兼容保留的 Task API。
- `agentspool worker`：上报节点状态，领取 Task，并代理沙盒 Operation。
- `agentspool sandboxd`：节点本地 root helper，用 systemd、bwrap 和 ext4 提供资源与隔离边界。
- `agentspool tool`：所有 Agent 共用的 JSON CLI。
- `.agents/skills/agentspool-compute/SKILL.md`：运行时无关的发现与协调协议。

整个发行包只有一个 `agentspool` 二进制。旧 Task Plane 仍可用于确定性的批处理任务，但它不是 Agent Session 管理器，也不会主动启动 Agent。

## 最短部署

```sh
make build
sudo ./scripts/install-server.sh ./dist/agentspool
sudo ./scripts/install-worker.sh ./dist/agentspool
```

生产安装会把 Token 写入权限受限的独立文件，env 只保存 locator，且不会打印明文；升级器会迁移旧 inline Token。Worker 注册完成后会移除不再需要的 Join Token。安装器同时部署每日 TLS 到期检查；加密异机备份需由所有者配置公开 age recipient 和 SSH 目标。公网或不可信网络必须额外部署 TLS、反向代理和防火墙。

## 验证

```sh
make check       # 仓库、API、SDK、单元与集成测试
make race        # Go race tests
make e2e         # 本地真实 server/worker Task E2E
make release     # 四架构单二进制发行包、SPDX SBOM 与反向校验
make release-verify # 两次复现比对及源码包脱离 Git 验证
```

真实 LAN 环境可继续运行：

```sh
./scripts/inspect-lan-persistent.sh
./scripts/e2e-lan-persistent.sh
./scripts/e2e-lan-sandboxes.sh
```

## 文档

- [完整文档导航](docs/README.md)
- [从这里开始](START_HERE.zh-CN.md)
- [通用 Agent 工具](docs/AGENT-TOOLS.zh-CN.md)
- [架构](docs/ARCHITECTURE.zh-CN.md)
- [0.4 迁移说明](docs/MIGRATION-v0.4.zh-CN.md)
- [安全边界](docs/SECURITY.md)
- [数据生命周期](docs/DATA-LIFECYCLE.zh-CN.md)
- [实现与验收状态](docs/IMPLEMENTATION_STATUS.zh-CN.md)
- [OpenAPI](api/openapi.yaml)

## Alpha 限制

0.4 仍是可信家庭 LAN 内的单控制面 Alpha：没有 HA、多租户敌对隔离或跨节点共享 POSIX 卷。持久沙盒是 Linux namespace/cgroup 边界，不是硬件虚拟机。GPU 仅可作为节点遥测记录，不参与申请、调度、挂载或本地模型部署。

默认控制面只监听 `127.0.0.1:8090`，Worker 只启用 shell executor。原生 shell/process Task 使用宿主网络，仅适用于可信任务；`ALLOW_NETWORK=false` 约束 Docker Task，不隔离原生任务。持久 Sandbox 默认独立网络 namespace，只有显式授予 `permissions.network=true` 才使用节点网络。
