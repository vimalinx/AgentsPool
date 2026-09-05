# AgentsPool

AgentsPool is a small, runtime-neutral coordination layer for Agents using computers on a trusted LAN. It exposes one discoverable Skill, one structured CLI, and scoped persistent sandboxes. It does not launch, host, or take over an Agent runtime.

```text
Codex / Claude / OMP / Pi / any Agent
                  │ discovers the agentspool-compute Skill
                  ▼
          agentspool tool <name>
                  │
                  ▼
 Device revision → ResourceLease → Persistent Sandbox → Operation → Artifact
```

Version **0.4.0-alpha.4** keeps provider credentials, models, prompts, context, sessions, and tool loops inside the calling Agent. AgentsPool owns only device discovery, CPU/memory/disk admission, scoped leases, sandbox lifecycle, and explicit artifact transfer.

The former `agentspool-agentd`, `agentspool-bridge`, `agentspool-agentctl`, runtime adapters, and Agent Plane API are removed from source, builds, and releases. The legacy Task API remains available for deterministic batch jobs, but it never manages Agent sessions.

Tagged GitHub releases publish checksums and an SPDX 2.3 SBOM, with GitHub attestations for both build provenance and the SBOM.

Agents discover the installed binary locally with `agentspool tool list --json` and `agentspool tool describe TOOL_NAME --json`. The versioned catalog exposes auth alternatives, mutability, input JSON Schema, and examples from the same registry used to execute calls. Protected `AGENTSPOOL_*_TOKEN_FILE` locators avoid placing credentials in command arguments or long-lived environment values.

`scripts/install-client.sh` installs the same CLI and Skill for every Agent under a user account and writes a private client profile containing only the server URL and allocator token-file locator. The 30-tool catalog includes explicit Operation cancellation, health-probed service discovery, sandbox-scoped Artifact cleanup, and dry-run-first lifecycle maintenance. Resource leases, execs, transfers, output capture, caches, and Worker drain have no default time or byte ceiling; CPU, memory, and persistent disk remain hardware-backed admission and cgroup limits.

Device snapshots keep measured capabilities separate from sourced descriptions. Humans can record availability, long-running service policy, preferred or forbidden workloads, and hard constraints; Agents add revision-based recommendations under their own names and cannot overwrite human contributions. An explicit human device choice maps to that exact `node_id`; autonomous placement applies only when no device was prescribed.

Start with the [English two-machine quickstart](docs/QUICKSTART.en.md) and [platform matrix](docs/PLATFORMS.md), or [START_HERE.zh-CN.md](START_HERE.zh-CN.md), [the generic Agent tool contract](docs/AGENT-TOOLS.zh-CN.md), [the architecture](docs/ARCHITECTURE.zh-CN.md), and [the data lifecycle](docs/DATA-LIFECYCLE.zh-CN.md). Existing 0.2/0.3 installations should also follow [the 0.4 migration guide](docs/MIGRATION-v0.4.zh-CN.md).

The control plane binds to `127.0.0.1:8090` by default. Workers enable only the shell executor. Native shell/process Tasks use the host network and are for trusted workloads; `ALLOW_NETWORK=false` restricts Docker Tasks, not native Tasks. Persistent Sandboxes have isolated network namespaces unless `permissions.network=true` explicitly grants the node network.
