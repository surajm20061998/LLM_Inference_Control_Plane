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
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ScriptedProvider replays a fixed sequence of answers.
//
// # Why a scripted fake and not a live Prometheus
//
// The assertion a canary test actually needs to make is "rollback fires on
// exactly the SECOND failure, not the first and not the third". Against a live
// Prometheus that is not merely awkward, it is impossible: you cannot make a
// real metric fail precisely twice and then recover, and any attempt produces a
// test that passes or fails depending on scrape timing.
//
// With a script — pass, pass, fail, fail — the whole rollout is deterministic,
// runs in microseconds, and asserts the exact behaviour the failure threshold
// is supposed to have. The real Prometheus provider is exercised separately
// against its own wire format, where the interesting cases are JSON shapes
// rather than rollout decisions.
type ScriptedProvider struct {
	mu sync.Mutex

	// steps is the queue of answers, consumed one per Query.
	steps []Step

	// pos is how many answers have been served.
	pos int

	// Repeat makes the script cycle rather than run out.
	//
	// Off by default. A script that silently repeats hides an off-by-one in the
	// test's own expectations: a rollout that queries five times against a
	// three-step script should say so, not quietly re-serve step one.
	Repeat bool

	// Queries records every query string, in order, so a test can assert on
	// the PromQL a rollout decision was made from.
	Queries []string
}

// Step is one scripted answer.
type Step struct {
	// Value is the sample value. Ignored when Err is set or Count is 0.
	Value float64

	// Count is how many series to report. Defaults to 1; set 0 for an empty
	// result and >1 for the "query did not aggregate" case.
	Count *int

	// Err makes the query fail at the transport level.
	Err error

	// Match, when set, restricts this step to queries CONTAINING it.
	//
	// The reason this exists: a single analysis round issues several queries —
	// a traffic-rate gate, then one per metric, then the primary side of any
	// ratio — and a positional script would depend on the order the analyzer
	// happens to issue them in. Matching on a substring lets a test say "the
	// error-rate query returns 0.2" without also asserting an implementation
	// detail that is free to change.
	Match string
}

// NewScripted builds a provider that serves steps in order.
func NewScripted(steps ...Step) *ScriptedProvider {
	return &ScriptedProvider{steps: steps}
}

// Pass is a step returning a single healthy value.
func Pass(value float64) Step { return Step{Value: value} }

// Empty is a step returning no series at all — the "not a measurement of zero"
// case.
func Empty() Step { zero := 0; return Step{Count: &zero} }

// Multi is a step returning n series, i.e. a query that failed to aggregate.
func Multi(n int) Step { return Step{Count: &n} }

// Fail is a step whose transport failed.
func Fail(err error) Step { return Step{Err: err} }

// For restricts a step to queries containing substr.
func (s Step) For(substr string) Step { s.Match = substr; return s }

var _ Provider = (*ScriptedProvider)(nil)

// Query implements Provider.
func (p *ScriptedProvider) Query(_ context.Context, query string) (Sample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Queries = append(p.Queries, query)

	step, ok := p.next(query)
	if !ok {
		// Running off the end of a script is a test bug, and reporting it as a
		// provider error would let the canary logic absorb it as an Error
		// verdict and carry on — turning a broken test into a passing one.
		return Sample{}, fmt.Errorf(
			"scripted provider exhausted after %d answers; query %d was: %s",
			len(p.steps), p.pos+1, query)
	}

	if step.Err != nil {
		return Sample{}, step.Err
	}

	count := 1
	if step.Count != nil {
		count = *step.Count
	}
	return Sample{Value: step.Value, Count: count}, nil
}

// next selects the answer for a query, honouring Match.
func (p *ScriptedProvider) next(query string) (Step, bool) {
	// Matched steps are consulted first and are NOT consumed: they describe a
	// standing answer for one kind of query ("the traffic gate always reports
	// 5 req/s") rather than a position in a sequence.
	for _, s := range p.steps {
		if s.Match != "" && strings.Contains(query, s.Match) {
			return s, true
		}
	}

	for p.pos < len(p.steps) {
		s := p.steps[p.pos]
		p.pos++
		if s.Match == "" {
			return s, true
		}
	}

	if p.Repeat && len(p.steps) > 0 {
		// Only positional answers can repeat here: matching standing answers
		// were already checked above. An all-matched script has no fallback,
		// so recursing from its start would never terminate for a new query.
		for i, s := range p.steps {
			if s.Match == "" {
				p.pos = i + 1
				return s, true
			}
		}
	}
	return Step{}, false
}

// Remaining reports how many unmatched steps have not been served.
//
// A test that finishes with steps left over usually expected more analysis
// rounds than actually ran, which is the same class of bug as one that ran off
// the end — and far easier to miss, because nothing fails.
func (p *ScriptedProvider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	remaining := 0
	for i := p.pos; i < len(p.steps); i++ {
		if p.steps[i].Match == "" {
			remaining++
		}
	}
	return remaining
}

// ErrProviderDown is a convenient transport failure for tests that need one.
var ErrProviderDown = errors.New("connection refused: the metric provider is unreachable")
