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
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Environment variable names. They are all optional; every one of them has a
// default that makes the engine usable with no configuration at all, because
// the operator's engine profile renders a bare container spec for it.
const (
	envLoadMS    = "LLMCP_FAKE_LOAD_MS"
	envTTFTMS    = "LLMCP_FAKE_TTFT_MS"
	envITLMS     = "LLMCP_FAKE_ITL_MS"
	envTokens    = "LLMCP_FAKE_TOKENS"
	envErrorRate = "LLMCP_FAKE_ERROR_RATE"
	envModel     = "LLMCP_FAKE_MODEL"
)

// Defaults. The load delay is deliberately non-zero: readiness probes that
// never observe a not-ready state are not actually testing readiness.
const (
	defaultLoadMS = 500
	defaultTTFTMS = 50
	defaultITLMS  = 10
	defaultTokens = 24
	defaultRate   = 0.0
	defaultModel  = "fake-model"

	// defaultHost is 0.0.0.0 and not 127.0.0.1 on purpose: this binary runs as
	// a pod, and a loopback bind would make the container unreachable from the
	// kubelet's probes and from the Service.
	defaultHost = "0.0.0.0"
	defaultPort = 8000
)

// Fixed response vocabulary. Generation must never be random, so the engine
// emits one repeated token; callers assert on exact token counts.
const (
	generatedToken = "tok "
	assistantRole  = "assistant"
	finishStop     = "stop"
)

const (
	contentTypeJSON = "application/json"
	headerType      = "Content-Type"
)

// OpenAI object-type discriminators. Clients switch on these strings, so they
// are wire contract rather than incidental labels.
const (
	objectModel          = "model"
	objectTextCompletion = "text_completion"
)

// config is the fully resolved runtime configuration of the fake engine. It is
// a value type built by newConfig from an injected getenv and argv so that
// tests can construct servers directly instead of mutating process state.
type config struct {
	host      string
	port      int
	loadDelay time.Duration
	ttft      time.Duration
	itl       time.Duration
	tokens    int
	errorRate float64
	model     string
}

// addr returns the listen address for this configuration.
func (c config) addr() string {
	return net.JoinHostPort(c.host, strconv.Itoa(c.port))
}

// logValue renders the whole configuration as a single structured log record.
func (c config) logValue() []any {
	return []any{
		"addr", c.addr(),
		"model", c.model,
		"loadMS", c.loadDelay.Milliseconds(),
		"ttftMS", c.ttft.Milliseconds(),
		"itlMS", c.itl.Milliseconds(),
		"tokens", c.tokens,
		"errorRate", c.errorRate,
	}
}

// newConfig resolves configuration from environment variables and llama.cpp
// style command line arguments.
//
// Parsing is deliberately lenient. A malformed value degrades to the default
// with a warning instead of aborting: this process is the engine under test in
// end-to-end runs, and a crash-on-startup would surface as an unrelated and
// confusing pod failure rather than as the misconfiguration it is.
func newConfig(getenv func(string) string, args []string, logger *slog.Logger) config {
	cfg := config{
		host:      defaultHost,
		port:      defaultPort,
		loadDelay: durationMS(getenv, envLoadMS, defaultLoadMS, logger),
		ttft:      durationMS(getenv, envTTFTMS, defaultTTFTMS, logger),
		itl:       durationMS(getenv, envITLMS, defaultITLMS, logger),
		tokens:    intValue(getenv, envTokens, defaultTokens, logger),
		errorRate: rateValue(getenv, envErrorRate, defaultRate, logger),
		model:     stringValue(getenv, envModel, defaultModel),
	}

	host, port, alias := scanArgs(args, logger)
	if host != "" {
		cfg.host = host
	}
	if port > 0 {
		cfg.port = port
	}
	// --alias wins over the environment default, matching llama-server, where
	// it renames the model reported at /v1/models. The operator passes the
	// user's spec.model.name this way, and downstream metrics are labelled with
	// it — so a stub that ignored the flag would silently report a different
	// model name than the real engine and make those labels wrong.
	if alias != "" {
		cfg.model = alias
	}
	return cfg
}

// stringValue returns the environment value, or def when it is unset or empty.
func stringValue(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

// intValue parses a non-negative integer environment value.
func intValue(getenv func(string) string, key string, def int, logger *slog.Logger) int {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		logger.Warn("ignoring invalid environment value", "key", key, "value", raw, "default", def)
		return def
	}
	return v
}

// durationMS parses an integer count of milliseconds into a duration.
func durationMS(getenv func(string) string, key string, def int, logger *slog.Logger) time.Duration {
	return time.Duration(intValue(getenv, key, def, logger)) * time.Millisecond
}

// rateValue parses a float in [0,1], clamping out-of-range input.
func rateValue(getenv func(string) string, key string, def float64, logger *slog.Logger) float64 {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) {
		logger.Warn("ignoring invalid environment value", "key", key, "value", raw, "default", def)
		return def
	}
	return math.Min(1, math.Max(0, v))
}

// scanArgs performs a permissive scan of llama-server style arguments.
//
// The operator renders the real llamacpp engine profile against this binary,
// so it is handed flags such as -m, -c, -t, --parallel, --metrics, -hf and
// --api-key. Those must be silently ignored: the standard flag package would
// abort on the first unknown flag and the pod would crash-loop. Only --host,
// --port and --alias change behaviour, because the operator relies on those
// taking effect. Both "--flag value" and "--flag=value" spellings are accepted.
func scanArgs(args []string, logger *slog.Logger) (host string, port int, alias string) {
	for i := 0; i < len(args); i++ {
		name, value, hasValue := splitArg(args[i])
		if name != "--host" && name != "--port" && name != "--alias" {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				logger.Warn("ignoring flag with no value", "flag", name)
				continue
			}
			i++
			value = args[i]
		}
		if name == "--host" {
			host = value
			continue
		}
		if name == "--alias" {
			alias = value
			continue
		}
		p, err := strconv.Atoi(value)
		if err != nil || p <= 0 || p > 65535 {
			logger.Warn("ignoring invalid port flag", "value", value)
			continue
		}
		port = p
	}
	return host, port, alias
}

// splitArg normalises "-x", "--x", "--x=v" into a "--x" name and its value.
func splitArg(arg string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", "", false
	}
	name, value, hasValue = strings.Cut(arg, "=")
	if !strings.HasPrefix(name, "--") {
		name = "-" + name
	}
	return name, value, hasValue
}

// apiError is the llama.cpp / OpenAI error object.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

// errorEnvelope wraps an apiError the way llama-server does.
type errorEnvelope struct {
	Error apiError `json:"error"`
}

// server is the fake inference engine.
type server struct {
	cfg    config
	logger *slog.Logger

	// readyAt is the instant the simulated model finishes loading. It is fixed
	// at construction so readiness is a pure function of the clock.
	readyAt time.Time

	// requests counts completion requests and drives deterministic error
	// injection. It is never used as a source of randomness.
	requests atomic.Uint64
}

// newServer builds a fake engine from an already resolved configuration.
func newServer(cfg config, logger *slog.Logger) *server {
	return &server{
		cfg:     cfg,
		logger:  logger,
		readyAt: time.Now().Add(cfg.loadDelay),
	}
}

// loaded reports whether the simulated model load has completed.
func (s *server) loaded() bool {
	return !time.Now().Before(s.readyAt)
}

// handler returns the fully routed HTTP handler for the engine.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleCompletion(true))
	mux.HandleFunc("POST /v1/completions", s.handleCompletion(false))
	return mux
}

// failsAt reports whether request number n (1-based) must be failed for the
// given error rate.
//
// This is a counter, not a random draw, and that is the entire point. A
// canary test needs to assert things like "rolled back after exactly two
// failed checks"; with math/rand the same test would pass or fail depending on
// the seed. Stepping the floor of n*rate emits failures evenly spaced through
// the request sequence and yields exactly floor(N*rate) failures in the first
// N requests, reproducibly, on every run.
func failsAt(n uint64, rate float64) bool {
	switch {
	case rate <= 0 || n == 0:
		return false
	case rate >= 1:
		return true
	}
	return math.Floor(float64(n)*rate) > math.Floor(float64(n-1)*rate)
}

// nextRequest reserves the next request number and reports whether it must
// fail.
func (s *server) nextRequest() (n uint64, fail bool) {
	n = s.requests.Add(1)
	return n, failsAt(n, s.cfg.errorRate)
}

// writeJSON marshals v and writes it with the given status.
func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Every payload here is a plain struct, so this is unreachable; log it
		// rather than pretending success.
		s.logger.Error("failed to marshal response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerType, contentTypeJSON)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.logger.Warn("failed to write response", "error", err)
	}
}

// writeAPIError writes an error in the llama.cpp envelope shape.
func (s *server) writeAPIError(w http.ResponseWriter, status int, message, kind string) {
	s.writeJSON(w, status, errorEnvelope{Error: apiError{Code: status, Message: message, Type: kind}})
}

// writeLoading writes the exact body llama-server returns while a model is
// still loading. The operator keys readiness off this 503, so the status and
// the shape are contract, not decoration.
func (s *server) writeLoading(w http.ResponseWriter) {
	s.writeAPIError(w, http.StatusServiceUnavailable, "Loading model", "unavailable_error")
}

// handleHealth mimics llama-server's /health endpoint.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if !s.loaded() {
		s.writeLoading(w)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// modelCard is one entry of the OpenAI /v1/models list.
type modelCard struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelList is the OpenAI /v1/models response.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelCard `json:"data"`
}

// handleModels serves the OpenAI model listing.
func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	if !s.loaded() {
		s.writeLoading(w)
		return
	}
	s.writeJSON(w, http.StatusOK, modelList{
		Object: "list",
		Data: []modelCard{{
			ID:      s.cfg.model,
			Object:  objectModel,
			Created: time.Now().Unix(),
			OwnedBy: "llmcp",
		}},
	})
}

// completionRequest is the subset of the OpenAI request bodies this engine
// cares about. Everything else is accepted and ignored.
type completionRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Prompt   json.RawMessage `json:"prompt"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// promptTokens is a deterministic, deliberately cheap token estimate. Nothing
// downstream depends on its accuracy, only on its stability.
func (req completionRequest) promptTokens() int {
	chars := 0
	for _, m := range req.Messages {
		chars += len(m.Content)
	}
	chars += len(req.Prompt)
	return max(1, chars/4)
}

// usage is the OpenAI usage block.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatMessage is a non-streaming chat message.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatDelta is a streaming chat delta. Both fields are omitted when empty so
// the role chunk, the content chunks and the terminating chunk each carry only
// what OpenAI clients expect.
type chatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// chatChoice is one choice of a non-streaming chat completion.
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatChunkChoice is one choice of a streaming chat chunk. FinishReason is a
// pointer so that it serialises as null rather than "" mid-stream.
type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

// textChoice is one choice of a legacy completion, streaming or not.
type textChoice struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// chatResponse is a non-streaming chat completion.
type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`
}

// chatChunk is a streaming chat completion chunk.
type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
}

// textResponse is a legacy completion, streaming chunk or full response.
type textResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []textChoice `json:"choices"`
	Usage   *usage       `json:"usage,omitempty"`
}

// generatedText is the deterministic completion body for n tokens.
func generatedText(tokens int) string {
	return strings.Repeat(generatedToken, tokens)
}

// handleCompletion serves both completion endpoints. chat selects the OpenAI
// chat shapes; otherwise the legacy text shapes are used. Sharing one handler
// keeps the latency and error-injection behaviour identical across the two
// endpoints, which is what makes them interchangeable in tests.
func (s *server) handleCompletion(chat bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loaded() {
			s.writeLoading(w)
			return
		}

		var req completionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeAPIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error")
			return
		}

		// The counter advances for every accepted request, including the ones
		// that are about to be failed, so the failure spacing is exact.
		n, fail := s.nextRequest()
		if fail {
			// Fail before sleeping: an injected failure should look like a
			// fast rejection, and it keeps error-rate tests quick.
			s.writeAPIError(w, http.StatusInternalServerError, "injected failure", "server_error")
			return
		}

		created := time.Now().Unix()
		id := fmt.Sprintf("chatcmpl-%d", n)
		if !chat {
			id = fmt.Sprintf("cmpl-%d", n)
		}

		if req.Stream {
			s.streamCompletion(w, r, id, created, chat)
			return
		}
		s.nonStreamCompletion(w, r, req, id, created, chat)
	}
}

// sleepCtx sleeps for d unless the client goes away first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nonStreamCompletion answers with a single JSON body, after waiting for the
// same wall-clock time a streamed answer would have taken. Latency has to be
// comparable across both modes or a rollout gate calibrated on one would
// misjudge the other.
func (s *server) nonStreamCompletion(
	w http.ResponseWriter, r *http.Request, req completionRequest, id string, created int64, chat bool,
) {
	tokens := s.cfg.tokens
	if !sleepCtx(r.Context(), s.cfg.ttft+time.Duration(tokens)*s.cfg.itl) {
		return
	}

	u := usage{
		PromptTokens:     req.promptTokens(),
		CompletionTokens: tokens,
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	text := generatedText(tokens)
	stop := finishStop

	if chat {
		s.writeJSON(w, http.StatusOK, chatResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   s.cfg.model,
			Choices: []chatChoice{{
				Index:        0,
				Message:      chatMessage{Role: assistantRole, Content: text},
				FinishReason: stop,
			}},
			Usage: u,
		})
		return
	}
	s.writeJSON(w, http.StatusOK, textResponse{
		ID:      id,
		Object:  objectTextCompletion,
		Created: created,
		Model:   s.cfg.model,
		Choices: []textChoice{{Index: 0, Text: text, FinishReason: &stop}},
		Usage:   &u,
	})
}

// chatChunks builds the full ordered chunk sequence for a chat stream: a role
// chunk, one chunk per generated token, then a terminating chunk.
func (s *server) chatChunks(id string, created int64) []any {
	chunk := func(delta chatDelta, finish *string) any {
		return chatChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   s.cfg.model,
			Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
	}
	out := make([]any, 0, s.cfg.tokens+2)
	out = append(out, chunk(chatDelta{Role: assistantRole}, nil))
	for range s.cfg.tokens {
		out = append(out, chunk(chatDelta{Content: generatedToken}, nil))
	}
	stop := finishStop
	return append(out, chunk(chatDelta{}, &stop))
}

// textChunks builds the chunk sequence for a legacy completion stream.
func (s *server) textChunks(id string, created int64) []any {
	chunk := func(text string, finish *string) any {
		return textResponse{
			ID:      id,
			Object:  objectTextCompletion,
			Created: created,
			Model:   s.cfg.model,
			Choices: []textChoice{{Index: 0, Text: text, FinishReason: finish}},
		}
	}
	out := make([]any, 0, s.cfg.tokens+1)
	for range s.cfg.tokens {
		out = append(out, chunk(generatedToken, nil))
	}
	stop := finishStop
	return append(out, chunk("", &stop))
}

// streamCompletion emits the response as Server-Sent Events.
func (s *server) streamCompletion(w http.ResponseWriter, r *http.Request, id string, created int64, chat bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without a Flusher every chunk would be buffered, which would make the
		// timing observations this engine exists to provide meaningless.
		s.writeAPIError(w, http.StatusInternalServerError, "streaming unsupported", "server_error")
		return
	}

	h := w.Header()
	h.Set(headerType, "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chunks := s.textChunks(id, created)
	// firstContent is the index of the first chunk that carries a TOKEN. For a
	// chat stream that is 1, because chunk 0 is the role frame; for a legacy
	// completion there is no role frame and it is 0.
	firstContent := 0
	if chat {
		chunks = s.chatChunks(id, created)
		firstContent = 1
	}

	ctx := r.Context()
	for i, c := range chunks {
		// The role frame goes out IMMEDIATELY, and cfg.ttft is spent before the
		// first frame that carries content.
		//
		// Two things break if the role frame is delayed instead. The observable
		// TTFT on a chat stream becomes ttft+itl while a legacy completion's
		// stays ttft, so the two endpoints disagree and every assertion of the
		// form "the histogram is about LLMCP_FAKE_TTFT_MS" is off by one ITL.
		// Worse, real llama.cpp emits the role frame at once — which is the
		// premise the shim's whole first-token measurement rests on — so a shim
		// that wrongly timed the first FRAME would report the configured TTFT
		// and look correct. The stub would be hiding the exact regression it
		// exists to catch.
		var delay time.Duration
		switch {
		case i < firstContent:
			delay = 0
		case i == firstContent:
			delay = s.cfg.ttft
		default:
			delay = s.cfg.itl
		}
		if !sleepCtx(ctx, delay) {
			// The client hung up; stop generating rather than burning the
			// remaining inter-token delays on a dead connection.
			return
		}
		if !s.writeEvent(w, flusher, c) {
			return
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

// writeEvent marshals and writes one SSE event, flushing immediately.
//
// The Flush call is load-bearing. Go buffers response writes, so without it
// every chunk would land on the wire together at the end of the response and
// any proxy measuring time-to-first-token would record TTFT equal to the total
// duration. Flushing per chunk is what makes the simulated TTFT observable.
func (s *server) writeEvent(w http.ResponseWriter, flusher http.Flusher, chunk any) bool {
	payload, err := json.Marshal(chunk)
	if err != nil {
		s.logger.Error("failed to marshal stream chunk", "error", err)
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		// The stream is already broken; there is nothing useful left to write.
		return false
	}
	flusher.Flush()
	return true
}
