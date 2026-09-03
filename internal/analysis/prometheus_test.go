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

package analysis

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// promServer serves a fixed body for every query.
func promServer(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			seen = append(seen, r.Form.Get("query"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func vectorBody(samples ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(samples, ",") + `]}}`
}

func vectorSample(value string) string {
	return `{"metric":{"variant":"canary"},"value":[1756900000.0,"` + value + `"]}`
}

func TestPrometheusDecodesASingleSample(t *testing.T) {
	t.Parallel()

	srv, queries := promServer(t, http.StatusOK, vectorBody(vectorSample("1.234")))
	p := &PrometheusProvider{Address: srv.URL}

	got, err := p.Query(context.Background(), "up")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Count != 1 || got.Value != 1.234 {
		t.Fatalf("sample = %+v, want {Value:1.234 Count:1}", got)
	}
	if len(*queries) != 1 || (*queries)[0] != "up" {
		t.Errorf("queries = %v, want [up]", *queries)
	}
}

// TestPrometheusPreservesSpecialValues is why the wire format uses a string for
// the sample value: JSON has no way to represent NaN or Inf, and decoding into
// a float64 directly would fail on exactly the inputs the verdict logic most
// needs to distinguish.
func TestPrometheusPreservesSpecialValues(t *testing.T) {
	t.Parallel()

	cases := map[string]func(float64) bool{
		LiteralNaN:    func(v float64) bool { return math.IsNaN(v) },
		LiteralPosInf: func(v float64) bool { return math.IsInf(v, 1) },
		LiteralNegInf: func(v float64) bool { return math.IsInf(v, -1) },
	}

	for literal, ok := range cases {
		srv, _ := promServer(t, http.StatusOK, vectorBody(vectorSample(literal)))
		p := &PrometheusProvider{Address: srv.URL}

		got, err := p.Query(context.Background(), "q")
		if err != nil {
			t.Fatalf("%s: %v", literal, err)
		}
		if got.Count != 1 || !ok(got.Value) {
			t.Errorf("%s decoded as %v; the special value did not survive", literal, got.Value)
		}
	}
}

func TestPrometheusEmptyResultIsNotAnError(t *testing.T) {
	t.Parallel()

	// Count 0 with a NIL error, deliberately: an empty result is a fact about
	// the data, and it leads to a different verdict (Inconclusive) than a
	// transport failure does (Error).
	srv, _ := promServer(t, http.StatusOK, vectorBody())
	p := &PrometheusProvider{Address: srv.URL}

	got, err := p.Query(context.Background(), "q")
	if err != nil {
		t.Fatalf("an empty result must not be an error, got: %v", err)
	}
	if got.Count != 0 {
		t.Fatalf("count = %d, want 0", got.Count)
	}
}

func TestPrometheusMultipleSamplesReportedThroughCount(t *testing.T) {
	t.Parallel()

	// Reported through Count so Evaluate can produce the Error verdict with a
	// message naming the real problem — a query that failed to aggregate —
	// rather than this layer inventing one.
	srv, _ := promServer(t, http.StatusOK,
		vectorBody(vectorSample("1"), vectorSample("2"), vectorSample("3")))
	p := &PrometheusProvider{Address: srv.URL}

	got, err := p.Query(context.Background(), "q")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Count != 3 {
		t.Fatalf("count = %d, want 3", got.Count)
	}
}

func TestPrometheusRejectedQueryIsAnError(t *testing.T) {
	t.Parallel()

	srv, _ := promServer(t, http.StatusBadRequest,
		`{"status":"error","errorType":"bad_data","error":"parse error at char 5"}`)
	p := &PrometheusProvider{Address: srv.URL}

	_, err := p.Query(context.Background(), "sum(")
	if err == nil {
		t.Fatal("expected an error for a rejected query")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("error = %v, want it to carry Prometheus's own message", err)
	}
}

func TestPrometheusNonJSONResponseNamesTheStatusCode(t *testing.T) {
	t.Parallel()

	// A 401 or a 503 from an ingress in front of Prometheus returns HTML, and
	// "invalid character '<'" on its own has sent more than one person looking
	// for a PromQL syntax error.
	srv, _ := promServer(t, http.StatusUnauthorized, `<html><body>401</body></html>`)
	p := &PrometheusProvider{Address: srv.URL}

	_, err := p.Query(context.Background(), "q")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to name the HTTP status", err)
	}
}

func TestPrometheusRejectsNonVectorResults(t *testing.T) {
	t.Parallel()

	// A matrix or a scalar means the query is not the shape a gate needs.
	// Coercing one would be guessing at the author's intent.
	srv, _ := promServer(t, http.StatusOK,
		`{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	p := &PrometheusProvider{Address: srv.URL}

	if _, err := p.Query(context.Background(), "q[5m]"); err == nil {
		t.Fatal("expected a matrix result to be rejected")
	}
}

func TestPrometheusRequiresAnAddress(t *testing.T) {
	t.Parallel()

	// Not defaulted anywhere: a wrong-but-plausible default would make every
	// canary report Error for a reason nobody would think to check.
	p := &PrometheusProvider{}
	_, err := p.Query(context.Background(), "q")
	if err == nil {
		t.Fatal("expected an error with no address configured")
	}
	if !strings.Contains(err.Error(), "provider.address") {
		t.Errorf("error = %v, want it to name the field to set", err)
	}
}

func TestPrometheusUsesPOST(t *testing.T) {
	t.Parallel()

	// Analysis queries carry label selectors and nested aggregations and
	// comfortably exceed the URL length some proxies enforce — and a truncated
	// query does not error, it evaluates to something else.
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, vectorBody(vectorSample("1")))
	}))
	defer srv.Close()

	p := &PrometheusProvider{Address: srv.URL}
	if _, err := p.Query(context.Background(), "q"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
}

func TestPrometheusHonoursTheTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	p := &PrometheusProvider{Address: srv.URL, Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := p.Query(context.Background(), "q"); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("query took %v; the timeout was not applied", elapsed)
	}
}

func TestPrometheusTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, vectorBody(vectorSample("1")))
	}))
	defer srv.Close()

	p := &PrometheusProvider{Address: srv.URL + "/"}
	if _, err := p.Query(context.Background(), "q"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if path != "/api/v1/query" {
		t.Errorf("path = %q, want /api/v1/query", path)
	}
}
