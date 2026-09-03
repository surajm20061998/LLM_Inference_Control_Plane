# ADR-0005: Scale on queue depth through the /scale subresource, with no `mode: HPA`

- **Status:** Accepted
- **Date:** 2026-09-03
- **Sprint:** 5

## Context

The project's thesis is that the control law the GPU-scale ecosystem converged
on — scale on queue depth, gate promotion on TTFT and error rate, never on CPU
utilization — applies unchanged to a tiny CPU model on a laptop. Sprint 4 built
the promotion gate. This sprint builds the scaling half.

Three questions had to be settled.

## Decision 1: queue depth, not CPU

An inference server saturates its fixed number of decode slots long before it
saturates the CPU. With `--parallel 4`, a llama.cpp pod is working on at most
four requests; the fifth waits. Requests therefore start queueing while CPU
utilisation still reads something unremarkable, and by the time utilisation is
high enough to cross a 70% target, tail latency has been bad for minutes.

`llmcp_inference_queue_depth` crosses its threshold at the moment work actually
starts waiting, which is the moment more capacity would have helped. The shim
already emits it — it is `max(0, in_flight - maxConcurrency)`, derived from the
engine's own concurrency setting, which is why the sidecar is told
`--max-concurrency` at injection time.

`concurrency` is offered as the second option for the case where the engine's
parallelism is high enough that queueing is rare but latency still degrades: it
is the smoother signal and reacts earlier, at the cost of being less sharp.

## Decision 2: no `mode: HPA`

An external HorizontalPodAutoscaler writes `.spec.replicas` through the /scale
subresource, and the controller reconciles that like any other spec change. An
`HPA` enum value would therefore change exactly zero lines of behaviour while
implying the operator does something special for it.

What ships instead is `test/chainsaw/06-external-hpa`, which proves a stock HPA
adopts the CR, reads a usable `/scale`, and can write back. That is worth more
than an enum value, and it is an assertion no other tier can make: envtest runs
no HPA controller, so it can only show that the subresource this operator
publishes has the right *shape*.

The enum is `Off | Builtin`, and `spec.autoscaling` is a nil-able pointer
rather than a struct with a defaulted `mode: Off`. That second choice is a
direct consequence of a bug this codebase already shipped once: a
`+kubebuilder:default` on a field referenced by a CEL `has()` expression makes
the rule unsatisfiable, because the API server fills the field in and `has()` is
always true. It cost a rejected sample in Sprint 4 to find. Modelling optionality
with a nil struct means `maxReplicas` can be plainly `+required` instead of
guarded by a rule that could go the same way.

## Decision 3: two functions own replicas, not one

`autoscale.Recommend` produces a desired **total**. `canary.Split` distributes
that total between the primary and canary variants. Two functions, one input, no
fight.

An autoscaler and a rollout controller that each believe they own the replica
count is the classic way for a progressive-delivery system to oscillate forever,
and it is the bug Argo Rollouts spent several releases resolving. The contract
was written into `internal/canary/weights.go` in Sprint 4, before `Recommend`
existed, precisely so the two could not be designed independently. The end-to-end
proof is a test that doubles the fleet mid-canary and asserts the weight ratio
does not drift.

The built-in autoscaler writes `.spec.replicas` — not the child Deployments —
because that is what makes it and an external HPA interchangeable. Writing the
Deployments directly would leave `.spec.replicas` stale, so `kubectl scale` and
`kubectl get` would report a count the cluster is not running. The write is a
**merge patch** naming one field, not an Update: an Update sends the whole
object and would clobber a concurrent edit, including a user changing the model
image in the same second.

## The algorithm

The HorizontalPodAutoscaler's, deliberately and almost literally. Reinventing it
would mean rediscovering the tolerance band and the asymmetric stabilization
windows the hard way, and an operator whose scaling behaviour surprises someone
who already knows how an HPA behaves is a worse operator regardless of how
clever the alternative is.

The order of the guards is the substance:

1. **No usable measurement ⇒ HOLD.** An unreachable provider, an empty result, a
   NaN — all of them freeze the fleet rather than producing a number. An
   autoscaler that treats "I could not measure" as "the measurement is zero"
   scales to its floor during a monitoring outage, turning a Prometheus incident
   into a serving incident. This is the same four-valued discipline the canary
   analysis uses, applied to a different decision.
2. **No ready pods ⇒ HOLD.** The ratio's denominator would be zero.
3. **Inside the tolerance band ⇒ HOLD.** 0.1, the HPA's default. Without it, a
   queue depth of 4.1 against a target of 4 is a scale event, and so is 3.9
   fifteen seconds later — forever, each change costing a model load, reacting
   mostly to the noise created by its own churn.
4. Otherwise scale by the usage ratio, clamp, then stabilize.

Stabilization takes the **minimum** recommendation over the scale-up window and
the **maximum** over the scale-down window. Both rules read as "be conservative
about changing": a single spike cannot add capacity, and a single quiet moment
cannot remove it. Because the two windows have different lengths — zero up, five
minutes down — the fleet grows promptly and shrinks reluctantly, which for a
workload whose pods take minutes to load a model is the difference between an
autoscaler that helps and one that spends its time recovering from itself.

One subtlety is easy to get wrong and is tested explicitly: a scale-UP decision
must never come out as a scale-down because the window's minimum happens to sit
below the current count. That would shrink a fleet at the exact moment the metric
is asking for more.

**The ready-pod count cancels out of the recommendation.** `ceil(ready ×
((value/ready) / target))` reduces to `ceil(value / target)`. That is the same
reduction the HPA's arithmetic makes, and far from being a curiosity it is the
property that keeps a fleet from chasing its own tail: mid-scale-up with 2 of 6
pods ready, the recommendation stays put while the rest finish loading their
weights, instead of climbing every poll because the ready pods look overloaded.
The ready count still does two jobs — it is the divisor for the per-pod figure
the tolerance band compares, and it is what gets reported — so the field is not
redundant, only narrower than it first appears.

## Consequences

- **The stabilization history lives in `status.autoscaling.recommendations`,**
  bounded by both a time window and a count. In memory it would be silently
  reset by a leader-election handover, and the replacement process would be free
  to scale down a fleet that was deliberately scaled up ninety seconds earlier.
  The count bound is what stops a long window plus a short poll interval from
  writing an unbounded list into etcd.
- **The autoscaler is the one thing in this operator that genuinely needs a
  timer.** Everything else is watch-driven, but "the load went up" is not an
  event any Kubernetes object emits — the only way to notice is to look. The
  reconcile requeue is the shorter of the canary's interval and the autoscaler's
  poll, because a timer that fires too often costs a no-op reconcile whereas one
  that fires too rarely misses a decision.
- **`Builtin` plus an external HPA is detected and refused,** at the cost of one
  cached List of `autoscaling/v2`. Two controllers writing `.spec.replicas` from
  different signals do not average out — they fight, and the fleet oscillates at
  the rate of the faster one. The symptom looks like a flapping workload rather
  than a configuration mistake, which is exactly why it is worth naming.
- **`AutoscalingReady` is False, not Unknown, when metrics are unreadable.** A
  frozen fleet is not a neutral state: demand can climb underneath it
  indefinitely and nothing else in the status says so.
- Scale-to-zero remains out of scope. `minReplicas` has a floor of 1, and on an
  engine with a multi-minute cold start a floor of zero would turn the first
  request after a quiet period into a timeout rather than a slow response.

## Verification

- `internal/autoscale`: a 13-case table over `Recommend`, an exhaustive bounds
  check over `current × ready × value`, a closed-loop convergence test that fails
  if the fleet oscillates, and explicit cases for the two directions of
  stabilization and for non-finite measurements.
- `internal/controller`: envtest specs for the `/scale` round-trip, a
  `status.selector` that is parsed and matched against real pod labels of both
  variants, scale-up/hold/scale-down against a load dial, a frozen fleet on
  provider failure, min/max clamping, HPA conflict detection, history surviving
  a reconciler swap, and a canary keeping its weight ratio while the fleet
  doubles.
- `test/chainsaw/05-scale-subresource` and `06-external-hpa`: the cluster tier —
  `kubectl scale` reaching real pods, and a stock HPA adopting the CR.
