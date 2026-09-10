package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

func TestStartedRequestsCountAdmissionOnly(t *testing.T) {
	p, m := newTestProxy(t, "http://engine.test")
	p.rp.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/completions"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/unknown"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, testHealthPath},
	} {
		p.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsStartedTotal, nil); got != 3 {
		t.Fatalf("admitted inference count = %v, want 3", got)
	}
	for _, operation := range []string{
		llmcpmetrics.OperationChat,
		llmcpmetrics.OperationCompletion,
		llmcpmetrics.OperationEmbeddings,
	} {
		labels := map[string]string{llmcpmetrics.LabelOperation: operation}
		if got := counterFor(t, m, llmcpmetrics.RequestsStartedTotal, labels); got != 1 {
			t.Errorf("admitted %s count = %v, want 1", operation, got)
		}
	}
}

func TestStartedRequestsCountUpstreamFailure(t *testing.T) {
	p, m := newTestProxy(t, "http://engine.test")
	p.rp.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	response := httptest.NewRecorder()
	p.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure status = %d", response.Code)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsStartedTotal, nil); got != 1 {
		t.Fatalf("failed request admission count = %v, want 1", got)
	}
}

func TestUsageObservationDoesNotBufferStream(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"
	const tail = "data: {\"usage\":{\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	p, m := newTestProxy(t, "http://engine.test")
	p.rp.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			defer func() { _ = writer.Close() }()
			if _, err := io.WriteString(writer, first); err != nil {
				return
			}
			<-release
			_, _ = io.WriteString(writer, tail)
		}()
		return &http.Response{
			StatusCode:    200,
			Header:        http.Header{"Content-Type": {contentTypeSSE}},
			Body:          reader,
			ContentLength: -1,
			Request:       r,
		}, nil
	})
	shim := httptest.NewServer(p.handler())
	defer shim.Close()
	defer unblock()
	client := shim.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Post(shim.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != strings.TrimSuffix(first, "\n") {
		t.Fatalf("first content must arrive before upstream completion: line=%q error=%v", line, err)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsStartedTotal, nil); got != 1 {
		t.Fatalf("started = %v", got)
	}
	if got := counterFor(t, m, llmcpmetrics.RequestsTotal, nil); got != 0 {
		t.Fatalf("stream completed before release: %v", got)
	}
	unblock()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if line+string(rest) != first+tail {
		t.Fatal("stream bytes changed")
	}
}

func TestCanonicalMetricsCarryResourceIdentity(t *testing.T) {
	_, m := newTestProxy(t, "http://127.0.0.1:8000")
	m.observe(&requestState{operation: llmcpmetrics.OperationChat, streamed: true, sawFirstToken: true, tokens: 2}, 200)
	m.upstreamError("test")
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "llmcp_") {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			wantLabels := map[string]string{
				"namespace":        testNamespace,
				"model_deployment": testDeployment,
				"model":            "test-model",
				"variant":          variantPrimary,
			}
			for key, want := range wantLabels {
				if labels[key] != want {
					t.Errorf("%s has %s=%q, want %q", family.GetName(), key, labels[key], want)
				}
			}
		}
	}
}

func TestProxyReportedUsageAndByteTransparency(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		chunks, usage, tokens   float64
	}{
		{
			"stream usage",
			contentTypeSSE,
			"data: {\"choices\":[{\"delta\":{\"content\":\"one frame with several words\"}}]}\n\n" +
				"data: {\"usage\":{\"completion_tokens\":9}}\n\ndata: [DONE]\n\n",
			1, 1, 9,
		},
		{"stream missing usage", contentTypeSSE, llamaCPPStream, 2, 0, 0},
		{
			"json usage",
			contentTypeJSON,
			`{"choices":[{"message":{"content":"hello world"}}],"usage":{"completion_tokens":7}}`,
			0, 1, 7,
		},
		{"json zero usage", contentTypeJSON, `{"usage":{"completion_tokens":0}}`, 0, 1, 0},
		{"json missing usage", contentTypeJSON, `{"choices":[]}`, 0, 0, 0},
		{
			"oversized json",
			contentTypeJSON,
			`{"padding":"` + strings.Repeat("x", maxSSELine) + `","usage":{"completion_tokens":9}}`,
			0, 0, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer engine.Close()
			proxy, m := newTestProxy(t, engine.URL)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			proxy.handler().ServeHTTP(response, request)
			if response.Body.String() != tc.body {
				t.Fatal("response body changed")
			}
			wantMetrics := map[string]float64{
				llmcpmetrics.OutputChunksTotal:         tc.chunks,
				llmcpmetrics.ReportedOutputTokensTotal: tc.tokens,
				llmcpmetrics.UsageRequestsTotal:        tc.usage,
			}
			for metric, want := range wantMetrics {
				if got := counterFor(t, m, metric, nil); got != want {
					t.Errorf("%s=%v want %v", metric, got, want)
				}
			}
		})
	}
}

func TestIncompleteOrFailedResponseCannotPublishReportedUsage(t *testing.T) {
	for _, tc := range []struct {
		name           string
		streamed, done bool
		code           int
		readErr        error
	}{
		{"missing DONE", true, false, 200, nil},
		{"cancelled", true, true, 200, io.ErrUnexpectedEOF},
		{"HTTP error", true, true, 500, nil},
		{"JSON incomplete", false, false, 200, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m := newTestProxy(t, "http://127.0.0.1:8000")
			m.observe(&requestState{
				operation:      llmcpmetrics.OperationChat,
				streamed:       tc.streamed,
				sawDone:        tc.done,
				hasUsage:       true,
				reportedTokens: 100,
				readErr:        tc.readErr,
				duration:       time.Second,
			}, tc.code)
			reported := counterFor(t, m, llmcpmetrics.ReportedOutputTokensTotal, nil)
			usage := counterFor(t, m, llmcpmetrics.UsageRequestsTotal, nil)
			if reported != 0 || usage != 0 {
				t.Fatal("published unverified usage")
			}
		})
	}
}
