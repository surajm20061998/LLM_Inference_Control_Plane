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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// discardLogger keeps test output readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestProxy wires a shim in front of upstream.
func newTestProxy(t *testing.T, upstream string, opts ...func(*config)) (*proxy, *metrics) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parsing upstream: %v", err)
	}
	cfg := config{
		upstream:       u,
		healthPath:     "/health",
		model:          "test-model",
		variant:        "primary",
		maxConcurrency: 2,
	}
	for _, o := range opts {
		o(&cfg)
	}
	m := newMetrics(cfg)
	return newProxy(cfg, m, discardLogger(), time.Now), m
}

// roundTripFunc adapts a function into an http.RoundTripper. Tests that are
// about the proxy's downstream behaviour can provide an exact upstream
// response without introducing a second loopback server and connection pool.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// gather reads one metric family out of the shim's registry.
func gather(t *testing.T, m *metrics, name string) *dto.MetricFamily {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// histogramCount returns the observation count of a single-series histogram.
func histogramCount(t *testing.T, m *metrics, name string) uint64 {
	t.Helper()
	f := gather(t, m, name)
	if f == nil {
		return 0
	}
	var total uint64
	for _, series := range f.GetMetric() {
		total += series.GetHistogram().GetSampleCount()
	}
	return total
}

// histogramSum returns the observation sum of a single-series histogram.
func histogramSum(t *testing.T, m *metrics, name string) float64 {
	t.Helper()
	f := gather(t, m, name)
	if f == nil {
		return 0
	}
	var total float64
	for _, series := range f.GetMetric() {
		total += series.GetHistogram().GetSampleSum()
	}
	return total
}

// counterFor returns the value of a counter series carrying the given labels.
func counterFor(t *testing.T, m *metrics, name string, want prometheus.Labels) float64 {
	t.Helper()
	f := gather(t, m, name)
	if f == nil {
		return 0
	}
	var total float64
series:
	for _, series := range f.GetMetric() {
		labels := map[string]string{}
		for _, l := range series.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		for k, v := range want {
			if labels[k] != v {
				continue series
			}
		}
		total += series.GetCounter().GetValue()
	}
	return total
}

// streamingEngine is an upstream that emits a role frame, waits ttft, then
// emits tokens spaced by itl. It is the minimum shape needed to tell a correct
// TTFT measurement from a buffered one.
func streamingEngine(ttft, itl time.Duration, tokens int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)

		write := func(s string) bool {
			if _, err := io.WriteString(w, s); err != nil {
				return false
			}
			return rc.Flush() == nil
		}

		// The role frame goes out immediately, exactly as llama.cpp does. A
		// shim that timed this would report a TTFT of nearly zero.
		if !write(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n") {
			return
		}

		select {
		case <-time.After(ttft):
		case <-r.Context().Done():
			return
		}

		for i := range tokens {
			if i > 0 {
				select {
				case <-time.After(itl):
				case <-r.Context().Done():
					return
				}
			}
			if !write(fmt.Sprintf(`data: {"choices":[{"delta":{"content":"t%d"}}]}`+"\n\n", i)) {
				return
			}
		}
		write("data: [DONE]\n\n")
	})
}

func TestProxyRecordsTTFTSeparatelyFromDuration(t *testing.T) {
	const (
		ttft   = 150 * time.Millisecond
		itl    = 20 * time.Millisecond
		tokens = 8
	)
	engine := httptest.NewServer(streamingEngine(ttft, itl, tokens))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}

	if got := histogramCount(t, m, llmcpmetrics.TTFTSeconds); got != 1 {
		t.Fatalf("ttft observations = %d, want 1", got)
	}

	gotTTFT := histogramSum(t, m, llmcpmetrics.TTFTSeconds)
	gotDuration := histogramSum(t, m, llmcpmetrics.RequestDurationSeconds)

	// The measurement must be near the injected TTFT and clearly below the
	// total. The generous lower bound tolerates scheduler jitter; the upper
	// bound is what actually catches the bug this test exists for.
	if gotTTFT < ttft.Seconds()*0.5 {
		t.Fatalf("ttft = %.3fs, below the injected %v — the role frame was probably timed", gotTTFT, ttft)
	}
	if gotTTFT > ttft.Seconds()*2.5 {
		t.Fatalf("ttft = %.3fs, far above the injected %v", gotTTFT, ttft)
	}

	// The exit criterion of the whole sprint, asserted in a unit test: TTFT
	// must be a genuine fraction of the duration. If the stream were buffered
	// the two would be equal.
	if gotTTFT > 0.75*gotDuration {
		t.Fatalf("ttft %.3fs is not meaningfully below duration %.3fs; the stream is being buffered",
			gotTTFT, gotDuration)
	}

	if got := counterFor(t, m, llmcpmetrics.OutputTokensTotal, nil); got != tokens {
		t.Fatalf("output tokens = %v, want %d", got, tokens)
	}
	if got := histogramCount(t, m, llmcpmetrics.TPOTSeconds); got != 1 {
		t.Fatalf("tpot observations = %d, want 1", got)
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	// The anti-buffering assertion from the client's side: the first frame must
	// arrive long before the last. A proxy without FlushInterval: -1 delivers
	// the whole body at once and this fails. The upstream is an io.Pipe rather
	// than a second httptest.Server: this test is about downstream flushing, and
	// an extra loopback connection only adds an unrelated source of transient
	// EOFs on loaded CI runners.
	const (
		ttft   = 50 * time.Millisecond
		itl    = 60 * time.Millisecond
		tokens = 5
	)
	p, _ := newTestProxy(t, "http://engine.test")
	p.rp.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			defer func() { _ = writer.Close() }()

			write := func(frame string) bool {
				_, err := io.WriteString(writer, frame)
				return err == nil
			}
			if !write(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n") {
				return
			}
			time.Sleep(ttft)
			for i := range tokens {
				if i > 0 {
					time.Sleep(itl)
				}
				if !write(fmt.Sprintf(`data: {"choices":[{"delta":{"content":"t%d"}}]}`+"\n\n", i)) {
					return
				}
			}
			_ = write("data: [DONE]\n\n")
		}()

		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {contentTypeSSE}},
			Body:          reader,
			ContentLength: -1,
			Request:       r,
		}, nil
	})
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	start := time.Now()
	resp, err := shim.Client().Post(
		shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var firstToken, lastToken time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"content"`)) {
			continue
		}
		if firstToken == 0 {
			firstToken = time.Since(start)
		}
		lastToken = time.Since(start)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}

	if firstToken == 0 {
		t.Fatal("no content frame reached the client")
	}
	// Four inter-token gaps separate the first content frame from the last.
	minSpread := 3 * itl
	if lastToken-firstToken < minSpread {
		t.Fatalf("first token at %v, last at %v: spread %v is under %v — the response was buffered",
			firstToken, lastToken, lastToken-firstToken, minSpread)
	}
}

func TestProxyNeverRecordsTTFTForNonStreaming(t *testing.T) {
	// The single most damaging way to get this wrong. A non-streaming response
	// delivers its first and last byte together, so recording TTFT for it
	// enters the full duration into the TTFT histogram — a fabricated latency
	// regression that a canary gate would roll back on.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := histogramCount(t, m, llmcpmetrics.TTFTSeconds); got != 0 {
		t.Fatalf("ttft observations = %d, want 0 for a non-streaming response", got)
	}
	if got := histogramCount(t, m, llmcpmetrics.TPOTSeconds); got != 0 {
		t.Fatalf("tpot observations = %d, want 0", got)
	}
	if got := histogramCount(t, m, llmcpmetrics.RequestDurationSeconds); got != 1 {
		t.Fatalf("duration observations = %d, want 1", got)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsTotal, prometheus.Labels{
		llmcpmetrics.LabelOperation: llmcpmetrics.OperationChat,
		llmcpmetrics.LabelCode:      "200",
	}); got != 1 {
		t.Fatalf("requests_total{code=200} = %v, want 1", got)
	}
}

func TestProxyCountsEngineErrorsByStatus(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()

	if got := counterFor(t, m, llmcpmetrics.RequestsTotal, prometheus.Labels{
		llmcpmetrics.LabelCode: "500",
	}); got != 1 {
		t.Fatalf("requests_total{code=500} = %v, want 1", got)
	}
	// An error the engine chose to return is NOT an upstream error. Conflating
	// them makes "the engine is down" indistinguishable from "the engine said
	// no", which are different incidents with different responses.
	if got := counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, nil); got != 0 {
		t.Fatalf("upstream errors = %v, want 0", got)
	}
}

func TestProxyCountsUnreachableUpstream(t *testing.T) {
	// A port nothing is listening on.
	p, m := newTestProxy(t, "http://127.0.0.1:1")
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
		llmcpmetrics.LabelReason: llmcpmetrics.ReasonUnreachable,
	}); got != 1 {
		t.Fatalf("upstream unreachable = %v, want 1", got)
	}
}

func TestProxyHealthMirrorsEngineStatus(t *testing.T) {
	// llama.cpp returns 503 with this exact body while the model loads, and the
	// readiness probe depends on that passing through untranslated.
	loaded := false
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("health check hit %q, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if !loaded {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer engine.Close()

	p, _ := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(shim.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get(naming.ShimHealthPath); code != http.StatusServiceUnavailable {
		t.Fatalf("healthz while loading = %d (%s), want 503", code, body)
	}

	// livez must answer 200 even while the engine is unavailable — it is what
	// the startup probe targets, and as a native sidecar the engine container
	// has not even been started until it passes.
	if code, _ := get(naming.ShimLivePath); code != http.StatusOK {
		t.Fatalf("livez while the engine is loading = %d, want 200", code)
	}

	loaded = true
	if code, body := get(naming.ShimHealthPath); code != http.StatusOK {
		t.Fatalf("healthz once loaded = %d (%s), want 200", code, body)
	}
}

func TestProxyLiveWorksWithNoEngineAtAll(t *testing.T) {
	// The deadlock case in its purest form: nothing is listening upstream.
	p, _ := newTestProxy(t, "http://127.0.0.1:1")
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Get(shim.URL + naming.ShimLivePath)
	if err != nil {
		t.Fatalf("GET livez: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("livez = %d, want 200 with no engine present", resp.StatusCode)
	}
}

func TestProxyQueueDepthTracksConcurrency(t *testing.T) {
	// maxConcurrency is 1 here, so the second concurrent request is queued by
	// definition and the gauge must say so.
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL, func(c *config) { c.maxConcurrency = 1 })
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			resp, err := http.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- struct{}{}
		}()
	}

	// Wait for both to be in flight.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.live.Load() < 2 {
		time.Sleep(2 * time.Millisecond)
	}

	if got := gaugeValue(t, m, llmcpmetrics.RequestsInFlight); got != 2 {
		t.Fatalf("in_flight = %v, want 2", got)
	}
	if got := gaugeValue(t, m, llmcpmetrics.QueueDepth); got != 1 {
		t.Fatalf("queue_depth = %v, want 1 (2 in flight, concurrency 1)", got)
	}

	close(release)
	for range 2 {
		<-done
	}

	if got := gaugeValue(t, m, llmcpmetrics.QueueDepth); got != 0 {
		t.Fatalf("queue_depth after drain = %v, want 0", got)
	}
}

// gaugeValue reads a single-series gauge.
func gaugeValue(t *testing.T, m *metrics, name string) float64 {
	t.Helper()
	f := gather(t, m, name)
	if f == nil {
		t.Fatalf("metric %q not present", name)
	}
	return f.GetMetric()[0].GetGauge().GetValue()
}

func TestProxyPublishesZeroSeriesBeforeAnyTraffic(t *testing.T) {
	// A counter that has never been incremented does not exist, and a PromQL
	// query over a nonexistent series returns an empty vector — which analysis
	// must read as "no data", not as "zero errors". Pre-registering the healthy
	// series means an idle variant reports honest zeroes instead.
	p, m := newTestProxy(t, "http://127.0.0.1:1")
	_ = p

	if gather(t, m, llmcpmetrics.RequestsTotal) == nil {
		t.Fatal("requests_total absent before any traffic")
	}
	if gather(t, m, llmcpmetrics.QueueDepth) == nil {
		t.Fatal("queue_depth absent before any traffic")
	}
	if gather(t, m, llmcpmetrics.ShimInfo) == nil {
		t.Fatal("shim_info absent")
	}
}

func TestProxyPassesUnknownRoutesThrough(t *testing.T) {
	// A metrics sidecar that only forwarded the endpoints it knew about would
	// break the day an engine adds one.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hit:"+r.URL.Path)
	}))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Get(shim.URL + "/v1/some/future/endpoint")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if string(body) != "hit:/v1/some/future/endpoint" {
		t.Fatalf("body = %q; the route was not proxied verbatim", body)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsTotal, prometheus.Labels{
		llmcpmetrics.LabelOperation: llmcpmetrics.OperationOther,
	}); got != 1 {
		t.Fatalf("requests_total{operation=other} = %v, want 1", got)
	}
}

func TestProxyClientCancelIsNotAnEngineError(t *testing.T) {
	// A client that hangs up mid-generation must not be recorded as an engine
	// failure: on the error-rate gate that would roll back a canary because a
	// load generator timed out.
	engine := httptest.NewServer(streamingEngine(10*time.Millisecond, 200*time.Millisecond, 20))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		shim.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	// Read one frame, then abandon the stream.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
			llmcpmetrics.LabelReason: llmcpmetrics.ReasonClientCanceled,
		}) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Both halves, in this order. Asserting only that stream_aborted is zero
	// passes trivially when NOTHING was recorded — which is precisely the bug
	// this test was meant to be sensitive to.
	if got := counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
		llmcpmetrics.LabelReason: llmcpmetrics.ReasonClientCanceled,
	}); got != 1 {
		t.Fatalf("client_canceled = %v, want 1: the cancel must be recorded, not dropped", got)
	}
	if got := counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
		llmcpmetrics.LabelReason: llmcpmetrics.ReasonStreamAborted,
	}); got != 0 {
		t.Fatalf("stream_aborted = %v, want 0: a client cancel is not an engine failure", got)
	}
}

// diesMidStream is an upstream that starts a well-formed SSE response, emits a
// few content frames, then drops the TCP connection without ever sending
// `data: [DONE]`. It is the shape of an engine that OOMs or segfaults during
// generation, which is the failure canary analysis exists to catch.
func diesMidStream(frames int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n")
		for i := range frames {
			frame := fmt.Sprintf("data: %s\n\n",
				`{"choices":[{"delta":{"content":"tok"}}]}`)
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(frame), frame)
			_ = buf.Flush()
			_ = i
		}
		// RST rather than FIN, so the shim sees a read error on a chunked body
		// that never terminated — not a clean EOF.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	})
}

func TestProxyRecordsAnAbortedStream(t *testing.T) {
	// httputil.ReverseProxy does not RETURN when the response body copy fails —
	// it panics with http.ErrAbortHandler (reverseproxy.go, "abort the
	// request"). Anything recorded after ServeHTTP is therefore skipped for
	// exactly the requests that matter most.
	//
	// The consequence is not a missing counter, it is a canary gate that cannot
	// see the failure: an engine dying on every streamed request emits no
	// upstream errors and no requests_total, so the availability SLI's
	// numerator and denominator both stay flat and the ratio stays perfect.
	engine := httptest.NewServer(diesMidStream(3))
	defer engine.Close()

	p, m := newTestProxy(t, engine.URL)
	shim := httptest.NewServer(p.handler())
	defer shim.Close()

	resp, err := http.Post(shim.URL+"/v1/chat/completions",
		"application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
			llmcpmetrics.LabelReason: llmcpmetrics.ReasonStreamAborted,
		}) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := counterFor(t, m, llmcpmetrics.UpstreamErrorsTotal, prometheus.Labels{
		llmcpmetrics.LabelReason: llmcpmetrics.ReasonStreamAborted,
	}); got != 1 {
		t.Errorf("upstream_errors_total{reason=stream_aborted} = %v, want 1", got)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsTotal, prometheus.Labels{
		llmcpmetrics.LabelOperation: llmcpmetrics.OperationChat,
		llmcpmetrics.LabelCode:      "200",
	}); got != 1 {
		t.Errorf("requests_total{chat,200} = %v, want 1: an aborted stream is still a request", got)
	}
	if got := gaugeValue(t, m, llmcpmetrics.RequestsInFlight); got != 0 {
		t.Errorf("requests_in_flight = %v, want 0 after the request unwound", got)
	}
}
