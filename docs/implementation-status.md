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
