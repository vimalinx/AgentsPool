# AgentsPool v0.4.0-alpha.4：从这里开始

## 先理解这一条边界

AgentsPool 不运行 Agent。Codex、Claude、OMP、Pi 或其他 Agent 保留自己的 Provider Token、模型、Prompt、上下文和 Session，只通过标准 Skill 调用：

```text
agentspool tool <tool-name> --input '<json>'
```

AgentsPool 只管理 `Device revision → ResourceLease → Persistent Sandbox → Operation → Artifact`。任何需要为具体运行时添加专用参数、解析事件或接管 Session 的功能都不属于核心。

Agent 加载 Skill 后先运行 `agentspool tool list --json`，再按需运行 `agentspool tool describe TOOL_NAME --json`。这两个命令只读取当前二进制的本地版本化目录，不连接控制面。

## 需要启动的程序

| 位置 | 程序 | 职责 |
|---|---|---|
| 常开主机 | `agentspool server` | 设备、资源租约、沙盒 Operation、Artifact 与兼容 Task API |
| 每台计算节点 | `agentspool worker` | 上报状态并代理 Task/沙盒请求 |
| 每台计算节点 | `agentspool sandboxd` | 本机受限 root helper，创建持久隔离沙盒 |
| Agent 所在环境 | `agentspool tool` | 结构化调用统一协调接口 |

0.4 不再提供或启动 `agentspool-agentd`、`agentspool-bridge`、`agentspool-agentctl`。如果机器曾安装旧版，请先执行 [迁移说明](docs/MIGRATION-v0.4.zh-CN.md)，确认旧服务已停止。

## 推荐阅读顺序

完整分类见 [文档导航](docs/README.md)。第一次使用建议按以下顺序：

1. [通用 Agent 工具与持久沙盒](docs/AGENT-TOOLS.zh-CN.md)
2. [架构与责任边界](docs/ARCHITECTURE.zh-CN.md)
3. [安全边界](docs/SECURITY.md)
4. [数据生命周期](docs/DATA-LIFECYCLE.zh-CN.md)
5. [实现与验收状态](docs/IMPLEMENTATION_STATUS.zh-CN.md)

## 第一次真实验收

1. 运行 `make check` 和 `make e2e`，确认本地控制面与 Worker 正常。
2. 在真实 LAN 运行 `scripts/inspect-lan-persistent.sh`，确认节点、版本与 GPU 禁用策略。
3. 运行 `scripts/e2e-lan-sandboxes.sh`，验证资源准入、持久数据、stop/start、rebind、文件传输和清理。
4. 从任意实际 Agent 加载 `.agents/skills/agentspool-compute/SKILL.md`，让它自行观察节点并调用 `agentspool tool`。
5. 在 Agent 自己的进程中验证真实模型调用；不要把 Provider Token、Prompt 或 Session 交给 AgentsPool。

验收结束后应确认没有 active ResourceLease、活动 Sandbox 或遗留挂载。Agent 的模型用量和 Session 终态由 Agent 运行时自身证明，不由 AgentsPool 冒充证明。
