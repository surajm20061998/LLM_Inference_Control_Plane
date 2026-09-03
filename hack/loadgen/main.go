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

// Command loadgen drives streaming load at an OpenAI-compatible endpoint and
// measures time-to-first-token from the client's side of the wire.
//
// # Why this exists rather than a generic HTTP benchmark
//
// oha, wrk and hey all measure time-to-LAST-byte. For a streaming LLM endpoint
// that number is dominated by how many tokens were generated, which says
// nothing about responsiveness and everything about the value of max_tokens.
// The signal that actually matters — and the one this project's canary analysis
// will gate promotions on — is time to the FIRST generated token. Measuring it
// requires parsing the SSE stream, which a generic benchmark does not do.
//
// # Why a client-side measurement is worth keeping after the shim exists
//
// A later sprint puts a reverse-proxy sidecar in front of every engine and has
// it export TTFT as a Prometheus histogram. That measurement can be silently
// wrong in one specific way: if anything buffers the response — Go's
// httputil.ReverseProxy does by default — then every recorded TTFT equals the
// total request duration, the histogram looks plausible, and every latency
// comparison built on it is meaningless. This tool measures the same quantity
// from outside the cluster with no proxy in the path, which is what makes it
// possible to check the shim against something independent rather than against
// itself.
//
// # Keep-alive
//
// -keepalive=false is not a micro-optimisation knob. kube-proxy load-balances
// per CONNECTION, not per request, so N concurrent clients holding N keep-alive
// connections pin themselves to whichever pods they first reached and stay
// there. Any traffic split measured under keep-alive is therefore an artifact
// of connection placement rather than of the weighting under test. Running the
// same load both ways is how you find out how large that distortion is.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

func main() {
	cfg := parseFlags()

	res, err := run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: encoding result: %v\n", err)
		os.Exit(1)
	}

	// A run in which every request failed still exits non-zero, so a script
	// that forgets to check the JSON does not silently report on nothing.
	if res.Overall.Requests == 0 || res.Overall.Errors == res.Overall.Requests {
		os.Exit(2)
	}
}

// config is the whole knob surface, gathered so that it can be echoed back into
// the result. A measurement whose parameters are not recorded next to it is not
// reproducible, and these runs end up pasted into an ADR.
type config struct {
	URL         string        `json:"url"`
	Model       string        `json:"model"`
	Concurrency int           `json:"concurrency"`
	Duration    time.Duration `json:"-"`
	DurationS   float64       `json:"duration_s"`
	MaxTokens   int           `json:"max_tokens"`
	Prompt      string        `json:"prompt"`
	KeepAlive   bool          `json:"keep_alive"`
	Window      time.Duration `json:"-"`
	WindowS     float64       `json:"window_s"`
	Timeout     time.Duration `json:"-"`
	TimeoutS    float64       `json:"timeout_s"`
	Label       string        `json:"label"`
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.URL, "url", "http://localhost:8080", "base URL of the OpenAI-compatible endpoint")
	flag.StringVar(&c.Model, "model", "qwen3-0.6b", "model name sent in the request body")
	flag.IntVar(&c.Concurrency, "concurrency", 8, "number of concurrent in-flight requests")
	flag.DurationVar(&c.Duration, "duration", 60*time.Second, "how long to sustain load")
	flag.IntVar(&c.MaxTokens, "tokens", 64, "max_tokens per request")
	flag.StringVar(&c.Prompt, "prompt", "Explain what a Kubernetes operator does, in two sentences.", "prompt text")
	flag.BoolVar(&c.KeepAlive, "keepalive", true, "reuse connections; false forces a new connection per request")
	flag.DurationVar(&c.Window, "window", 10*time.Second, "bucket size for the per-window breakdown")
	flag.DurationVar(&c.Timeout, "timeout", 120*time.Second, "per-request timeout")
	flag.StringVar(&c.Label, "label", "", "free-form label echoed into the result (e.g. the variant name)")
	flag.Parse()

	c.DurationS = c.Duration.Seconds()
	c.WindowS = c.Window.Seconds()
	c.TimeoutS = c.Timeout.Seconds()
	return c
}

// sample is one completed request.
type sample struct {
	start time.Time
	// ttft is the delay until the first generated token. It is only meaningful
	// for a successful streaming response; failed requests carry ok == false
	// and are excluded from every percentile rather than recorded as zero,
	// because a fast failure would otherwise improve the latency numbers.
	ttft     time.Duration
	duration time.Duration
	tokens   int
	ok       bool
}

// Stats is a latency summary. Every duration is in seconds, because that is
// what Prometheus uses and these numbers are compared against PromQL output.
type Stats struct {
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	RPS      float64 `json:"rps"`

	TTFTp50 float64 `json:"ttft_p50_s"`
	TTFTp95 float64 `json:"ttft_p95_s"`
	TTFTp99 float64 `json:"ttft_p99_s"`

	DurationP50 float64 `json:"duration_p50_s"`
	DurationP95 float64 `json:"duration_p95_s"`
	DurationP99 float64 `json:"duration_p99_s"`

	// TTFTRatio is ttft_p95 / duration_p95. It is the anti-buffering check: a
	// value at or near 1.0 means the first token arrived at the same moment as
	// the last one, which for a multi-token streaming response is not a latency
	// result but proof that something in the path buffered the stream.
	TTFTRatio float64 `json:"ttft_p95_over_duration_p95"`

	OutputTokens int `json:"output_tokens"`
}

// Window is one time bucket of the run. The per-window p95 series is what the
// coefficient of variation is computed from: a threshold check that fires on a
// single window is only trustworthy if windows resemble each other.
type Window struct {
	StartS   float64 `json:"start_s"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	RPS      float64 `json:"rps"`
	TTFTp95  float64 `json:"ttft_p95_s"`
}

// Result is the whole run.
type Result struct {
	Config  config   `json:"config"`
	Overall Stats    `json:"overall"`
	Windows []Window `json:"windows"`

	// TTFTp95CV is the coefficient of variation (stddev / mean) of the
	// per-window p95 TTFT series. It answers the question canary analysis
	// depends on: how much does this metric move when NOTHING has changed? A
	// threshold set inside that band produces rollbacks at random.
	TTFTp95CV float64 `json:"ttft_p95_cv"`

	// ErrorSample carries up to a few distinct failure strings. Errors that are
	// only counted are errors nobody diagnoses.
	ErrorSample []string `json:"error_sample,omitempty"`
}

func run(cfg config) (*Result, error) {
	if cfg.Concurrency < 1 {
		return nil, errors.New("concurrency must be at least 1")
	}

	transport := &http.Transport{
		DisableKeepAlives: !cfg.KeepAlive,
		// Without this, Go advertises gzip and transparently decompresses,
		// which buffers the response and destroys the very measurement this
		// tool exists to take.
		DisableCompression:  true,
		MaxIdleConnsPerHost: cfg.Concurrency,
		MaxConnsPerHost:     cfg.Concurrency,
		// Below the server's own keep-alive timeout, which for llama.cpp
		// (cpp-httplib) is 5 seconds. Whoever closes an idle connection
		// second loses a race: if the server closes first, the client can
		// pick that connection out of its pool and write a request into a
		// socket that is already going away. Closing first, locally, means
		// the race is simply not entered. See the Idempotency-Key header in
		// doRequest for the second half of this fix.
		IdleConnTimeout: 2 * time.Second,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: cfg.Timeout}

	body, err := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"messages":   []map[string]string{{"role": "user", "content": cfg.Prompt}},
		"max_tokens": cfg.MaxTokens,
		// Non-negotiable. A non-streaming request has no first-token event, so
		// its "TTFT" is its total duration — recording that in the same
		// histogram as real streaming measurements poisons every percentile
		// derived from it.
		"stream": true,
		// Qwen3 and other hybrid-reasoning models emit a thinking block before
		// their answer unless told not to. Leaving it on would make output
		// length depend on how much the model chose to deliberate, which is
		// exactly the kind of variance that makes a latency baseline useless.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		// Greedy decoding, so two runs against the same build differ only by
		// the system under test.
		"temperature": 0,
	})
	if err != nil {
		return nil, fmt.Errorf("building request body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	var (
		mu      sync.Mutex
		samples []sample
		errMsgs []string
	)

	started := time.Now()
	var wg sync.WaitGroup

	for range cfg.Concurrency {
		wg.Go(func() {
			for ctx.Err() == nil {
				s, reqErr := doRequest(ctx, client, cfg, body)

				mu.Lock()
				// A request cut short by the run ending is not a failure of the
				// system under test, so it is dropped rather than counted.
				if reqErr != nil && ctx.Err() != nil {
					mu.Unlock()
					return
				}
				samples = append(samples, s)
				if reqErr != nil && len(errMsgs) < 5 {
					errMsgs = append(errMsgs, reqErr.Error())
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	elapsed := time.Since(started)

	res := &Result{
		Config:      cfg,
		Overall:     summarize(samples, elapsed),
		Windows:     windows(samples, started, cfg.Window),
		ErrorSample: errMsgs,
	}
	res.TTFTp95CV = coefficientOfVariation(res.Windows)
	return res, nil
}

// doRequest issues one streaming completion and times the first generated
// token. It always returns a sample, so that failures are counted rather than
// silently dropped.
func doRequest(ctx context.Context, client *http.Client, cfg config, body []byte) (sample, error) {
	s := sample{start: time.Now()}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return s, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// This header is what makes net/http willing to retry the request when a
	// pooled connection turns out to have been closed by the server.
	//
	// Go retries such a failure automatically for GET, but treats POST as
	// non-replayable unless the caller asserts otherwise — and Idempotency-Key
	// is the assertion it looks for (see Request.isReplayable). The assertion
	// is true here: the failure mode being retried is one where nothing was
	// written to the connection, so the server never saw the first attempt.
	//
	// Without this, cpp-httplib closing a connection after its keep-alive max
	// count shows up as a client-side error rate of several percent that has
	// nothing to do with the system under test — and error rate is one of the
	// signals canary analysis will gate on, so a spurious floor under it is
	// not something to tolerate.
	req.Header.Set("Idempotency-Key", "loadgen")

	resp, err := client.Do(req)
	if err != nil {
		s.duration = time.Since(s.start)
		return s, err
	}
	defer func() {
		// Drain before closing, or the connection cannot be reused and every
		// request pays a fresh TCP and TLS handshake — which would make the
		// keep-alive comparison meaningless.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		s.duration = time.Since(s.start)
		return s, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ttft, tokens, err := readStream(resp.Body, s.start)
	s.duration = time.Since(s.start)
	if err != nil {
		return s, err
	}
	if tokens == 0 {
		return s, errors.New("stream carried no generated tokens")
	}

	s.ttft = ttft
	s.tokens = tokens
	s.ok = true
	return s, nil
}

// sseChunk is the subset of an OpenAI streaming chunk this tool reads.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		// Present on /v1/completions rather than /v1/chat/completions. Read so
		// this tool works against either endpoint.
		Text string `json:"text"`
	} `json:"choices"`
}

// readStream consumes an SSE body and returns the delay until the first
// generated token.
//
// The definition of "first token" is the load-bearing detail. It is NOT the
// first byte of the response and NOT the first SSE frame: llama.cpp sends
// response headers as soon as the request is accepted, and typically emits a
// role-only opening frame carrying no text at all. Timing either of those
// measures how fast the server said hello. TTFT is the first frame that carries
// generated text — including reasoning text, which is genuinely generated
// output even when a client chooses not to display it.
func readStream(r io.Reader, start time.Time) (ttft time.Duration, tokens int, err error) {
	sc := bufio.NewScanner(r)
	// SSE frames from a chat completion are small, but a single frame can carry
	// a long tool-call payload; the default 64 KiB limit would silently error.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}

		var chunk sseChunk
		if jsonErr := json.Unmarshal(payload, &chunk); jsonErr != nil {
			// A frame we cannot parse is not a reason to discard a run;
			// upstream adds fields over time. It simply carries no token.
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		d := chunk.Choices[0]
		if d.Delta.Content == "" && d.Delta.ReasoningContent == "" && d.Text == "" {
			continue
		}

		if tokens == 0 {
			ttft = time.Since(start)
		}
		tokens++
	}

	return ttft, tokens, sc.Err()
}

func summarize(samples []sample, elapsed time.Duration) Stats {
	st := Stats{Requests: len(samples)}

	ttfts := make([]float64, 0, len(samples))
	durs := make([]float64, 0, len(samples))

	for _, s := range samples {
		if !s.ok {
			st.Errors++
			continue
		}
		ttfts = append(ttfts, s.ttft.Seconds())
		durs = append(durs, s.duration.Seconds())
		st.OutputTokens += s.tokens
	}

	if secs := elapsed.Seconds(); secs > 0 {
		st.RPS = float64(len(ttfts)) / secs
	}

	slices.Sort(ttfts)
	slices.Sort(durs)

	st.TTFTp50, st.TTFTp95, st.TTFTp99 = percentile(ttfts, 0.50), percentile(ttfts, 0.95), percentile(ttfts, 0.99)
	st.DurationP50, st.DurationP95, st.DurationP99 = percentile(durs, 0.50), percentile(durs, 0.95), percentile(durs, 0.99)

	if st.DurationP95 > 0 {
		st.TTFTRatio = st.TTFTp95 / st.DurationP95
	}

	return st
}

func windows(samples []sample, started time.Time, size time.Duration) []Window {
	if size <= 0 {
		return nil
	}

	buckets := map[int][]sample{}
	maxIdx := -1
	for _, s := range samples {
		idx := max(int(s.start.Sub(started)/size), 0)
		buckets[idx] = append(buckets[idx], s)
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	out := make([]Window, 0, maxIdx+1)
	for i := 0; i <= maxIdx; i++ {
		bucket := buckets[i]
		w := Window{StartS: (time.Duration(i) * size).Seconds(), Requests: len(bucket)}

		ttfts := make([]float64, 0, len(bucket))
		for _, s := range bucket {
			if !s.ok {
				w.Errors++
				continue
			}
			ttfts = append(ttfts, s.ttft.Seconds())
		}
		slices.Sort(ttfts)
		w.TTFTp95 = percentile(ttfts, 0.95)
		w.RPS = float64(len(ttfts)) / size.Seconds()

		out = append(out, w)
	}
	return out
}

// coefficientOfVariation measures how much the per-window p95 moves when
// nothing about the system has changed.
//
// The first and last windows are dropped: the first contains the run's ramp-up,
// and the last is usually truncated by the deadline. Including either would
// report startup and shutdown as if they were steady-state noise.
func coefficientOfVariation(ws []Window) float64 {
	if len(ws) < 4 {
		return 0
	}

	vals := make([]float64, 0, len(ws))
	for _, w := range ws[1 : len(ws)-1] {
		if w.TTFTp95 > 0 {
			vals = append(vals, w.TTFTp95)
		}
	}
	if len(vals) < 2 {
		return 0
	}

	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	if mean == 0 {
		return 0
	}

	var sumSq float64
	for _, v := range vals {
		sumSq += (v - mean) * (v - mean)
	}
	// Sample standard deviation (n-1): these windows are a sample of the
	// process, not the entire population of windows it could produce.
	return math.Sqrt(sumSq/float64(len(vals)-1)) / mean
}

// percentile returns the p-th percentile of a SORTED slice using nearest-rank.
// Nearest-rank rather than interpolation, because Prometheus's
// histogram_quantile does not interpolate between observations either, and
// these numbers get compared against it.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	idx = min(max(idx, 0), len(sorted)-1)
	return sorted[idx]
}
