/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"strconv"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

// version is stamped at build time with -ldflags "-X main.version=...". It is
// only ever a label on the info gauge.
var version = "dev"

// Histogram buckets come from internal/metrics, shared with the SLO rule
// generator.
//
// They are not defined here, and that is load-bearing rather than tidy. A
// latency SLI is a ratio of a histogram bucket at an exact `le` boundary over
// the total count, and Prometheus matches `le` as an exact string: a rule
// asking for a boundary this histogram does not have returns an empty vector
// rather than an error, so the recording rule produces nothing and the
// burn-rate alert never fires. One definition means the producer and the
// consumer cannot drift apart. See internal/metrics/buckets.go.
var (
	ttftBuckets     = llmcpmetrics.TTFTBuckets
	durationBuckets = llmcpmetrics.DurationBuckets
	tpotBuckets     = llmcpmetrics.TPOTBuckets
)

// metrics holds every collector the shim emits, already curried with the
// model and variant labels that are constant for the lifetime of the process.
//
// Currying is worth the small amount of machinery. The alternative is passing
// model and variant into every observation call, which is exactly the shape of
// code where one call site eventually gets them in the wrong order — and a
// canary labelled `variant="qwen3"` does not fail, it simply never matches the
// analysis query.
type metrics struct {
	registry *prometheus.Registry

	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	ttft        prometheus.Observer
	tpot        prometheus.Observer
	outTokens   prometheus.Counter
	inFlight    prometheus.GaugeFunc
	queueDepth  prometheus.GaugeFunc
	upstreamErr *prometheus.CounterVec

	// concurrency mirrors the engine's --parallel and is the subtrahend in the
	// queue-depth derivation.
	concurrency int64

	// live is the current in-flight count, tracked separately from the gauge
	// because a Prometheus gauge is write-only: queue depth has to be derived
	// from the same number, and reading it back out of the collector is not
	// something the API supports.
	live atomic.Int64
}

// newMetrics builds and registers the shim's collectors.
func newMetrics(cfg config) *metrics {
	reg := prometheus.NewRegistry()

	// The Go and process collectors are registered as well. They are not
	// decoration here: the shim sits in the request path of every inference
	// call, so if it is the thing that got slow, its own GC pauses and CPU time
	// are the evidence — and without them a shim-induced latency regression
	// would be indistinguishable from an engine one, on the very metric the
	// rollout gate reads.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	constLabels := prometheus.Labels{
		llmcpmetrics.LabelModel:   cfg.model,
		llmcpmetrics.LabelVariant: cfg.variant,
	}

	m := &metrics{
		registry:    reg,
		concurrency: int64(cfg.maxConcurrency),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        llmcpmetrics.RequestsTotal,
			Help:        "Inference requests completed, by operation and HTTP status code.",
			ConstLabels: constLabels,
		}, []string{llmcpmetrics.LabelOperation, llmcpmetrics.LabelCode}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        llmcpmetrics.RequestDurationSeconds,
			Help:        "End-to-end inference request duration in seconds.",
			Buckets:     durationBuckets,
			ConstLabels: constLabels,
		}, []string{llmcpmetrics.LabelOperation}),
		outTokens: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        llmcpmetrics.OutputTokensTotal,
			Help:        "Generated tokens observed on streamed responses.",
			ConstLabels: constLabels,
		}),

		upstreamErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        llmcpmetrics.UpstreamErrorsTotal,
			Help:        "Failures reaching or reading from the engine, as opposed to errors it returned.",
			ConstLabels: constLabels,
		}, []string{llmcpmetrics.LabelReason}),
	}

	ttft := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:        llmcpmetrics.TTFTSeconds,
		Help:        "Time to the first generated token, in seconds. Streaming requests only.",
		Buckets:     ttftBuckets,
		ConstLabels: constLabels,
	})
	tpot := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:        llmcpmetrics.TPOTSeconds,
		Help:        "Mean time per output token after the first, in seconds.",
		Buckets:     tpotBuckets,
		ConstLabels: constLabels,
	})
	m.ttft, m.tpot = ttft, tpot

	// PULLED at scrape time, not pushed on every begin/end.
	//
	// The push form — atomic Add, then Gauge.Set of the result — is two
	// independent steps, so two concurrent requests can interleave such that the
	// OLDER count is written last and the gauge stays above the true value until
	// the next request happens to arrive. There is no data race for -race to
	// find, because Gauge.Set is internally atomic; only the pair is not.
	//
	// It is not a cosmetic drift: llmcp_inference_queue_depth is the built-in
	// autoscaler's scaling signal, and a queue depth stuck above zero on an idle
	// deployment holds replicas up indefinitely. Deriving both gauges from the
	// one atomic at collection time makes the lost update impossible.
	m.inFlight = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        llmcpmetrics.RequestsInFlight,
		Help:        "Inference requests currently being proxied.",
		ConstLabels: constLabels,
	}, func() float64 { return float64(m.live.Load()) })
	m.queueDepth = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        llmcpmetrics.QueueDepth,
		Help:        "In-flight requests beyond the engine's concurrency, i.e. waiting for a slot.",
		ConstLabels: constLabels,
	}, func() float64 { return float64(max(m.live.Load()-m.concurrency, 0)) })

	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: llmcpmetrics.ShimInfo,
		Help: "Always 1. Carries shim build and target metadata as labels.",
		ConstLabels: prometheus.Labels{
			llmcpmetrics.LabelModel:   cfg.model,
			llmcpmetrics.LabelVariant: cfg.variant,
			"upstream":                cfg.upstream.String(),
			"version":                 version,
		},
	})
	info.Set(1)

	reg.MustRegister(
		m.requests, m.duration, ttft, tpot,
		m.outTokens, m.inFlight, m.queueDepth, m.upstreamErr, info,
	)

	// Publish the zero values immediately.
	//
	// A counter that has never been incremented does not exist as far as
	// Prometheus is concerned, and a query over a series that does not exist
	// returns an empty vector rather than zero. For canary analysis that is the
	// difference between "the error rate is 0%" and "there is no data" — and
	// those must produce different verdicts, because one is a pass and the
	// other must never be. Initialising the series here means an idle-but-
	// healthy variant reports honest zeroes from the first scrape.
	// inFlight and queueDepth need no priming: a GaugeFunc is evaluated on every
	// scrape, so both report an honest 0 from the first one.
	m.outTokens.Add(0)
	for _, op := range []string{
		llmcpmetrics.OperationChat,
		llmcpmetrics.OperationCompletion,
	} {
		m.requests.WithLabelValues(op, strconv.Itoa(200)).Add(0)
	}

	return m
}

// begin records the start of a proxied request.
func (m *metrics) begin() {
	m.live.Add(1)
}

// end records the completion of a proxied request.
func (m *metrics) end() {
	m.live.Add(-1)
}

// observe records one completed request.
//
// The guards on ttft and tpot are the correctness requirements of the whole
// metric, not defensive coding:
//
//   - TTFT is recorded only when the response was actually a stream AND a
//     content-bearing frame was seen. Recording it for a non-streaming request
//     would enter the request's TOTAL duration into the TTFT histogram, because
//     that is when the single response arrives. A histogram poisoned that way
//     does not look broken — it looks like a latency regression, and the canary
//     gate rolls back a healthy release over it.
//   - TPOT needs at least two tokens: with one there is no interval between
//     tokens to average, and (duration-ttft)/0 is +Inf.
func (m *metrics) observe(st *requestState, code int) {
	m.duration.WithLabelValues(st.operation).Observe(st.duration.Seconds())
	m.requests.WithLabelValues(st.operation, strconv.Itoa(code)).Inc()

	if !st.streamed {
		return
	}

	if st.tokens > 0 {
		m.outTokens.Add(float64(st.tokens))
	}
	if !st.sawFirstToken {
		return
	}

	m.ttft.Observe(st.ttft.Seconds())

	if st.tokens >= 2 {
		decode := st.duration - st.ttft
		if decode > 0 {
			m.tpot.Observe(decode.Seconds() / float64(st.tokens-1))
		}
	}
}

// upstreamError counts a failure to reach or read from the engine.
func (m *metrics) upstreamError(reason string) {
	m.upstreamErr.WithLabelValues(reason).Inc()
}
