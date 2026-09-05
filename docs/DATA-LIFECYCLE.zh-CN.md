# AgentsPool 数据生命周期

本文描述 0.4 Alpha 实际保存的数据、保留边界和删除语义。AgentsPool 是单管理员可信域，不是合规归档系统或敌对多租户存储。

## 数据分类

| 位置 | 内容 | 默认保护 | 当前保留语义 |
|---|---|---|---|
| Server `state.json` | Node、带来源的 Device descriptions、Task、Execution、Lease、Sandbox、Artifact metadata、事件和 token hash | 独立 `agentspool-server` 账号、数据目录 `0700`、文件 `0600`、原子替换和目录同步 | 高频 heartbeat 遥测只保留在内存；Device description 按 `source + author` 显式更新/删除；历史不按时间自动过期 |
| Server `logs/` | Execution stdout/stderr JSONL | 文件 `0600`、追加后同步 | 不自动轮转；删除终态 Task 时同步清理其 Execution 日志 |
| Server `artifacts/` | 内容寻址 Artifact bytes | 目录 `0700`、SHA-256 校验；默认只受文件系统容量约束 | Sandbox Token 可列举/删除本沙箱桥接副本；destroy 清理无外部引用副本；GC 默认仅 dry-run |
| Worker `credentials.json` | Node Token 明文和节点身份 | Worker 数据目录 `0700`、文件 `0600`、原子替换 | 节点重新注册或卸载时由管理员处理 |
| Worker `cache/`、`jobs/` | Artifact 缓存和临时工作区 | 专用服务账号 | cache 默认只受磁盘容量约束；成功 job 默认清理，失败 job 默认保留 |
| Sandbox `storage.ext4` | `/workspace` 和 `/home/agent` 持久数据 | root 管理、独立 ext4、bwrap/cgroup 边界 | ResourceLease 过期不会删除；显式 destroy 才删除本机镜像 |

## 会意外持久化的敏感内容

Provider Token、Prompt、上下文和 Agent Session 不属于 AgentsPool 对象，但以下调用内容会被持久化：

- Task 的脚本、命令、`executor.env`、labels 和 metadata；
- Sandbox exec 的命令、环境变量、stdout、stderr 和错误；
- Execution 日志、Artifact metadata 和 Artifact bytes；
- 事件中的资源名称、错误和状态信息。
- Device description 中的人类说明、Agent 建议、可用时段和工作负载约束。

不要把秘密作为 literal env、命令参数、metadata 或输出传入。Task secret 只使用 Worker secrets 文件中的 `${secret:NAME}` 引用，并确保任务本身不打印值。Provider Token 不应进入 AgentsPool、远端 Sandbox 或 Artifact。

## 删除和到期

- 删除 Sandbox API 会停止挂载、删除节点上的 sandbox 根目录，并清理没有 Task/Execution/其他 Sandbox 外部引用的控制面桥接 Artifact；这是逻辑删除，不承诺介质级安全擦除。
- `devices.unannotate` 只删除一个匹配的 `source + author` description；删除节点注册会同时删除该节点的全部 descriptions。
- ResourceLease 默认没有时间截止；调用方显式设置正数 `ttl_seconds` 时，到期只释放资源准入，不删除持久 Sandbox。
- `agentspool tasks delete TASK_ID` 只接受无活动 Lease 的终态 Task，并删除 Task、Execution、历史 Lease、幂等映射和 Execution 日志；它不会自动删除输出 Artifact。
- `agentspool artifacts delete ARTIFACT_ID` 在仍有 Task、Execution 或 Sandbox Operation 引用时拒绝删除。完成关联对象清理后再删除；严格数据擦除场景可显式使用 `--force` 同时移除这些 metadata 引用。
- 普通 Agent 使用 `artifacts.list` 查看一个 Sandbox 的桥接副本，并在相关 Operation 终态后用 `artifacts.delete` 删除；它不能删除其他 Sandbox 或 Task 拥有的数据，共享内容仍有引用时不会删除底层文件。
- `agentspool artifacts gc` 默认只报告 metadata 未引用的孤儿内容文件；人工确认后使用 `agentspool artifacts gc --execute` 删除。GC 与上传/删除在控制面内串行，避免删除正在写入的内容。
- `maintenance.prune` 默认 dry-run；只有显式 `execute:true` 才执行预览中的对象。它会压缩仍存活 Sandbox 的终态 Operation，删除已失去 Sandbox 主体或只由待清理 Operation 引用的桥接 Artifact，删除已 destroy 且全部 Operation 终态的 Sandbox/Volume/Lease metadata，并移除旧版本产生的 `device.observed` registry 噪声；非终态服务 Operation 永远保留。
- 当前没有自动历史 TTL，事件默认也不按条数截断。需要强制到期的部署应由管理员先预览、再调用显式删除命令，或者为控制面使用独立、可整体销毁的加密数据卷。
- 备份、Worker cache、客户端副本和浏览器缓存有独立生命周期；删除主数据不会自动删除这些副本。

## 运维要求

1. 使用 LUKS、ZFS native encryption 或等价磁盘加密保护静态数据。
2. 定期监控 Server 数据目录、Worker cache 和 Sandbox 卷容量。
3. 按 [备份与恢复](BACKUP_RECOVERY.zh-CN.md) 创建加密备份，并定期做隔离恢复演练。
4. 到期销毁整个控制面时，先停止 Server 和 Worker，再撤销 Admin/Join Token，删除 Worker credentials 和 Sandbox，最后按底层存储能力销毁加密卷或密钥。
5. 对受法规约束、相互不信任或要求可验证介质级擦除的工作负载，不要使用当前 Alpha；显式删除和 GC 不能替代法规评估或底层介质擦除。

## 后续兼容方向

后续数据治理可增加由所有者明确启用的历史 TTL、总容量配额和调度清理，以及可验证恢复。任何自动能力都不能作为默认行为，并必须为每次清理产生不含秘密的审计摘要。

alpha.4 的 Sandbox 输出独立存入 Server `logs/sandbox-operations/` 与 Worker Sandbox `operations/`，默认完整保留，显式轮转才丢弃旧段并报告游标缺口。Operation 仅保存 64 KiB 尾部预览。`maintenance.prune` 同步清理中央日志，destroy 清理该 Sandbox 本地输出。stop 释放计算预留并保留磁盘；磁盘只有销毁后才从持久预留中去除。
