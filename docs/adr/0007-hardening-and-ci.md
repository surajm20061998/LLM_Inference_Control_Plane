# ADR-0007: Lint rules and generated-artifact checks as the guard against regressions found the hard way

- **Status:** Accepted
- **Date:** 2026-09-03
- **Sprint:** 7

## Context

Six sprints produced a working control plane. This one is about making its
invariants survive the next person to touch it — including a future me who has
forgotten why any of them exist.

The distinguishing feature of this project's bugs is that **almost none of them
error**. A shim without `FlushInterval: -1` still produces a TTFT metric. A CEL
rule referencing a defaulted field still compiles. A dashboard panel querying a
series nobody emits still renders. In every case the system keeps working and
quietly reports something that is not true.

Documentation does not defend against that class. A check that fails the build
does.

## Decision: encode the expensive lessons as checks, and verify each check bites

Four guards ship, and each one exists because this codebase already shipped the
bug it catches.

### 1. `forbidigo`: no `time.Now()` outside `main`

Every timing decision — a canary's analysis interval, an autoscaler's
stabilization window, a warm-up delay — runs through an injected
`clock.PassiveClock`. That is what lets a five-step rollout with a 60-second
warm-up replay in microseconds instead of a test suite that sleeps for minutes
and flakes on a busy runner.

A single `time.Now()` in a decision path silently converts one of those tests
from deterministic to timing-dependent, and the failure surfaces later as an
unrelated flake in a different package. The ban makes the injection something
the compiler enforces rather than something people remember.

Exclusions are per-path and each is justified in the config: `cmd/main.go`
constructs the real clock, the shim and stub engine *measure* wall time as their
entire job, and tests seed fake clocks from real timestamps.

### 2. `forbidigo`: no `Result.Requeue`

Deprecated in controller-runtime, and worse than deprecated for this codebase:
it requeues with exponential backoff that is invisible in the return value,
whereas `RequeueAfter` states the delay as a number a test can assert on. Both
pure state machines here return their requeue timing as a **value**, and
`Requeue` would reintroduce a side effect in its place.

### 3. A structural check for the CEL/default collision

This is the guard I most wish had existed three sprints ago.

A `+kubebuilder:default` on a field that a CEL rule tests with `has()` makes that
rule **unsatisfiable**. The API server applies the default before validation, so
`has(self.x)` is always true, and an "at most one of x and y" rule rejects every
object that sets y — because the API server set x for them. The user did not
write x. The error message says they did.

Nothing about the Go source looks wrong. The collision exists only in the
generated schema, and it took a rejected sample to find.

`TestNoDefaultOnAFieldInsideAHasRule` walks every validation rule in the
generated CRD, extracts every `has(self.FIELD)`, and fails if FIELD carries a
default at that schema node. It is mechanical, complete, and needs no fixture to
stay in step with the API.

A sibling check requires every rule to carry a `message`, because the API
server's fallback quotes the raw CEL expression — accurate, and useless to
whoever pasted the manifest.

### 4. Generated artifacts are checked, not trusted

`docs/api.md` is generated from the Go doc comments by `crd-ref-docs`, and CI
regenerates the CRD, the RBAC role, the deepcopy functions and the reference,
then fails if anything changed. Hand-maintained API docs are wrong within two
sprints, and a reader who catches them being wrong once stops trusting the rest.

The drift check distinguishes **untracked** from **modified**: an uncommitted
file is repo hygiene, not drift, and failing on it would make `make verify` red
in a perfectly good working tree — which trains people to stop running it.

## Decision: three test tiers with explicit jobs

The gap this sprint closes is that Sprints 3 and 4 shipped with their exit
criteria proven only at the envtest tier, because Docker was not running. That
is a gap rather than a preference, and it is worth writing down what each tier
can and cannot prove.

| Tier | Has | Proves | Runtime |
|---|---|---|---|
| unit | nothing | `canary.Next`, `autoscale.Recommend`, `analysis.Evaluate`, every query string | milliseconds |
| envtest | a real API server and etcd | CEL, defaulting, `/scale` round-trips, SSA idempotency, conditions, ownership | ~20 s |
| Chainsaw | a real cluster | that pods START, that the built-in Deployment and HPA controllers act, that traffic reaches the shim | minutes |

**envtest runs no controllers and no kubelet.** A Deployment created there never
creates a ReplicaSet, never schedules a pod, and its `.status` stays at the zero
value forever — which is why `test/helpers.MarkDeploymentAvailable` exists. So
envtest can prove this operator publishes a correct `/scale` subresource, and
only a cluster can prove a stock HorizontalPodAutoscaler *reads* it and writes
back.

Six Chainsaw suites ship. Every one runs against the **deterministic stub
engine**: a real model takes minutes to load and its latency varies by orders of
magnitude between runs, so assertions written against it would be flaky by
construction. That is why `cmd/fakeengine` was a required Sprint 1 deliverable
rather than a convenience — the canary rollback suite injects a reproducible 3×
TTFT regression, which no real model can be asked to produce.

**The canary suites run nightly, not on every push.** Standing up
kube-prometheus-stack, driving load for several minutes and waiting for an
analysis verdict is genuinely slow, and paying that on every push pushes people
towards skipping CI rather than towards fixing it. The four fast suites gate
every change; the two slow ones catch integration drift on a schedule.

The `helm/kind-action` step pins **both** the kind version and the node image.
The action's defaults track whatever it shipped with, and a Kubernetes minor
version difference is exactly where CEL behaviour, native sidecar semantics and
`/scale` handling change underneath you.

## Consequences

- Every guard in this ADR was **verified to bite** by deliberately reintroducing
  the bug it catches and confirming the build went red: a `time.Now()` in
  `planCanary`, a `Result{Requeue: true}`, the original `stepWeight` default,
  and a dashboard panel pointed at a made-up series. A check nobody has seen
  fail is a check nobody knows works.
- `make verify` is the single local command: manifests, generate, fmt, vet,
  lint, test, api-docs-check. `make e2e-up && make e2e-chainsaw` is the cluster
  tier.
- The exclusion list for `forbidigo` is a maintenance surface. It is written
  per-path with a stated reason for each, so adding one is a decision someone
  has to defend in review rather than a wildcard that quietly grows.
- Operator metrics were already delivered in Sprint 6 rather than here, because
  the canary dashboard needed them and shipping panels that query nothing would
  have been the exact failure that sprint was about.

## What the first real CI run found

The section below used to say the Chainsaw suites had never been executed and
that "the first real run will find something; it always does." It did. Recording
what, because the pattern is more useful than the individual bugs.

**Every one of them was invisible to `make verify`, and for the same structural
reason: the local environment is not the deployed one.**

1. **`.dockerignore` starved `//go:embed`.** The Grafana dashboards were added
   in Sprint 3; `.dockerignore` ignores everything and re-included only `*.go`.
   `//go:embed` is resolved by the compiler, so the image build failed with
   `pattern dashboards/*.json: no matching files found` — while every local
   build, test and lint run succeeded, because the working tree has the files.
   Four sprints passed before an image was built.

2. **Chainsaw compares arrays by index.** Six suites asserted
   `status.conditions: [- type: Ready]`, which means *"conditions[0] is Ready"*,
   not *"the Ready condition"*. This operator's `conditions[0]` is `SpecValid`,
   always. The fix is a JMESPath lookup, which is also order-independent.

3. **`sh` is dash on Ubuntu.** Chainsaw runs script steps with `sh -c`. All
   eighteen began `set -euo pipefail`, which dash rejects outright. It works on
   macOS only because `/bin/sh` there is bash in POSIX mode.

4. **kind ships no metrics-server.** A suite asserted an HPA's `ScalingActive`
   condition, which cannot go True without one. `AbleToScale` is the correct
   assertion anyway: it tests the /scale contract this operator provides, rather
   than Kubernetes' metrics pipeline.

5. **`config/dev` required an optional CRD.** It contained a ServiceMonitor, so
   `kubectl apply` failed on any cluster without prometheus-operator — making
   `make dev-deploy` impossible on a fresh cluster, in the exact order the README
   prescribes. The operator handles this situation correctly at runtime, probing
   for the CRD and reporting `MetricsRegistered`; the manifests had simply never
   been held to the same standard. They now live in `config/monitoring`, applied
   by `make monitoring-install`.

Each is now a test in `internal/build`, a package whose subject is **how this
repository is packaged** rather than how it behaves — the one thing no other
tier can see. Each was verified by reintroducing the bug.

The sixth fix was to CI itself. It had been reproducing the deployment by hand
(`kustomize build config/default | sed ...`), and the `sed` matched nothing, so
the controller would have injected a shim image nobody built. CI now runs
`make kind-up && make dev-deploy` — the documented local workflow — so it proves
the instructions in the README rather than a parallel path only CI uses.

## What is still not proven

Honesty is worth more here than a green tick.

The Chainsaw suites are **written and schema-validated, but have not been
executed**, because Docker was unavailable throughout the implementation of
Sprints 5 to 7.

`make chainsaw-lint` validates every suite against Chainsaw's schema, and
`internal/build` now checks the semantics a schema cannot express. Together they
caught five real bugs. But note what they still cannot do: **schema-valid,
semantically-checked suites are not passing suites.** The image build is fixed
and the deployment path is fixed; whether the assertions themselves hold against
a running cluster is unverified, because Docker has been unavailable throughout.

`make verify` — everything that needs no cluster — is green, and is what every
other claim in this repository rests on.

There is a small lesson in how that lint step was added, worth recording because
it is the same failure this ADR is about. The first attempt validated the suites
with `chainsaw lint ... | grep -qi valid`, which reported all six as passing —
because "The schema is **not valid**" contains the word "valid". Checking the
exit code instead immediately failed two of them. A check that cannot fail is
indistinguishable from a check that passes, which is precisely why every guard
here was verified by deliberately breaking the thing it watches.
