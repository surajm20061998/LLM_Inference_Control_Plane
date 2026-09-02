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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	chatPath    = "/v1/chat/completions"
	dataPrefix  = "data: "
	doneLine    = "data: [DONE]"
	testModelID = "unit-test-model"
)

// The llama-server flag spellings the fake is handed in production. They are
// written out here independently of the parser, so renaming one in scanArgs
// fails these tests rather than being followed silently.
const (
	flagHost    = "--host"
	flagPort    = "--port"
	flagAlias   = "--alias"
	flagMetrics = "--metrics"
)

// modelFromEnv is the model name supplied through envModel, used by the cases
// that pit the environment against the --alias flag.
const modelFromEnv = "from-env"

// testLogger discards output so failing-config tests stay quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// envMap turns a map into a getenv function, so configuration can be injected
// without touching process environment (which would break parallel tests).
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// newTestServer builds a ready-to-use handler from environment overrides.
func newTestServer(t *testing.T, env map[string]string) *server {
	t.Helper()
	return newServer(newConfig(envMap(env), nil, testLogger()), testLogger())
}

// doRequest runs one request through the handler and returns the recorder.
func doRequest(t *testing.T, s *server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthReportsLoadingThenOK(t *testing.T) {
	t.Parallel()

	const loadMS = 120
	s := newTestServer(t, map[string]string{envLoadMS: "120"})

	rec := doRequest(t, s, http.MethodGet, "/health", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("while loading: got status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	// The operator keys readiness off this exact llama-server body.
	const wantBody = `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("while loading: got body %q, want %q", got, wantBody)
	}
	if ct := rec.Header().Get(headerType); ct != contentTypeJSON {
		t.Errorf("while loading: got content type %q, want %q", ct, contentTypeJSON)
	}

	// /v1 endpoints must refuse with the same shape before the model is up.
	if rec := doRequest(t, s, http.MethodGet, "/v1/models", ""); rec.Body.String() != wantBody {
		t.Errorf("models while loading: got body %q, want %q", rec.Body.String(), wantBody)
	}
	if rec := doRequest(t, s, http.MethodPost, chatPath, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("chat while loading: got status %d, want 503", rec.Code)
	}

	time.Sleep(loadMS*time.Millisecond + 50*time.Millisecond)

	rec = doRequest(t, s, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("after loading: got status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf(`after loading: got body %q, want {"status":"ok"}`, got)
	}
}

// TestDeterministicErrorInjection pins the property canary tests depend on:
// the failure pattern is a function of the request number alone.
func TestDeterministicErrorInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		rate         string
		requests     int
		wantFailures int
	}{
		{name: "rate zero never fails", rate: "0", requests: 100, wantFailures: 0},
		{name: "rate one always fails", rate: "1", requests: 100, wantFailures: 100},
		{name: "rate quarter fails a quarter", rate: "0.25", requests: 100, wantFailures: 25},
		{name: "rate tenth fails a tenth", rate: "0.1", requests: 100, wantFailures: 10},
		{name: "invalid rate falls back to default", rate: "not-a-number", requests: 100, wantFailures: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, map[string]string{
				envErrorRate: tc.rate,
				envLoadMS:    "0",
				envTTFTMS:    "0",
				envITLMS:     "0",
				envTokens:    "1",
			})

			failures := 0
			for i := range tc.requests {
				rec := doRequest(t, s, http.MethodPost, chatPath, `{"messages":[]}`)
				switch rec.Code {
				case http.StatusInternalServerError:
					failures++
				case http.StatusOK:
				default:
					t.Fatalf("request %d: unexpected status %d", i+1, rec.Code)
				}
			}
			if failures != tc.wantFailures {
				t.Errorf("got %d failures in %d requests, want %d", failures, tc.requests, tc.wantFailures)
			}
		})
	}
}

// TestFailsAtIsEvenlySpaced documents that failures are spread through the
// sequence rather than clustered, which is what makes "fail the Nth check"
// scenarios expressible.
func TestFailsAtIsEvenlySpaced(t *testing.T) {
	t.Parallel()

	var got []uint64
	for n := uint64(1); n <= 12; n++ {
		if failsAt(n, 0.25) {
			got = append(got, n)
		}
	}
	want := []uint64{4, 8, 12}
	if len(got) != len(want) {
		t.Fatalf("got failures at %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got failures at %v, want %v", got, want)
		}
	}
}

func TestNonStreamingChatCompletion(t *testing.T) {
	t.Parallel()

	const tokens = 7
	s := newTestServer(t, map[string]string{
		envLoadMS: "0",
		envTTFTMS: "0",
		envITLMS:  "0",
		envTokens: "7",
		envModel:  testModelID,
	})

	body := `{"model":"whatever","messages":[{"role":"user","content":"12345678"}]}`
	rec := doRequest(t, s, http.MethodPost, chatPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("got object %q, want chat.completion", resp.Object)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") {
		t.Errorf("got id %q, want a chatcmpl- prefix", resp.ID)
	}
	if resp.Model != testModelID {
		t.Errorf("got model %q, want %q", resp.Model, testModelID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.Message.Role != assistantRole {
		t.Errorf("got role %q, want %q", choice.Message.Role, assistantRole)
	}
	if choice.FinishReason != finishStop {
		t.Errorf("got finish reason %q, want %q", choice.FinishReason, finishStop)
	}
	if want := strings.Repeat(generatedToken, tokens); choice.Message.Content != want {
		t.Errorf("got content %q, want %q", choice.Message.Content, want)
	}
	if resp.Usage.CompletionTokens != tokens {
		t.Errorf("got %d completion tokens, want %d", resp.Usage.CompletionTokens, tokens)
	}
	if resp.Usage.PromptTokens != 2 { // 8 characters of content / 4
		t.Errorf("got %d prompt tokens, want 2", resp.Usage.PromptTokens)
	}
	if sum := resp.Usage.PromptTokens + resp.Usage.CompletionTokens; resp.Usage.TotalTokens != sum {
		t.Errorf("got total tokens %d, want %d", resp.Usage.TotalTokens, sum)
	}
}

// streamResult is the parsed form of one SSE response.
type streamResult struct {
	chunks   []chatChunk
	rawLines []string
	firstAt  time.Duration
	totalAt  time.Duration
}

// readChatStream consumes an SSE body incrementally, timing the first event.
func readChatStream(t *testing.T, body io.Reader, start time.Time) streamResult {
	t.Helper()

	var out streamResult
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if out.firstAt == 0 {
			out.firstAt = time.Since(start)
		}
		out.rawLines = append(out.rawLines, line)
		if !strings.HasPrefix(line, dataPrefix) {
			t.Fatalf("unexpected SSE line %q", line)
		}
		payload := strings.TrimPrefix(line, dataPrefix)
		if payload == "[DONE]" {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decoding chunk %q: %v", payload, err)
		}
		out.chunks = append(out.chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	out.totalAt = time.Since(start)
	return out
}

func TestStreamingChatCompletionShape(t *testing.T) {
	t.Parallel()

	const tokens = 5
	s := newTestServer(t, map[string]string{
		envLoadMS: "0",
		envTTFTMS: "0",
		envITLMS:  "0",
		envTokens: "5",
		envModel:  testModelID,
	})

	rec := doRequest(t, s, http.MethodPost, chatPath, `{"stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get(headerType); ct != "text/event-stream" {
		t.Errorf("got content type %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("got cache control %q, want no-cache", cc)
	}
	if conn := rec.Header().Get("Connection"); conn != "keep-alive" {
		t.Errorf("got connection %q, want keep-alive", conn)
	}

	res := readChatStream(t, rec.Body, time.Now())

	if n := len(res.rawLines); n == 0 || res.rawLines[n-1] != doneLine {
		t.Fatalf("stream does not end with %q: %v", doneLine, res.rawLines)
	}
	// One role chunk, one chunk per token, one terminating chunk.
	if want := tokens + 2; len(res.chunks) != want {
		t.Fatalf("got %d chunks, want %d", len(res.chunks), want)
	}

	first := res.chunks[0].Choices[0]
	if first.Delta.Role != assistantRole {
		t.Errorf("first chunk: got role %q, want %q", first.Delta.Role, assistantRole)
	}
	if first.FinishReason != nil {
		t.Errorf("first chunk: got finish reason %v, want null", *first.FinishReason)
	}

	contentChunks := 0
	for _, c := range res.chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("got object %q, want chat.completion.chunk", c.Object)
		}
		if c.Model != testModelID {
			t.Errorf("got model %q, want %q", c.Model, testModelID)
		}
		if c.Choices[0].Delta.Content == "" {
			continue
		}
		contentChunks++
		if c.Choices[0].Delta.Content != generatedToken {
			t.Errorf("got token %q, want %q", c.Choices[0].Delta.Content, generatedToken)
		}
	}
	if contentChunks != tokens {
		t.Errorf("got %d content chunks, want %d", contentChunks, tokens)
	}

	last := res.chunks[len(res.chunks)-1].Choices[0]
	if last.FinishReason == nil || *last.FinishReason != finishStop {
		t.Errorf("last chunk: got finish reason %v, want %q", last.FinishReason, finishStop)
	}
}

// TestStreamingIsIncremental guards the http.Flusher call. Without a flush per
// chunk the whole body would arrive at once and a downstream proxy measuring
// time-to-first-token would see TTFT equal to the total duration, silently
// invalidating every latency assertion built on this engine.
func TestStreamingIsIncremental(t *testing.T) {
	t.Parallel()

	const (
		ttft   = 200 * time.Millisecond
		tokens = 6
	)
	cfg := newConfig(envMap(map[string]string{
		envLoadMS: "0",
		envTTFTMS: "200",
		envITLMS:  "50",
		envTokens: "6",
	}), nil, testLogger())

	ts := httptest.NewServer(newServer(cfg, testLogger()).handler())
	defer ts.Close()

	start := time.Now()
	resp, err := http.Post(ts.URL+chatPath, contentTypeJSON, strings.NewReader(`{"stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing body: %v", err)
		}
	}()

	res := readChatStream(t, resp.Body, start)

	if want := tokens + 2; len(res.chunks) != want {
		t.Fatalf("got %d chunks, want %d", len(res.chunks), want)
	}
	// The first event must not arrive before the configured TTFT has elapsed.
	if res.firstAt < ttft {
		t.Errorf("first chunk arrived after %v, want at least %v", res.firstAt, ttft)
	}
	// And it must arrive well before the stream completes. The remaining
	// chunks account for ~350ms of sleeps; requiring only 150ms of separation
	// leaves plenty of slack for a loaded CI machine.
	if gap := res.totalAt - res.firstAt; gap < 150*time.Millisecond {
		t.Errorf("first chunk at %v, stream done at %v: response looks buffered", res.firstAt, res.totalAt)
	}
}

func TestModelsReportsConfiguredModel(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, map[string]string{envLoadMS: "0", envModel: testModelID})
	rec := doRequest(t, s, http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	var list modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("got object %q, want list", list.Object)
	}
	if len(list.Data) != 1 {
		t.Fatalf("got %d models, want 1", len(list.Data))
	}
	card := list.Data[0]
	if card.ID != testModelID {
		t.Errorf("got model id %q, want %q", card.ID, testModelID)
	}
	if card.Object != "model" || card.OwnedBy != "llmcp" {
		t.Errorf("got object %q owned_by %q, want model/llmcp", card.Object, card.OwnedBy)
	}
	if card.Created <= 0 {
		t.Errorf("got created %d, want a unix timestamp", card.Created)
	}
}

// TestArgsToleratesLlamaCppFlags proves the operator can hand this binary a
// real llama-server command line unchanged.
func TestArgsToleratesLlamaCppFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantHost string
		wantPort int
	}{
		{
			name:     "no args uses defaults",
			args:     nil,
			wantHost: defaultHost,
			wantPort: defaultPort,
		},
		{
			name: "full llama-server command line",
			args: []string{
				flagHost, "127.0.0.1", flagPort, "9001",
				"-m", "/models/qwen.gguf", "-c", "4096", "-t", "8",
				"--parallel", "4", flagMetrics, "-hf", "org/repo", "--api-key", "secret",
			},
			wantHost: "127.0.0.1",
			wantPort: 9001,
		},
		{
			name:     "equals form",
			args:     []string{"--host=1.2.3.4", "--port=1234", "--unknown-flag=value"},
			wantHost: "1.2.3.4",
			wantPort: 1234,
		},
		{
			name:     "unknown flags alone are ignored",
			args:     []string{"--flash-attn", "--no-mmap", "--ctx-size", "8192"},
			wantHost: defaultHost,
			wantPort: defaultPort,
		},
		{
			name:     "invalid port falls back to the default",
			args:     []string{flagPort, "not-a-port"},
			wantHost: defaultHost,
			wantPort: defaultPort,
		},
		{
			name:     "trailing port flag with no value is ignored",
			args:     []string{flagMetrics, flagPort},
			wantHost: defaultHost,
			wantPort: defaultPort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := newConfig(envMap(nil), tc.args, testLogger())
			if cfg.host != tc.wantHost {
				t.Errorf("got host %q, want %q", cfg.host, tc.wantHost)
			}
			if cfg.port != tc.wantPort {
				t.Errorf("got port %d, want %d", cfg.port, tc.wantPort)
			}
			// Whatever the flags, the server must still be constructible.
			if newServer(cfg, testLogger()).handler() == nil {
				t.Error("handler is nil")
			}
		})
	}
}

// TestAliasFlagRenamesTheModel pins --alias handling.
//
// llama-server uses --alias to rename the model it reports at /v1/models, and
// the operator passes the user's spec.model.name that way. A stub that ignored
// the flag would report a different model name than the real engine — and since
// that name becomes a metric label downstream, the divergence would silently
// mislabel every measurement rather than fail visibly.
func TestAliasFlagRenamesTheModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{
			name: "alias overrides the built-in default",
			args: []string{flagAlias, "qwen3-0.6b"},
			want: "qwen3-0.6b",
		},
		{
			name: "equals form is accepted",
			args: []string{"--alias=demo-model"},
			want: "demo-model",
		},
		{
			// The flag is the operator's channel and reflects the user's spec,
			// so it must win over an image-baked environment default.
			name: "alias wins over the environment",
			env:  map[string]string{envModel: modelFromEnv},
			args: []string{flagAlias, "from-flag"},
			want: "from-flag",
		},
		{
			name: "environment still applies when no alias is given",
			env:  map[string]string{envModel: modelFromEnv},
			args: []string{flagMetrics},
			want: modelFromEnv,
		},
		{
			name: "trailing alias with no value is ignored",
			args: []string{flagAlias},
			want: defaultModel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := newConfig(envMap(tc.env), tc.args, testLogger())
			if cfg.model != tc.want {
				t.Errorf("got model %q, want %q", cfg.model, tc.want)
			}
		})
	}
}

func TestConfigDefaultsAndLenientParsing(t *testing.T) {
	t.Parallel()

	def := newConfig(envMap(nil), nil, testLogger())
	if def.model != defaultModel || def.tokens != defaultTokens {
		t.Errorf("got model %q tokens %d, want %q/%d", def.model, def.tokens, defaultModel, defaultTokens)
	}
	if def.loadDelay != defaultLoadMS*time.Millisecond {
		t.Errorf("got load delay %v, want %dms", def.loadDelay, defaultLoadMS)
	}
	if def.ttft != defaultTTFTMS*time.Millisecond || def.itl != defaultITLMS*time.Millisecond {
		t.Errorf("got ttft %v itl %v, want %dms/%dms", def.ttft, def.itl, defaultTTFTMS, defaultITLMS)
	}
	if def.addr() != "0.0.0.0:8000" {
		t.Errorf("got addr %q, want 0.0.0.0:8000", def.addr())
	}

	// Garbage must degrade to defaults rather than crash the process.
	bad := newConfig(envMap(map[string]string{
		envTokens:    "banana",
		envTTFTMS:    "-5",
		envErrorRate: "17",
		envLoadMS:    "",
	}), nil, testLogger())
	if bad.tokens != defaultTokens {
		t.Errorf("got tokens %d, want %d", bad.tokens, defaultTokens)
	}
	if bad.ttft != defaultTTFTMS*time.Millisecond {
		t.Errorf("got ttft %v, want %dms", bad.ttft, defaultTTFTMS)
	}
	if bad.errorRate != 1 {
		t.Errorf("got error rate %v, want it clamped to 1", bad.errorRate)
	}
	if bad.loadDelay != defaultLoadMS*time.Millisecond {
		t.Errorf("got load delay %v, want %dms", bad.loadDelay, defaultLoadMS)
	}
}

func TestLegacyCompletionsStreamAndNonStream(t *testing.T) {
	t.Parallel()

	const tokens = 3
	s := newTestServer(t, map[string]string{
		envLoadMS: "0",
		envTTFTMS: "0",
		envITLMS:  "0",
		envTokens: "3",
		envModel:  testModelID,
	})

	rec := doRequest(t, s, http.MethodPost, "/v1/completions", `{"prompt":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	var resp textResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Object != "text_completion" || !strings.HasPrefix(resp.ID, "cmpl-") {
		t.Errorf("got object %q id %q, want text_completion/cmpl-", resp.Object, resp.ID)
	}
	if resp.Usage == nil || resp.Usage.CompletionTokens != tokens {
		t.Fatalf("got usage %+v, want %d completion tokens", resp.Usage, tokens)
	}
	if want := strings.Repeat(generatedToken, tokens); resp.Choices[0].Text != want {
		t.Errorf("got text %q, want %q", resp.Choices[0].Text, want)
	}

	rec = doRequest(t, s, http.MethodPost, "/v1/completions", `{"prompt":"hello","stream":true}`)
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	// One chunk per token plus the terminating chunk plus [DONE].
	if want := tokens + 2; len(lines) != want {
		t.Fatalf("got %d SSE events, want %d: %q", len(lines), want, rec.Body.String())
	}
	if last := strings.TrimSpace(lines[len(lines)-1]); last != doneLine {
		t.Errorf("got last line %q, want %q", last, doneLine)
	}
}
