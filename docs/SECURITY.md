# AgentsPool 安全模型与加固

## 1. 信任模型

AgentsPool v0.4 Alpha 是一个单管理员域，不是公共多租户云。默认假设：

- 控制面主机和数据目录可信。
- 持有 Admin Token 的人或 Agent 可以在 Worker 上提交代码执行。
- 已注册 Worker 彼此属于同一个管理域。
- 家庭 LAN 可能存在不完全可信设备，因此仍需认证和网络隔离。
- Task 内容可能由 LLM 生成，存在错误命令、提示注入和意外数据外传风险。
- Agent 运行时及其 Provider Token、Prompt、上下文和 Session 位于 AgentsPool 之外。

## 2. 凭据

### Admin Token

权限最大，可管理节点、执行任务、读取日志和 Artifact。使用 `agentspool token` 生成，不少于 24 字符；不要放入 Task、日志、截图、Git 或浏览器同步配置。

### Allocator Token

普通 Agent 的入口凭据。只能读取 Device/Service、写入或移除 `source=agent` 的机器说明，以及申请 CPU/内存/磁盘 ResourceLease；不能管理节点、Task、Artifact、human description 或历史清理。client profile 只保存其受保护文件 locator，不保存明文。

### Join Token

允许机器注册；同 machine_id 再注册会轮换 Node Token。它不是只读邀请码，而是 enrollment authority。首次注册后从 Worker 环境删除，集中保存在受控密码库。

### Node Token

只允许以该 Node 身份 heartbeat、claim、下载 Artifact，以及配合 Lease Token 操作自己的 Lease。服务端只保存 hash；明文位于 Worker `credentials.json`。

### Lease Token

每次 claim 随机生成，只在该 Execution 生命周期使用。Lease API 同时验证 Node Token 与 Lease Token。

### ResourceLease / Sandbox / Operation Token

Compute Tool 首次读取 Device 和申请 ResourceLease 使用 Allocator Token（Admin 仅作为所有者兼容入口）。后续调用只传对应 ResourceLease Token；创建沙盒后再降到对应 Sandbox Token。CLI 一旦发现 scoped token 就不会读取或发送 allocator/admin 文件。rebind 必须同时持有原 Sandbox Token 与新 ResourceLease Token。Worker 回报单次操作还必须同时持有自己的 Node Token 与一次性 Operation Token。

Allocator 就是运行时无关的 placement/admission 身份，不需要 OMP、Codex、Claude 等专用 broker 或适配器。任何 AgentsPool 令牌都不得写入沙盒、命令输出或仓库；Provider Token 更不得交给 AgentsPool 控制面、Worker 或 Sandbox。

CLI、Server 和 Worker 支持 `AGENTSPOOL_*_TOKEN_FILE` locator。优先把单个令牌保存在 `0600`、`0400` 或对应服务组只读的 `0640` 普通文件中，通过 locator 读取；解析器拒绝 group 写入/执行、任何 other 权限、空文件和同时配置明文值与文件。Server 使用独立的 `agentspool-server` 账号和 `root:agentspool-server 0640` 凭据，不能与 Worker、sandboxd 或原生 Task 共用 `agentspool` 账号。这样可避免令牌进入命令行参数、长期环境变量或 Worker 可读文件。locator 是路径，不是令牌值，日志和错误不得输出文件内容。

## 3. 网络

### 推荐

- 只绑定可信 LAN 地址，或 `0.0.0.0` 后由防火墙限制源网段。
- 跨 Wi-Fi、校园网、异地或不可信交换网络时使用 HTTPS、WireGuard、Tailscale 类私有 overlay，或可信反向代理。
- 不做公网端口转发。
- 反向代理必须关闭对 Authorization 的日志记录。

### TLS

控制面原生支持：

```bash
agentspool server --tls-cert /path/fullchain.pem --tls-key /path/key.pem ...
```

也可由 Caddy/Nginx/Traefik 终止 TLS。若 TLS 在代理终止，代理到控制面的链路仍应位于本机或可信私网。

systemd 安装会启用每日证书到期检查；它从 `server.env` 读取证书 locator，不读取私钥或 Token：

```bash
systemctl status agentspool-tls-check.timer
sudo /usr/lib/agentspool/check-tls-expiry.sh
```

默认在剩余 30 天内返回失败并写入 journal。证书由外部 CA/私有 CA 续发后，原子替换证书与私钥并重启控制面；不要让检查脚本代替续发权限或私钥管理。

Worker 的 `--insecure-skip-tls-verify` 只用于临时实验。它会使攻击者可以冒充控制面并获取 Worker 提交的数据。

## 4. Compute 持久沙盒

Compute Tool 的 root helper 只监听 `root:agentspool 0660` Unix socket。它只接受固定数据根和 transfer root 下的严格对象，使用独立 ext4 镜像、systemd CPU/内存/进程限制、可选的显式执行截止时间、bwrap mount/PID/IPC/UTS/cgroup namespace，再降权并清空 capabilities。默认网络隔离；只有显式 `network:true` 才复用节点网络。宿主 HOME、SSH、`/etc/shadow`、DRM/NVIDIA 设备均不挂载。

这是 Linux namespace/cgroup 边界，不是 microVM，也不承诺抵御内核漏洞。GPU 只可作为 Node 遥测记录，不进入 ResourceLease、placement admission、沙盒设备或本地模型部署。

## 5. 原生执行器

`shell` / `process` 与 Worker 服务账号同权限，不是容器或 VM 沙箱。

风险包括：

- 读取该账号可读的文件。
- 消耗 CPU、内存、磁盘和进程数。
- 访问网络。
- 扫描本机和 LAN。
- 修改 Worker 数据目录与失败工作区。

加固：

- 使用专用无登录用户，不复用个人桌面账号。
- 不给该用户 SSH 私钥、浏览器配置、云 CLI 凭据。
- systemd 使用 `ProtectSystem`、`ProtectHome`、`PrivateTmp`、`NoNewPrivileges`。
- 任务秘密只通过 Worker secrets 文件按名称显式注入。
- 默认保持 `AGENTSPOOL_WORKER_INHERIT_HOST_ENV=false`。
- 对不可信任务禁用原生执行器，只开 Docker，或者使用专用 VM Worker。

环境白名单只能防止意外 env 泄露，不能阻止进程从文件系统读取该用户能访问的秘密。

## 6. Docker 执行器

推荐默认：

```json
{
  "network": "none",
  "read_only_root": true,
  "cpu_limit": 2,
  "memory_mb": 4096
}
```

Worker 策略：

```dotenv
AGENTSPOOL_WORKER_ALLOW_NETWORK=false
AGENTSPOOL_WORKER_ALLOW_HOST_NETWORK=false
```

只有确有下载/API 需求的节点允许 bridge。Host Network 默认永远关闭。

Docker Task 的环境变量通过 Worker job 目录外、权限为 `0600` 的临时 `--env-file` 注入，执行后删除，secret 值不会直接出现在 `docker run` 参数列表中。Docker env-file 不能可靠表达换行值，因此多行私钥应先编码，或作为受控 Artifact 传入；本机 root 仍可读取 Worker 内存和临时文件。

注意：能访问 Docker daemon 的账号通常能获得接近 root 的宿主控制。AgentsPool 限制生成的 `docker run` 参数，但它不能把本身不可信的池管理员变成安全租户。更强边界应使用 rootless container runtime、gVisor/Kata、microVM 或整台隔离 VM。

## 7. Artifact

- 默认不设置单文件字节上限，只受真实文件系统容量约束；管理员仍可显式配置正数防御性上限。
- 内容按 SHA-256 存储，逻辑 metadata 与内容分离；Worker 下载后依据强 ETag 重新计算并校验摘要。
- 路径经过 root containment 校验。
- Worker 输入复制而非 hard link，防缓存污染。
- 输出跳过 symlink 和非普通文件。
- 所有 Node 当前可下载池内任意 Artifact；因此不要把相互不信任的数据域放进同一控制面。

Artifact 未加密存储。磁盘层使用 LUKS/BitLocker/ZFS encryption 或受控物理环境。

## 8. Agent 与提示注入

当 Agent 读取网页、邮件或仓库后生成命令，外部内容可能试图诱导它：

- 上传本机秘密。
- 启用网络或 Host Network。
- 选择原生执行器绕过容器。
- 删除数据或发布内容。

策略建议：

- 研究/解析任务使用无网络容器和只读输入。
- 将“读不可信内容”和“执行有副作用动作”拆成不同 Task。
- 副作用 Task 使用 `non_idempotent`、最小 Secret、固定命令模板和人工审批。
- Agent 做完 placement 后立即降权到 scoped ResourceLease/Sandbox Token，不把 Admin Token 放进远端 Sandbox。
- Provider Token、Prompt、上下文、Session 和 usage 只留在 Agent 自己的运行时。
- 输出证据 Artifact，再由第二阶段验证器决定是否继续。

## 9. Secret 文件

格式为一行一个 `KEY=value`，权限建议 `0600`。Worker 启动时读取到内存，Task spec 只写 `${secret:KEY}`。

目前 secrets 不热重载；修改后重启 Worker。不要在日志中打印 env。程序自身不会主动记录 secret 值，但任务代码仍可能打印它们，日志查看者拥有 Admin 权限。

## 10. 拒绝服务

管理员或持有 Allocator Token 的 Agent 能消耗池资源。默认不替 Agent 增加任务、传输、输出、缓存或 drain 时间/字节上限；防护主要依靠：

- ResourceLease 的 CPU/内存/磁盘原子准入与 Sandbox cgroup/ext4 硬边界。
- Worker `max_concurrency` 和 Docker CPU/内存限制。
- 网络认证、防火墙、专用服务账号和独立数据卷容量。
- JSON 结构、路径、单条日志和令牌长度等协议/安全校验；这些不是任务运行时限。
- 管理员按部署风险显式启用的正数 request/Artifact/output/cache/transfer 上限或期限。

当前版本没有 per-user quota，因为没有多用户身份。若需要把 API 提供给多个不互信用户，应在前面增加自己的授权/配额网关，而不是共享 Admin Token。

## 11. 安全检查清单

- [ ] Admin、Allocator 与 Join Token 随机且互不相同。
- [ ] Join Token 已从完成注册的 Worker 环境移除。
- [ ] 控制面端口不在公网暴露。
- [ ] 跨不可信网络使用 TLS/overlay。
- [ ] Server/Worker 数据目录权限为 0700 或更严。
- [ ] Worker 使用专用服务账号。
- [ ] Server 使用独立 `agentspool-server` 账号，不能与 Worker/native Task 共用账号或数据目录权限。
- [ ] 普通 Agent 只有 Allocator locator，并在 placement 后使用 scoped ResourceLease/Sandbox Token；没有 Admin Token。
- [ ] Provider Token、Prompt、上下文和 Session 未进入 AgentsPool 服务、日志或沙盒。
- [ ] sandboxd Unix socket 为 `root:agentspool 0660`，GPU/宿主凭据未挂载。
- [ ] 不可信任务不使用 native executor。
- [ ] Docker Host Network 关闭。
- [ ] 副作用任务标记 non_idempotent。
- [ ] 定期查看 authentication failures 和 needs_review。
- [ ] 备份使用独立 age recipient 加密并发送到异机，私有 identity 不留在控制面。
- [ ] 最新异机备份已执行 `restore-drill.sh` 隔离恢复和全部 Artifact SHA-256 验证。
- [ ] `agentspool-tls-check.timer` 已启用且最近一次检查成功。

具体落盘内容、保留和删除语义见 [数据生命周期](DATA-LIFECYCLE.zh-CN.md)。当前 Alpha 已提供终态 Task/Execution/日志清理、引用安全的 Artifact 删除和默认 dry-run 的孤儿 GC，但没有自动 TTL、介质级安全擦除或敌对多租户隔离；有这些要求的工作负载不得使用当前版本。
