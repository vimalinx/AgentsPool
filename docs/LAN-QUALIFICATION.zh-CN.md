# 局域网三节点验收模板

本页提供一套公开、可复用的三节点验收模板：一台控制面兼 Worker、一台 ARM 边缘 Worker、一台 x86_64 工作站 Worker。它用于验证控制面、节点身份、systemd 恢复、跨架构调度和持久沙盒；不记录任何维护者的真实 IP、SSH 别名、主机名、Node ID 或 Task ID。

Agent 运行时始终位于 AgentsPool 之外。AgentsPool 只提供 CLI、Compute Tool 与可安装 Skill，不启动、恢复或接管 OMP、Codex、Claude 等 Agent 会话。

## 示例拓扑

| 角色 | 默认 SSH 别名 | 默认节点名 | 架构 | 服务 |
|---|---|---|---|---|
| 控制面 + Worker | `agentspool-control` | `control-x86_64` | `x86_64` | server、worker、sandboxd |
| 边缘 Worker | `agentspool-edge` | `edge-arm64` | `aarch64` | worker、sandboxd |
| 工作站 Worker | `agentspool-workstation` | `workstation-x86_64` | `x86_64` | worker、sandboxd |

这些名称只是安全的文档默认值。运行脚本前，用环境变量映射到自己的 SSH 配置和节点名：

```bash
export AGENTSPOOL_LAN_CONTROL_HOST=your-control-ssh-alias
export AGENTSPOOL_LAN_EDGE_HOST=your-edge-ssh-alias
export AGENTSPOOL_LAN_WORKSTATION_HOST=your-workstation-ssh-alias
export AGENTSPOOL_LAN_CONTROL_NODE_NAME=your-control-node
export AGENTSPOOL_LAN_EDGE_NODE_NAME=your-edge-node
export AGENTSPOOL_LAN_WORKSTATION_NODE_NAME=your-workstation-node
export AGENTSPOOL_LAN_SERVER_URL=http://127.0.0.1:8090
export AGENTSPOOL_LAN_NETWORK_HEALTH_URL=http://192.0.2.10:8090/healthz
```

`192.0.2.10` 属于文档专用 TEST-NET-1 地址，不能直接用于真实 LAN。必须把 `AGENTSPOOL_LAN_NETWORK_HEALTH_URL` 替换成 Worker 可访问的控制面健康检查地址。不要把真实地址和节点标识提交回公开仓库。

## 收紧的执行基线

三个 Worker 的建议基线：

```dotenv
AGENTSPOOL_WORKER_QUEUES=default
AGENTSPOOL_WORKER_DETECT_GPU=false
AGENTSPOOL_WORKER_ALLOWED_EXECUTORS=shell
AGENTSPOOL_WORKER_MAX_CONCURRENCY=1
AGENTSPOOL_WORKER_ALLOW_NETWORK=false
AGENTSPOOL_WORKER_ALLOW_HOST_NETWORK=false
AGENTSPOOL_WORKER_INHERIT_HOST_ENV=false
```

- GPU 不检测、不发布 capability、不参与调度。
- 默认只开放 `shell` 执行器，不开放 Docker 或宿主网络。
- 不继承宿主环境变量。
- systemd Worker 使用 `ProtectSystem=strict` 与 `ProtectHome=true`。
- 这是可信私有网络内的 Alpha，不是面向敌对租户的公共计算服务。

## 凭据与数据位置

控制面的 Admin Token 和 Join Token 保存在控制面主机：

```text
/etc/agentspool/server.env             root:root 0600
/etc/agentspool/admin.token            root:agentspool-server 0640
/etc/agentspool/join.token             root:agentspool-server 0640
```

每个 Worker 的独立 Node Token 只保存在本机：

```text
/var/lib/agentspool/worker/credentials.json  agentspool:agentspool 0600
```

首次注册完成后，从 `/etc/agentspool/worker.env` 删除 `AGENTSPOOL_JOIN_TOKEN` 或 `AGENTSPOOL_JOIN_TOKEN_FILE`，并删除对应 Worker Join Token 文件。管理命令只在授权会话中加载 token locator，不要把令牌复制进仓库、命令历史或普通日志。完整数据生命周期见 [DATA-LIFECYCLE.zh-CN.md](DATA-LIFECYCLE.zh-CN.md)。

## 验收入口

验收端需要 `bash`、`ssh`、`sudo`、`jq` 和 `python3`，三个 SSH 目标需要支持 BatchMode 登录。

只读检查，不提交任务、不重启服务：

```bash
./scripts/inspect-lan-persistent.sh
```

三节点 Task/Artifact 验收：

```bash
./scripts/e2e-lan-persistent.sh
```

脚本要求三个配置的节点都在线、版本一致、只发布 Shell executor、禁用 GPU 遥测，并让三条并发 Task 分别落到三个不同节点且上传输出 Artifact。

包含服务重启和节点身份保持的验收：

```bash
./scripts/e2e-lan-restart.sh
```

它比较重启前后的 Worker 凭据摘要，只做相等性判断，不输出 Node Token。

Compute Tool 与持久沙盒验收：

```bash
./scripts/e2e-lan-sandboxes.sh
```

它验证 revision 准入、ResourceLease、scoped token、systemd+bwrap 执行、Artifact push/pull、跨 exec 持久性、在线扩容、同节点 rebind、旧租约释放，以及默认网络拒绝和显式网络允许。脚本结束时不得留下 active ResourceLease、active Sandbox 或 ext4 挂载。

真实验收结果只应保存于部署方自己的受控运维记录，不应把 IP、SSH 别名、主机名、Node/Task/Execution/Artifact ID 或凭据摘要提交到公开仓库。

## 回滚边界

非清数据卸载：

```bash
sudo ./scripts/uninstall.sh
```

它停用服务并删除 unit 与二进制，但保留 `/etc/agentspool` 和 `/var/lib/agentspool`。只有明确决定丢弃配置、节点身份、Task 状态、Artifact 和 Sandbox 数据时才使用 `--purge`。
