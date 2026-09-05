# Platform support / 平台支持

A cross-compiled binary is not evidence that every executor works on that platform.
发行架构表示二进制可构建，不代表所有执行器均已在该平台验收。

| Platform / 平台 | Control plane + CLI / 控制面与客户端 | Native Task Worker | Persistent Sandbox / 持久沙盒 |
|---|---|---|---|
| Linux amd64, systemd + cgroup v2 | Supported / 支持 | Trusted shell/process | Supported; real Ubuntu VM/CI qualification |
| Linux arm64, systemd + cgroup v2 | Supported / 支持 | Trusted shell/process | Supported design; re-run qualification on your node |
| Linux armv7, systemd + cgroup v2 | Binary supplied / 提供二进制 | Depends on installed tools | Requires bwrap, loop ext4 and working cgroup limits; not qualified by amd64 CI |
| OpenWrt/procd, including mipsle softfloat | Control-plane/CLI package supplied | Optional trusted lightweight tasks | Unsupported without systemd/bwrap; does not advertise sandbox capability |
| Linux container | Control plane; source or matching binary image | Native host installation preferred | Containerized sandboxd unsupported |
| macOS / Windows | No native release supplied | Unsupported | Use a Linux VM |

Sandbox dependencies: Linux systemd as PID 1, cgroup v2, bubblewrap, util-linux
(`setpriv`, `mount`, `umount`, `flock`), e2fsprogs (`mkfs.ext4`, `e2fsck`,
`resize2fs`), root installation, loop devices and enough local disk.
Ubuntu/Debian: `sudo apt-get install bubblewrap e2fsprogs util-linux python3 curl age`.
For shell examples and installation tests also install `jq`. Docker is optional
and is not needed for persistent Sandboxes. GPU detection remains off.

原生 shell/process Task 使用 Worker 账号权限和宿主网络，仅用于可信任务。
Docker Task 的网络开关与 Sandbox 的 `permissions.network` 是各自独立的边界。
系统内核隔离不能作为敌对多租户或硬件虚拟机承诺。
