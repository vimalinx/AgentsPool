# AgentsPool 0.1.0 历史实现与验收报告

> 本文只记录 v0.1.0 当时的行为。当前 alpha.2 已删除 GPU 调度、GPU 评分与 GPU Executor；GPU 仅保留为节点遥测。本文中的 GPU 任务说明不得作为当前契约使用。

日期：2026-08-30
状态：可运行的家庭单控制面版本
实现目标：把软路由作为持续在线的 Control Plane，把家中 Linux 电脑作为可热插拔 Worker，完成 Agent/Task 级统一调度。

## 1. 本版本交付范围

AgentsPool 0.1.0 已形成从“节点入池”到“执行结果回收”的完整闭环：

```text
Worker 注册与能力探测
        ↓
心跳、在线状态与资源快照
        ↓
管理员 / SDK 提交 Task
        ↓
中央调度器原子选择 Task
        ↓
Worker 获得限时 Lease
        ↓
输入 Artifact 下载、校验与工作区物化
        ↓
Shell / Process / Docker 执行
        ↓
日志、输出 Artifact、Execution Receipt
        ↓
成功 / 失败 / 重试 / 死信 / 人工复核
```

本版本明确池化 Task、Agent 和 Operation，不实现跨机器共享内存、显存、进程或透明单机镜像。

## 2. 可执行组件

同一个静态 Go 二进制提供四类能力：

- `agentspool server`：控制面、调度器、租约管理器、Artifact Store、日志、审计事件、指标和内嵌 Web 控制台。
- `agentspool worker`：资源探测、任务领取、租约续期、输入缓存、执行器、输出收集和日志回传。
- `agentspool <resource> <action>`：节点、任务、Artifact、事件和状态管理 CLI。
- `agentspool token`：生成 Admin / Join Token。

Go 主程序不依赖第三方模块，适合交叉编译和放入资源有限的软路由。Python SDK 同样只使用标准库。

## 3. 控制面已经实现的语义

### 节点与凭据

Worker 使用 Join Token 首次注册，控制面签发独立 Node Token。控制面只保存 Token 的 SHA-256 hash；Node Token 明文仅保存在 Worker 的 `credentials.json`。节点支持 `online`、`offline`、`draining` 和 `disabled`。

### 调度

硬约束包括 Queue、OS、架构、CPU threads、可用内存、可用磁盘、Capability、Label、GPU 数量、空闲显存、允许节点和排除节点。通过硬约束后，再结合优先级、Preferred Node、输入缓存局部性、资源余量、GPU 保留策略和活动租约数排序。

并发上限由控制面依据该节点尚未过期的真实活动 Lease 计算。Worker 上报的 `active_jobs` 只作为观测输入，不能绕过 `max_concurrency`；这也阻止同一节点凭据被两个意外重复进程同时超额领取任务。

### 租约和失败恢复

每次领取创建独立 Lease 与 Execution。Lease 必须持续续租；失联、睡眠、断网或控制面重启后，统一进入恢复逻辑：

- 未开始的任务可以重新排队。
- 普通或幂等任务在预算内指数退避重试。
- 已开始的非幂等任务进入 `needs_review`，不自动重放。
- 尝试预算耗尽后进入 `dead_letter`。
- 再次领取重试任务时清除上一次失败的临时 Result、FinishedAt 和 LastError，Execution 历史仍完整保留。

### Artifact

文件上传时边写入边计算 SHA-256，内容按摘要分片保存，多个逻辑 Artifact 可以复用同一内容文件。下载响应携带强 ETag；Go 客户端和 Worker 会在临时文件中重新计算 SHA-256，只有校验一致才原子安装到目标位置。Worker 缓存访问已串行保护，防止并发任务重复下载或互相淘汰正在物化的文件；工作区始终复制缓存内容，不使用 hard link。

控制 API 与 Artifact 传输使用不同截止时间：默认分别为 30 秒和 1 小时。Go Client 只在调用方没有显式 deadline 时应用默认值，Worker 的长传输上下文不会被短控制请求超时覆盖；控制面也不再用全局 60 秒 ReadTimeout 截断经过认证且有大小上限的 Artifact body。

### 执行器

- Shell：适合可信脚本和 Agent Harness。
- Process：结构化 argv，不经过 Shell 解析。
- Docker：支持镜像、命令、网络、GPU、CPU、内存、用户和只读根文件系统策略。

原生执行器默认不继承整套服务环境，只保留明确白名单。任务通过 `${secret:NAME}` 引用 Worker 本地 secrets 文件，Task JSON 中不出现秘密明文。Docker 执行器使用私有临时 `--env-file`，不把 secret 值写进进程参数；文件在容器命令结束后删除。

## 4. 持久化与软路由适配

控制面使用内存状态加原子 JSON 快照：临时文件写入、`fsync`、rename，再尽力同步目录。任务创建、领取、终态、取消、重试和 Lease 到期等关键事实持久化；心跳与正常续租属于可重建高频状态，先更新内存，在下一次关键转换或优雅关机时落盘，从而降低闪存写放大。

执行日志按 Execution 写入独立 JSONL；Artifact 内容与元数据分离。整个控制面只需要一个数据目录即可备份和恢复。

## 5. 管理与集成入口

本版本同时包含：

- 内嵌 Web Dashboard：节点、任务、日志、事件和原始 Task JSON。
- 完整管理 CLI。
- OpenAPI 3.1 合约。
- 零第三方依赖 Python SDK。
- systemd 单元和环境文件。
- OpenWrt/procd init 与 UCI 配置模板。
- Dockerfile 和 Docker Compose 控制面示例。
- amd64、arm64、armv7、mipsle soft-float 交叉编译发布脚本。

## 6. 实际验收结果

发布前在 Linux amd64、Go 1.23.2、Python 3.13.5 环境执行：

```text
make check   PASS
make race    PASS
make e2e     PASS
make release PASS
```

`make check` 包含：

- 仓库版本、示例 JSON、Markdown 内部链接和内嵌 Web 文件一致性检查。
- 实际 HTTP Router 与 OpenAPI 合约对照：32 个 API operation 一一匹配。
- Bash 与 OpenWrt init 脚本语法检查。
- Python SDK 的 6 个客户端测试。
- `gofmt`、`go vet` 和全部 Go 单元/集成测试。

Go 测试覆盖 Store 原子持久化与失败回滚、易失更新、调度约束、优先级、数据局部性、真实 Lease 并发限制、重试状态清理、非幂等执行前/执行后失败语义、死信、完整 HTTP 生命周期、Artifact、客户端 deadline 覆盖、日志、重叠输出 Glob、CLI 参数前后交错、进程取消、Secret 引用、Docker secret 参数隔离和宿主环境隔离。Race Detector 覆盖全部 Go package，并包含并发 Artifact Cache 物化测试。

真实 E2E 启动独立控制面和 Worker，执行以下链路：

```text
上传输入 Artifact
→ 提交带输入和递归输出规则的 Shell Task
→ Worker 注册并领取
→ 下载输入并验证 SHA-256
→ 物化到独立 workspace
→ 执行、回传 stdout/system 日志
→ 收集 outputs/**/*.txt
→ 上传输出 Artifact
→ Task succeeded
→ CLI 下载并核对输出内容
→ 检查 metrics 完成计数
```

当前验收环境没有 Docker daemon，因此没有在本机实际拉取镜像运行 Docker Executor；Docker 参数生成、策略校验和相关代码参与 Go 测试，部署文件参与仓库一致性检查。正式启用 GPU/Docker Worker 时，应在目标机器额外运行 `examples/tasks/docker-isolated.json` 与 `examples/tasks/gpu-docker.json` 做现场资格验收。

## 7. 发布物

发布脚本生成：

- Linux amd64 二进制包。
- Linux arm64 二进制包。
- Linux armv7 二进制包。
- Linux mipsle soft-float 二进制包。
- 完整源代码 ZIP。
- `SHA256SUMS`。

每个二进制包同时带 README、文档、OpenAPI、示例、Python SDK、systemd/OpenWrt 配置、安装脚本、Dockerfile 和 Compose 文件。

## 8. 0.1.0 的有意边界

本版本是可以长期运行和继续演进的家庭池基础设施，但不是多租户云平台：

- 单控制面，不含 HA、共识或自动故障转移。
- 单一 Admin 安全域，不含 RBAC、组织和配额计费。
- 所有 Node 可下载池内 Artifact，因此同一池内节点应互相信任。
- 没有自动 Artifact GC；需要结合备份策略人工清理。
- 原生执行器不是恶意代码沙箱；Docker daemon 也不是敌对多租户安全边界。
- 日志是实时证据流，但在极端网络故障与 Lease 丢失窗口内不承诺零丢失。
- 调度器当前没有 Placement Explain API，未匹配任务通过 Task 与 Node detail 对照排查。

这些边界是刻意保留的：先把家庭单控制面、数台到数十台可信 Worker 的核心语义做稳，再决定是否引入数据库、消息总线、mTLS、RBAC、HA 或跨地点调度。

## 9. 首次部署判定标准

一套家庭 AgentsPool 可以认为部署成功，需要同时满足：

1. 软路由控制面重启后 Web、CLI、Task 与 Artifact 元数据仍可读取。
2. 至少两台 Worker 能注册，能力与标签正确，停止后按超时变为 offline。
3. 普通任务能在不同节点执行并回收 Artifact。
4. GPU Task 只被满足显存与 Capability 的节点领取。
5. Drain 后节点不领取新任务，现有 Lease 能完成。
6. 杀死运行普通任务的 Worker 后任务按预算重试。
7. 杀死运行非幂等任务的 Worker 后任务进入 `needs_review`。
8. 备份的数据目录能在另一位置恢复，并按文档重新注册 Worker。

达到以上标准后，这个池子已经可以作为后续主 Agent、学习工作区、内容流水线、代码 Agent 和家庭模型网关共同使用的基础执行层。
