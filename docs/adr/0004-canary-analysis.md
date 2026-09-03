# ADR-0004: Four-valued analysis verdicts with disjoint counters, driven by a pure state machine

- **Status:** Accepted
- **Date:** 2026-09-03
- **Sprint:** 4

## Context

Sprint 4 is the centrepiece: run a new revision alongside the old one, shift a
growing share of traffic to it, and promote or roll back based on measured
metrics. Argo Rollouts and Flagger both do this well, and neither is a
dependency here — building the controller *is* the project. What is borrowed is
their vocabulary (`stepWeight`, `maxWeight`, `failureThreshold`,
`thresholdRange`), because inventing a synonym for each term would make every
existing runbook wrong.

Three decisions in this sprint are load-bearing, and each of them is a place
where a homegrown canary controller typically goes wrong.

## Decision 1: four verdicts, with disjoint counters

A check does not produce pass/fail. It produces one of four values, and each
spends its own budget:

| Verdict | Meaning | Counter → limit |
|---|---|---|
| `Fail` | Query succeeded, value outside `thresholdRange` | `failedChecks` → `failureThreshold` ⇒ **rollback** |
| `Error` | Provider unreachable, timed out, or answered with something that is not a number | `consecutiveErrors` → `consecutiveErrorLimit` |
| `Inconclusive` | Query OK but unusable: no samples, NaN, ±Inf, or below `minRequestRate` | `consecutiveInconclusive` → `inconclusiveLimit` ⇒ `onInconclusive` |
| `Pass` | In range | resets Error and Inconclusive, **not** `failedChecks` |

**A monitoring outage must never cause a rollback.** Prometheus going down
yields `Error`, and the error budget resets on the first healthy round. A single
"strikes" counter cannot express this: it cannot distinguish "the release is
bad" from "Prometheus is down" from "nobody is sending traffic", and those three
call for a rollback, a retry and a human respectively.

Two subtleties follow, both counter-intuitive and both deliberate.

**Error outranks Fail when aggregating a round.** With three metrics and a
half-broken Prometheus, one query times out while another returns a partial
result that happens to look bad. If `Fail` won, that round would spend the
rollback budget and, repeated, tear down a canary because the monitoring stack
is sick. The cost of the chosen ordering is a real failure being masked while
Prometheus is *also* unhealthy — which is the right trade, because an unnoticed
regression is recoverable by the next round whereas a spurious rollback destroys
the deployment in flight and erases the evidence.

**A Pass does not reset `failedChecks`** (Flagger's semantics). A canary that
fails, passes, fails, passes, fails is not healthy — it is intermittently
broken, which for a latency SLI is the more common shape of a real regression
than a clean step change. Resetting would make such a canary immortal: it would
climb the ladder forever, never accumulating enough consecutive failures to be
stopped.

**`minRequestRate` is a first-class spec field, not a PromQL idiom buried in a
template.** Without it the whole demo is a lie: with no traffic, the error rate
is 0 and every latency percentile is absent, so a canary passes every check
having served nothing and is promoted. The gate is applied to the **whole
round** before any metric is queried, rather than per metric — a per-metric gate
happens to work with the default metric set and stops working the moment someone
configures a rollout whose only check is error rate.

Every built-in query clamps its denominator (`clamp_min(..., 1e-9)`), because
PromQL division by zero yields NaN and **NaN compares false against every
threshold** — so an unclamped ratio reports `Pass` for a canary that served
nothing. `analysis.Evaluate` is tested against the full matrix of things a
backend can return: empty result, multi-sample, NaN, ±Inf, exactly-at-boundary,
and a genuine measured zero.

## Decision 2: the core is a pure function

`canary.Next(Input) Output` takes a value and returns a value. No client, no
context, no `time.Now()`, no logger. The clock reading, the analysis outcome and
the canary's readiness all arrive as struct fields; advance, promote, roll back
and *requeue in 30 seconds* all leave as struct fields for the caller to carry
out.

This is not stylistic. Rollout logic is where the expensive bugs live and is
also the hardest part to exercise against a real cluster: reproducing "the
fourth analysis round failed while the third step was still scaling up" takes
minutes of wall time and a lot of luck. As a pure function the same scenario is
four lines in a table test that runs in microseconds, and an entire five-step
canary — including its 60-second warm-up and 30-second intervals — replays
deterministically by stepping a fake clock.

The specific consequence: **`RequeueAfter` is a return value, not a side
effect.** A test asserts on it directly rather than measuring how long the
controller slept.

Determinism at the integration tier comes from the same idea applied to the
other input. `analysis.ScriptedProvider` replays a fixed sequence — pass, pass,
fail, fail — which is what makes `rolls back on exactly the second failure, not
the first` an assertion rather than an aspiration. Against a live Prometheus it
cannot be written at all: you cannot induce a metric that fails precisely twice.

## Decision 3: two functions own replicas, not one

`autoscale.Recommend` produces `desiredTotal`; `canary.Split` distributes it
into `(primary, canary)`. Two functions, one input, no fight. A canary
controller and an autoscaler that each believe they own the replica count is the
classic way for a rollout to oscillate forever, and it is the bug Argo Rollouts
spent several releases resolving. The contract is written down here even though
`Recommend` does not exist until Sprint 5.

Traffic is split by pod count, so achievable weights are the multiples of
1/total. **Quantisation is reported, not hidden**: `status.canary.desiredWeight`
says 20 and `currentWeight` says 33 at three replicas, and a `WeightQuantized`
event fires. Publishing only the request would let an operator believe the blast
radius is a fifth of production when it is a third, and would make the analysis
appear to be judging an exposure it is not.

Two invariants hold across the full input space (`total ∈ [1,20] × weight ∈
[0,100]`, exhaustively tested rather than sampled): `primary + canary == total`
exactly, and the primary never drops below one pod while the weight is under
100. At a total of **one** these cannot both hold, and the primary wins — handing
the single pod to the canary would put all of production on an unproven revision
while calling it a 20% exposure. The state machine detects that case up front
and declines to canary with `ReasonInsufficientReplicas`, because a canary
Deployment with zero replicas never becomes available and the rollout would
otherwise sit at "waiting for canary replicas" forever with no hint that it never
can.

## Other decisions worth recording

**The primary runs the last known-good revision during a canary, rendered from
its `ControllerRevision`.** Rendering both variants from `md.Spec` would produce
two identical pod templates and an analysis that compares a revision against
itself. The stored payload is **merged** into the live spec, never assigned
wholesale: a revision records only model, engine, serving port and shim, so
assigning it would null out `replicas` — undoing whatever an autoscaler had
decided — and turn a rollback into an unintended scale-down.

**A rollback records `status.canary.failedRevision` and sticks.** The spec still
names the rejected revision, so target and stable still differ; without the
guard the machine starts an identical canary on the very next reconcile, forever,
each iteration costing a full analysis window and a Deployment churn.

**Promotion is a phase, not an instant.** The canary Deployment is removed and
the primary is re-pointed at the winning revision, which then rolls out through
its own rolling update — seconds for a stub, minutes for a real model. Until
`lastGoodRevision` catches up the machine stays in `Promoting`, re-asserting the
same desired state. Without that phase the next reconcile would see target ≠
stable and start a brand-new canary for the revision it just promoted.

**The manual gate is on promotion, not on each step.** A human asked to approve
every 20% increment stops reading and starts clicking, which is worse than no
gate at all; asked once, at the point of no return, they actually look. Nothing
but `llmcp.io/promote` or `llmcp.io/abort` leaves a pause — a timeout would make
the gate advisory — and the annotation is **consumed** once acted on, so an
earlier "yes" cannot silently approve the next release. Abort outranks approve
within a single pass: an operator hitting the brakes must not race with the
machine's own optimism.

**Automatic rollback on `ProgressDeadlineExceeded` applies to every strategy.**
It is the one thing a Deployment genuinely cannot do for itself: once it sets
`Progressing=False`, nothing in Kubernetes ever revisits the decision. A stalled
`RollingUpdate` is the more common failure in practice — a bad image reference,
a missing secret, a model file that will not parse.

## Consequences

- `status.canary` is the state machine's only persistent home, which makes the
  controller restartable mid-rollout: a leader-election handover, a crash or an
  operator upgrade all resume at the step reached, with the failure budget
  intact. An in-memory cache would silently reset that budget on every restart,
  which is how a broken canary gets promoted by an operator that was merely
  rescheduled.
- Canary events (`CanaryStarted`, `CanaryAdvanced`, `CanaryPromoted`,
  `CanaryRolledBack`) are emitted on transitions only, never per reconcile. They
  are the audit trail the canary dashboard sources its annotations from, and a
  duplicate is a second vertical line on the graph at a moment when nothing
  happened.
- `internal/traffic/replica.go` from the original plan was folded into
  `internal/canary/weights.go`. The split would have separated `Split` from the
  state machine that is its only caller and from the invariants they share, for
  no gain.
- `analysis.Provider` has one method taking a finished query string. Query
  construction sits outside the interface so the PromQL a rollout decision rests
  on can be asserted character by character with no backend of any kind.

## Verification

- `internal/canary`: a 25-case table over `Next`, a full-input-space property
  test over `Split`, an exhaustive verdict-precedence table, and a replay of a
  complete three-rung rollout driven by the returned `RequeueAfter`.
- `internal/analysis`: the NaN matrix over `Evaluate`, golden strings for every
  built-in query, and the Prometheus wire format including `NaN`/`±Inf`
  survival, empty results, multi-sample results and non-vector results.
- `internal/controller`: envtest specs for promote, rollback-on-exactly-the-
  second-failure, provider-down-yields-Error-not-rollback, too-little-traffic
  holds, approval gating, abort, mid-flight spec change, honest quantisation,
  the single-replica refusal, and the `/scale` replica sum across variants.
