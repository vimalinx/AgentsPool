# Changelog

## 0.4.0-alpha.4

- Renew running sandbox leases from authoritative policy, including finite-to-unlimited changes.
- Serialize operations per sandbox; acknowledge real process termination before stop; retain persistent disk while releasing compute.
- Stage idle resize resources until helper acknowledgement and keep conservative reservations after partial disk growth.
- Honor draining nodes for sandbox claims.
- Persist and stream full sandbox output with byte cursors, bounded inline previews and explicit opt-in rotation.
- Add encrypted, checksum-verified offline Worker volume backup/restore.
- Reject example/shared bootstrap credentials and generate three random Compose tokens without displaying them.
- Correct native Task network guarantees and add bilingual two-machine sandbox quickstarts and platform support matrix.
- Repair binary archive installers and Docker context; back up, restart and verify shared binaries during upgrades; restore on failure and remove all maintenance units on uninstall.
- Add real systemd/bwrap, backup/restore, installation and history-secret checks to CI and release qualification.
- State schema advances to 5; back up before upgrading and restore the matching snapshot before downgrading.

## 0.4.0-alpha.3 — 2026-09-01

- Eliminated idle Worker write amplification: heartbeat telemetry stays volatile and empty Task/Sandbox claims no longer rewrite the complete state snapshot or append registry observations.
- Added sandbox-scoped Artifact discovery/deletion, destroy-time bridge cleanup, and maintenance cleanup for stale Artifact metadata/content.
- Extended explicit maintenance to compact terminal Operation history from live sandboxes and remove obsolete `device.observed` registry events without introducing age, count, or runtime limits.
- Added Worker/systemd reconciliation for orphaned long-running services and live `health_url` results to service discovery.
- Added age-encrypted off-host backup, isolated restore-drill tooling, and a daily systemd TLS certificate expiry check.
- Expanded the runtime-neutral tool catalog from 27 to 29 tools.

## 0.4.0-alpha.2 — 2026-09-01

- Added an allocator credential and private client profile so any Agent can discover devices and reserve CPU/memory/disk without receiving the administrator token.
- Added cancellable, restart-reconcilable sandbox Operations and concurrent execution across independent sandboxes; no Agent task or sandbox command receives a default deadline.
- Added declared service endpoints with pool-wide discovery while a long-running Operation is active.
- Added explicit dry-run-first maintenance pruning for destroyed sandbox history; AgentsPool still performs no automatic time-based deletion.
- Added a shared client/Skill installer and expanded the generic catalog to 27 runtime-neutral tools.

## 0.4.0-alpha.1 — 2026-08-31

- Made the runtime-neutral `agentspool-compute` Skill plus `agentspool tool` the only Agent integration surface.
- Removed the embedded browser dashboard so the maintained control surfaces are the CLI, Skill, and HTTP API only.
- Removed `agentspool-agentd`, `agentspool-bridge`, `agentspool-agentctl`, the Agent Plane API, Pi RPC adapter, runtime fixtures, and takeover services.
- Reduced every build and release target to one `agentspool` binary.
- Kept provider credentials, models, prompts, context, sessions, and usage reporting entirely inside each calling Agent runtime.
- Retained Device revision, CPU/memory/disk ResourceLease, persistent Sandbox, Operation, Artifact, and the compatibility Task API.
- Added migration checks for disabling and removing previously installed takeover services.
- Extended the persistent LAN qualification topology and scripts to a third workstation Worker.
- Removed stale Agent Plane variables from the public environment template and added a repository guard against their return.
- Changed direct server startup to loopback-only and made Workers shell-only with network denied unless explicitly enabled.
- Made scoped tool calls omit the admin Bearer and applied one bounded timeout to requests, file transfers, and operation waits.
- Added a versioned local `tool list/describe` catalog with auth alternatives, JSON input schemas, and examples from the same registry used for execution.
- Added provenance-preserving Device descriptions so human availability/workload policy remains authoritative while named Agents contribute separate revision-based recommendations.
- Added protected token-file locators for CLI, Server, Worker, ResourceLease, and Sandbox credentials, and made systemd installers use separate credential files.
- Separated the control plane into an `agentspool-server` service account so Workers and native Tasks cannot read server state or credential files on a combined node.
- Added CI coverage for Compose validation, container execution, byte-for-byte release reproducibility, and source archives verified without Git metadata.
- Upgraded release builds to Go 1.27 and Alpine 3.24, pinned Docker/Actions inputs, added govulncheck, SPDX SBOM/provenance attestations, and Dependabot updates.
- Added public security reporting, data-lifecycle documentation, deterministic archive metadata, and source-archive builds that no longer require `.git`.
- Made Worker credential replacement crash-durable and closed Python SDK HTTP error responses deterministically.
- Added terminal Task/Execution/log deletion, reference-safe Artifact deletion, forced metadata scrubbing, and dry-run-first orphan Artifact GC.

## 0.3.0-alpha.1 — 2026-08-31

- Added revisioned real-time Device views and long-poll watch events.
- Added Agent-selected, optimistic-concurrency CPU/memory/disk resource leases.
- Added scoped persistent sandbox lifecycle, systemd cgroups, bwrap namespaces, and per-sandbox ext4 disk quotas.
- Added persistent stop/start, online resize, and same-node resource-lease rebind without data loss.
- Added content-addressed scoped file push/pull and persistent sandbox rebind after lease expiry.
- Added the runtime-neutral `agentspool tool` surface and project Agent Skill.
- Kept GPU strictly telemetry-only; resource and sandbox requests reject GPU fields.

## 0.2.0-alpha.2 — 2026-08-30

- Restricted GPU information to observational node telemetry.
- Removed GPU requirements, GPU-aware scoring, GPU queue examples, and Docker GPU allocation.
- Added regression tests proving identical eligibility and score with or without GPU telemetry.
- Added a LAN qualification path for the minimum CPU/RAM/disk scheduler.

## 0.2.0-alpha.1 — 2026-08-30

- Added an independent Worksite-affine Agent Plane rather than extending batch TaskSpec.
- Added `agentspool-agentd`, per-user `agentspool-bridge`, and `agentspool-agentctl`.
- Added Pi v0.82.1 RPC session management with prompt, steer, follow-up, abort, compact, fork, clone and state/usage synchronization.
- Added server-enforced `agent_settled` settlement, native Session pointers, Worksite leases and no-automatic-migration semantics.
- Added private prompt/command delivery, double-layer event redaction, persistent Bridge credentials and 0700/0600 session hardening.
- Added protocol-level two-Run E2E validation with native Session reuse.
- Preserved the complete 0.1 Task Plane for deterministic batch workloads.

## 0.1.0 — 2026-08-30

First complete runnable release of AgentsPool.

- Single static Go binary for control plane, worker, CLI and Web dashboard.
- Worker registration, capability/resource discovery, heartbeats and node lifecycle.
- Priority/requirements/data-locality-aware task scheduling with server-authoritative lease concurrency accounting.
- Lease-based execution with renewal, cancellation, retry, dead-letter and `needs_review` protection for non-idempotent work.
- Shell, process and Docker executors with worker-side policy gates and private Docker env-file secret injection.
- Content-addressed artifacts, SHA-256 download verification, concurrency-safe worker cache, separate control/transfer deadlines, input materialization and recursive output collection.
- Persistent task state, execution history, event audit trail, JSONL logs and Prometheus text metrics.
- OpenWrt/procd, systemd and Docker control-plane deployment assets.
- Python SDK, examples, OpenAPI contract, automated unit/integration/race/E2E tests.
