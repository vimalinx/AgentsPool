# AgentsPool 文档导航

AgentsPool 的核心只有一条：

```text
任意 Agent → agentspool-compute Skill → agentspool tool
          → Device → ResourceLease → Sandbox → Operation → Artifact
```

## 核心契约

1. [通用 Agent 工具](AGENT-TOOLS.zh-CN.md)：Agent 自发现、资源申请、持久沙盒和文件传输。
2. [架构](ARCHITECTURE.zh-CN.md)：组件职责、信任边界和不接管 Agent 的原则。
3. [HTTP API](API.md) 与 [OpenAPI](../api/openapi.yaml)：机器接口。
4. [安全模型](SECURITY.md) 与 [数据生命周期](DATA-LIFECYCLE.zh-CN.md)：令牌、隔离、持久化和删除语义。

## 部署与维护

- [快速部署](QUICKSTART.zh-CN.md)
- [运维手册](OPERATIONS.zh-CN.md)
- [故障排查](TROUBLESHOOTING.zh-CN.md)
- [备份与恢复](BACKUP_RECOVERY.zh-CN.md)
- [OpenWrt 部署](OPENWRT.zh-CN.md)
- [局域网三节点验收模板](LAN-QUALIFICATION.zh-CN.md)

## 兼容 Task API

Task API 保留给确定性的批处理任务，不是 Agent Session 管理器，也不是推荐的 Agent 集成面：

- [Task Spec](TASK_SPEC.zh-CN.md)
- [Python SDK](SDK.zh-CN.md)

新 Agent 集成不应从 Task API 开始；应加载项目 Skill 并调用 `agentspool tool`。

## 版本与历史

- [当前实现状态](IMPLEMENTATION_STATUS.zh-CN.md)
- [从旧 Agent Plane 迁移到 0.4](MIGRATION-v0.4.zh-CN.md)
- [历史文档归档](archive/README.md)

归档内容只用于追溯，不属于当前产品契约。路线与行为以核心契约、OpenAPI 和测试为准。

- [English two-machine quickstart](QUICKSTART.en.md)
- [Platform support / 平台支持](PLATFORMS.md)
