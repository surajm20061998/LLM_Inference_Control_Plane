# LLMCP Improvement Plan

- Status: Draft for review
- Last researched: 2026-09-04
- Scope: README presentation, correctness fixes, and a staged product roadmap. This document proposes work; it does not claim that the proposed capabilities are implemented.

## 1. Product statement to preserve

LLMCP is a Kubernetes-native release-safety and elasticity controller for self-hosted, OpenAI-compatible LLM inference.

Its strongest implemented use case is not merely deploying a model. It is detecting and stopping a candidate revision that remains Kubernetes-healthy and returns successful responses but makes the user experience worse, particularly through regressed time to first token (TTFT).

The README should consistently describe the project as providing **operational rollout safety**:

- serving availability and readiness;
- TTFT and request-duration regression detection;
- HTTP 5xx gating;
- queue-depth/concurrency autoscaling;
- durable promotion, abort, and rollback state;
- Prometheus/Grafana observability.

It must not imply that the controller currently evaluates factuality, hallucination, toxicity, policy compliance, or general response quality.

## 2. Current implementation baseline

The following facts are the source of truth for the README and the proposed changes.

| Concern | Implemented behavior | Source |
|---|---|---|
| Public API | One namespaced `ModelDeployment` CRD owns model, engine, serving, rollout, observability, and autoscaling policy | [`api/v1alpha1/modeldeployment_types.go`](api/v1alpha1/modeldeployment_types.go#L37-L88) |
| Child resources | The controller renders primary/canary Deployments, a shared serving Service, and a headless metrics Service | [`internal/controller/children.go`](internal/controller/children.go#L139-L184) |
| Reconciliation | Validation, revision recording, autoscaling, rollout planning, child apply, observability, and status happen in that order | [`internal/controller/modeldeployment_controller.go`](internal/controller/modeldeployment_controller.go#L135-L279) |
| Model delivery | OCI image weights are copied through an init container; Hugging Face weights are downloaded by llama.cpp into `emptyDir` | [`internal/controller/children.go`](internal/controller/children.go#L297-L417) |
| PVC source | The API models a PVC source, but llama.cpp validation rejects it as not implemented | [`internal/engine/llamacpp.go`](internal/engine/llamacpp.go#L94-L136) |
| Engine support | Only `llamacpp` is accepted by the API | [`api/v1alpha1/engine_types.go`](api/v1alpha1/engine_types.go#L23-L35) |
| Request path | The serving Service sends traffic through a native-sidecar reverse proxy and then to the engine over loopback | [`internal/controller/shim.go`](internal/controller/shim.go#L77-L151), [`cmd/shim/proxy.go`](cmd/shim/proxy.go#L160-L214) |
| Decision metrics | The shim emits canonical `llmcp_*` TTFT, duration, request, queue, concurrency, and upstream-error metrics | [`cmd/shim/metrics.go`](cmd/shim/metrics.go#L95-L179) |
| Metric identity | Workload metrics currently carry only `model` and `variant` as constant identity labels | [`cmd/shim/metrics.go`](cmd/shim/metrics.go#L95-L98) |
| Canary analysis | Low traffic short-circuits to `Inconclusive`; results are classified as Pass, Fail, Inconclusive, or Error | [`internal/analysis/analyzer.go`](internal/analysis/analyzer.go#L53-L127), [`internal/analysis/evaluate.go`](internal/analysis/evaluate.go#L38-L135) |
| Traffic routing | The only implemented routing mode approximates traffic using primary/canary replica counts behind one Service | [`api/v1alpha1/rollout_types.go`](api/v1alpha1/rollout_types.go#L184-L215) |
| Autoscaling | The built-in controller chooses a total replica count from queue depth or concurrency and the canary code divides that total | [`internal/autoscale/recommend.go`](internal/autoscale/recommend.go#L177-L299), [`internal/canary/weights.go`](internal/canary/weights.go#L19-L85) |
| SLOs | The operator generates TTFT and availability recording rules, multi-window burn alerts, and a no-traffic alert | [`internal/observability/prometheusrule.go`](internal/observability/prometheusrule.go#L121-L155) |
| Dashboards | Three Grafana dashboards are embedded in the manager image and installed as dashboard ConfigMaps | [`internal/observability/dashboards.go`](internal/observability/dashboards.go#L36-L49) |

## 3. Guiding principles

1. Every headline README claim must link to an implementation file, an ADR, a reproducible command, or a captured result.
2. Present behavior and roadmap behavior must be visually separated.
3. Fix incorrect behavior before making diagrams or screenshots that depend on it.
4. Preserve the controller's ownership contract: autoscaling chooses total capacity; rollout routing decides exposure.
5. Missing measurements must never be silently converted into zero or success.
6. Optional integrations must fail visibly without preventing an otherwise healthy model from serving.
7. New metric labels must be bounded by Kubernetes resources, not requests, users, prompts, or other unbounded data.
8. Prefer staged improvements that leave the current zero-dependency replica-routing mode useful.

## 4. Workstream A: README and GitHub presentation

### Goal

Replace the current README with a product-first, implementation-grounded explanation that a platform engineer can understand before reading the CRD or source.

GitHub renders Mermaid diagrams directly in Markdown, so the diagrams should remain text-reviewable and change with the code instead of becoming stale binary artwork. GitHub documents Mermaid support for repository Markdown files in [Creating diagrams](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/creating-diagrams).

### Proposed README structure

1. **Hero section**
   - Name and one-sentence value proposition.
   - A short statement of the failure it catches: “healthy but slower.”
   - Badges only for workflows and artifacts that actually exist.
   - Links to Quick start, Demo, Architecture, API, and Limitations.

2. **The problem**
   - Explain why readiness and rolling updates cannot detect TTFT regressions.
   - Explain why queue depth is a more direct serving-pressure signal than CPU for this implementation.
   - State that safety means operational safety, not semantic answer quality.

3. **End-to-end architecture**
   - Mermaid flowchart showing `ModelDeployment`, controller, revisions, primary/canary Deployments, serving and metrics Services, native sidecar, llama.cpp, Prometheus, rollout analysis, autoscaling, rules, and dashboards.
   - Distinguish the request path from the control/measurement path by edge style or color.

4. **What happens during a rollout**
   - Mermaid sequence diagram: spec change → revision record → canary creation → readiness/warm-up → Prometheus query → verdict → advance/promote/rollback → status/Event.
   - Include a note that the first deployment cannot be canaried because no last-good revision exists.

5. **Decision semantics**
   - A compact diagram or table for Pass, Fail, Inconclusive, and Error.
   - Document what each counter does and what happens when its limit is reached.
   - Avoid the absolute claim that a monitoring outage “can never cause rollback.” The accurate claim is that provider errors do not spend the failed-check budget; the controller holds initially and aborts the rollout after the configured consecutive-error limit.

6. **Autoscaling ownership**
   - Diagram: Prometheus fleet metric → desired total → primary/canary split.
   - Explain that `/scale`, `kubectl scale`, external HPA, and the built-in autoscaler all operate on the same total-replica field.

7. **Model delivery and engine runtime**
   - OCI image and Hugging Face flows.
   - State clearly that OCI is recommended, Hugging Face re-downloads into ephemeral storage, PVC is not implemented, and only llama.cpp is supported.

8. **Customer scenarios**
   - Healthy-but-slow upgrade.
   - Queue-driven burst scaling.
   - Private/on-prem OCI model delivery with manual approval.

9. **Quick starts**
   - A fast deterministic path using `fakeengine`.
   - A separate real-model path using Qwen3/llama.cpp.
   - Do not mix fake-engine environment variables into commands described as a real llama.cpp demo.

10. **Current scope and limitations**
    - Replica-based traffic distribution is approximate.
    - Operational metrics only.
    - llama.cpp only.
    - No scale-to-zero, public gateway, persistent Hub cache, or authenticated Prometheus provider yet.

11. **Documentation map and roadmap**
    - Link ADRs, API reference, SLO runbook, demo, and this plan.
    - Use a small “Implemented / Planned” table instead of unqualified future-facing prose.

### Required diagrams

#### Diagram 1: system architecture

Ground it in `buildChildren`, `buildShimContainer`, the reconciliation order, and the observability builders. It should show only resources that the controller actually creates.

#### Diagram 2: request and metric flow

Show:

```text
client → ClusterIP Service → shim:8080 → llama.cpp:8000
                               └──────→ metrics:9090 → headless Service → Prometheus
```

The engine port must not be shown as exposed through the serving Service because the implementation intentionally prevents that bypass.

#### Diagram 3: canary lifecycle

Show first deployment, canary start, readiness/warm-up, analysis, pause, promotion, abort, rollback, and the sticky failed revision.

#### Diagram 4: verdict behavior

Show the separate paths and counters for measurement failure, insufficient evidence, and measured regression.

#### Diagram 5: capacity versus exposure

Show that the autoscaler owns the total and the routing implementation divides it. For replica routing, explicitly show integer quantization.

### Figures and screenshots

After the correctness work below:

- add `docs/assets/canary-rollback.png` showing primary/canary TTFT and the rollback annotation;
- add `docs/assets/queue-autoscaling.png` showing queue depth and replica response;
- add `docs/assets/modeldeployment-status.png` showing useful status fields and conditions;
- capture figures from a committed sample configuration and record that sample in each caption;
- provide descriptive alt text;
- do not use mock dashboard values without marking them as illustrative.

### README acceptance criteria

- Mermaid blocks render on GitHub without third-party extensions.
- Every architecture node maps to a real resource or process.
- Every “automatic” behavior links to its controller/state-machine implementation.
- All example outputs are captured from, or mechanically derived from, a committed sample.
- Implemented and planned capabilities are visibly distinct.
- The limitations section is reachable from the top of the README.
- The 10-minute demo and README do not contradict one another.

## 5. Workstream B: make replica-based canary ladders truthful

### Problem

The flagship sample requests `[25, 50, 75]` but fixes the canary at one replica. `CanaryReplicas` ignores the requested weight when a replica override is present. With four total replicas, the actual configured split therefore remains 3 primary / 1 canary, or 25%, at every rung.

The API description of fixed canary scale assumes traffic can be controlled independently, but the only current traffic mechanism is replica count.

### Recommended immediate design

For `trafficRouting.mode: Replica`:

1. Always calculate replicas with `Split(total, desiredWeight)`.
2. Reject or ignore-with-an-explicit-condition `canary.scale.replicas`; rejection is preferred because silently accepting it preserves the current contradiction.
3. Treat `matchTrafficWeight: true` as the only meaningful replica-mode behavior.
4. Remove `scale.replicas: 1` from `config/samples/canary_qwen3.yaml`.
5. Update the expected ladder to the actual four-replica splits:
   - 25% → primary 3, canary 1;
   - 50% → primary 2, canary 2;
   - 75% → primary 1, canary 3.
6. Keep reporting desired and mathematically configured weight separately.

Because the canary Deployment will grow between steps, the availability contract also needs tightening:

- pass the canary's ready-replica count into the state machine rather than a Boolean “at least one is ready”;
- require the ready count for the current rung before analysis;
- reset the rung's warm-up timestamp when its desired canary replica count changes;
- do not present desired replica weight as active exposure while new endpoints are still loading.

### API compatibility choice

`CanaryScaleSpec` can remain in `v1alpha1` for the future Gateway mode, but validation should state that fixed replicas require a routing mode capable of independent weighting. Since `Replica` is currently the only enum value, the immediate behavior is effectively “field reserved but unsupported.”

### Verification

- State-machine cases for all ladder steps and ready-count transitions.
- Property check that primary + canary always equals total.
- Controller check that analysis does not run until all canary replicas for the rung are ready.
- Sample validation proving no fixed override is combined with replica routing.
- Demo output generated from the real status fields rather than hand-written expectations.

### Acceptance criteria

- `desiredWeight=25/50/75` produces 3/1, 2/2, and 1/3 with four replicas.
- No supported configuration claims to decouple canary pods from traffic while replica count is the router.
- Analysis never runs against a partially ready new rung.
- README and `docs/demo.md` report the same values produced by status.

## 6. Workstream C: scope decision metrics to a ModelDeployment

### Problem

Canary and autoscaling PromQL currently select only `model` and `variant`, while SLO recording rules aggregate only by `model`. Two resources serving the same model name can influence each other's rollout, scaling, alerts, and dashboards.

Prometheus recommends labels when a dimension is required to identify or aggregate a metric, while warning against unbounded cardinality. Namespace and ModelDeployment name are bounded by the number of deployed resources and are necessary for correctness; request IDs, user IDs, prompt text, and pod UIDs are not appropriate identity labels. See Prometheus's [instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/) and [metric and label naming guidance](https://prometheus.io/docs/practices/naming/).

### Recommended design

Add two canonical labels to every decision metric:

```text
namespace
model_deployment
```

Keep `model` for human aggregation and `variant` for primary/canary comparison.

Implementation steps:

1. Add label-name constants in `internal/metrics/names.go`.
2. Add required `--namespace` and `--model-deployment` shim configuration.
3. Pass `md.Namespace` and `md.Name` from `shimArgs`.
4. Add the labels to all shim collectors, including `llmcp_shim_info`.
5. Extend `analysis.QueryContext` with namespace and resource name.
6. Update built-in canary, request-rate, ratio, and fleet autoscaling selectors.
7. Add `{{.Namespace}}` and `{{.ModelDeployment}}` substitutions for custom PromQL while retaining existing variables.
8. Scope Prometheus recording rules and alert labels by namespace and resource name.
9. Update Grafana variables and panel queries to select a concrete ModelDeployment.
10. Update metric documentation and dashboard legends.

Use labels emitted directly by the shim rather than relying only on ServiceMonitor target relabeling. This keeps the identity correct if a user scrapes the endpoint without the generated ServiceMonitor.

### Migration notes

- Old pods will continue emitting the old label set until rolled.
- The controller change modifies shim arguments in the pod template, causing serving pods to restart on operator upgrade even if the user specification is unchanged. This behavior and its rollout impact must be documented.
- Queries should not temporarily fall back to unscoped series because that would reintroduce cross-resource contamination.
- Dashboard variables should hide or clearly separate legacy unscoped series during the transition.

### Verification

- Two resources with the same `spec.model.name` in different namespaces produce disjoint PromQL results.
- Two resources with the same model in one namespace remain disjoint by resource name.
- Autoscaling one resource cannot read the other's queue.
- SLO recording-rule names may be shared, but their output label sets must remain unique.
- Cardinality tests ensure no request-derived values become labels.

### Acceptance criteria

- Every automated query includes namespace and ModelDeployment selectors.
- Same-model deployments cannot affect one another's decisions.
- Dashboard selection uniquely identifies a resource.
- Raw-query users have documented identity template variables.

## 7. Workstream D: fix monitoring reconciliation truthfulness

### Immediate bug

Change the ServiceMonitor apply branch from testing `err` to testing `applyErr` in [`internal/controller/observability.go`](internal/controller/observability.go#L162-L170).

On failure:

- return the wrapped apply error so reconciliation retries;
- report `MetricsRegistered=False` with the apply error;
- do not report metrics as registered;
- do not prevent the existing serving workload from continuing.

### Adjacent reconciliation gap

Optional monitoring CRDs and their children are not continuously watched. A steady resource with no canary or autoscaler timer may not notice that prometheus-operator was installed later or that a generated monitoring child was deleted.

Recommended staged approach:

1. Add `RequeueAfter` to the observability verdict.
2. Retry discovery quickly, for example after the existing negative-discovery TTL, while an enabled CRD is absent.
3. Reconcile enabled observability resources on a slower periodic resync even when healthy.
4. Include observability in the controller's `soonest(...)` scheduling calculation.
5. Later evaluate a dynamic watch for optional unstructured resources; do not make manager startup depend on an absent CRD.

### Verification

- Inject a ServiceMonitor apply failure and assert `MetricsRegistered=False`, an error return, and no false success.
- Install the optional CRD after a ModelDeployment reaches steady state and verify the resource appears without touching the ModelDeployment.
- Delete or mutate a generated ServiceMonitor/PrometheusRule and verify it is restored.
- Verify clusters without prometheus-operator continue serving and do not enter a hot reconciliation loop.

### Acceptance criteria

- Apply failures are observable in status and logs.
- Optional-CRD absence is reported factually.
- Later installation and child drift converge without an unrelated workload event.

## 8. Workstream E: separate requested, configured, and observed traffic exposure

### Immediate clarification

The current `status.canary.currentWeight` is derived from desired replica counts. It is not a measurement of the fraction of requests that reached the candidate. Kubernetes Service routing and client connection reuse can cause the request distribution to differ from the pod ratio.

Until a new API is introduced:

- document `currentWeight` as **configured replica share**;
- never call it “measured traffic” or “actual request percentage”;
- show the connection-level limitation next to replica-routing examples;
- use a single 25% rung for the first polished rollback demo if repeatable multi-rung readiness cannot yet be guaranteed.

### Add observed exposure

After metric scoping is fixed, add a query over request rates for both variants:

```text
observedWeight = canaryRequestRate / (primaryRequestRate + canaryRequestRate)
```

Expose it separately, for example as `status.canary.observedWeight`, together with the window and timestamp. Missing or insufficient traffic must produce an unknown/empty observation, not zero.

Keep these concepts distinct:

- `desiredWeight`: the state-machine target;
- `currentWeight`: the routing configuration or replica-derived share;
- `observedWeight`: sampled request distribution from Prometheus.

### Exact routing with Gateway API

Gateway API `HTTPRoute` supports multiple backend references with weights; the effective share is each weight divided by the sum of weights. The official reference includes a 90/10 example: [Gateway API HTTPRoute](https://gateway-api.sigs.k8s.io/reference/api-types/httproute/).

Recommended future mode:

```yaml
trafficRouting:
  mode: GatewayAPI
  gatewayRef:
    name: inference-gateway
    namespace: gateway-system
  hostnames:
    - chat.example.com
```

Implementation outline:

1. Add separate primary and canary Services whose selectors include `variant`.
2. Create or manage an `HTTPRoute` with both Services as `backendRefs`.
3. Set backend weights from the canary state machine rather than changing pod counts.
4. Read route `Accepted`, `ResolvedRefs`, and programmed/readiness conditions before analysis.
5. Allow fixed canary replicas only for this independent-routing mode.
6. Set canary weight to zero on rollback and primary weight to 100 on promotion before removing obsolete resources.
7. Discover Gateway API as an optional dependency and report `TrafficRoutingReady` accurately when it is absent.
8. Decide whether the operator owns the `HTTPRoute` or patches a user-owned route. Operator ownership is recommended initially because it gives one clear reconciliation contract; the Gateway itself should remain user-supplied infrastructure.

Gateway weights configure request forwarding more accurately than pod ratios, although implementations may still have a small precision error, as the [Gateway API specification](https://gateway-api.sigs.k8s.io/reference/api-spec/main/spec/) notes.

### Acceptance criteria

- Status never presents a mathematical pod ratio as a measured request ratio.
- Replica mode remains dependency-free and explicitly approximate.
- Gateway mode can change 25→50→75 traffic without resizing a fixed one-pod canary.
- Routing readiness gates analysis.
- Rollback restores stable routing before deleting the candidate.

## 9. Workstream F: define operational safety and add a path to semantic evaluation

### Phase 1: documentation now

Use “operational rollout safety” throughout the README. List the exact built-in signals from [`analysis_types.go`](api/v1alpha1/analysis_types.go#L188-L225).

Add a prominent statement:

> LLMCP determines whether a candidate is operationally worse according to configured metrics. It does not currently determine whether an answer is factually correct, safe, unbiased, or useful.

Document custom PromQL as an integration point, not as a built-in semantic evaluator.

### Phase 2: external semantic signals

Define and document a contract for an external evaluator to publish bounded, resource-scoped metrics such as:

- answer-quality pass ratio;
- safety-policy violation ratio;
- retrieval-groundedness score;
- evaluation sample count.

The canary can consume these through existing custom PromQL after metric identity is fixed. Require a minimum evaluation sample count so a candidate cannot pass on one or zero scored responses.

### Phase 3: first-class evaluation workflow

Two complementary designs should be prototyped before changing the API:

1. **Pre-promotion evaluation Job**
   - The controller creates an owned Kubernetes Job for a named dataset/evaluator.
   - The Job calls the candidate endpoint and reports a structured result.
   - Promotion waits for Job completion and threshold evaluation.
   - Kubernetes Jobs are the native primitive for one-off work that runs to completion; see the [Kubernetes Job documentation](https://kubernetes.io/docs/concepts/workloads/controllers/job/).

2. **Opt-in shadow evaluation**
   - Duplicate a controlled portion of traffic to a candidate/evaluator without using its response for the client.
   - Gateway API provides a `RequestMirror` filter and specifies that the mirrored response is ignored; see [HTTP request mirroring](https://gateway-api.sigs.k8s.io/guides/user-guides/http-request-mirroring/).
   - Request mirroring alone cannot score the ignored candidate response. A dedicated shadow/evaluation component must capture candidate output, apply privacy controls, and publish aggregate results.

Risks that must be designed explicitly:

- doubled inference cost and load;
- prompts containing sensitive information;
- non-idempotent tool calls or side effects;
- judge-model instability and versioning;
- delayed semantic results versus rollout deadlines;
- storage and retention of prompts/responses.

### Acceptance criteria

- Current documentation makes no semantic-safety claim.
- Custom quality gates require sample-count evidence.
- Any future first-class evaluator versions its dataset, evaluator, and threshold in status/history.
- Shadowing is off by default and has explicit privacy and side-effect warnings.

## 10. Workstream G: grow deliberately beyond a single-engine alpha

This is a roadmap, not a README claim.

### G1. vLLM engine profile

Implement a second `engine.Profile` that supplies:

- a pinned image or digest policy;
- OpenAI-compatible server arguments;
- health and metrics endpoints;
- supported model-source validation;
- API-key handling through Secrets;
- CPU/GPU-specific configuration;
- canonical shim behavior unchanged from llama.cpp.

vLLM documents an OpenAI-compatible HTTP server and metrics APIs in its official [online serving documentation](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html). The point of this work is to prove that the engine abstraction and canonical shim metrics survive a real engine swap.

Do not gate rollout decisions directly on engine-native metric names. Continue using the shim contract for cross-engine metrics, with native vLLM metrics available for diagnostics.

### G2. PVC model delivery

Implement the existing `persistentVolumeClaim` union member by:

1. adding a `PersistentVolumeClaimVolumeSource` to the pod;
2. mounting it read-only at a deterministic runtime directory;
3. resolving and validating the configured file path without an init copy;
4. including relevant mount settings in revision identity;
5. documenting required access modes for multiple replicas and multi-node clusters.

Kubernetes defines PVCs as durable storage requests with a lifecycle independent of an individual pod; see [Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/). A shared multi-replica deployment must not assume that every storage class supports ReadOnlyMany or ReadWriteMany.

### G3. Persistent Hugging Face cache

Add an optional cache volume configuration rather than silently replacing the current `emptyDir` behavior. Possible modes:

- ephemeral `emptyDir` as today;
- user-supplied PVC;
- later, an operator-created claim template.

Document concurrent download and access-mode behavior. OCI model images should remain the recommended immutable/offline production path.

### G4. Workload placement and accelerators

Add a controlled pod-template subset:

- `nodeSelector`;
- tolerations;
- affinity;
- topology spread constraints;
- priority class;
- runtime class if required by accelerator stacks.

The existing resource map can already represent extended resource names, but the project needs an engine/image that can use them and placement fields to target suitable nodes. Kubernetes exposes GPUs through device-plugin extended resources such as `nvidia.com/gpu`; see [Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/). Node affinity and topology spread are the standard placement mechanisms; see [Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/) and [Pod Topology Spread Constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).

All pod-template-affecting fields must be added deliberately to revision identity.

### G5. Public ingress and authenticated Prometheus

Public exposure should build on Gateway API rather than adding a separate, unrelated Ingress abstraction after Gateway-based traffic routing exists.

Extend `AnalysisProviderSpec` with Secret-backed authentication and TLS configuration shared by canary and autoscaling providers:

- bearer token or authorization credentials;
- optional basic authentication;
- CA bundle and server-name configuration;
- optional client certificate/key;
- explicitly allowed static tenancy headers.

Never place credentials in arguments, Events, status, or logs. The Prometheus Operator API demonstrates Secret-based authorization and TLS configuration patterns in its [API reference](https://prometheus-operator.dev/docs/api-reference/api/), although LLMCP's query client will need its own transport implementation.

### G6. Scale to zero

Do not implement scale-to-zero by merely changing `minReplicas` to zero. When no shim pod exists, its queue/concurrency metric also disappears, so there is no signal that can reactivate the deployment.

Scale-to-zero requires an always-available request/activation layer or an external event source that can observe demand while the model is absent. KEDA similarly notes that CPU or memory triggers cannot activate from zero when there are no running pods producing those metrics; see [KEDA concepts](https://keda.sh/docs/2.20/concepts/).

Recommended sequence:

1. implement a Gateway/router path;
2. define request queueing and maximum activation wait;
3. expose an activation signal independent of serving pods;
4. model cold-start and failure behavior;
5. only then permit `minReplicas: 0`.

### Acceptance criteria for broader product maturity

- A second real engine completes the same operational canary scenario.
- PVC-backed deployments work across documented access modes.
- accelerator placement is represented without exposing an unrestricted raw PodSpec.
- provider credentials remain Secret-backed and redacted.
- scale-to-zero has a functioning activation path and bounded request wait.

## 11. Additional correctness items discovered during review

These are not part of the original six points, but should be considered before calling the API production-ready.

### Revision identity

- `serving.startupTimeout` changes the rendered pod probes but is excluded from the revision hash.
- `serving.port` is included in the revision hash even though it changes the Service rather than the pod workload.

Audit every field using one rule: if it changes the candidate pod template or runtime behavior being compared, it belongs in revision identity; if it only changes rollout policy or a stable external Service, it does not.

### Engine concurrency contract

`engine.extraArgs` can override llama.cpp `--parallel`, while the shim still derives queue depth from `spec.engine.maxConcurrency`. Either prevent overrides of controller-owned flags or parse/resolve one authoritative effective value and pass it to both engine and shim.

### Output-token metrics

The shim currently counts content-bearing SSE frames, which are not guaranteed to equal tokenizer tokens. Rename the metric to reflect chunks, or implement protocol/engine-supported token accounting before using it for billing or precise throughput claims.

### Monitoring and dashboard drift

- Generated monitoring children need continuous reconciliation as described above.
- Dashboard burn-rate calculations currently assume default objectives in places; parameterized objectives require generated dashboard values or variables.
- API comments that still say Hugging Face is unimplemented must be corrected.

## 12. Proposed delivery order

| Order | Work package | Priority | Relative size | Why now |
|---|---|---:|---:|---|
| 1 | ServiceMonitor apply-error fix | P0 | Small | Removes a direct false-success condition |
| 2 | Resource-scoped metric identity and queries | P0 | Medium | Required for correctness whenever model names repeat |
| 3 | Replica-mode ladder semantics and readiness | P0 | Medium | Makes the flagship behavior and sample truthful |
| 4 | Immediate traffic/safety documentation corrections | P0 | Small | Prevents misleading external claims |
| 5 | README rewrite with Mermaid diagrams | P1 | Medium | Presentation should be built on corrected behavior |
| 6 | Capture real dashboard/status figures | P1 | Small/Medium | Adds credible visual proof after behavior is stable |
| 7 | Observed traffic-share status | P1 | Medium | Makes routing behavior empirically visible |
| 8 | Optional monitoring resync/drift repair | P1 | Medium | Improves long-running operator behavior |
| 9 | Gateway API weighted routing | P2 | Large | Enables true traffic/replica decoupling and public routing |
| 10 | Prometheus authentication and TLS | P2 | Medium | Needed in secured and managed environments |
| 11 | PVC and persistent Hub cache | P2 | Medium | Removes repeated model downloads and supports existing storage |
| 12 | vLLM plus placement/GPU controls | P2 | Large | Expands the product beyond its current llama.cpp scope |
| 13 | Semantic evaluation integration | P3 | Large | Expands “operational safety” into model-quality evidence |
| 14 | Scale-to-zero activation architecture | P3 | Large | Requires a router/activator, not only an autoscaler change |

## 13. Review decisions requested

Before implementation, confirm these design choices:

- [ ] In replica-routing mode, reject fixed `canary.scale.replicas` and always match replica share to desired weight.
- [ ] Add `namespace` and `model_deployment` directly to shim metrics rather than depending on ServiceMonitor-added target labels.
- [ ] Keep `currentWeight` for configured share and add a separate `observedWeight`, avoiding a breaking rename during `v1alpha1`.
- [ ] Make a future Gateway mode own the `HTTPRoute` while referencing a user-managed `Gateway`.
- [ ] Describe the current guarantee as operational rollout safety and defer first-class semantic evaluation until after core routing/identity work.
- [ ] Use vLLM as the second engine profile.
- [ ] Keep OCI model images as the recommended production source after PVC/cache support is added.

## 14. Primary research references

- [GitHub: Creating diagrams](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/creating-diagrams)
- [Gateway API: HTTPRoute](https://gateway-api.sigs.k8s.io/reference/api-types/httproute/)
- [Gateway API specification: backend weights](https://gateway-api.sigs.k8s.io/reference/api-spec/main/spec/)
- [Gateway API: HTTP request mirroring](https://gateway-api.sigs.k8s.io/guides/user-guides/http-request-mirroring/)
- [Prometheus: Instrumentation practices](https://prometheus.io/docs/practices/instrumentation/)
- [Prometheus: Metric and label naming](https://prometheus.io/docs/practices/naming/)
- [Prometheus Operator API reference](https://prometheus-operator.dev/docs/api-reference/api/)
- [Kubernetes: Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
- [Kubernetes: Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Kubernetes: Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
- [Kubernetes: Pod Topology Spread Constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/)
- [Kubernetes: Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/)
- [vLLM: OpenAI-compatible online serving](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html)
- [KEDA: Scaling concepts](https://keda.sh/docs/2.20/concepts/)
