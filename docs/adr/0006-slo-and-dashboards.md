# ADR-0006: TTFT as the primary SLI, with a multiwindow burn-rate ladder and a no-traffic alert

- **Status:** Accepted
- **Date:** 2026-09-03
- **Sprint:** 6

## Context

Sprints 3 to 5 produced the signals. This sprint turns them into objectives an
operator can be paged on, and into dashboards that make a rollout legible
without reading YAML.

Three decisions carry it.

## Decision 1: the SLI is time to first token, not request duration

End-to-end request duration is the obvious latency SLI and it is wrong for a
language model, because **it scales with how many tokens the caller asked for**.

A p95 over mixed traffic therefore measures the request *mix* as much as the
server: it moves when a client changes `max_tokens`, and it stays flat through a
genuine regression that coincides with shorter prompts. An SLO built on it burns
budget for reasons the service cannot act on, and stays green through incidents
it should have caught.

TTFT is length-independent — it is how long the user waits before anything
happens. The shim also records observed gaps between successive content-bearing
SSE events so operators can distinguish "slow to start" from "slow while
streaming". Those gaps are not tokenizer-token timing—one event can carry
multiple tokens—so this operator's latency SLI remains TTFT.

Duration is still measured and still graphed, on a panel titled "NOT the SLI",
because it is the right number for capacity planning. It is simply not an
objective.

## Decision 2: the SLO threshold is snapped to a real bucket, loudly

A latency SLI is a ratio over a histogram bucket at an exact `le` label, and
**Prometheus matches `le` as a string**. A rule asking for `le="0.5"` against a
histogram whose nearest boundaries are 0.4 and 0.6 does not approximate and does
not error — it returns an empty vector. The recording rule produces nothing, the
burn-rate alert compares against no data, and the alert never fires.

An SLO that cannot alert is worse than no SLO, because it is believed.

So the threshold is snapped **up** to the nearest real boundary — up, because
snapping down would silently hold a service to a stricter target than its owner
declared — and the snapping is reported in the `MetricsRegistered` condition
rather than applied silently. Quietly changing a number someone wrote in a spec
is the kind of helpfulness that costs an afternoon the first time an SLO does
not mean what its author believes.

Making that possible required moving the bucket definitions out of `cmd/shim`
and into `internal/metrics`, shared between the binary that *produces* the
histogram and the generator that *queries* it. Two definitions of the same
boundaries is precisely the drift this prevents.

## Decision 3: an alert for the case where nothing is wrong

`LLMCPNoTraffic` exists because **the burn-rate alerts cannot fire without
traffic**, and that is arithmetic rather than a gap in them.

With no requests, both SLIs are ratios whose denominators are clamped away from
zero, so both evaluate to a perfect 1.0 and every budget alert stays silent. A
ModelDeployment whose Service selector broke, or whose shim stopped being
scraped, looks flawless on every SLO panel.

This is the third time the same distinction has had to be drawn explicitly in
this codebase, which is what makes it worth an ADR section rather than a
comment:

| Where | "No problems" | "No data" |
|---|---|---|
| Canary analysis | `Pass` | `Inconclusive`, gated by `minRequestRate` |
| Autoscaler | a measured zero | `Valid: false`, freeze the fleet |
| SLO alerts | budget intact | `LLMCPNoTraffic` |

A system that cannot tell them apart reports the second as the first, every
time, and in every case the failure is invisible.

## Other decisions

**The burn-rate ladder is multiwindow.** Each rung requires BOTH a long and a
short window to be burning: the long window establishes the budget is genuinely
being consumed at that rate, the short one establishes that it still is. Without
the short window an alert stays lit for an hour after the incident ends, and
people learn to ignore it. The factors — 14.4× paging, 6× ticketing — come from
the Google SRE workbook's worked example and are chosen so each rung fires after
a defined fraction of the budget is spent.

**The 1d and 3d rungs ship but are gated off.** A demo cluster is minutes old, so
those windows have no data and every alert built on them shows "No Data" — which
looks worse than not having them. Being explicit that the demo's SLO window is
compressed is more credible than shipping rules that cannot evaluate.

**The latency SLI divides by the histogram's own `_count`, not by the request
counter.** TTFT is recorded only for *streamed* responses, so dividing streamed
successes by all requests would report an SLI that falls whenever a client sends
a non-streaming request. Numerator and denominator have to be over the same
population.

**Recording rules keep the reserved colon.** `llmcp:ttft_sli:ratio_rate1h` is a
recording rule and the convention is `level:metric:operation`. That is the same
convention which makes llama.cpp's exported `llamacpp:*` series wrong — those
are scraped metrics masquerading as recording rules, which is why the
ServiceMonitor rewrites them to underscores at scrape time. The symmetry is
worth noticing: one colon belongs, the other does not.

**Operator metrics were pulled forward from Sprint 7.** The canary dashboard was
written against `llmcp_canary_promotions_total`, `_rollbacks_total` and
`llmcp_analysis_verdicts_total`, none of which existed. A dashboard whose panels
reference series nobody emits renders as "No Data" — indistinguishable, on a
rollout dashboard, from "no rollbacks". Shipping the panels without the counters
would have been the exact failure this sprint is about, so the counters moved
forward instead.

Their **gauges are deleted** when a ModelDeployment goes away, and their
**counters are not**. A gauge for a deleted resource keeps reporting a canary
weight for a rollout that ended, holding dashboards red indefinitely; a deleted
counter makes `increase()` over a window spanning the deletion report a
decrease, which Prometheus reads as a counter reset and mis-attributes.

## Consequences

- Two new dashboards, three in total, all `//go:embed`ed and published as
  ConfigMaps the Grafana sidecar discovers by label. Nothing is imported by
  hand.
- `PrometheusRule` is guarded by the same optional-CRD probe as `ServiceMonitor`
  and is probed **separately** from it. They ship together in practice, but a
  cut-down or mid-upgrade prometheus-operator install can genuinely serve one
  and not the other, and assuming otherwise turns into a failed apply rather
  than a clear message.
- The rule's outcome is folded into the `MetricsRegistered` message rather than
  given its own condition. A consumer already reads that condition to learn
  whether a deployment is observable at all; splitting "is it scraped" from "are
  its SLOs defined" would mean two things to check for one question.
- The Grafana Foundation SDK was considered and rejected for all three
  dashboards. Its API churns, and betting three hand-tuned dashboards on it is a
  bad trade for a project whose dashboards are largely static.

## Verification

- `TestEveryDashboardPanelQueriesAnEmittedSeries` walks **every** panel of
  **every** dashboard and fails if it queries a series nothing emits. The
  expected set is assembled from the metric-name constants, so a rename breaks
  the build rather than emptying a panel. Verified to bite by pointing a panel at
  a made-up series.
- `TestEveryAlertReferencesADefinedRecordingRule` applies the same idea to
  alerts: an alert naming an undefined recording rule does not error, it simply
  never fires.
- `TestLatencySLIUsesAnExactBucketBoundary` checks the generated `le` against the
  shim's real bucket list.
- `TestBuildPrometheusRuleIsDeterministic` runs 200 renders and compares bytes,
  because a byte-unstable Server-Side Apply loops against its own watch.
- envtest covers the applied object, its owner reference, apply idempotency, the
  snapped-threshold message, and the disabled and CRD-absent paths.
