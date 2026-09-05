# 备份与恢复

## 1. 需要备份什么

控制面数据目录是事实源：

```text
state.json
logs/
artifacts/
```

优先级：

1. `state.json`：必须。包含 Task、Execution、Artifact 索引和事件。
2. `artifacts/`：按业务价值决定。缺失后 metadata 仍在，但内容不可下载。
3. `logs/`：用于审计和排障。

Worker 的 `cache/` 和 `jobs/` 都可重建，通常不备份。`credentials.json` 可备份，但恢复到另一台机器会复制身份；更安全的是重新注册。

令牌不在 state.json 明文出现。Admin/Join Token 来自环境文件或密码库，必须单独备份。

## 2. 一致备份

### 推荐：age 加密异机备份

备份机生成独立 identity，私钥只留在备份机：

```bash
sudo install -d -m 0700 /etc/agentspool
sudo age-keygen -o /etc/agentspool/backup-identity.txt
sudo age-keygen -y /etc/agentspool/backup-identity.txt
```

把最后一条命令输出的公开 recipient 放到控制面
`/etc/agentspool/backup-recipients.txt`。控制面只持有公钥，并配置：

```dotenv
AGENTSPOOL_BACKUP_RECIPIENT_FILE=/etc/agentspool/backup-recipients.txt
AGENTSPOOL_BACKUP_DESTINATION=backup-user@backup-host:/srv/backups/agentspool
```

把配置保存为 root-only 的 `/etc/agentspool/backup.env`，确认控制面 root
可通过专用 SSH key 非交互写入该目录，然后执行并验证第一次备份：

```bash
sudo systemctl start agentspool-backup.service
sudo systemctl enable --now agentspool-backup.timer
systemctl status agentspool-backup.timer
```

`backup-server.sh` 会短暂停止控制面，打包完整 `server/`，立即恢复服务，
使用 age 公钥加密，发送到异机并在异机执行 SHA-256 校验。明文归档只存在于
权限受限的临时目录并在退出时清理；Token 不包含在数据归档中。长期 Sandbox
服务会在控制面恢复后由 Worker 根据真实 systemd unit 自动协调。

### 最稳妥：短暂停机

```bash
sudo systemctl stop agentspool-server
sudo tar --xattrs --acls -C /var/lib/agentspool -czf \
  /backup/agentspool-server-$(date +%F-%H%M%S).tar.gz server
sudo systemctl start agentspool-server
```

### 在线快照

状态更新使用原子 rename，因此单独复制 `state.json` 不会得到半写文件。但 logs 和 artifacts 可能正在追加/上传。支持文件系统 snapshot 时，优先使用 ZFS/Btrfs/LVM snapshot。

普通 rsync 可运行两遍：

```bash
rsync -a --delete /var/lib/agentspool/server/ /backup/agentspool-server/
```

仍建议在低负载窗口执行。

## 3. 恢复到同一控制面

```bash
sudo systemctl stop agentspool-server
sudo mv /var/lib/agentspool/server /var/lib/agentspool/server.broken
sudo mkdir -p /var/lib/agentspool/server
sudo tar -C /var/lib/agentspool -xzf /backup/agentspool-server-....tar.gz
sudo chown -R agentspool-server:agentspool-server /var/lib/agentspool/server
sudo chmod 0700 /var/lib/agentspool/server
sudo systemctl start agentspool-server
```

启动时所有保存为 active 的 Lease 会被判为中断，并依据 side-effect mode 进入 retry/review/dead-letter。Worker 下一次 heartbeat 恢复 online。

## 4. 控制面全丢失

如果没有 state 备份：

1. 新建空数据目录和新 Admin/Join Token。
2. 所有历史 Task、Execution、Artifact 索引无法自动重建。
3. 在每台 Worker 删除 `credentials.json`，配置新 Join Token 并重新启动。
4. Worker cache 中的 Artifact 文件名只有旧 `art_id`，新控制面没有对应 metadata，不能直接作为输入；需要重新上传原文件。

因此 state.json 是最关键的备份对象。

## 5. 验证备份

不要只看压缩包存在。推荐在持有 age 私有 identity 的备份机执行：

```bash
./scripts/restore-drill.sh \
  --archive /srv/backups/agentspool/agentspool-control-TIMESTAMP.tar.gz.age \
  --identity-file /etc/agentspool/backup-identity.txt \
  --binary /usr/local/bin/agentspool
```

脚本先校验归档路径，再解密到临时目录，使用全新临时 Token 在 loopback
启动隔离控制面，验证 `/readyz`，并逐个读取所有 Artifact 内容计算 SHA-256。
它不会连接真实 Worker，也不会覆盖生产目录。成功输出包含恢复后的 schema、
Node、Sandbox 和已验证 Artifact 数量。

也可以手工验证：

```bash
mkdir /tmp/agentspool-restore-test
# 解压到该目录
AGENTSPOOL_ADMIN_TOKEN=<test-token-24+> \
AGENTSPOOL_JOIN_TOKEN=<test-join-token-24+> \
agentspool server --listen 127.0.0.1:18090 \
  --data-dir /tmp/agentspool-restore-test
```

注意测试 token 与原控制面 token 不同并不影响读取 state，因为 Admin/Join Token 不存入 state。

验证 `/readyz`、`status`、历史任务和随机 Artifact 下载。不要让测试实例与真实 Worker 网络互通，避免 Worker 误连。

## 6. 保留策略

一个实用家庭策略：

- 每小时或每 6 小时复制 state.json。
- 每日增量备份 logs/artifacts。
- 保留 7 个日备份、4 个周备份、3 个完整月备份。
- 重要发布/付款/科研结果 Artifact 额外复制到对象存储或离线盘。
- 备份加密，令牌与数据备份分开保存。

## Worker 持久卷备份与恢复（alpha.4）

控制面备份中的 Sandbox 记录只有元数据，不能恢复远端工作目录。每个 Worker 的 `/var/lib/agentspool/sandboxes/sbx_*/storage.ext4` 包含 workspace 与 home，需要另外备份。

先通过 scoped `sandboxes.stop` 停止目标沙盒并等待 Operation 成功。以下命令在所属 Worker 上执行，私钥不应放在 Worker 长期在线文件系统中；示例恢复可使用临时取回的 `0600` 私钥。

```sh
sudo python3 scripts/sandbox-volume.py backup --sandbox-id sbx_REPLACE \
  --recipient-file /etc/agentspool/backup.recipient \
  --archive /safe/volume.tar.gz.age
sudo python3 scripts/sandbox-volume.py restore --sandbox-id sbx_REPLACE \
  --identity-file /safe/backup.agekey --archive /safe/volume.tar.gz.age \
  --data-dir /safe/isolated-restored-sandboxes
```

脚本持有与 helper 相同的逐沙盒锁，拒绝活动操作或挂载中的卷；age 加密包包含 ext4、metadata 和各文件 SHA256/大小清单。恢复拒绝链接、路径逃逸、重复条目、损坏内容和已有目标，完成身份、校验和、只读 ext4 检查后才发布恢复目录。备份不包含 Worker Node Token、Server token、中央状态或本地 Operation 日志。

先在隔离目录恢复并检查文件，再由所有者安排切换。生产恢复需要匹配的控制面 Sandbox/Lease/Node 状态和相同 Worker 节点身份；卷的 UID/GID 也应与原 Worker 账号一致。该操作不提供自动跨节点迁移。切勿用控制面备份成功代替 Worker 卷恢复验收。
