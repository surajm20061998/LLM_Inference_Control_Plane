# Improvement implementation status

This is the execution record for [the improvement plan](../improvement_plan.md).
The original audit baseline is `69c8e35`. Work resumed on 2026-09-06 after usage interruptions.

| Package | Current state | Remaining acceptance work |
|---|---|---|
| PR-01 ServiceMonitor errors | Implemented; full unit/envtest suite passed | Live apply-failure fixture remains part of the isolated cluster evidence |
| PR-02 resource metric identity | Producer, every built-in consumer, schema handshake, rules and dashboards implemented; suite 07 authored and linted | Live two-resource Prometheus isolation run and capture |
| PR-03 replica rung readiness | Proportional routing, whole-generation readiness, fresh rung windows and insufficient-capacity hold implemented; suite 08 authored and linted | Live 25/50/75 ladder run and capture |
| PR-04 revision/configuration safety | Versioned workload payload, legacy decoder/adoption, active-rollout hold, effective defaults and reserved settings implemented | Migration passes envtest; a packaged old-controller-to-new-controller Kind run remains |
| PR-05 stream telemetry | Usage-aware tokens, chunk/inter-chunk telemetry and bounded SSE parsing implemented | Live dashboard capture |
| PR-06 monitoring resync | Periodic discovery and owned-resource repair implemented; guarded suite 09 and its dedicated CI job are authored and linted | First live late-install/drift CI run |
| PR-07 exposure and customer evidence | Display-only observed request share, a genuine local request capture, grounded README diagrams and Mermaid CI gate implemented | Prometheus dashboard captures and full live R1 Chainsaw evidence |
| PR-08–16 | Not started | The later release gates in the plan still apply |

Implementation is not a release claim. A fakeengine test does not establish real-engine,
GPU, Gateway, or shared-storage support. Exact verification commands and outcomes will
be recorded here as they finish. Installer-owned dashboard ConfigMaps remain managed by
Kustomize; per-ModelDeployment reconciliation repairs its owned ServiceMonitor and
PrometheusRule only.

## Current verification record

On 2026-09-10 the regenerated manifests and API reference were deterministic and the
following checks passed:

```text
GOCACHE=/tmp/llmcp-final-gocache make test               # every package; 89 controller specs
make lint                                                # 0 issues
GOCACHE=/tmp/llmcp-final-race-gocache go test -race ./cmd/shim ./internal/analysis -count=1
KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.36.2-darwin-arm64" \
  go test ./internal/controller -run TestController -count=1 -ginkgo.focus='Legacy revision migration'
make docs-mermaid-check                                  # 8 README diagrams render
make chainsaw-lint                                       # suites 01-09 valid
git diff --check
```

The full unit/envtest run passed every package, including the controller suite against
Kubernetes 1.36.2 envtest binaries. The generated CRD, RBAC, deepcopy output and API
reference were regenerated twice and retained identical SHA-256 values. A live isolated
Kind run and the remaining dashboard screenshots were attempted but not claimed:
Docker Desktop's client was available while its server returned `EOF`, including with
host access. Those cluster-only evidence items remain open rather than being replaced
by mock dashboard values. The nightly workflow now contains the live lanes, but it is
not evidence until GitHub Actions has executed it successfully.

## 2026-10-08 nightly lane: root causes and live Kind evidence

`e2e-monitoring.yml` had failed every scheduled run since it was added. Each
suite hid the next, because the canary step runs under `set -e`; running them
one by one on Kind found these faults:

| Where | Fault | Fix |
|---|---|---|
| 03 | Never changed the spec, so no canary could start (a first revision is not canaried) | Ship a healthy new revision before asserting `Canarying` |
| 09 | Guarded on `kubectl config current-context`, which is always `chainsaw` inside Chainsaw | Context check in the Makefile; node `providerID` check in the suite |
| 08 | `length(null)` on cleared checks is a JMESPath type error, which Chainsaw does not retry | Empty-list fallback inside `length()`; guarded by `TestChainsawExpressionsAreNullSafe` |
| 04, 07 | `failedRevision != ''` is true when status.canary is absent, so it could pass vacuously | Also exclude `null` |
| controller | A canary rollback was reported as `RolloutStalled`; `CanaryRolledBack` never fired | `StallReverted` keys the stall event |
| controller | After a rollback the phase stayed `Progressing` forever | Converged rollback reports `Progressing=False/RevisionRejected`, phase `Available` |
| controller | The pass after a rollback planned from a stale cache read, recreated the canary, and its conflict retry overwrote the rollback | Uncached `APIReader` read; `updateStatus` refuses a stale plan |
| analysis | `error-rate` matched no series for a variant with zero 5xx, so a healthy canary was Inconclusive forever | Traffic-tied zero: `(5xx or 0 * total) / total` |

Every new unit/envtest guard was checked by reintroducing the bug it catches.
Local Kind (Docker 8 GiB / 6 CPU), each job's steps run verbatim from the workflow
(the canary suites in one `set -e` pass, final code):

```text
09-observability-resync   PASS (612s)
03-canary-promote         PASS (181s)
04-canary-rollback        PASS (89s)
07-metric-isolation       PASS (138s)
08-canary-ladder          PASS (151s)
make verify-ttft          PASS (ttft_p95/duration_p95 = 0.200 < 0.50)
make verify               PASS (0 lint issues, every package)
```

Not yet evidence: a green GitHub Actions run of the workflow, which needs these
changes pushed.

## PR-04 revision/configuration safety

The hardening now:

- fails closed when the recorded stable revision is missing instead of rendering the
  live candidate under the stable revision label;
- pins `status.stableRevision`, `status.lastGoodRevision`, and the active canary's
  target, stable, and failed revisions during history pruning;
- rejects llama.cpp arguments and environment variables that could override
  controller-owned model, endpoint, concurrency, metrics, cache, or credential
  settings;
- uses the API-key environment name expected by the pinned llama.cpp image;
- writes a `v2` revision envelope and keeps an explicit decoder for the original
  unversioned bytes;
- adopts an equivalent steady legacy workload without inventing a rollout, preserves
  sticky legacy failures, and holds an active old-telemetry rung for explicit abort
  without changing its Deployments, counters, readiness target, or timestamps;
- removes Service-only port from new workload identity and includes startup timeout;
- freezes effective engine image, shim image, defaults and derived thread count in
  the rollback payload.

Verification on 2026-09-10:

```text
GOCACHE=/tmp/llmcp-revision-gocache go test ./internal/revision ./internal/engine -count=1
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine

KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.36.2-darwin-arm64" \
  go test ./internal/controller -run TestController -count=1 \
  -ginkgo.focus='Legacy revision migration'
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/controller

GOCACHE=/tmp/llmcp-final-gocache make test
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/cmd/shim
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/canary
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/controller
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability
ok  github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision
```

The wire contract and field classification are recorded in
[ADR 0008](adr/0008-versioned-workload-revisions.md). Legacy hashes are never
recalculated. Adoption requires canonical legacy bytes, matching persisted identity,
unchanged legacy-recorded workload fields, and compatible live Deployment evidence.
If that evidence is absent or contradictory, migration fails closed instead of
guessing. The envtest fixture exercises the API server, Deployments and
ControllerRevision objects in process. The remaining PR-04 evidence is a packaged
old-controller-to-new-controller upgrade on a disposable Kind cluster.
