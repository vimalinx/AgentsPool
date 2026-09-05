# Security policy

## Supported versions

| Version | Security fixes |
|---|---|
| 0.4.x Alpha | Supported |
| 0.3.x and older | Unsupported; upgrade before reporting |

## Reporting a vulnerability

Use the repository **Security** tab and choose **Report a vulnerability**. Do not include exploit details, credentials, private network addresses or user data in a public issue. If private vulnerability reporting is not available yet, open a public issue that only asks the maintainer to establish a private contact channel.

Include the affected version, deployment shape, impact and a minimal reproduction that uses synthetic tokens and data. The maintainer targets an initial acknowledgement within 7 days and a status update within 30 days. Complex fixes may take longer; disclosure timing will be coordinated with the reporter.

## Security boundary

AgentsPool is an administrative execution fabric. Possession of the admin token grants task submission and pool-management authority. Possession of the join token permits a machine to enroll and rotate the credential for the same machine ID. Store both as root-only secrets and use different random values.

Native `shell` and `process` tasks execute with the operating-system permissions of the Worker service account. They are for trusted workloads. Use the Docker executor with `network: none`, resource limits and `read_only_root: true` for stronger isolation, while recognizing that access to a Docker daemon is itself highly privileged.

Do not expose the HTTP control plane directly to the public Internet. Bind it to a trusted LAN or private overlay, apply firewall rules, and use TLS or a trusted reverse proxy whenever traffic can traverse an untrusted network. See `docs/SECURITY.md` for the full threat model and hardening checklist.

AgentsPool is currently a single-administrator trusted domain, not a hostile multi-tenant service. All registered Nodes can download pool Artifacts. Task definitions, command environment values, stdout/stderr, logs and Artifact metadata may be persisted. See `docs/DATA-LIFECYCLE.zh-CN.md` before processing sensitive data.
