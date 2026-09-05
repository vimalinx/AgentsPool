# 故障排查

## 控制面启动失败：token must contain at least 24 characters

Admin、Allocator（如启用）和 Join Token 都必须至少 24 字符，且建议由程序生成：

```bash
agentspool token
```

确认 systemd EnvironmentFile 没有引号、空格或未展开的 shell 表达式。

## Worker 提示 no valid credentials and join token is not configured

Worker 数据目录没有可用 `credentials.json`，且当前没有 Join Token。首次运行必须提供：

```bash
AGENTSPOOL_JOIN_TOKEN_FILE=/etc/agentspool/worker-join.token agentspool worker ...
```

Token 文件必须是普通文件，推荐 `root:agentspool 0640`；注册成功后从 Worker 配置删除 locator，并删除该 Join Token 文件。

如果 `credentials.json` 在别的 data-dir，使用正确 `--data-dir`。

## Worker 一启动就 credentials rejected

常见原因：

- 控制面数据目录被清空或换成新实例。
- 管理员 disable 了节点。
- 同 machine_id 使用 Join Token 重新注册，旧 Token 已被轮换。
- Worker 指向了另一台控制面，但 credentials 仍记录旧节点。

处理：先在控制面查节点事件。确认可重新注册后，停 Worker、备份并删除 `credentials.json`，配置当前 Join Token，再启动。

## 节点显示 offline

检查：

```bash
journalctl -u agentspool-worker -n 200
curl http://控制面IP:8090/healthz
```

再检查防火墙、DNS、HTTP/HTTPS scheme 和系统时钟。Heartbeat 使用相对超时，对时钟漂移不高度敏感，但 TLS 证书验证依赖正确时间。

## 节点 online，但一直不领取任务

- 节点可能是 draining。
- Active jobs 达到 max concurrency。
- Queue 不匹配。
- executor 在 Worker policy 中禁用或运行时没探测到。
- CPU、内存、磁盘或 label 条件不满足。

使用 `agentspool tasks get` 和 `agentspool nodes get` 对照完整字段。

## Worker 没发布 executor.docker

Worker 启动时执行：

```bash
docker version --format '{{.Server.Version}}'
```

必须成功。检查：

- Docker CLI 是否在 PATH。
- Docker daemon 是否运行。
- Worker 服务用户是否能访问 socket。
- `AGENTSPOOL_WORKER_ALLOWED_EXECUTORS` 是否包含 docker。

systemd 加入 docker 组后要重启服务，必要时重启登录/session。

## GPU 遥测没被识别

以 Worker 服务账号运行：

```bash
sudo -u agentspool nvidia-smi \
  --query-gpu=index,name,uuid,memory.total,memory.free,driver_version \
  --format=csv,noheader,nounits
```

GPU 只用于硬件记录。即使未识别，也不会阻塞任务领取或改变调度结果；AgentsPool 不把 GPU 分配给 Docker 或本地模型。

## 任务失败：executor is disabled by worker policy

Task 被领取时 capability 本应阻止不匹配；若 Worker policy 在领取后被改动或 capability 过期，执行器仍会做最终检查并失败。重启 Worker 更新 capability，或把任务约束到正确节点。

## Docker network access is disabled

任务使用 `bridge`/`host`，但 Worker policy 不允许。优先把任务改为 `network: none`。确需联网时开启 `--allow-network`；Host Network 还需单独开启，不建议。

## required output pattern matched no files

命令可能写到错误目录，或 pattern 不匹配。所有相对路径以 workspace 为根；`work_dir` 只改变进程 cwd，不改变 output pattern 的根。

例如进程 cwd 为 `repo`，写出 `repo/build/result.json`，输出仍应写：

```json
{"pattern": "repo/build/result.json"}
```

## Artifact 或输出占满磁盘

默认的 Artifact、output、cache 和 transfer 都不设人工字节/时间上限，只受真实磁盘与文件系统容量约束。先检查：

- Server 数据卷与 Worker cache 所在文件系统的可用容量和 inode。
- Sandbox ResourceLease 的 `disk_mb` 硬上限。
- 是否由管理员显式设置了正数 `max-output-bytes`、`max-artifact-bytes` 或 `cache-max-bytes`。

大目录可在任务内压缩，或接入外部对象存储并只输出 manifest/receipt。不要在磁盘已满时反复重试写入。

若慢速链路超时，确认没有显式设置正数 `AGENTSPOOL_WORKER_TRANSFER_TIMEOUT` 或 CLI `--timeout`；两者默认 0、不会给传输增加总截止。`AGENTSPOOL_WORKER_REQUEST_TIMEOUT` 只管短控制请求。

## Artifact metadata exists but content is missing

有人手工删除了 `artifacts/` 内容，或恢复备份时只恢复 state.json。根据 `sha256` 从备份找回对应文件并放回分片路径；否则重新上传会产生新 Artifact ID，旧 Task 结果仍指向缺失 ID。

## 任务在 needs_review

它是保护机制，不是普通错误。说明非幂等任务已经开始，但 Lease 中断。检查外部系统和 Execution 日志，确认副作用未发生后，才使用：

```bash
agentspool tasks retry TASK_ID --acknowledge-non-idempotent --reset-attempts
```

## 日志不完整

Worker 本地 log buffer 在极端输出下会丢块，并插入 system 提示。调整任务减少噪声、写日志文件作为 Artifact，或提高上层程序日志级别。单个 log event 上限 256 KiB，Worker 默认切成 64 KiB。

## 控制面重启后任务被重试

这是预期。控制面无法证明旧 Lease 仍安全，所以启动恢复会结束 active Lease。普通/幂等任务重试；已开始非幂等任务进入 needs_review。

升级前 drain 或等待任务结束可以避免。

## state.json decode error

不要覆盖原文件。停服务，复制整个数据目录，然后：

1. 检查磁盘/文件系统错误。
2. 尝试从最近备份恢复 state.json。
3. 查看同目录是否残留 `.state-*.tmp`，仅在确认它是完整 JSON 且时间更新时人工评估。
4. 不要用文本编辑器随意删字段；对象之间有引用关系。

## CLI 参数看起来被忽略

Go flag 语法要求 flags 放在 positional ID 之前。例如：

```bash
agentspool artifacts download --output result.txt ARTIFACT_ID
```

而不是把 `--output` 放在 ID 后。仓库示例均按可解析顺序书写。
