# First remote sandbox

Use two trusted machines: **A** runs the control plane; **B** runs the Worker and
persistent sandboxes. Run the client on B initially, or install it on a third
Linux machine. See the [platform matrix](PLATFORMS.md). The examples below target
Ubuntu/Debian with systemd. They use no GPU and need no Docker.

## 1. Download and prepare both machines

Download and verify the release checksums, then extract the archive matching
`uname -m`: `x86_64` → `linux-amd64`, `aarch64` → `linux-arm64`.
Run installation commands from that extracted directory. No Go compiler is
required for binary releases. From source, use Go 1.27 and `make build`; installers
also find `dist/agentspool` automatically.

```sh
sudo apt-get update
sudo apt-get install -y bubblewrap e2fsprogs util-linux python3 curl age jq
./agentspool version
```

## 2. Start the control plane on A

```sh
sudo ./scripts/install-server.sh
sudoedit /etc/agentspool/server.env
```

Set `AGENTSPOOL_LISTEN` to A's actual private IP and port, for example
`192.0.2.10:8090`. That address is reserved for documentation: replace it.
Keep the generated `*_TOKEN_FILE` settings. Never copy the example environment
file over a configured installation. The installer generates three distinct
credentials in `/etc/agentspool/{admin,allocator,join}.token` without printing them.

```sh
sudo systemctl restart agentspool-server
curl --fail http://192.0.2.10:8090/readyz
```

Allow TCP 8090 only from the trusted LAN/private overlay. Use TLS or a trusted TLS
proxy across untrusted networks; see [security](SECURITY.md). Admin credentials
stay on A. Transfer the Join Token file to B and the Allocator Token file to the
client account over your existing secure file-transfer channel. Store each copy
with mode `0600`; do not paste token values into shell commands or chat.

## 3. Install B's Worker

Assume the protected Join Token copy is `/root/agentspool-join.token` on B.

```sh
sudo env AGENTSPOOL_SERVER=http://192.0.2.10:8090 \
  AGENTSPOOL_JOIN_TOKEN_FILE=/root/agentspool-join.token \
  AGENTSPOOL_WORKER_NAME=worker-b ./scripts/install-worker.sh
sudo systemctl status agentspool-worker agentspool-sandboxd --no-pager
```

The Worker connects outbound to A. No inbound Worker port or control-plane SSH
access is needed. Successful registration saves node credentials in
`/var/lib/agentspool/worker/credentials.json`. After confirming it exists, remove
the bootstrap locator from `/etc/agentspool/worker.env` and securely remove your
Join Token copies, including `/etc/agentspool/worker-join.token`. Keep the durable
node credentials. Subsequent upgrades preserve that node identity.

## 4. Give the client allocator access

Run as the normal client user. `ALLOCATOR_FILE` is the absolute path to the
owner-only allocator file you transferred; the value is a path, never a token.

```sh
ALLOCATOR_FILE="$HOME/.config/agentspool/allocator.token"
chmod 0600 "$ALLOCATOR_FILE"
./scripts/install-client.sh ./agentspool http://192.0.2.10:8090 "$ALLOCATOR_FILE"
export PATH="$HOME/.local/bin:$PATH"
agentspool tool list --json
agentspool tool devices.list
```

For source builds substitute `./dist/agentspool` for `./agentspool`. Select B's
`node_id` from the returned devices; it must advertise `sandbox.systemd-bwrap`.
All Agents use this same CLI and installed `agentspool-compute` Skill.

## 5. Create, execute, download and clean up

```sh
./scripts/first-sandbox.sh node_REPLACE_WITH_WORKER_ID /tmp/agentspool-result.txt
cat /tmp/agentspool-result.txt
```

Expected result: `HELLO FROM AGENTSPOOL`. The readable script takes a fresh device
revision, acquires 250m CPU / 128 MiB RAM / 64 MiB disk, saves scoped credentials
in a private temporary directory, creates a sandbox, pushes a file, executes a
command, reads logs, pulls the result, then waits for sandbox destruction and
releases its lease. It refuses to overwrite an existing output file. If cleanup
cannot reach the server it leaves private recovery credentials and prints their
location, rather than claiming success.

Explore other operations with `agentspool tool describe TOOL_NAME --json`.
Stopping a sandbox preserves its disk and releases CPU/RAM; starting it must
reacquire capacity. Resize is allowed only while idle. Running output is available
through `operations.logs`; stdout/stderr have separate byte cursors and full logs
are retained by default, while operation results contain a 64 KiB tail.

## Upgrades, recovery and removal

Drain B with the Admin CLI on A, stop running sandboxes, and wait for Tasks to
finish. Install the new Server, then Worker/helper binaries using the same
scripts. Installers retain root-only rollback snapshots under
`/var/backups/agentspool-install`, restart previously active services sharing the
binary, verify their executable, and restore the saved version on failure.
Deliberately stopped services remain stopped. State schema 5 requires restoring
the matching state snapshot when downgrading.

`sudo ./scripts/uninstall.sh` stops sandbox units before the Worker, removes
programs and all maintenance timers, and preserves configuration/data.
`--purge` additionally deletes the standard configuration/data directories.
Installer backups remain for explicit owner retention decisions. Custom data
directories remain owner-managed.

Back up both the [control plane and Worker volumes](BACKUP_RECOVERY.zh-CN.md).
For a stopped sandbox on its owning Worker:

```sh
sudo python3 scripts/sandbox-volume.py backup --sandbox-id sbx_REPLACE \
  --recipient-file /etc/agentspool/backup.recipient --archive /safe/volume.tar.gz.age
sudo python3 scripts/sandbox-volume.py restore --sandbox-id sbx_REPLACE \
  --identity-file /safe/backup.agekey --archive /safe/volume.tar.gz.age \
  --data-dir /safe/isolated-restored-sandboxes
```

Restore validates encrypted contents, identity, checksums and ext4 before making
the volume available. It refuses active/mounted sandboxes and existing targets.
A restored volume also needs the matching control-plane state and node identity;
this is not automatic cross-node migration.

Compose runs the control plane only. From source or a matching binary archive,
run `./scripts/init-compose.sh`, then `docker compose up -d`. Initialization
creates a protected `.env` and refuses to overwrite an existing one.
