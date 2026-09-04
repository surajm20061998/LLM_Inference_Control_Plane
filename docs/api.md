# API Reference

## Packages
- [inference.llmcp.io/v1alpha1](#inferencellmcpiov1alpha1)


## inference.llmcp.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the inference v1alpha1 API group.

### Resource Types
- [ModelDeployment](#modeldeployment)



#### AnalysisMetric



AnalysisMetric is one gate a canary must pass.



_Appears in:_
- [AnalysisSpec](#analysisspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name identifies the check in status and events. It is the list map key,<br />so it must be unique within the spec. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `builtin` _[BuiltinMetric](#builtinmetric)_ | Builtin selects a query this operator writes. |  | Enum: [ttft-p95 ttft-p99 request-duration-p95 error-rate success-rate queue-depth output-token-rate] <br />Optional: \{\} <br /> |
| `query` _string_ | Query is raw PromQL, for checks the built-ins do not cover.<br />Two template variables are substituted before execution: \{\{.Variant\}\} and<br />\{\{.Model\}\} for the label values, and \{\{.Window\}\} for the analysis window.<br />A query returning more than one series is an Error, not a silent<br />first-match — picking arbitrarily from an ambiguous result is how a gate<br />ends up measuring the wrong pod. |  | Optional: \{\} <br /> |
| `thresholdRange` _[ThresholdRange](#thresholdrange)_ | ThresholdRange is the band the value must fall inside to Pass. |  | Required: \{\} <br /> |
| `compareToPrimary` _boolean_ | CompareToPrimary evaluates the RATIO of the canary's value to the<br />primary's rather than the canary's absolute value.<br />Usually the better gate for latency. An absolute TTFT threshold has to be<br />re-tuned for every model, every node type and every prompt length, and<br />one that is stale fires on a busy afternoon rather than on a bad release.<br />A ratio asks the only question that matters — is the new version worse<br />than the one it is replacing? — and answers it under whatever conditions<br />happen to be true right now. | false | Optional: \{\} <br /> |


#### AnalysisProviderSpec



AnalysisProviderSpec points at the metric backend.



_Appears in:_
- [AnalysisSpec](#analysisspec)
- [AutoscalingSpec](#autoscalingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[AnalysisProviderType](#analysisprovidertype)_ | Type selects the backend implementation. | Prometheus | Enum: [Prometheus] <br />Optional: \{\} <br /> |
| `address` _string_ | Address is the base URL of the metric backend, e.g.<br />http://prometheus-operated.monitoring.svc:9090<br />It has no default. A wrong-but-plausible default would make every canary<br />report Error for a reason nobody would think to check, and unlike most<br />misconfigurations this one is silent until a rollout is already in<br />flight. |  | Optional: \{\} <br /> |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Timeout bounds a single query. | 10s | Optional: \{\} <br /> |


#### AnalysisProviderType

_Underlying type:_ _string_

AnalysisProviderType selects the metric backend.

_Validation:_
- Enum: [Prometheus]

_Appears in:_
- [AnalysisProviderSpec](#analysisproviderspec)

| Field | Description |
| --- | --- |
| `Prometheus` |  |


#### AnalysisSpec



AnalysisSpec configures the metric checks that gate a canary.

# The defaults are measured, not guessed

Interval 30s and Window 60s come from this project's own feasibility spike
against a 0.6B model on a laptop CPU. The rule of thumb behind them is that a
percentile needs on the order of 100 requests per variant to be stable; at
the throughput measured there, 60 seconds supplies that and 30 seconds does
not. MinRequestRate 0.5 req/s comes from the same run: below it, the p95 of
the primary swung by more than the regression the analysis is meant to catch.



_Appears in:_
- [CanarySpec](#canaryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Interval is how often an analysis round runs. | 30s | Optional: \{\} <br /> |
| `window` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Window is the PromQL lookback each metric is evaluated over.<br />It must be comfortably larger than the scrape interval, or a rate() over<br />it is computed from two samples — a difference between two points, with<br />no way to tell a trend from a blip. | 60s | Optional: \{\} <br /> |
| `initialDelay` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | InitialDelay is how long to wait after the canary becomes available<br />before the first check.<br />Not politeness — correctness. A freshly started inference pod has a cold<br />KV cache and, on the very first requests, is still faulting the model's<br />pages in. Its first few seconds of TTFT are genuinely terrible and<br />genuinely unrepresentative, and measuring them would fail every canary<br />that was ever going to be fine. | 60s | Optional: \{\} <br /> |
| `failureThreshold` _integer_ | FailureThreshold is how many FAILED checks trigger a rollback.<br />The counter is NOT reset by an intervening pass, matching Flagger. A<br />canary that fails, passes, fails, passes, fails is not healthy — it is<br />intermittently broken, which for a latency SLI is the more common shape<br />of a real regression than a clean step change. | 3 | Minimum: 1 <br />Optional: \{\} <br /> |
| `consecutiveErrorLimit` _integer_ | ConsecutiveErrorLimit is how many consecutive provider ERRORS abort the<br />rollout.<br />Errors are counted separately from failures and are reset by any<br />successful check, because a Prometheus that is unreachable says nothing<br />whatsoever about the canary. Rolling back on it would mean a monitoring<br />outage causes a production rollback — the exact inversion of what the<br />monitoring is for. | 5 | Minimum: 1 <br />Optional: \{\} <br /> |
| `inconclusiveLimit` _integer_ | InconclusiveLimit is how many consecutive INCONCLUSIVE rounds trigger<br />OnInconclusive. | 5 | Minimum: 1 <br />Optional: \{\} <br /> |
| `onInconclusive` _[InconclusiveAction](#inconclusiveaction)_ | OnInconclusive is what to do once InconclusiveLimit is reached. | Wait | Enum: [Wait Rollback Promote] <br />Optional: \{\} <br /> |
| `minRequestRate` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | MinRequestRate is the request rate, per second and per variant, below<br />which a check is Inconclusive rather than Pass.<br />This is a first-class field rather than a PromQL idiom buried in a query<br />template, because without it the entire demo is a lie: with no traffic,<br />error rate is 0 and every latency percentile is absent, so a canary<br />"passes" every check and is promoted having served nothing. Anyone who<br />has run a homegrown canary controller has shipped this bug at least once.<br />Expressed as a Quantity because the Kubernetes API convention forbids<br />floats: write "500m" for half a request per second, or "2" for two. | 500m | Optional: \{\} <br /> |
| `provider` _[AnalysisProviderSpec](#analysisproviderspec)_ | Provider points at the metric backend. |  | Optional: \{\} <br /> |
| `metrics` _[AnalysisMetric](#analysismetric) array_ | Metrics are the checks evaluated each round. When empty, a sensible<br />default set is used — see DefaultMetrics. |  | MaxItems: 10 <br />Optional: \{\} <br /> |


#### AutoscalingMetric

_Underlying type:_ _string_

AutoscalingMetric is the signal the built-in autoscaler tracks.

# Why not CPU

This is the thesis of the whole project, so it is worth stating where the
field is defined rather than only in a README. An inference server saturates
its fixed number of decode slots long before it saturates the CPU: requests
start queueing while utilisation still reads 45%, and by the time CPU is high
enough to trip a utilisation target, tail latency has already been bad for
minutes. Queue depth crosses its threshold at the moment work actually starts
waiting, which is the moment more capacity would have helped.

_Validation:_
- Enum: [queue-depth concurrency]

_Appears in:_
- [AutoscalingSpec](#autoscalingspec)

| Field | Description |
| --- | --- |
| `queue-depth` | AutoscalingQueueDepth targets the mean number of requests waiting for a<br />decode slot, per pod. A target of 0 is not achievable in practice and a<br />target of 1 means "tolerate one request queued per pod".<br /> |
| `concurrency` | AutoscalingConcurrency targets in-flight requests per pod, whether or not<br />they are queued.<br />Useful when the engine's concurrency is high enough that queueing is rare<br />but latency still degrades with load — the difference between "work is<br />waiting" and "work is plentiful". Queue depth is the sharper signal and<br />is the default; this one is smoother and reacts earlier.<br /> |


#### AutoscalingMode

_Underlying type:_ _string_

AutoscalingMode selects who decides how many replicas exist.

There is deliberately no `HPA` value. An external HorizontalPodAutoscaler
writes `.spec.replicas` through the /scale subresource and the controller
reconciles that like any other spec change — so an `HPA` enum value would
change exactly zero lines of behaviour while implying the operator does
something special for it. A Chainsaw test proving a stock HPA drives this CR
is worth more than the enum value, and it is what ships instead.

_Validation:_
- Enum: [Off Builtin]

_Appears in:_
- [AutoscalingSpec](#autoscalingspec)

| Field | Description |
| --- | --- |
| `Off` | AutoscalingOff leaves .spec.replicas alone. Something else owns it:<br />a human with `kubectl scale`, a GitOps commit, or an external HPA.<br /> |
| `Builtin` | AutoscalingBuiltin lets this operator write .spec.replicas from a<br />measured signal.<br /> |


#### AutoscalingSpec



AutoscalingSpec configures the built-in autoscaler.

The whole struct is a pointer on ModelDeploymentSpec, and nil means off. That
is deliberate rather than a defaulted `mode: Off`: an absent block cannot
carry a maxReplicas that is required-but-meaningless, so `maxReplicas` can be
plainly required here instead of guarded by a CEL rule.

That choice is itself a lesson from this codebase. A `+kubebuilder:default`
on a field referenced by a CEL `has()` expression makes the rule
unsatisfiable — the API server fills the field in, so `has()` is always true
and an "exactly one of" or "required when" rule can never be satisfied. The
failure is invisible until a manifest is rejected for setting a field nobody
wrote. Modelling optionality with a nil struct sidesteps the whole class.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[AutoscalingMode](#autoscalingmode)_ | Mode selects who owns .spec.replicas. | Builtin | Enum: [Off Builtin] <br />Optional: \{\} <br /> |
| `minReplicas` _integer_ | MinReplicas is the floor.<br />It is not 0: scale-to-zero is explicitly out of scope for this project,<br />and a floor of zero on an engine with a multi-minute cold start would<br />turn the first request after a quiet period into a timeout rather than a<br />slow response. | 1 | Minimum: 1 <br />Optional: \{\} <br /> |
| `maxReplicas` _integer_ | MaxReplicas is the ceiling. Required: an autoscaler without one is a<br />budget incident waiting for a traffic spike. |  | Minimum: 1 <br />Required: \{\} <br /> |
| `metric` _[AutoscalingMetric](#autoscalingmetric)_ | Metric is the signal to track. | queue-depth | Enum: [queue-depth concurrency] <br />Optional: \{\} <br /> |
| `target` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | Target is the desired value of Metric, PER POD.<br />A Quantity rather than a float because the Kubernetes API convention<br />forbids floats in a spec: they serialise inconsistently across language<br />bindings and cannot round-trip through protobuf. Write "2" or "1500m". | 2 | Optional: \{\} <br /> |
| `tolerance` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | Tolerance is the fraction by which the measured ratio may differ from 1.0<br />before any scaling happens.<br />0.1 is the HPA's own default and it is not decoration. Without a<br />tolerance band, a queue depth of 4.1 against a target of 4 is a scale<br />event — and so is 3.9 fifteen seconds later. The fleet then oscillates<br />forever, each change costing a model load, and the metric it is reacting<br />to is mostly the noise created by its own churn. | 100m | Optional: \{\} <br /> |
| `pollInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | PollInterval is how often the metric is sampled. | 15s | Optional: \{\} <br /> |
| `window` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Window is the PromQL lookback the metric is averaged over.<br />Averaged, not instantaneous: queue depth is spiky by nature — it is the<br />difference between arrivals and a fixed number of decode slots — and an<br />instant read samples whichever microsecond the scrape landed on. | 60s | Optional: \{\} <br /> |
| `scaleUpStabilization` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | ScaleUpStabilization is how far back to look when scaling UP.<br />Zero by default, deliberately asymmetric with ScaleDownStabilization.<br />Adding capacity late costs latency the user experiences; removing it<br />early costs a cold start on the next spike. The asymmetry says which<br />mistake is cheaper, and it is the same asymmetry the HPA ships with. | 0s | Optional: \{\} <br /> |
| `scaleDownStabilization` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | ScaleDownStabilization is how far back to look when scaling DOWN.<br />Five minutes, and for an inference workload that is if anything short: a<br />pod removed here has to load its weights again to come back, which for a<br />real model is minutes of unavailable capacity bought to save seconds of<br />idle capacity. | 300s | Optional: \{\} <br /> |
| `provider` _[AnalysisProviderSpec](#analysisproviderspec)_ | Provider points at the metric backend.<br />Separate from the canary's provider block even though both normally name<br />the same Prometheus. They fail independently and for different reasons —<br />a rollout can be over while autoscaling runs forever — and sharing one<br />field would mean a ModelDeployment with no canary could not configure an<br />autoscaler at all. |  | Optional: \{\} <br /> |


#### AutoscalingStatus



AutoscalingStatus reports what the built-in autoscaler is doing.



_Appears in:_
- [ModelDeploymentStatus](#modeldeploymentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `desiredReplicas` _integer_ | DesiredReplicas is the count the autoscaler most recently asked for. |  | Optional: \{\} <br /> |
| `currentMetric` _string_ | CurrentMetric is the measured value, aggregated across the whole fleet,<br />rendered as a string.<br />A string, not a Quantity: this is an observation rather than a<br />specification, so it is not bound by the no-floats convention, and it<br />must be able to stay empty when there was no measurement at all. A zero<br />would be indistinguishable from a measured zero — which is precisely the<br />confusion that makes an autoscaler freeze a fleet while load climbs. |  | Optional: \{\} <br /> |
| `currentMetricPerPod` _string_ | CurrentMetricPerPod is CurrentMetric divided by the ready pods — the<br />number actually compared against the target. |  | Optional: \{\} <br /> |
| `lastScaleTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#time-v1-meta)_ | LastScaleTime is when .spec.replicas last changed because of this<br />autoscaler. |  | Optional: \{\} <br /> |
| `recommendations` _[ScaleRecommendation](#scalerecommendation) array_ | Recommendations is the recent decision history that stabilization reads.<br />It lives in status rather than in memory for the same reason the canary's<br />position does: a leader-election handover, a crash or an operator upgrade<br />must not reset it. An in-memory window silently forgets that the fleet<br />was scaled up ninety seconds ago, and the replacement process is free to<br />scale it straight back down. |  | MaxItems: 32 <br />Optional: \{\} <br /> |
| `message` _string_ | Message explains the most recent decision, including the decision not to<br />act. |  | Optional: \{\} <br /> |


#### BuiltinMetric

_Underlying type:_ _string_

BuiltinMetric names a query this operator writes, correctly, once.

Raw PromQL in user hands is a NaN factory: an unclamped denominator, a
forgotten rate(), a window shorter than the scrape interval, and the result
is a number that looks fine and means nothing. Each built-in below is written
with the denominator clamped away from zero and the window supplied from
AnalysisSpec, so the common cases cannot be got wrong. The Query escape hatch
remains for the cases that are not common.

_Validation:_
- Enum: [ttft-p95 ttft-p99 request-duration-p95 error-rate success-rate queue-depth output-token-rate]

_Appears in:_
- [AnalysisMetric](#analysismetric)

| Field | Description |
| --- | --- |
| `ttft-p95` | MetricTTFTP95 is the 95th percentile time to first token, in seconds.<br />The primary latency SLI: length-independent, unlike total duration.<br /> |
| `ttft-p99` | MetricTTFTP99 is the 99th percentile time to first token.<br /> |
| `request-duration-p95` | MetricRequestDurationP95 is the 95th percentile end-to-end duration.<br />Useful, but it scales with output length, so a change can mean the<br />request mix moved rather than the server.<br /> |
| `error-rate` | MetricErrorRate is the share of requests answered 5xx, as a fraction<br />in [0,1].<br /> |
| `success-rate` | MetricSuccessRate is 1 - error rate.<br /> |
| `queue-depth` | MetricQueueDepth is the mean number of requests waiting for a slot.<br /> |
| `output-token-rate` | MetricOutputTokenRate is generated tokens per second — the throughput<br />signal that reflects work done, rather than requests accepted.<br /> |


#### CanaryScaleSpec



CanaryScaleSpec decouples canary replicas from canary traffic weight.

Exactly one member may be set.



_Appears in:_
- [CanarySpec](#canaryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | Replicas pins the canary to a fixed number of pods for the whole<br />rollout, regardless of its traffic weight. |  | Minimum: 1 <br />Optional: \{\} <br /> |
| `matchTrafficWeight` _boolean_ | MatchTrafficWeight sizes the canary in proportion to its weight. This is<br />the default behaviour. |  | Optional: \{\} <br /> |


#### CanarySpec



CanarySpec configures a metric-gated progressive rollout.

The vocabulary is deliberately Flagger's — stepWeight, maxWeight,
failureThreshold, thresholdRange — because it is the vocabulary the
progressive-delivery ecosystem converged on and inventing a synonym for each
term would make every existing runbook wrong. What differs is what the
operator does with them; see AnalysisSpec on why analysis has four verdicts
rather than two.



_Appears in:_
- [RolloutSpec](#rolloutspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `stepWeight` _integer_ | StepWeight is the traffic share added at each step: 20 produces the<br />ladder 20, 40, 60, ... up to MaxWeight. Defaults to 20 when unset.<br />Deliberately NOT given a kubebuilder default, even though every other<br />optional field here has one. A defaulted field is always "present" to<br />CEL, which would make the mutual-exclusion rule above unsatisfiable: a<br />user who set only stepWeights would be rejected for also setting a<br />stepWeight they never wrote. The default lives in StepLadder instead,<br />where it can coexist with the validation that matters more. |  | Maximum: 100 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `stepWeights` _integer array_ | StepWeights is an explicit, strictly increasing ladder, e.g. [5, 20, 50].<br />It overrides StepWeight and MaxWeight entirely.<br />An explicit ladder exists because a uniform step is the wrong shape for<br />most real rollouts: the interesting information arrives in the first few<br />percent of traffic, so a small first step followed by larger ones spends<br />the risk budget where it buys the most. |  | MaxItems: 20 <br />Optional: \{\} <br /> |
| `maxWeight` _integer_ | MaxWeight is the traffic share at which the canary is promoted rather<br />than stepped further. Ignored when StepWeights is set.<br />It is deliberately below 100 by default. Once a canary is carrying half<br />the traffic and every check has passed, additional steps buy very little<br />information and cost real time; promotion at that point is a decision,<br />not a gamble. | 50 | Maximum: 100 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `analysis` _[AnalysisSpec](#analysisspec)_ | Analysis configures the metric checks that gate each step. |  | Optional: \{\} <br /> |
| `trafficRouting` _[TrafficRoutingSpec](#trafficroutingspec)_ | TrafficRouting selects how traffic is divided between the variants. |  | Optional: \{\} <br /> |
| `scale` _[CanaryScaleSpec](#canaryscalespec)_ | Scale decouples the canary's replica count from its traffic weight.<br />Borrowed from Argo Rollouts' canaryScale, and it earns its place for<br />inference specifically: a model server has a long, expensive warm-up, so<br />a canary that is scaled up in lockstep with its weight spends the first<br />analysis window of every step loading a model rather than serving. Fixing<br />the replica count up front means the canary is warm before it is<br />measured. |  | Optional: \{\} <br /> |
| `requireApproval` _boolean_ | RequireApproval pauses before the final promotion until an operator sets<br />the llmcp.io/promote annotation to "true".<br />The gate is on PROMOTION, not on each step. A human asked to approve<br />every 20% increment stops reading and starts clicking, which is worse<br />than no gate at all; asked once, at the point of no return, they actually<br />look. | false | Optional: \{\} <br /> |


#### CanaryStatus



CanaryStatus reports the state of an in-flight or just-finished canary.



_Appears in:_
- [ModelDeploymentStatus](#modeldeploymentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `revision` _string_ | Revision is the revision hash under evaluation. |  | Optional: \{\} <br /> |
| `stableRevision` _string_ | StableRevision is what a rollback would revert to. |  | Optional: \{\} <br /> |
| `failedRevision` _string_ | FailedRevision is the revision the most recent rollback rejected.<br />Without it the controller would immediately re-canary the same revision<br />it just rolled back from: the spec still names it, so the target and the<br />stable revision still differ, and the state machine would dutifully start<br />again — producing an infinite loop of identical failing rollouts, each<br />one costing a full analysis window and a Deployment churn.<br />Recording the rejection makes a rollback STICK until a human changes the<br />spec, which is exactly the semantics people expect from one. |  | Optional: \{\} <br /> |
| `desiredWeight` _integer_ | DesiredWeight is the traffic share the current step asked for. |  | Optional: \{\} <br /> |
| `currentWeight` _integer_ | CurrentWeight is the configured replica share after quantisation.<br />Reported separately from DesiredWeight because replica-based splitting<br />cannot configure 20% with 3 pods: the nearest share is 33%. This field is<br />derived from desired replica counts, not measured request distribution;<br />connection reuse can make observed traffic differ from the pod ratio. |  | Optional: \{\} <br /> |
| `step` _integer_ | Step is the zero-based index into the weight ladder. |  | Optional: \{\} <br /> |
| `steps` _integer_ | Steps is how many steps the ladder has. |  | Optional: \{\} <br /> |
| `failedChecks` _integer_ | FailedChecks counts rounds that produced a Fail. Not reset by a<br />subsequent Pass. |  | Optional: \{\} <br /> |
| `consecutiveErrors` _integer_ | ConsecutiveErrors counts back-to-back provider failures. Reset by any<br />round that reaches a verdict. |  | Optional: \{\} <br /> |
| `consecutiveInconclusive` _integer_ | ConsecutiveInconclusive counts back-to-back unusable rounds. |  | Optional: \{\} <br /> |
| `checks` _[MetricCheck](#metriccheck) array_ | Checks is the most recent round's per-metric outcomes. |  | Optional: \{\} <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#time-v1-meta)_ | StartTime is when this canary began. |  | Optional: \{\} <br /> |
| `lastAnalysisTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#time-v1-meta)_ | LastAnalysisTime is when the last round ran. It is what the interval is<br />measured from. |  | Optional: \{\} <br /> |
| `availableSince` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#time-v1-meta)_ | AvailableSince is when the canary's pods first became ready.<br />The warm-up delay is measured from here rather than from StartTime,<br />because a canary that spent four minutes pulling a model image has not<br />been warm for four minutes — and measuring a cold inference pod's first<br />requests would fail every canary that was ever going to be fine. |  | Optional: \{\} <br /> |
| `message` _string_ | Message is a human-readable summary of the current state. |  | Optional: \{\} <br /> |


#### EngineSpec



EngineSpec selects and configures the inference server.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[EngineType](#enginetype)_ | Type selects the engine implementation. | llamacpp | Enum: [llamacpp] <br />Optional: \{\} <br /> |
| `image` _string_ | Image overrides the engine profile's default container image.<br />This is also the supported way to substitute a deterministic stub server<br />in tests and CI: the stub is selected here rather than through a<br />dedicated enum value, so the real profile's argument-building code path<br />is exercised rather than bypassed. |  | Optional: \{\} <br /> |
| `imagePullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#pullpolicy-v1-core)_ | ImagePullPolicy for the engine image. | IfNotPresent | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |
| `contextSize` _integer_ | ContextSize is the maximum context window, in tokens. | 4096 | Minimum: 256 <br />Optional: \{\} <br /> |
| `maxConcurrency` _integer_ | MaxConcurrency is how many requests the engine may process concurrently<br />(llama.cpp's --parallel).<br />It also determines when the engine starts queueing, which is the signal<br />autoscaling is driven from — so it is a capacity knob, not just a<br />performance one. | 4 | Minimum: 1 <br />Optional: \{\} <br /> |
| `threads` _integer_ | Threads is the engine's worker thread count (llama.cpp's -t).<br />When unset it is DERIVED from the container's CPU limit. This matters<br />more than it looks: llama.cpp reads the host's /proc and is unaware of<br />cgroup limits, so left alone every replica spawns one thread per host<br />core. Several replicas on one node then oversubscribe the CPU badly<br />enough that latency percentiles become noise — which in turn makes<br />metric-driven canary analysis produce false rollbacks. |  | Minimum: 1 <br />Optional: \{\} <br /> |
| `extraArgs` _string array_ | ExtraArgs are appended verbatim to the engine's command line, after every<br />argument the profile generates. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#envvar-v1-core) array_ | Env are additional environment variables for the engine container. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#resourcerequirements-v1-core)_ | Resources are the engine container's compute resources. Setting a CPU<br />limit is strongly recommended: it is what Threads is derived from. |  | Optional: \{\} <br /> |
| `apiKeySecretRef` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#secretkeyselector-v1-core)_ | APIKeySecretRef supplies an API key the engine will require on requests. |  | Optional: \{\} <br /> |


#### EngineType

_Underlying type:_ _string_

EngineType selects the inference server implementation.

New values are added only in the release that implements them. An enum value
whose only behaviour is to fail validation is worse than no enum value:
adding one later is backward compatible, but shipping a broken one is not.

_Validation:_
- Enum: [llamacpp]

_Appears in:_
- [EngineSpec](#enginespec)

| Field | Description |
| --- | --- |
| `llamacpp` | EngineLlamaCPP runs llama.cpp's llama-server.<br /> |


#### HuggingFaceModelSource



HuggingFaceModelSource downloads weights from the Hugging Face Hub.



_Appears in:_
- [ModelSourceSpec](#modelsourcespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repo` _string_ | Repo is the Hub repository, e.g. "unsloth/Qwen3-0.6B-GGUF". |  | Pattern: `^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `file` _string_ | File is the specific file to fetch, e.g. "Qwen3-0.6B-Q4_K_M.gguf".<br />Required for GGUF engines; omit for engines that consume a whole repo. |  | Optional: \{\} <br /> |
| `tokenSecretRef` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#secretkeyselector-v1-core)_ | TokenSecretRef references a Hugging Face token for gated repositories. |  | Optional: \{\} <br /> |


#### ImageModelSource



ImageModelSource mounts model weights from an OCI image.



_Appears in:_
- [ModelSourceSpec](#modelsourcespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | Image is the OCI reference containing the model file. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `path` _string_ | Path is the absolute path to the model file inside that image.<br />This is where the file lives in the MODEL image, which is a different<br />place from where the engine will read it: the operator copies it onto a<br />shared volume and mounts that at a fixed location. The default matches<br />the convention used by this project's own Dockerfile.model. | /weights/model.gguf | Pattern: `^/.*` <br />Optional: \{\} <br /> |
| `pullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#pullpolicy-v1-core)_ | PullPolicy for the model image. | IfNotPresent | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |


#### InconclusiveAction

_Underlying type:_ _string_

InconclusiveAction is what to do when a canary cannot be judged.

_Validation:_
- Enum: [Wait Rollback Promote]

_Appears in:_
- [AnalysisSpec](#analysisspec)

| Field | Description |
| --- | --- |
| `Wait` | InconclusiveWait holds the canary at its current weight indefinitely.<br />The default, and the only safe one to default to. "We cannot tell" is not<br />evidence of health and is not evidence of harm; holding position keeps<br />the blast radius fixed and leaves the decision to a human, which is what<br />should happen when the automation has run out of information.<br /> |
| `Rollback` | InconclusiveRollback reverts. Appropriate when no traffic reaching the<br />canary is itself a symptom worth acting on.<br /> |
| `Promote` | InconclusivePromote continues as though the checks had passed.<br />Deliberately available and deliberately not the default: it is correct<br />for a genuinely low-traffic service where waiting forever is the worse<br />outcome, and dangerous everywhere else.<br /> |


#### MetricCheck



MetricCheck records one metric's outcome for status.



_Appears in:_
- [CanaryStatus](#canarystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the AnalysisMetric this check evaluated. |  | Required: \{\} <br /> |
| `verdict` _[Verdict](#verdict)_ | Verdict is the outcome. |  | Enum: [Pass Fail Inconclusive Error] <br />Required: \{\} <br /> |
| `value` _string_ | Value is the observed value, rendered as a string.<br />A string rather than a Quantity: this is an observation, not a<br />specification, so it is not bound by the no-floats convention, and it<br />must be able to say "NaN" or stay empty when there was no value at all.<br />A zero here would be indistinguishable from a measured zero. |  | Optional: \{\} <br /> |
| `threshold` _string_ | Threshold restates the band that was applied, so a status read weeks<br />later still explains itself without the spec beside it. |  | Optional: \{\} <br /> |
| `message` _string_ | Message explains a non-Pass verdict. |  | Optional: \{\} <br /> |


#### ModelDeployment



ModelDeployment is a declarative deployment of an LLM inference server:
it owns the serving workload, health-checks it, and reports a status other
controllers (and an HPA, via the /scale subresource) can act on.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `inference.llmcp.io/v1alpha1` | | |
| `kind` _string_ | `ModelDeployment` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[ModelDeploymentSpec](#modeldeploymentspec)_ | spec is the desired state. |  | Required: \{\} <br /> |


#### ModelDeploymentSpec



ModelDeploymentSpec defines the desired state of a ModelDeployment.



_Appears in:_
- [ModelDeployment](#modeldeployment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | Replicas is the TOTAL number of serving pods across all variants<br />(primary + canary combined).<br />This field backs the /scale subresource, so `kubectl scale` and any<br />HorizontalPodAutoscaler targeting this resource write here. During a<br />canary the controller distributes this total between the two variants<br />rather than adding to it — an autoscaler decides how much capacity<br />exists, the rollout decides how it is split.<br />It is a pointer so that "unset" is distinguishable from zero, which<br />matters when an external autoscaler owns the field. | 1 | Minimum: 0 <br />Optional: \{\} <br /> |
| `model` _[ModelSpec](#modelspec)_ | Model identifies what to serve and where its weights come from. |  | Required: \{\} <br /> |
| `engine` _[EngineSpec](#enginespec)_ | Engine selects and configures the inference server. |  | Required: \{\} <br /> |
| `serving` _[ServingSpec](#servingspec)_ | Serving configures the network surface in front of the engine. |  | Optional: \{\} <br /> |
| `rollout` _[RolloutSpec](#rolloutspec)_ | Rollout controls how replacements are rolled out when the pod template<br />changes. |  | Optional: \{\} <br /> |
| `observability` _[ObservabilitySpec](#observabilityspec)_ | Observability configures what the operator publishes for Prometheus and<br />Grafana to consume. |  | Optional: \{\} <br /> |
| `autoscaling` _[AutoscalingSpec](#autoscalingspec)_ | Autoscaling configures the built-in autoscaler.<br />Nil means off, and something else owns .spec.replicas: a human with<br />`kubectl scale`, a GitOps commit, or an external HorizontalPodAutoscaler<br />writing through the /scale subresource. All three work without any<br />configuration here, which is why this is a pointer rather than a struct<br />with a defaulted `mode: Off`. |  | Optional: \{\} <br /> |




#### ModelSourceSpec



ModelSourceSpec is a discriminated union: exactly one member must be set.

The union is modelled explicitly so that adding a source remains a backward
compatible change; turning a scalar field into a union later would not be.



_Appears in:_
- [ModelSpec](#modelspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _[ImageModelSource](#imagemodelsource)_ | Image mounts model weights from an OCI image.<br />This is the recommended source: it is deterministic, layer-cached by the<br />container runtime, works fully offline, and decouples the model's version<br />from the engine's — which is what allows a model change and an engine<br />change to be rolled out independently. |  | Optional: \{\} <br /> |
| `huggingFace` _[HuggingFaceModelSource](#huggingfacemodelsource)_ | HuggingFace downloads weights from the Hugging Face Hub at pod start.<br />Convenient, but startup is non-deterministic, every replica re-downloads<br />on every restart, and it is subject to upstream availability and rate<br />limits. |  | Optional: \{\} <br /> |
| `persistentVolumeClaim` _[PVCModelSource](#pvcmodelsource)_ | PersistentVolumeClaim mounts weights from an existing PVC.<br />Not implemented yet. |  | Optional: \{\} <br /> |


#### ModelSpec



ModelSpec describes the model being served.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the model identifier clients pass as "model" in OpenAI-compatible<br />requests, echoed at GET /v1/models. It is also the `model` label on every<br />metric this project emits, so keep it stable and low-cardinality. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `source` _[ModelSourceSpec](#modelsourcespec)_ | Source is where the model weights come from. |  | Required: \{\} <br /> |


#### ObservabilitySpec



ObservabilitySpec configures what the operator publishes about a
ModelDeployment for Prometheus and Grafana to consume.

Every knob here is optional and enabled by default. That is deliberate: the
metrics this produces are not decoration, they are the inputs canary analysis
and autoscaling read. A ModelDeployment whose observability was accidentally
off would still serve traffic, but every rollout decision made about it would
be made blind — so opting OUT is the choice that has to be explicit.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serviceMonitor` _[ServiceMonitorSpec](#servicemonitorspec)_ | ServiceMonitor configures the prometheus-operator ServiceMonitor the<br />controller creates for this ModelDeployment.<br />If the prometheus-operator CRDs are not installed in the cluster, no<br />ServiceMonitor is created and the MetricsRegistered condition explains<br />why. That is reported as a fact, not an error: a cluster without<br />Prometheus is a legitimate configuration, and failing the reconcile over<br />it would make the whole resource unusable. |  | Optional: \{\} <br /> |
| `prometheusRule` _[PrometheusRuleSpec](#prometheusrulespec)_ | PrometheusRule configures the recording rules and burn-rate alerts the<br />controller creates. |  | Optional: \{\} <br /> |
| `slo` _[SLOSpec](#slospec)_ | SLO defines the objectives those alerts are written against. |  | Optional: \{\} <br /> |


#### PVCModelSource



PVCModelSource mounts weights from a PersistentVolumeClaim.



_Appears in:_
- [ModelSourceSpec](#modelsourcespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `claimName` _string_ | ClaimName is the PVC to mount. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `path` _string_ | Path is the absolute path to the model file within the volume. |  | Pattern: `^/.*` <br />Required: \{\} <br /> |
| `readOnly` _boolean_ | ReadOnly mounts the volume read-only. | true | Optional: \{\} <br /> |


#### Phase

_Underlying type:_ _string_

Phase is a coarse, human-facing summary of where a ModelDeployment is.
Machine logic should read Conditions, not Phase.

The state machine is:

	Pending -> Progressing -> Available
	Available -> Canarying -> Promoting   -> Available
	                       -> RollingBack -> Degraded
	                       -> Paused      (awaiting a human)

Three invariants are enforced by unit tests rather than by comments:
Promoting and RollingBack always make progress; Paused never leaves without
external input; and entering Canarying requires a non-empty lastGoodRevision,
because a canary with nothing to roll back TO is not a canary, it is a
deploy with extra steps.

_Validation:_
- Enum: [Pending Progressing Available Canarying Promoting RollingBack Paused Degraded]

_Appears in:_
- [ModelDeploymentStatus](#modeldeploymentstatus)

| Field | Description |
| --- | --- |
| `Pending` | PhasePending means no revision has ever become available.<br /> |
| `Progressing` | PhaseProgressing means a rollout is in flight.<br /> |
| `Available` | PhaseAvailable is the steady state: desired replicas are ready.<br /> |
| `Canarying` | PhaseCanarying means a canary is running and being analysed.<br /> |
| `Promoting` | PhasePromoting means the canary passed and is becoming the primary.<br /> |
| `RollingBack` | PhaseRollingBack means the canary failed and is being torn down.<br /> |
| `Paused` | PhasePaused means the rollout is waiting for an operator to approve or<br />abort it. Nothing moves out of this phase without external input.<br /> |
| `Degraded` | PhaseDegraded means the rollout failed or availability was lost.<br /> |


#### PrometheusRuleSpec



PrometheusRuleSpec configures the generated recording and alerting rules.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled creates the PrometheusRule. Defaults to true. | true | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels are added to the PrometheusRule's metadata, for a Prometheus whose<br />ruleSelector is not empty. Same escape hatch, and same silent failure<br />mode, as ServiceMonitorSpec.Labels. |  | Optional: \{\} <br /> |
| `longWindowAlerts` _boolean_ | LongWindowAlerts emits the slow-burn tier as well: 3x over 1d and 1x over<br />3d, the two rungs of the Google SRE ladder that catch a budget being<br />consumed steadily rather than suddenly.<br />Off by default, and the reason is honesty rather than laziness. A demo<br />cluster is minutes old, so a 3d window has no data and every panel and<br />alert built on it shows "No Data" — which looks worse than not having<br />them and trains a viewer to ignore the alert list. Being explicit that<br />the SLO window is compressed for the demo is more credible than shipping<br />rules that cannot evaluate.<br />Turn this on for a real deployment, where the windows mean what they say. | false | Optional: \{\} <br /> |


#### RolloutSpec



RolloutSpec controls rollout behaviour.

The mechanics of a rolling update are delegated to the child Deployment,
which already implements them correctly. What this operator adds on top is
the behaviour a Deployment does NOT have: a Deployment whose rollout stalls
stays stalled forever, whereas exceeding ProgressDeadline here reverts to the
last revision that was known good — and, under the Canary strategy, a
revision that rolls out perfectly well but degrades a measured SLI is
reverted too.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[RolloutStrategyType](#rolloutstrategytype)_ | Type selects the update strategy. | RollingUpdate | Enum: [RollingUpdate Recreate Canary] <br />Optional: \{\} <br /> |
| `maxSurge` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#intorstring-intstr-util)_ | MaxSurge is the number or percentage of pods that may exist above the<br />desired count during an update. Ignored when Type is Recreate. |  | Optional: \{\} <br /> |
| `maxUnavailable` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#intorstring-intstr-util)_ | MaxUnavailable is the number or percentage of pods that may be<br />unavailable during an update. Ignored when Type is Recreate. |  | Optional: \{\} <br /> |
| `progressDeadline` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | ProgressDeadline is how long a rollout may make no progress before it is<br />declared failed. | 600s | Optional: \{\} <br /> |
| `autoRollback` _boolean_ | AutoRollback reverts to the last known-good revision when the deadline is<br />exceeded.<br />This applies to EVERY strategy, not just Canary. A stalled RollingUpdate<br />is the more common failure in practice — a bad image reference, a missing<br />secret, a model file that will not parse — and a Deployment left in that<br />state stays there indefinitely with no further signal. | true | Optional: \{\} <br /> |
| `canary` _[CanarySpec](#canaryspec)_ | Canary configures progressive delivery. Required when Type is Canary and<br />ignored otherwise. |  | Optional: \{\} <br /> |


#### RolloutStrategyType

_Underlying type:_ _string_

RolloutStrategyType selects how pod template changes are rolled out.

_Validation:_
- Enum: [RollingUpdate Recreate Canary]

_Appears in:_
- [RolloutSpec](#rolloutspec)

| Field | Description |
| --- | --- |
| `RollingUpdate` | RolloutRollingUpdate replaces pods incrementally, keeping the service up.<br /> |
| `Recreate` | RolloutRecreate tears down all pods before creating replacements.<br /> |
| `Canary` | RolloutCanary runs the new revision alongside the old one, shifts a<br />growing share of traffic to it, and promotes or rolls back based on<br />measured metrics.<br />This is the strategy the project exists to demonstrate. Kubernetes<br />already does rolling updates correctly and this operator delegates them<br />to the Deployment controller unchanged; what a Deployment cannot do is<br />notice that the new pods are HEALTHY BUT WORSE — passing their readiness<br />probes while serving 3x the latency — and undo itself.<br /> |


#### SLOSpec



SLOSpec declares the service level objectives alerts are written against.

# Why TTFT and not end-to-end latency

End-to-end request duration is a bad primary SLI for a language model,
because it scales with how many tokens the caller asked for. A p95 over mixed
traffic measures the request MIX as much as the server, so it moves when a
client changes its max_tokens and stays flat through a genuine regression that
happens to coincide with shorter prompts. An SLO built on it burns budget for
reasons the service cannot act on.

Time to first token is length-independent: it is how long the user waits
before anything happens. Time per output token is the other half, also
length-independent. That is exactly why OpenTelemetry's gen_ai semantic
conventions define the two separately rather than shipping one latency
metric, and it is why this operator's SLIs are TTFT and availability.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ttftThreshold` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | TTFTThreshold is the latency a request must beat to count as good.<br />It is SNAPPED to the nearest histogram bucket boundary at or above the<br />value, and the snapped figure is what the rules use. That is not a<br />rounding convenience: a latency SLI is a ratio over the bucket at an<br />exact `le` label, Prometheus matches `le` as a string, and a boundary the<br />histogram does not have returns an empty vector rather than an error. The<br />recording rule would then produce nothing and the burn-rate alert would<br />never fire — an SLO that cannot alert, which is worse than none because<br />it is believed. | 1500m | Optional: \{\} <br /> |
| `ttftObjective` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | TTFTObjective is the fraction of requests that must beat TTFTThreshold,<br />as a value in (0, 1).<br />0.99 means a 1% error budget. Note the budget is what the burn-rate<br />alerts are scaled against, so a stricter objective makes every alert more<br />sensitive rather than merely raising a bar. | 990m | Optional: \{\} <br /> |
| `availabilityObjective` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | AvailabilityObjective is the fraction of requests that must not fail with<br />a 5xx.<br />5xx only: a 4xx is the caller's fault — a malformed prompt, a model name<br />that does not exist, a context overflow — and counting one against the<br />service would let a single misbehaving client burn an error budget the<br />service is not responsible for. | 995m | Optional: \{\} <br /> |


#### ScaleRecommendation



ScaleRecommendation is one autoscaler decision, kept for stabilization.



_Appears in:_
- [AutoscalingStatus](#autoscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `time` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#time-v1-meta)_ | Time is when the recommendation was computed. |  | Required: \{\} <br /> |
| `replicas` _integer_ | Replicas is the raw recommendation, before stabilization and before<br />clamping to the current value. |  | Required: \{\} <br /> |


#### ServiceMonitorSpec



ServiceMonitorSpec configures scraping of a ModelDeployment's pods.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled creates the ServiceMonitor. Defaults to true. | true | Optional: \{\} <br /> |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Interval is the scrape interval.<br />The default is deliberately tighter than Prometheus's own 30s default.<br />Canary analysis evaluates a rate() over a short window — 60s by<br />measurement — and a rate over a window holding two samples is not a rate,<br />it is a difference between two points with no way to tell a trend from a<br />blip. 15s puts four samples in that window. | 15s | Optional: \{\} <br /> |
| `scrapeTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | ScrapeTimeout bounds a single scrape. It must not exceed Interval;<br />Prometheus rejects a ServiceMonitor where it does. | 10s | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels are added to the ServiceMonitor's metadata.<br />This is the escape hatch for a Prometheus whose serviceMonitorSelector is<br />not empty: prometheus-operator only picks up ServiceMonitors matching<br />that selector, and the failure mode when it does not match is total<br />silence — no error, no event, simply no metrics. Setting the label the<br />local Prometheus selects on is how a user fixes that without patching the<br />operator. |  | Optional: \{\} <br /> |
| `scrapeEngineMetrics` _boolean_ | ScrapeEngineMetrics also scrapes the inference engine's own metrics<br />endpoint, in addition to the shim's. Defaults to true.<br />The two are not interchangeable and neither replaces the other. The<br />shim's llmcp_* metrics are the canonical SLIs: they mean the same thing<br />under any engine, and they are what analysis and alerting consume. The<br />engine's native metrics are DIAGNOSTICS — KV-cache occupancy, slot<br />counts, tokens per second as the engine itself counts them — which are<br />invaluable when a human is debugging why a canary failed and useless as<br />something to gate a promotion on, because their names and semantics<br />change with the engine. | true | Optional: \{\} <br /> |


#### ServingSpec



ServingSpec configures the network surface in front of the engine.



_Appears in:_
- [ModelDeploymentSpec](#modeldeploymentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `port` _integer_ | Port is the port the ModelDeployment's Service listens on. | 8080 | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `startupTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | StartupTimeout bounds how long the engine may take to load its model and<br />report healthy before the pod is restarted.<br />Model load dominates startup and varies by orders of magnitude between<br />engines — seconds for a quantized GGUF, minutes for a framework that<br />compiles kernels first — so this is generous by default and is enforced<br />with a startupProbe rather than a livenessProbe. | 300s | Optional: \{\} <br /> |
| `shim` _[ShimSpec](#shimspec)_ | Shim configures the llmcp metrics sidecar. |  | Optional: \{\} <br /> |


#### ShimSpec



ShimSpec configures the llmcp metrics sidecar.

# Why a sidecar exists at all

It is not a convenience layer. llama.cpp exposes GAUGES only — no
histograms, no request duration, no time-to-first-token, no HTTP status-code
counter. p95 latency and success rate are therefore not merely inconvenient
to compute from the engine's own metrics, they are impossible, and canary
analysis would have nothing to gate a promotion on. The shim reverse-proxies
the OpenAI surface and emits the missing signals.

The second reason is engine independence. `llmcp_inference_ttft_seconds`
means exactly the same thing whether the engine underneath is llama.cpp,
vLLM or the deterministic test stub, because one binary measures it in one
place. That is what makes the EngineSpec abstraction real rather than
decorative: swapping engines does not invalidate a single alert, dashboard
or rollout gate.



_Appears in:_
- [ServingSpec](#servingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled injects the shim. Defaults to true.<br />Turning it off leaves the engine serving directly and is supported, but<br />it disables every llmcp_* metric — which means canary analysis, the<br />built-in autoscaler and the SLO alerts all lose their inputs. The<br />MetricsRegistered condition says so out loud rather than leaving an<br />operator to discover it from empty graphs. | true | Optional: \{\} <br /> |
| `image` _string_ | Image overrides the shim container image.<br />The operator's own build-pinned shim image is used when this is empty,<br />which is almost always what is wanted: the shim's metric names are the<br />contract the operator's analysis code reads, so running a shim from a<br />different build than the controller is a version skew with silent<br />consequences. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#resourcerequirements-v1-core)_ | Resources are the shim container's compute resources.<br />The shim is a streaming byte pump with a histogram attached; it is<br />deliberately given a small, explicit default rather than left<br />unconstrained, because an unbounded sidecar sharing a pod with a<br />CPU-starved inference engine can steal exactly the cycles whose absence<br />the metrics are supposed to be measuring. |  | Optional: \{\} <br /> |
| `logLevel` _string_ | LogLevel sets the shim's log verbosity. | info | Enum: [debug info warn error] <br />Optional: \{\} <br /> |


#### ThresholdRange



ThresholdRange is an inclusive band. At least one bound must be set.

Both bounds are resource.Quantity, not float64, because the Kubernetes API
convention forbids floats in a spec — they serialise inconsistently across
language bindings and cannot round-trip through the API server's protobuf
encoding. Write max: "1.5" or max: "1500m".



_Appears in:_
- [AnalysisMetric](#analysismetric)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `min` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | Min is the inclusive lower bound. |  | Optional: \{\} <br /> |
| `max` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#quantity-resource-api)_ | Max is the inclusive upper bound. |  | Optional: \{\} <br /> |


#### TrafficRoutingMode

_Underlying type:_ _string_

TrafficRoutingMode selects the traffic-splitting mechanism.

_Validation:_
- Enum: [Replica]

_Appears in:_
- [TrafficRoutingSpec](#trafficroutingspec)

| Field | Description |
| --- | --- |
| `Replica` | TrafficRoutingReplica splits traffic by REPLICA COUNT: both variants sit<br />behind one Service, and the share each receives is approximately its<br />share of the ready pods.<br />The approximation is real. kube-proxy<br />load-balances per CONNECTION, not per request, so a client using HTTP<br />keep-alive — which every OpenAI SDK does by default — pins itself to one<br />pod for its whole session. At a requested 20% with 8 concurrent clients,<br />the realised split is some multiple of 1/8, stable and wrong for the<br />entire analysis window.<br />This project's feasibility spike measured the effect at a skew of 1.02<br />with keep-alive on and 1.09 with it off, which is well inside the noise —<br />so replica-based splitting stands, and the load generator disables<br />keep-alive as the documented mitigation. status.canary.currentWeight<br />reports the configured, quantized replica share; it is not a measurement<br />of request distribution.<br /> |


#### TrafficRoutingSpec



TrafficRoutingSpec configures how traffic is divided between variants.



_Appears in:_
- [CanarySpec](#canaryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[TrafficRoutingMode](#trafficroutingmode)_ | Mode selects the splitting mechanism. | Replica | Enum: [Replica] <br />Optional: \{\} <br /> |




#### Verdict

_Underlying type:_ _string_

Verdict is the outcome of one metric check.

Four values, with DISJOINT counters, because collapsing them into pass/fail
is how canary controllers act on the wrong evidence. The distinction that
matters most: a Prometheus outage yields Error, never Fail, and does not
spend the failed-check budget. The state machine holds initially and aborts
safely to the stable revision at the configured consecutive-error limit.

_Validation:_
- Enum: [Pass Fail Inconclusive Error]

_Appears in:_
- [MetricCheck](#metriccheck)

| Field | Description |
| --- | --- |
| `Pass` | VerdictPass means the query succeeded and the value is inside the<br />threshold range. It resets the Error and Inconclusive counters but NOT<br />the failure counter — see AnalysisSpec.FailureThreshold.<br /> |
| `Fail` | VerdictFail means the query succeeded and the value is outside the<br />range. This is the only verdict that counts toward a rollback.<br /> |
| `Inconclusive` | VerdictInconclusive means the query succeeded but the answer is<br />unusable: no samples, NaN, or a request rate below MinRequestRate.<br /> |
| `Error` | VerdictError means the provider could not be queried, or answered with<br />something that is not a number.<br /> |


