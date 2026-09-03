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
	"math"
	"strings"
	"testing"
	"time"
)

// llamaCPPOpeningFrames is a verbatim capture from
// ghcr.io/ggml-org/llama.cpp:server-b10731 serving Qwen3-0.6B-Q4_K_M, taken on
// 2026-09-02. The detail that matters is the FIRST frame: it carries
// "content": null and exists only to announce the assistant role.
//
// This is the fixture that pins down the definition of TTFT. Timing the first
// frame — or the first response byte, which arrives even earlier — would report
// how quickly llama.cpp acknowledged the request. That number is roughly
// constant regardless of how loaded the engine is, so a canary analysis built
// on it would compare two identical values and conclude that a badly degraded
// revision was healthy.
// The frames are assembled by concatenation rather than written as one raw
// string so that no source line exceeds the line-length limit. The bytes are
// unchanged from the capture.
const llamaCPPOpeningFrames = "" +
	`data: {"choices":[{"finish_reason":null,"index":0,` +
	`"delta":{"role":"assistant","content":null}}],` +
	`"created":1788392036,"id":"chatcmpl-x","model":"qwen3-0.6b",` +
	`"object":"chat.completion.chunk"}` + "\n\n" +
	`data: {"choices":[{"finish_reason":null,"index":0,` +
	`"delta":{"content":"Hello"}}],` +
	`"created":1788392036,"id":"chatcmpl-x","model":"qwen3-0.6b",` +
	`"object":"chat.completion.chunk"}` + "\n\n" +
	`data: {"choices":[{"finish_reason":null,"index":0,` +
	`"delta":{"content":"!"}}],` +
	`"created":1788392036,"id":"chatcmpl-x","model":"qwen3-0.6b",` +
	`"object":"chat.completion.chunk"}` + "\n\n" +
	"data: [DONE]\n\n"

func TestReadStreamCountsOnlyContentBearingFrames(t *testing.T) {
	t.Parallel()

	_, tokens, err := readStream(strings.NewReader(llamaCPPOpeningFrames), time.Now())
	if err != nil {
		t.Fatalf("readStream() error = %v", err)
	}
	// Two, not three: the role-only opening frame carries no generated text.
	if tokens != 2 {
		t.Errorf("tokens = %d, want 2 (the role-only opening frame must not count)", tokens)
	}
}

func TestReadStreamTTFTSkipsTheRoleFrame(t *testing.T) {
	t.Parallel()

	// A start time well in the past, so any TTFT is comfortably positive and
	// the assertion is about which frame was timed rather than about clock
	// resolution.
	start := time.Now().Add(-time.Second)

	ttft, tokens, err := readStream(strings.NewReader(llamaCPPOpeningFrames), start)
	if err != nil {
		t.Fatalf("readStream() error = %v", err)
	}
	if tokens == 0 {
		t.Fatal("no tokens read")
	}
	if ttft < time.Second {
		t.Errorf("ttft = %v, want >= 1s; it was measured from the wrong frame", ttft)
	}
}

func TestReadStreamCountsReasoningContent(t *testing.T) {
	t.Parallel()

	// A reasoning model streams its thinking into reasoning_content before any
	// user-visible content appears. Those are generated tokens: the engine did
	// the work and the client received it. Ignoring them would report a TTFT
	// inflated by the entire length of the thinking block, which is a property
	// of the prompt rather than of the engine's health.
	const body = `data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me think"}}]}

data: {"choices":[{"index":0,"delta":{"content":"Answer"}}]}

data: [DONE]
`
	_, tokens, err := readStream(strings.NewReader(body), time.Now())
	if err != nil {
		t.Fatalf("readStream() error = %v", err)
	}
	if tokens != 2 {
		t.Errorf("tokens = %d, want 2; reasoning_content is generated output", tokens)
	}
}

func TestReadStreamHandlesCompletionsEndpoint(t *testing.T) {
	t.Parallel()

	// /v1/completions puts text at choices[].text rather than in a delta.
	const body = `data: {"choices":[{"index":0,"text":"once"}]}

data: {"choices":[{"index":0,"text":" upon"}]}

data: [DONE]
`
	_, tokens, err := readStream(strings.NewReader(body), time.Now())
	if err != nil {
		t.Fatalf("readStream() error = %v", err)
	}
	if tokens != 2 {
		t.Errorf("tokens = %d, want 2", tokens)
	}
}

func TestReadStreamIgnoresUnparseableFrames(t *testing.T) {
	t.Parallel()

	// Upstream adds fields over time, and a frame this tool cannot parse is not
	// a reason to discard an otherwise good run.
	const body = `data: {"this is not": json

data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}

data: [DONE]
`
	_, tokens, err := readStream(strings.NewReader(body), time.Now())
	if err != nil {
		t.Fatalf("readStream() error = %v", err)
	}
	if tokens != 1 {
		t.Errorf("tokens = %d, want 1", tokens)
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	t.Parallel()

	// Nearest rank, matching Prometheus's histogram_quantile, which does not
	// interpolate between observations either. These numbers get compared
	// against PromQL output, so the two must agree on what "p95" means.
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	tests := []struct {
		p    float64
		want float64
	}{
		{0.50, 5},
		{0.95, 10},
		{0.99, 10},
		{1.00, 10},
	}
	for _, tc := range tests {
		if got := percentile(sorted, tc.p); got != tc.want {
			t.Errorf("percentile(p=%v) = %v, want %v", tc.p, got, tc.want)
		}
	}

	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

func TestCoefficientOfVariationDropsEdgeWindows(t *testing.T) {
	t.Parallel()

	// The first window contains ramp-up and the last is truncated by the
	// deadline. Both are wildly unrepresentative, and including either would
	// report startup and shutdown as if they were steady-state noise — which
	// would make a perfectly stable system look far too noisy to set a
	// threshold against.
	ws := []Window{
		{TTFTp95: 100}, // ramp-up, must be excluded
		{TTFTp95: 10},
		{TTFTp95: 10},
		{TTFTp95: 10},
		{TTFTp95: 0.001}, // truncated tail, must be excluded
	}

	if got := coefficientOfVariation(ws); got != 0 {
		t.Errorf("CV = %v, want 0 for a perfectly stable interior", got)
	}
}

func TestCoefficientOfVariationMeasuresSpread(t *testing.T) {
	t.Parallel()

	// Interior values 8, 10, 12: mean 10, sample stddev 2, so CV = 0.2.
	ws := []Window{
		{TTFTp95: 999},
		{TTFTp95: 8},
		{TTFTp95: 10},
		{TTFTp95: 12},
		{TTFTp95: 999},
	}

	got := coefficientOfVariation(ws)
	if math.Abs(got-0.2) > 1e-9 {
		t.Errorf("CV = %v, want 0.2", got)
	}
}

func TestCoefficientOfVariationNeedsEnoughWindows(t *testing.T) {
	t.Parallel()

	// Too few windows to say anything. Returning a confident number from two
	// data points would be worse than returning none, because it would be
	// compared against a kill threshold.
	if got := coefficientOfVariation([]Window{{TTFTp95: 1}, {TTFTp95: 50}}); got != 0 {
		t.Errorf("CV = %v, want 0 when there are too few windows", got)
	}
}

func TestSummarizeExcludesFailuresFromPercentiles(t *testing.T) {
	t.Parallel()

	// A request that failed fast must not be recorded as a fast request. If it
	// were, an engine returning instant 503s would show excellent latency —
	// which is precisely the failure mode canary analysis exists to catch.
	samples := []sample{
		{ttft: 100 * time.Millisecond, duration: time.Second, tokens: 10, ok: true},
		{ttft: 0, duration: time.Millisecond, ok: false},
		{ttft: 0, duration: time.Millisecond, ok: false},
	}

	st := summarize(samples, 10*time.Second)

	if st.Requests != 3 {
		t.Errorf("requests = %d, want 3", st.Requests)
	}
	if st.Errors != 2 {
		t.Errorf("errors = %d, want 2", st.Errors)
	}
	if st.TTFTp50 != 0.1 {
		t.Errorf("ttft p50 = %v, want 0.1; failures leaked into the percentiles", st.TTFTp50)
	}
	// RPS counts successful requests only, for the same reason.
	if st.RPS != 0.1 {
		t.Errorf("rps = %v, want 0.1", st.RPS)
	}
}
