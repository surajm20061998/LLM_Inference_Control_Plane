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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PrometheusProvider queries a Prometheus HTTP API.
//
// It speaks the /api/v1/query wire format directly rather than importing
// github.com/prometheus/client_golang/api. That client is perfectly good, but
// it pulls a large transitive graph into an operator that needs exactly one
// endpoint from it, and its model types (model.Value, a sum type over four
// shapes) would have to be flattened into this package's Sample anyway. Forty
// lines of JSON decoding is the cheaper trade, and it makes every failure mode
// visible in one file.
type PrometheusProvider struct {
	// Address is the base URL, e.g. http://prometheus-operated.monitoring.svc:9090
	Address string

	// Client is the HTTP client. Nil means a default with Timeout applied.
	Client *http.Client

	// Timeout bounds a single query.
	Timeout time.Duration
}

var _ Provider = (*PrometheusProvider)(nil)

// maxResponseBytes caps how much of a response is read.
//
// A query with a badly-chosen aggregation can legitimately return tens of
// thousands of series, and a controller that decoded all of them would allocate
// megabytes per analysis round. The cap is far above any correct result — a
// gate query returns one series — so hitting it is itself a signal that the
// query is wrong.
const maxResponseBytes = 4 << 20

// promResponse is the subset of Prometheus's response envelope we read.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			// Value is [unixTimestampFloat, "stringValue"]. Prometheus encodes
			// the sample value as a STRING, not a number, precisely so that
			// "NaN", "+Inf" and "-Inf" survive JSON — which has no way to
			// represent them. Decoding into a float64 here would fail on
			// exactly the inputs this package most needs to distinguish.
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Warnings  []string `json:"warnings"`
}

// Query implements Provider.
func (p *PrometheusProvider) Query(ctx context.Context, query string) (Sample, error) {
	if strings.TrimSpace(p.Address) == "" {
		// Not defaulted anywhere. A wrong-but-plausible default address would
		// make every canary report Error for a reason nobody would think to
		// check, and unlike most misconfigurations this one is invisible until
		// a rollout is already in flight.
		return Sample{}, errors.New("no metric provider address configured: " +
			"set spec.rollout.canary.analysis.provider.address")
	}

	endpoint, err := url.Parse(strings.TrimRight(p.Address, "/") + "/api/v1/query")
	if err != nil {
		return Sample{}, fmt.Errorf("parsing provider address %q: %w", p.Address, err)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	form := url.Values{}
	form.Set("query", query)

	// POST, not GET. Analysis queries carry label selectors and nested
	// aggregations and comfortably exceed the URL length some proxies enforce;
	// a truncated query does not error, it evaluates to something else.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return Sample{}, fmt.Errorf("building query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return Sample{}, fmt.Errorf("querying %s: %w", p.Address, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Sample{}, fmt.Errorf("reading response from %s: %w", p.Address, err)
	}

	var decoded promResponse
	if jsonErr := json.Unmarshal(body, &decoded); jsonErr != nil {
		// Include the status code: a 401 or a 503 from an ingress in front of
		// Prometheus returns HTML, and "invalid character '<'" on its own has
		// sent more than one person looking for a PromQL syntax error.
		return Sample{}, fmt.Errorf("decoding response from %s (HTTP %d): %w",
			p.Address, resp.StatusCode, jsonErr)
	}

	if decoded.Status != statusSuccess {
		msg := decoded.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return Sample{}, fmt.Errorf("query rejected by %s: %s (%s)", p.Address, msg, decoded.ErrorType)
	}

	if decoded.Data.ResultType != "vector" {
		// A matrix or a scalar means the query is not the shape a gate needs.
		// Coercing one would be guessing at the author's intent.
		return Sample{}, fmt.Errorf("query returned a %q result; an instant vector is required",
			decoded.Data.ResultType)
	}

	results := decoded.Data.Result
	if len(results) == 0 {
		// Count 0 with a NIL error, deliberately. An empty result is a fact
		// about the data, not a failure of the query, and the two lead to
		// different verdicts — Inconclusive versus Error.
		return Sample{Count: 0}, nil
	}
	if len(results) > 1 {
		// Reported through Count so Evaluate can produce the Error verdict with
		// a message naming the real problem, rather than this layer inventing
		// one.
		return Sample{Count: len(results)}, nil
	}

	value, err := scalarFrom(results[0].Value)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Value: value, Count: 1}, nil
}

// scalarFrom extracts the sample value from Prometheus's [timestamp, "value"]
// pair.
//
// ParseFloat is what makes the special values work: it accepts LiteralNaN,
// LiteralPosInf and LiteralNegInf and returns exactly those float64 values,
// which is why the wire format uses a string in the first place. Evaluate then classifies them, so
// they must survive this far rather than being rejected here.
func scalarFrom(pair []json.RawMessage) (float64, error) {
	if len(pair) != 2 {
		return 0, fmt.Errorf("malformed sample: expected [timestamp, value], got %d elements", len(pair))
	}

	var raw string
	if err := json.Unmarshal(pair[1], &raw); err != nil {
		return 0, fmt.Errorf("decoding sample value: %w", err)
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("sample value %q is not a number: %w", raw, err)
	}
	return v, nil
}
