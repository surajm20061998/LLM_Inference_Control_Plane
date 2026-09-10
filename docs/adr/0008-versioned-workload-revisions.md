# ADR 0008: Version workload revision payloads and freeze runtime defaults

- Status: Accepted
- Date: 2026-09-09

## Context

`ControllerRevision` is the rollback source for a `ModelDeployment`. The original
payload stored an unversioned projection of the API object. It included
`serving.port`, although that field changes only the stable Service, and omitted
`serving.startupTimeout`, although that field changes every engine Pod's startup
probe. It also stored empty engine and shim image fields when the controller chose
their effective images from operator defaults. A later controller could therefore
render an old revision using a new default image.

Changing the old projection in place is unsafe: existing revision names are hashes
of the original bytes. Recalculating those names would orphan status references and
make rollback history unreachable.

## Decision

New records use a `v2` envelope whose version is included in the hashed bytes:

```json
{
  "version": "v2",
  "workload": {
    "model": {},
    "engine": {},
    "serving": {
      "startupTimeout": "300s",
      "shim": {}
    }
  }
}
```

Before encoding, the controller resolves values that otherwise come from API or
operator defaults: model image path and pull policy; engine type, image, pull
policy, context size, concurrency and derived thread count; startup timeout; and
shim enablement, image, resources and log level. The stored workload can therefore
be rendered without consulting a future controller's defaults. The canonical bytes
and their FNV-based revision identifier are returned together and the recorder
rejects an explicit payload under a different identifier.

The workload/policy boundary is:

| Field family | V2 workload identity | Reason |
|---|---:|---|
| Model name and source | Yes | Changes the artifact, mount or served identity in the Pod. |
| Engine type, effective image, args, env, credentials and resources | Yes | Changes the engine container or runtime. |
| Effective engine threads/concurrency | Yes | Changes runtime capacity and shim queue calculation. |
| Shim enabled, effective image, resources and log level | Yes | Changes the request-path sidecar. |
| Serving startup timeout | Yes | Changes the engine startup probe. |
| Serving port | No | Changes the stable Service only; engine and shim ports are controller-owned constants. |
| Replicas and autoscaling | No | Capacity changes must not create workload revisions. |
| Rollout, analysis and routing policy | No | These describe how to move between revisions. |
| Observability and SLO policy | No | These change monitoring resources, not serving Pods. |

The decoder identifies the original format by the absence of `version` and never
recalculates its identity. During the one-time transition, the compatibility path
requires the canonical stored legacy bytes, the persisted status identity, the
workload fields the legacy schema recorded, and a matching live Deployment. That
Deployment proves the effective engine image, startup-probe budget, shim image and
shim resources the old payload omitted.

An equivalent steady workload keeps its legacy identity and refreshes the
controller-owned telemetry arguments without fabricating a customer release. A
genuine workload change receives a v2 identity and follows the configured rollout.
If an active legacy canary still uses the pre-resource-scope telemetry shape, the
controller leaves both Deployments and all rung evidence untouched, reports
`MetricScopeReady=False` with reason `MetricsScopePending`, and waits without a
retry timer for `llmcp.io/abort=true`. This is deliberately an abort-only migration:
promoting an unmeasurable candidate would turn compatibility uncertainty into a
production decision. A rejected legacy target remains sticky after its candidate is
removed, so the format change cannot retry a known failure.

If required history or live evidence is absent or contradictory, the controller
fails closed instead of guessing. Once a v2 workload is recorded, later rollbacks
use its frozen effective values.

Unknown payload versions, empty or malformed data, missing referenced history, and
payload/hash mismatches fail closed. They are not substituted with the live
candidate specification.

## Consequences

- A controller or default-image upgrade cannot silently change a v2 rollback.
- Startup-timeout changes create a candidate; Service-port and policy-only changes
  do not.
- Existing history remains addressable by its stored legacy hash.
- A hash-format upgrade alone does not restart an active rung or retry a sticky
  failed target; an active old-telemetry rollout requires an explicit abort.
- Effective values missing from legacy bytes are recoverable only while the matching
  primary Deployment remains available. Missing evidence is an explicit migration
  error rather than a best-effort reconstruction.
- Adding a future pod-template field requires classifying it in this table and, if
  the wire shape changes, introducing a new payload version rather than editing v2.

## Implementation and verification

- Encoding and legacy decoding: [`internal/revision/hash.go`](../../internal/revision/hash.go)
- Immutable history and hash validation: [`internal/revision/history.go`](../../internal/revision/history.go)
- Effective default resolution and workload merge: [`internal/controller/revision_snapshot.go`](../../internal/controller/revision_snapshot.go)
- Reconcile integration: [`internal/controller/modeldeployment_controller.go`](../../internal/controller/modeldeployment_controller.go)
- Migration and identity tests: [`internal/revision/hash_test.go`](../../internal/revision/hash_test.go), [`internal/revision/history_test.go`](../../internal/revision/history_test.go), [`internal/controller/revision_safety_test.go`](../../internal/controller/revision_safety_test.go), and [`internal/controller/revision_migration_test.go`](../../internal/controller/revision_migration_test.go)
