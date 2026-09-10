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
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

const (
	// contentTypeSSE identifies a streamed response.
	contentTypeSSE = "text/event-stream"
	// contentTypeJSON identifies a non-streaming OpenAI-compatible response.
	contentTypeJSON = "application/json"
)

// stateKeyType is the private context key type for per-request state.
type stateKeyType struct{}

var stateKey stateKeyType

// requestState is what one proxied request accumulated.
//
// It is carried through the request CONTEXT rather than through a field on the
// handler, because httputil.ReverseProxy splits the work across three
// callbacks — Rewrite, ModifyResponse and ErrorHandler — that share nothing but
// the request. The context is the only channel between them.
type requestState struct {
	start     time.Time
	operation string

	// streamed is true when the engine answered with an SSE body. It is read
	// off the RESPONSE content type rather than off the request's "stream":true
	// field, which is a deliberate choice: the response is the ground truth
	// about what actually happened, it needs no buffering or replaying of the
	// request body, and it is correct even for an engine that declines to
	// stream a request that asked to.
	streamed bool

	sawFirstToken  bool
	ttft           time.Duration
	tokens         int
	sawDone        bool
	lastContent    time.Time
	interChunk     func(float64)
	reportedTokens int64
	hasUsage       bool

	duration time.Duration

	// readErr is the first non-EOF error seen while reading the upstream body.
	// It is what distinguishes a stream that ended from one that broke.
	readErr error
}

// proxy is the shim's request handler.
type proxy struct {
	cfg     config
	metrics *metrics
	logger  *slog.Logger
	rp      *httputil.ReverseProxy

	// now is injected so that tests can drive time without sleeping.
	now func() time.Time
}

// newProxy builds the reverse proxy.
func newProxy(cfg config, m *metrics, logger *slog.Logger, now func() time.Time) *proxy {
	p := &proxy{cfg: cfg, metrics: m, logger: logger, now: now}

	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.upstream)
			// Preserve the client's Host header rather than rewriting it to the
			// upstream's. SetURL would otherwise leave the outbound Host as
			// 127.0.0.1, which some engines echo back in error bodies and links.
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},

		// FlushInterval: -1 flushes the response to the client after EVERY
		// write from the upstream.
		//
		// This single field is the difference between a working shim and one
		// that silently destroys the metric it exists to produce. The default
		// is 0, which means "copy with no periodic flush" — Go's bufio then
		// holds SSE frames until its buffer fills or the response ends, the
		// client receives the entire generation in one burst at the end, and
		// every measured TTFT equals the total duration. Nothing errors. The
		// histogram simply becomes a duplicate of the duration histogram, and
		// the canary gate is comparing a metric that no longer means what its
		// name says.
		//
		// ReverseProxy special-cases text/event-stream to flush immediately
		// anyway, but relying on that would make correctness depend on an
		// upstream setting a header exactly right. -1 makes it unconditional.
		FlushInterval: -1,

		Transport:      newTransport(),
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.handleUpstreamError,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	return p
}

// newTransport builds the HTTP transport used to reach the engine.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()

	// DisableCompression is the second half of the anti-buffering requirement.
	//
	// Left enabled, Go's transport adds `Accept-Encoding: gzip` and transparently
	// decompresses the response. gzip is a BLOCK compressor: it emits nothing
	// until it has accumulated a block, so an engine that honours the header
	// turns a token-by-token stream into a chunk-by-chunk one and the first
	// token arrives whenever the first block fills. That is the same failure as
	// a missing flush, arriving from the other end of the connection, and it is
	// harder to spot because the shim's own code looks correct.
	//
	// Compression buys nothing here regardless: the hop is localhost inside a
	// single pod.
	t.DisableCompression = true

	// The engine is a loopback address inside the same pod, so the connection
	// pool only ever needs to serve one host. Sizing it to the concurrency the
	// engine can actually deliver avoids the transport becoming the queue —
	// which would move waiting time out of the engine's measurable latency and
	// into an invisible pool wait.
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 64
	t.MaxConnsPerHost = 0

	// No ResponseHeaderTimeout and no overall timeout: a long generation is
	// normal and must not be cut off by the proxy. Dial and TLS handshake
	// timeouts stay, because those failures are genuinely fast or never.
	t.ExpectContinueTimeout = 1 * time.Second
	t.IdleConnTimeout = 90 * time.Second

	return t
}

// handler returns the shim's full routing table.
//
// The two llmcp paths are matched before the catch-all, so they are answered by
// the shim and never forwarded. Everything else — the entire OpenAI surface,
// including routes this shim has never heard of — is proxied verbatim. A
// metrics sidecar that only knew about the endpoints it was written against
// would quietly break the day an engine adds one.
func (p *proxy) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(naming.ShimLivePath, p.handleLive)
	mux.HandleFunc(naming.ShimHealthPath, p.handleHealth)
	mux.Handle("/", http.HandlerFunc(p.serveProxy))
	return mux
}

// serveProxy proxies one request and records what it observed.
func (p *proxy) serveProxy(w http.ResponseWriter, r *http.Request) {
	st := &requestState{
		start:     p.now(),
		operation: llmcpmetrics.NormalizeOperation(r.URL.Path),
	}
	if r.Method == http.MethodPost {
		switch st.operation {
		case llmcpmetrics.OperationChat, llmcpmetrics.OperationCompletion, llmcpmetrics.OperationEmbeddings:
			p.metrics.requestsStarted.WithLabelValues(st.operation).Inc()
		}
	}

	p.metrics.begin()
	defer p.metrics.end()

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	// DEFERRED, not sequential. httputil.ReverseProxy does not return when the
	// response body copy fails: it panics with http.ErrAbortHandler
	// ("Since we're streaming the response, if we run into an error all we can
	// do is abort the request"). That covers both halves of the case this shim
	// exists to measure — an engine that dies mid-generation, and a client that
	// hangs up — so recording after ServeHTTP records everything EXCEPT the
	// failures.
	//
	// The symptom is not a missing counter, it is a canary gate that cannot
	// see the outage. An engine failing 100% of streamed requests would emit no
	// upstream errors and no requests_total at all, leaving the availability
	// SLI's numerator and denominator both flat and the ratio at a perfect 1.0
	// while nothing works.
	defer func() {
		st.duration = p.now().Sub(st.start)
		p.metrics.observe(st, rec.status)
		p.countStreamOutcome(r, st)

		if p.logger.Enabled(r.Context(), slog.LevelDebug) {
			p.logger.Debug("proxied",
				"operation", st.operation, "code", rec.status,
				"streamed", st.streamed, "tokens", st.tokens,
				"ttftMS", st.ttft.Milliseconds(), "durationMS", st.duration.Milliseconds())
		}
	}()

	p.rp.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), stateKey, st)))
}

// countStreamOutcome attributes a stream that did not finish cleanly.
//
// A client that hangs up mid-generation is counted separately from an engine
// that stopped sending. Folding them together is a real hazard for this
// project: a load generator with a request timeout would inflate the upstream
// error rate of whichever variant happened to be slower, which is exactly the
// variant the canary gate is already suspicious of. The gate would then roll
// back on the client's behaviour rather than the server's.
func (p *proxy) countStreamOutcome(r *http.Request, st *requestState) {
	if !st.streamed || st.sawDone || st.readErr == nil {
		return
	}
	if errors.Is(st.readErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		p.metrics.upstreamError(llmcpmetrics.ReasonClientCanceled)
		return
	}
	p.metrics.upstreamError(llmcpmetrics.ReasonStreamAborted)
}

// modifyResponse wraps a streamed body so tokens can be timed as they pass.
func (p *proxy) modifyResponse(resp *http.Response) error {
	st, ok := resp.Request.Context().Value(stateKey).(*requestState)
	if !ok {
		return nil
	}
	if !isEventStream(resp.Header.Get("Content-Type")) {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType == contentTypeJSON {
			resp.Body = &usageObserver{upstream: resp.Body, state: st}
		}
		return nil
	}
	st.streamed = true
	st.interChunk = p.metrics.interChunk.Observe
	resp.Body = newStreamObserver(resp.Body, st, p.now)
	return nil
}

// isEventStream reports whether a Content-Type header names an SSE body.
//
// Parsed rather than compared, because the header legitimately carries
// parameters ("text/event-stream; charset=utf-8") and a string equality check
// would classify those as non-streaming — which would suppress TTFT entirely
// for any engine that spells the header that way.
func isEventStream(header string) bool {
	if header == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(header), contentTypeSSE)
	}
	return strings.EqualFold(mediaType, contentTypeSSE)
}

// handleUpstreamError answers a request the engine could not serve.
//
// It returns 502 rather than propagating a bare connection error, and — the
// part that matters for analysis — it counts the failure as an UPSTREAM error,
// separately from a 5xx the engine chose to return. The two have different
// causes and different remedies, and a rollout gate that cannot tell "the
// engine is down" from "the engine rejected the prompt" is diagnosing the wrong
// thing half the time.
func (p *proxy) handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		p.metrics.upstreamError(llmcpmetrics.ReasonClientCanceled)
		// The client is gone. Writing a status now would either be discarded or
		// race with the closing connection; 499 is recorded by the recorder as
		// the effective outcome instead.
		w.WriteHeader(499)
		return
	}

	p.metrics.upstreamError(llmcpmetrics.ReasonUnreachable)
	p.logger.Warn("upstream request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "upstream engine unavailable", http.StatusBadGateway)
}

// handleLive reports that the shim's own listener is up.
//
// It deliberately does NOT touch the engine. This is what the shim's startup
// probe targets, and as a native sidecar the shim must pass that probe before
// the kubelet will start the engine container at all — so a startup probe that
// depended on the engine would wait forever for a process that is waiting for
// it. See naming.ShimLivePath.
func (p *proxy) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleHealth proxies the engine's readiness endpoint.
//
// The engine's status code is mirrored EXACTLY, including llama.cpp's 503 while
// the model is loading. That 503 is correct and useful — it is what keeps a pod
// out of the Service's endpoints until the model is resident — and translating
// it into anything else would either make a loading pod look healthy or make it
// look broken.
//
// Serving this from the shim rather than probing the engine directly closes a
// real gap: with a direct probe there is a window in which the engine is up,
// the pod is Ready, traffic arrives, and the shim is not listening yet.
func (p *proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultUpstreamTimeout)
	defer cancel()

	target := *p.cfg.upstream
	target.Path = p.cfg.healthPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "building upstream health request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := p.rp.Transport.RoundTrip(req)
	if err != nil {
		// Not counted as an upstream inference error: a failing health check
		// during model load is the expected path, and counting it would put a
		// permanent floor under every variant's error rate.
		http.Error(w, "engine unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeader(w.Header(), resp.Header)
	// The body below is TRUNCATED at 64 KiB, so the upstream's Content-Length
	// may not describe what is actually written. Forwarding it would promise a
	// length the shim then fails to deliver, and net/http answers a short write
	// by killing the connection — turning an oversized health body into a probe
	// that fails for a reason unrelated to the engine's health. Dropping it lets
	// the response be chunked or length-computed from what is really sent.
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	// A health body is small and bounded; the cap is only there so a
	// misconfigured -upstream pointing at something enormous cannot be used to
	// exhaust the shim's memory through its probe endpoint.
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 64*1024))
}

// copyHeader copies response headers, skipping hop-by-hop ones.
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// hopByHopHeaders must not be forwarded between connections (RFC 9110 §7.6.1).
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func isHopByHop(name string) bool {
	_, ok := hopByHopHeaders[strings.ToLower(name)]
	return ok
}

// statusRecorder captures the response status while staying transparent.
//
// It implements Unwrap so that http.ResponseController — which is what
// httputil.ReverseProxy uses to flush — reaches the real ResponseWriter. That
// is not optional here: without it the proxy cannot flush, and the very
// buffering that FlushInterval: -1 exists to prevent comes back through the
// wrapper meant to observe it.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
