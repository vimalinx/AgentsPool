# 在 OpenWrt 软路由上部署控制面

AgentsPool 控制面没有第三方运行时依赖，适合放在 x86_64、ARM64、ARMv7 或部分 MIPSLE OpenWrt 设备上。软路由只做控制塔，不建议承担重任务。

## 1. 先确认架构和存储

```sh
uname -m
cat /etc/openwrt_release
mount
```

release 包映射：

| OpenWrt 输出 | 包 |
|---|---|
| `x86_64` | `linux-amd64` |
| `aarch64` | `linux-arm64` |
| `armv7l` | `linux-armv7` |
| `mipsel` | `linux-mipsle-softfloat` |

MIPS 型号差异很大；若预编译包出现 `Illegal instruction`，应按设备的 Go 架构参数自行交叉编译。

**不要默认把数据目录放在内部 flash。** Artifact 和日志会持续增长。推荐 USB SSD：

```text
/mnt/ssd/agentspool/server
```

即使高频 heartbeat/renew 不逐次写盘，关键任务状态、日志和 Artifact 仍需持久化。

## 2. 安装文件

```sh
scp agentspool-linux-ARCH/agentspool root@router:/usr/bin/agentspool
scp deploy/openwrt/agentspool-server.init root@router:/etc/init.d/agentspool-server
scp deploy/openwrt/agentspool.config root@router:/etc/config/agentspool

ssh root@router
chmod 0755 /usr/bin/agentspool /etc/init.d/agentspool-server
chmod 0600 /etc/config/agentspool
```

## 3. 生成令牌并配置 UCI

在可信电脑生成：

```sh
./agentspool token
./agentspool token
./agentspool token
```

编辑 `/etc/config/agentspool`：

```text
config server 'main'
        option enabled '1'
        option listen '0.0.0.0:8090'
        option data_dir '/mnt/ssd/agentspool/server'
        option admin_token '...'
        option allocator_token '...'
        option join_token '...'
        option lease_ttl '45s'
        option node_timeout '35s'
        option public_metrics '0'
```

UCI 文件含明文 bootstrap 凭据，因此保持 `0600`，不要打包进公开的 sysupgrade 备份。

## 4. 启动 procd 服务

```sh
/etc/init.d/agentspool-server enable
/etc/init.d/agentspool-server start
logread -e agentspool
```

验证：

```sh
wget -qO- http://127.0.0.1:8090/healthz
```

## 5. 防火墙

只允许 LAN 或指定管理网访问。示例 UCI：

```sh
uci add firewall rule
uci set firewall.@rule[-1].name='Allow-AgentsPool-LAN'
uci set firewall.@rule[-1].src='lan'
uci set firewall.@rule[-1].proto='tcp'
uci set firewall.@rule[-1].dest_port='8090'
uci set firewall.@rule[-1].target='ACCEPT'
uci commit firewall
/etc/init.d/firewall restart
```

不要添加 WAN allow rule。异地 Worker 使用 WireGuard/Tailscale 类私网，让它们从可信 zone 访问。

## 6. 数据盘和权限

```sh
mkdir -p /mnt/ssd/agentspool/server
chmod 0700 /mnt/ssd/agentspool/server
```

OpenWrt init 脚本默认以 root 运行，因为精简系统不一定有可用动态用户。控制面本身不执行 Task，但 root 运行会扩大 Artifact/配置泄露影响。资源允许时，创建专用用户并在 init 脚本中设置 `procd_set_param user` / `group`。

## 7. 升级

```sh
/etc/init.d/agentspool-server stop
cp -a /mnt/ssd/agentspool/server /mnt/ssd/agentspool/server.backup
install -m 0755 /tmp/new-agentspool /usr/bin/agentspool
/etc/init.d/agentspool-server start
logread -e agentspool
```

控制面重启会中断兼容 Task Plane 的活动 Lease，最好先 drain Worker 或等待普通任务完成；Sandbox exec 会进入 `orphaned` 并允许原 Worker继续回报。

## 8. 在 OpenWrt 上同时运行 Worker

通常没必要，但可以使用 `deploy/openwrt/agentspool-worker.init` 和同一 UCI 文件的 `config worker` 段。建议只开启 `shell,process`，并把并发设为 1：

```text
config worker 'router'
        option enabled '0'
        option server 'http://127.0.0.1:8090'
        option data_dir '/mnt/ssd/agentspool/worker'
        option name 'soft-router'
        option join_token '...'
        option queues 'router-light'
        option executors 'shell,process'
        option max_concurrency '1'
        option request_timeout '30s'
        option transfer_timeout '0'
```

把这类节点限制在 `router-light` 队列，避免 Agent 把编译、模型或大文件任务派到路由器。

## 9. 性能与闪存建议

- 控制面 state 是内存驻留；任务/节点规模增大时，`state.json` 编码成本随总状态增长。
- 把 Artifact 放外接盘。
- Event retention 默认 0（不自动截断）；存储确实受限时可由管理员显式设置正数，例如 1000。
- 不在软路由上保留超大执行日志；上层可定期归档。
- 对内存极小设备，优先使用 x86/ARM64 小主机或 NAS 作为控制面，软路由只做网络入口。
