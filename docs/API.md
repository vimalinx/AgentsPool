# HTTP API overview

Base path: `/api/v1`.

Authentication:

- Admin endpoints: `Authorization: Bearer <admin-token>`.
- Allocator endpoints: `Authorization: Bearer <allocator-token>`; Admin is also accepted.
- Worker endpoints: `Authorization: Bearer <node-token>`.
- Registration: `X-AgentsPool-Join-Token: <join-token>`.
- Lease endpoints: Node bearer token plus `X-AgentsPool-Lease-Token: <lease-token>`.
- Resource lease endpoints: Admin bearer or matching `X-AgentsPool-Resource-Lease-Token`.
- Sandbox endpoints: Admin bearer or matching `X-AgentsPool-Sandbox-Token`.
- Worker sandbox completion/export: Node bearer plus matching `X-AgentsPool-Sandbox-Operation-Token`.

All JSON request decoders reject unknown fields. Errors use:

```json
{"error":{"code":"machine_code","message":"human readable message"}}
```

The complete machine-readable contract is [`../api/openapi.yaml`](../api/openapi.yaml).
Agents should start with `agentspool tool list --json` and `agentspool tool describe TOOL_NAME --json`; that local versioned catalog exposes only the runtime-neutral Agent tools, their auth alternatives, mutability, input JSON Schema, and examples.

## Public health

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Process liveness and version |
| GET | `/readyz` | State store can produce a snapshot |

`GET /metrics` requires Admin auth unless `public_metrics` is enabled.

## Worker lifecycle

| Method | Path |
|---|---|
| POST | `/workers/register` |
| POST | `/workers/{nodeID}/heartbeat` |
| POST | `/workers/{nodeID}/claim` |

Claim returns a Task copy, Lease ID, one-time Lease Token, Execution ID, expiry and TTL.

## Lease lifecycle

| Method | Path |
|---|---|
| POST | `/leases/{leaseID}/start` |
| POST | `/leases/{leaseID}/renew` |
| POST | `/leases/{leaseID}/logs` |
| POST | `/leases/{leaseID}/artifacts` |
| POST | `/leases/{leaseID}/complete` |
| POST | `/leases/{leaseID}/fail` |
| POST | `/leases/{leaseID}/cancelled` |

A completion may only reference Artifacts produced by the same Execution. Terminal reports against an expired or finished Lease receive 409.

## Admin

| Method | Path |
|---|---|
| GET | `/status` |
| GET | `/nodes` |
| GET | `/nodes/{nodeID}` |
| POST | `/nodes/{nodeID}/drain` |
| POST | `/nodes/{nodeID}/enable` |
| POST | `/nodes/{nodeID}/disable` |
| DELETE | `/nodes/{nodeID}` |
| POST | `/tasks` |
| GET | `/tasks` |
| GET | `/tasks/{taskID}` |
| POST | `/tasks/{taskID}/cancel` |
| POST | `/tasks/{taskID}/retry` |
| GET | `/tasks/{taskID}/logs` |
| GET | `/executions/{executionID}/logs` |
| POST | `/artifacts` |
| GET | `/artifacts` |
| GET | `/artifacts/{artifactID}` |
| GET | `/artifacts/{artifactID}/content` |
| GET | `/events` |

## Compute Tool Plane

| Method | Path | Purpose |
|---|---|---|
| GET | `/devices`, `/devices/{nodeID}`, `/devices-watch` | Revisioned placement snapshots and immediate changes |
| GET | `/services` | Endpoints declared by active long-lived Operations plus live declared-health results |
| PUT/DELETE | `/devices/{nodeID}/descriptions` | Upsert or remove one sourced human/Agent description contribution |
| POST/GET | `/resource-leases` | Agent-selected CPU/memory/disk reservation and listing |
| GET/DELETE | `/resource-leases/{leaseID}` | Inspect or release a scoped reservation |
| POST | `/resource-leases/{leaseID}/renew`, `/resize` | Renew or resize an unattached reservation |
| POST/GET | `/sandboxes` | Create or list persistent sandboxes |
| GET/DELETE | `/sandboxes/{sandboxID}` | Inspect or destroy a scoped sandbox |
| POST | `/sandboxes/{sandboxID}/exec`, `/resize`, `/rebind` | Execute or change resources/lease binding |
| POST | `/sandboxes/{sandboxID}/start`, `/stop` | Resume or stop while preserving persistent data |
| POST | `/sandboxes/{sandboxID}/files`, `/files/export` | Push or pull explicit files through Artifacts |
| GET/DELETE | `/sandboxes/{sandboxID}/artifacts`, `/sandboxes/{sandboxID}/artifacts/{artifactID}` | List or delete sandbox-owned bridge Artifacts |
| GET | `/sandbox-operations/{operationID}` | Inspect asynchronous operation state |
| POST | `/sandbox-operations/{operationID}/cancel` | Explicitly cancel a queued or running Operation |
| POST | `/maintenance/prune` | Admin dry-run/execute cleanup of terminal Sandbox history, stale bridge Artifacts, and obsolete observation events |

Resource requests deliberately contain only CPU, memory and disk. Unknown GPU fields are rejected. Full request/response schemas and Worker-only routes are in OpenAPI.

Artifact content may also be downloaded by any enabled Node Token because workers need to materialize inputs.

## Pagination

List APIs currently use a bounded `limit` but not cursors. Results are newest-first for Tasks, Artifacts and Events. This is deliberate for the single-control-plane Alpha state store; API clients should not assume unlimited historical enumeration.

## Idempotency

`TaskSpec.idempotency_key` is persisted globally. Repeating `POST /tasks` with the same non-empty key returns HTTP 200, `created: false`, and the original task. A new key returns HTTP 201.
