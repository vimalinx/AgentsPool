# 从 Agent Plane 迁移到 0.4

0.4 删除了运行时接管层。升级的目标是只保留 `agentspool server`、`agentspool worker`、`agentspool sandboxd` 和 Agent 主动调用的 `agentspool tool`。

## 1. 停止旧后台接管服务

在曾运行控制面的机器上：

```sh
sudo systemctl disable --now agentspool-agentd.service
```

在每个曾安装 Bridge 的真实 Unix 用户会话中：

```sh
systemctl --user disable --now agentspool-bridge.service
```

如果 unit 不存在，命令失败可以忽略。先确认进程已经退出，再删除旧文件。

## 2. 安装 0.4

```sh
make build
sudo ./scripts/install-server.sh ./dist/agentspool
sudo ./scripts/install-worker.sh ./dist/agentspool
```

`install-server.sh` 和 `uninstall.sh` 会清理系统级旧 `agentd` unit 和旧二进制；它们无法替任意登录用户操作 user systemd，因此用户级 Bridge 必须按上一步逐用户停止。

安装器还会创建独立的 `agentspool-server` 服务账号并把现有 Server 数据目录迁移给它。不要把 Server unit 改回 Worker 使用的 `agentspool` 账号；同机部署 Server 与 Worker 时，账号隔离用于阻止 native Task 读取控制面状态和凭据。新安装会把 Admin/Join Token 放入 `root:agentspool-server 0640` 的独立文件，旧版 `server.env` 明文 Token 仍可启动，但应安排迁移到 file locator。

alpha.4 首次启动会将旧状态原子迁移为 schema 5，保存停止后的计算资源释放状态、待确认扩缩容和日志预览标记。旧二进制拒绝 schema 5；降级必须同时恢复升级前的控制面快照。先 drain 节点并等待任务结束，停止正在执行的沙盒，再依次升级 Server 和全部 Worker/helper；混用旧 Worker 无法提供实时续期与日志保证。安装器会备份配置、二进制和已停止的 Server/Worker 状态，并验证重启后的实际进程。

## 3. 清理用户级旧文件

确认不再需要旧 Agent Plane 本地状态后，由对应用户执行：

```sh
rm -f ~/.config/systemd/user/agentspool-bridge.service
rm -f ~/.config/agentspool/bridge.env
systemctl --user daemon-reload
```

旧 Bridge 数据可能包含运行时关联信息。不要在未检查备份需求时递归删除其数据目录，也不要把其中凭据迁入 AgentsPool 沙盒。

## 4. 验证没有接管残留

```sh
systemctl is-active agentspool-agentd.service
systemctl --user is-active agentspool-bridge.service
pgrep -af 'agentspool-(agentd|bridge|agentctl)'
```

前两条应返回 inactive 或 unit not found，最后一条不应返回旧进程。随后运行：

```sh
agentspool version
make check
make e2e
```

预期版本为 `0.4.0-alpha.4`，发行包中只应存在一个 `agentspool` 二进制。

## 5. Agent 的新调用方式

把 `.agents/skills/agentspool-compute/SKILL.md` 放在 Agent 能发现的项目或全局 Skill 目录中。Agent 自己保留 Provider Token、Prompt、上下文和 Session，只在需要远端资源时调用：

```text
agentspool tool devices.list
agentspool tool leases.acquire
agentspool tool sandboxes.ensure
agentspool tool sandboxes.exec
```

不要把旧 Worksite、RuntimeProfile、AgentSession 或 AgentRun 数据转换成新对象；0.4 没有这些对象。需要继续的 Agent 会话由原运行时自身恢复。
