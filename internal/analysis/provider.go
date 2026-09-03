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

// Package analysis turns metric queries into rollout verdicts.
//
// It is split into three pieces with deliberately different testability:
//
//   - Provider is the I/O boundary — one method, one query, one answer.
//   - builtin.go renders PromQL and is a pure string function.
//   - Evaluate turns an answer into a verdict and is a pure function over the
//     full space of things a metric backend can return, including the ones that
//     are not numbers.
//
// The split exists because the interesting bugs are all in the third part, and
// they are impossible to provoke against a live Prometheus. "What happens when
// the query returns NaN because every bucket was empty" is a two-line table
// case here and an afternoon of contrivance in a cluster.
package analysis

import (
	"context"
	"time"
)

// The three non-numeric values Prometheus can return, spelled as they appear on
// the wire and in status.
//
// They are constants because they cross three boundaries: the provider parses
// them, Evaluate classifies them, and status renders them back. A typo in any
// one of the three would produce a value that reads correctly to a human and
// matches nothing in code.
const (
	LiteralNaN    = "NaN"
	LiteralPosInf = "+Inf"
	LiteralNegInf = "-Inf"
	statusSuccess = "success"
)

// Sample is what a metric backend returned for one query.
//
// Count is carried alongside Value because "how many series came back" is a
// verdict-changing fact, not a detail:
//
//   - 0 means the query matched nothing. That is Inconclusive — not zero, and
//     emphatically not Pass. A rollout gate that reads an empty result as a
//     healthy zero promotes every release against a broken exporter.
//   - 1 is the expected case.
//   - more than 1 means the query did not aggregate. Picking the first is how a
//     gate ends up measuring an arbitrary pod, so it is reported as an Error.
type Sample struct {
	// Value is the scalar result. Meaningful only when Count == 1.
	Value float64

	// Count is how many series the query returned.
	Count int
}

// Provider queries a metric backend.
//
// One method, taking a finished query string. Query construction lives in
// builtin.go rather than behind this interface so that the PromQL a rollout
// decision rests on can be asserted character by character in a unit test,
// without a backend of any kind.
type Provider interface {
	// Query evaluates an instant query.
	//
	// It returns an error only for TRANSPORT-level problems: unreachable
	// backend, timeout, malformed response, a rejected query. A query that
	// succeeds and matches nothing is a Sample with Count 0 and a nil error,
	// because those two outcomes lead to different verdicts and collapsing them
	// would make a monitoring outage indistinguishable from a quiet service.
	Query(ctx context.Context, query string) (Sample, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, query string) (Sample, error)

// Query implements Provider.
func (f ProviderFunc) Query(ctx context.Context, query string) (Sample, error) {
	return f(ctx, query)
}

// QueryContext is the substitution environment for a metric query.
type QueryContext struct {
	// Model is spec.model.name — the `model` label on every emitted series.
	Model string

	// Variant is "primary" or "canary".
	Variant string

	// Window is the PromQL lookback, already rendered ("60s").
	Window string
}

// PromDuration renders a Go duration in Prometheus's duration syntax.
//
// time.Duration.String() produces "1m0s" and "1.5s". Prometheus rejects the
// first outright and truncates the second, so a 1.5s window would silently
// become 1s — a difference that matters when the window is being sized against
// a scrape interval.
func PromDuration(d time.Duration) string {
	secs := int64(d.Round(time.Second) / time.Second)
	secs = max(secs, 1)
	return formatInt(secs) + "s"
}

// formatInt renders a positive int64 without importing strconv into a file
// whose only other job is string templating.
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
