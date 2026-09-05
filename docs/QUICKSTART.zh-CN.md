# 第一个远端持久沙盒

准备两台可信机器，A 运行控制面，B 运行 Worker 与持久沙盒。客户端先放在 B，也可以装在第三台 Linux 机器上。以下按 Ubuntu/Debian + systemd 编写，不使用 GPU，不要求 Docker。其他平台先看[支持矩阵](PLATFORMS.md)；[English guide](QUICKSTART.en.md) 提供相同流程。

## 1. 两台机器准备发行包和依赖

下载并核对发行包 SHA256，解压后进入目录。`x86_64` 选择 `linux-amd64`，`aarch64` 选择 `linux-arm64`。二进制发行包不需要 Go；从源码安装时使用 Go 1.27，先运行 `make build`，安装脚本也会自动发现 `dist/agentspool`。

```sh
sudo apt-get update
sudo apt-get install -y bubblewrap e2fsprogs util-linux python3 curl age jq
./agentspool version
```

沙盒需要 systemd、cgroup v2、loop ext4 和足够磁盘空间。OpenWrt/procd 可部署控制面和可选可信原生 Task Worker，不能因此获得持久沙盒能力。

## 2. 在 A 安装控制面

```sh
sudo ./scripts/install-server.sh
sudoedit /etc/agentspool/server.env
```

将 `AGENTSPOOL_LISTEN` 改为 A 的实际私网地址，例如 `192.0.2.10:8090`。这个地址仅供文档展示，必须替换。保留安装器生成的三个 `*_TOKEN_FILE` 配置，不要用 example 文件覆盖已有配置。

```sh
sudo systemctl restart agentspool-server
curl --fail http://192.0.2.10:8090/readyz
```

只允许可信 LAN 或私有组网访问 8090；经过不可信网络时配置 TLS 或可信 TLS 反代，详见[安全文档](SECURITY.md)。安装器在 `/etc/agentspool/` 生成互不相同的 `admin.token`、`allocator.token` 和 `join.token`，不会显示令牌值。

Admin 留在 A。使用已有的安全文件传输方式，将 Join Token 文件交给 B，将 Allocator Token 文件交给客户端用户。副本设为 `0600`，不要将令牌值粘贴进命令、聊天或仓库。

## 3. 在 B 安装 Worker

假设收到的受保护 Join Token 文件为 `/root/agentspool-join.token`。

```sh
sudo env AGENTSPOOL_SERVER=http://192.0.2.10:8090 \
  AGENTSPOOL_JOIN_TOKEN_FILE=/root/agentspool-join.token \
  AGENTSPOOL_WORKER_NAME=worker-b ./scripts/install-worker.sh
sudo systemctl status agentspool-worker agentspool-sandboxd --no-pager
```

Worker 主动连接 A，不需要开放入站端口，也不需要控制面通过 SSH 进入 B。注册成功后会保存 `/var/lib/agentspool/worker/credentials.json`。确认该文件存在后，从 `worker.env` 删除 Join Token locator，并删除 `/etc/agentspool/worker-join.token` 和传输副本；保留节点凭据，后续升级继续使用相同节点身份。

## 4. 给客户端配置普通 Agent 权限

以下命令以客户端普通用户运行。`ALLOCATOR_FILE` 是刚收到的受保护令牌文件的绝对路径。

```sh
ALLOCATOR_FILE="$HOME/.config/agentspool/allocator.token"
chmod 0600 "$ALLOCATOR_FILE"
./scripts/install-client.sh ./agentspool http://192.0.2.10:8090 "$ALLOCATOR_FILE"
export PATH="$HOME/.local/bin:$PATH"
agentspool tool list --json
agentspool tool devices.list
```

源码构建时将 `./agentspool` 换成 `./dist/agentspool`。从 devices 输出中找到 B 的 `node_id`，确认 capabilities 包含 `sandbox.systemd-bwrap`。任意 Agent 都使用相同的 CLI 和安装到共享目录的 `agentspool-compute` Skill。

## 5. 申请、执行、取回文件、清理

```sh
./scripts/first-sandbox.sh node_REPLACE_WITH_WORKER_ID /tmp/agentspool-result.txt
cat /tmp/agentspool-result.txt
```

预期内容为 `HELLO FROM AGENTSPOOL`。脚本会读取最新设备 revision，申请 250m CPU、128 MiB 内存和 64 MiB 磁盘，将 scoped token 保存在私有临时目录，依次创建沙盒、推送文件、执行转换、读取日志、下载结果，最后等待销毁完成并释放租约。已有输出文件会被拒绝覆盖。若网络故障导致清理失败，脚本保留受保护的恢复凭据并提示路径，不会误报全部成功。

脚本位于 `scripts/first-sandbox.sh`，可以直接阅读各步命令；其他输入结构用 `agentspool tool describe TOOL_NAME --json` 查询。[工具契约](AGENT-TOOLS.zh-CN.md) 解释各对象权限与行为。

## 6. 生命周期与隔离边界

- stop 成功后进程已退出，CPU/内存预留释放，磁盘与文件保留；start 必须重新做准入，容量不足会失败。
- resize 仅允许空闲沙盒，CPU/内存新额度在 helper 成功后提交，磁盘不能缩小。
- `operations.logs` 可在运行时读取完整日志；stdout/stderr 各有字节游标。Operation 本身只内联最后最多 64 KiB，完整日志默认不自动删除。
- 原生 shell/process Task 使用宿主网络和 Worker 账号权限，只接受可信任务。`ALLOW_NETWORK=false` 约束 Docker Task；持久 Sandbox 默认使用独立网络 namespace，只有明确授予 `permissions.network=true` 才使用节点网络。

## 7. 升级、备份与卸载

先在 A 使用 Admin CLI drain B，停止正在执行的 Sandbox，并等待 Task 结束；再依次安装新 Server 和全部 Worker/helper。安装器在 `/var/backups/agentspool-install` 保存 root-only 备份，重启之前运行的共享二进制服务，检查实际进程版本与就绪状态，失败时恢复快照。刻意停止的旧服务不会被自动启动。alpha.4 使用 schema 5，降级需要恢复匹配的旧状态备份。

控制面备份不包含各 Worker 的磁盘。工作目录和 home 卷需要在所属 Worker 停止沙盒后单独使用 `scripts/sandbox-volume.py` 加密备份和校验恢复，见[备份恢复](BACKUP_RECOVERY.zh-CN.md)。

`sudo ./scripts/uninstall.sh` 先停止沙盒进程，再停止 Worker，移除程序与维护 timer，保留配置和数据。加 `--purge` 会删除标准配置/数据目录；安装备份和自定义数据目录仍由所有者显式管理。

Compose 只运行控制面。在源码或匹配架构的二进制包里运行 `./scripts/init-compose.sh` 后再执行 `docker compose up -d`。初始化自动生成三个随机令牌并保护 `.env`，已有文件不会被覆盖。
