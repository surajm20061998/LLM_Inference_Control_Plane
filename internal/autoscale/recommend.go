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

// Package autoscale decides how many serving pods should exist.
//
// Like internal/canary it is a pure function with no I/O: Recommend takes a
// value and returns a value, the clock arrives as a field, and the decision
// leaves as a field for the caller to carry out. The reasoning is the same —
// autoscaling bugs are timing bugs, and reproducing "the fleet scaled up, then
// the metric recovered inside the stabilization window" against a real cluster
// takes minutes of wall time and a lot of luck. Here it is six lines of a table
// test.
//
// # The replica-ownership contract
//
// Recommend produces a desired TOTAL. canary.Split distributes that total
// between the primary and canary variants. Two functions, one input, no fight.
//
// An autoscaler and a rollout controller that each believe they own the replica
// count is the classic way for a progressive delivery system to oscillate
// forever, and it is the bug Argo Rollouts spent several releases resolving.
// The separation is why a canary in flight keeps its weight ratio while the
// fleet grows underneath it.
package autoscale

import (
	"math"
	"slices"
	"time"
)

// Direction is which way a recommendation moves.
type Direction string

const (
	// DirectionNone means the recommendation equals the current count.
	DirectionNone Direction = "None"
	// DirectionUp means more replicas.
	DirectionUp Direction = "Up"
	// DirectionDown means fewer replicas.
	DirectionDown Direction = "Down"
)

// maxHistory bounds the recommendation window stored in status.
//
// Unbounded history in a status field is an etcd leak with a slow fuse: the
// object grows on every poll, every watcher re-reads it, and the failure
// surfaces weeks later as an oversized resource nobody can update. The cap is
// well above what the longest sensible stabilization window needs at the
// default poll interval.
const maxHistory = 32

// Config is the resolved, fully-defaulted autoscaler configuration.
type Config struct {
	MinReplicas int32
	MaxReplicas int32

	// Target is the desired per-pod value of the tracked metric.
	Target float64

	// Tolerance is the fraction by which the ratio may differ from 1.0 before
	// anything happens. See the field doc on AutoscalingSpec.Tolerance.
	Tolerance float64

	ScaleUpStabilization   time.Duration
	ScaleDownStabilization time.Duration
}

// Observation is the measured signal for one poll.
type Observation struct {
	// Value is the metric aggregated across the WHOLE fleet, both variants.
	//
	// Fleet-wide on purpose: an autoscaler decides how much capacity exists and
	// a rollout decides how it is split, so measuring one variant would size
	// the fleet from a fraction of its load — and during a canary that fraction
	// changes at every step, which would make the autoscaler react to the
	// rollout rather than to demand.
	Value float64

	// Valid is false when there was no usable measurement: no series, NaN, an
	// infinity, or a provider that could not be reached.
	//
	// Deliberately distinct from a Value of zero. A measured zero means "no
	// work is queued" and is a reason to scale DOWN; an absent measurement
	// means nothing at all, and acting on it would size the fleet from a
	// number nobody produced.
	Valid bool

	// Reason explains an invalid observation, for the status message.
	Reason string
}

// Recommendation is one raw decision, before stabilization.
type Recommendation struct {
	Time     time.Time
	Replicas int32
}

// Input is everything Recommend needs.
type Input struct {
	// Now is the current time. Injected, never read from the clock.
	Now time.Time

	// Current is .spec.replicas as it stands.
	Current int32

	// ReadyReplicas is how many pods are actually serving.
	//
	// It does two jobs, and it is worth being precise about which, because the
	// obvious third job is one it does not do.
	//
	//  1. It is the divisor for the per-pod figure the TOLERANCE BAND compares
	//     against the target, and the guard against dividing by zero.
	//  2. It is what gets reported, so a human reading the status sees the same
	//     number the decision was made from.
	//
	// It does NOT change the size of the recommendation. Work through it:
	// ceil(ready x ((value/ready) / target)) is just ceil(value / target) — the
	// ready count cancels. That is the same reduction the HorizontalPodAutoscaler's
	// arithmetic makes, and far from being a curiosity it is the property that
	// keeps a fleet from chasing its own tail. Mid-scale-up, with 2 of 6 pods
	// ready, the queued demand is whatever it is; the recommendation derived
	// from it stays put while the remaining pods finish loading their weights,
	// instead of climbing every poll because the ready pods look overloaded.
	ReadyReplicas int32

	Observation Observation
	Config      Config

	// History is the recent recommendations, oldest first, restored from
	// status.
	History []Recommendation
}

// Output is the decision. Nothing here has happened yet.
type Output struct {
	// Replicas is what .spec.replicas should be.
	Replicas int32

	// Changed reports whether Replicas differs from Input.Current.
	Changed bool

	// Direction is which way it moved.
	Direction Direction

	// Raw is the unstabilized recommendation, for status and for debugging the
	// difference between "the metric says 6" and "stabilization says stay at 4".
	Raw int32

	// PerPod is the measured value divided by ready pods — the number actually
	// compared against the target.
	PerPod float64

	// Valid reports whether the decision rests on a real measurement.
	Valid bool

	// History is the pruned, appended history to persist.
	History []Recommendation

	Reason  string
	Message string
}

// Recommend computes the desired replica count.
//
// The algorithm is the HorizontalPodAutoscaler's, deliberately and almost
// literally. Reinventing it would mean rediscovering the tolerance band and the
// asymmetric stabilization windows the hard way, and an operator whose scaling
// behaviour surprises someone who already knows how an HPA behaves is a worse
// operator regardless of how clever the alternative is.
//
// The order of the guards is the substance:
//
//  1. No usable measurement ⇒ HOLD. Never scale on data nobody produced.
//  2. No ready pods ⇒ HOLD. The ratio's denominator would be zero, and a fleet
//     with nothing serving has no per-pod load to speak of.
//  3. Inside the tolerance band ⇒ HOLD. Without this, 4.1 against a target of 4
//     is a scale event and so is 3.9 fifteen seconds later, forever.
//  4. Otherwise scale by the usage ratio, clamp to [min, max], then stabilize.
func Recommend(in Input) Output {
	cfg := in.Config
	current := clamp(in.Current, cfg.MinReplicas, cfg.MaxReplicas)

	hold := func(reason, message string) Output {
		return Output{
			Replicas:  current,
			Changed:   current != in.Current,
			Direction: directionOf(in.Current, current),
			Raw:       current,
			Valid:     false,
			History:   prune(in.History, in.Now, cfg),
			Reason:    reason,
			Message:   message,
		}
	}

	if !in.Observation.Valid {
		reason := in.Observation.Reason
		if reason == "" {
			reason = "no usable measurement"
		}
		// Note this still returns `current` CLAMPED, so a spec whose bounds
		// changed while the metric was unavailable is still honoured. Holding
		// literally would leave a fleet outside its own declared limits for as
		// long as the outage lasted.
		return hold(ReasonNoMetrics,
			"Holding at "+itoa(current)+" replicas: "+reason)
	}

	if in.ReadyReplicas < 1 {
		return hold(ReasonNoMetrics,
			"Holding at "+itoa(current)+" replicas: no pods are ready, so there is no per-pod load to measure")
	}

	if cfg.Target <= 0 {
		// A target of zero would make every ratio infinite. CEL should prevent
		// it; holding is the safe reading if one slips through, because the
		// alternative is scaling straight to maxReplicas.
		return hold(ReasonNoMetrics,
			"Holding at "+itoa(current)+" replicas: the autoscaling target must be greater than zero")
	}

	perPod := in.Observation.Value / float64(in.ReadyReplicas)
	ratio := perPod / cfg.Target

	out := Output{
		PerPod:  perPod,
		Valid:   true,
		History: nil,
	}

	if math.Abs(ratio-1.0) <= cfg.Tolerance {
		out.Replicas = current
		out.Raw = current
		out.Changed = current != in.Current
		out.Direction = directionOf(in.Current, current)
		out.History = prune(append(slices.Clone(in.History),
			Recommendation{Time: in.Now, Replicas: current}), in.Now, cfg)
		out.Reason = ReasonWithinTolerance
		out.Message = "Holding at " + itoa(current) + " replicas: " +
			formatFloat(perPod) + " per pod is within " + formatFloat(cfg.Tolerance*100) +
			"% of the target " + formatFloat(cfg.Target)
		return out
	}

	// Ceiling, matching the HPA: a fractional pod cannot serve, and rounding
	// down would leave the fleet knowingly under target.
	//
	// Written as ready x ratio to mirror the HPA's own formulation, though it
	// reduces to value/target — see the note on Input.ReadyReplicas for why
	// that reduction is a feature rather than a redundancy.
	//
	// The intermediate is bounded before the int32 conversion. A NaN or an
	// infinity reaching here — which means the provider layer let one through —
	// converts to an unspecified int32, and the resulting replica count would
	// be arbitrary rather than merely wrong.
	scaled := float64(in.ReadyReplicas) * ratio
	raw := clamp(ceilToInt32(scaled), cfg.MinReplicas, cfg.MaxReplicas)
	out.Raw = raw

	history := prune(append(slices.Clone(in.History),
		Recommendation{Time: in.Now, Replicas: raw}), in.Now, cfg)
	out.History = history

	stabilized := stabilize(raw, current, history, in.Now, cfg)
	out.Replicas = clamp(stabilized, cfg.MinReplicas, cfg.MaxReplicas)
	out.Changed = out.Replicas != in.Current
	out.Direction = directionOf(current, out.Replicas)

	switch {
	case out.Replicas > current:
		out.Reason = ReasonScaleUp
		out.Message = "Scaling up to " + itoa(out.Replicas) + " replicas: " +
			formatFloat(perPod) + " per pod against a target of " + formatFloat(cfg.Target)
	case out.Replicas < current:
		out.Reason = ReasonScaleDown
		out.Message = "Scaling down to " + itoa(out.Replicas) + " replicas: " +
			formatFloat(perPod) + " per pod against a target of " + formatFloat(cfg.Target)
	default:
		out.Reason = ReasonStabilized
		out.Message = "Holding at " + itoa(current) + " replicas: the metric suggests " +
			itoa(raw) + ", but the stabilization window has not cleared"
	}

	return out
}

// Decision reasons, mirrored into the AutoscalingReady condition.
const (
	ReasonNoMetrics       = "NoMetrics"
	ReasonWithinTolerance = "WithinTolerance"
	ReasonScaleUp         = "ScaleUp"
	ReasonScaleDown       = "ScaleDown"
	ReasonStabilized      = "Stabilized"
)

// stabilize damps a raw recommendation using the recent history.
//
// # Why the two directions use opposite aggregations
//
// This is the part that looks backwards on first reading, and it is the HPA's
// behaviour for a good reason.
//
//   - Scaling UP takes the MINIMUM recommendation over the up window. A single
//     spike therefore cannot add capacity on its own; the load has to have been
//     high for the whole window.
//   - Scaling DOWN takes the MAXIMUM over the down window. A single quiet
//     moment cannot remove capacity; the load has to have been low for the
//     whole window.
//
// Both rules are "be conservative about changing", and because the two windows
// have different lengths — zero up, five minutes down by default — the fleet
// grows promptly and shrinks reluctantly. For a workload whose pods take
// minutes to load a model, that asymmetry is the difference between an
// autoscaler that helps and one that spends its time recovering from itself.
func stabilize(raw, current int32, history []Recommendation, now time.Time, cfg Config) int32 {
	if raw > current {
		if cfg.ScaleUpStabilization <= 0 {
			return raw
		}
		stabilized := minOver(history, now.Add(-cfg.ScaleUpStabilization))
		// Never let an UP decision turn into a scale-down as a side effect: the
		// window's minimum can sit below the current count if load was recently
		// much lower, and acting on that would shrink a fleet at the exact
		// moment the metric is asking for more.
		return max(stabilized, current)
	}

	if raw < current {
		if cfg.ScaleDownStabilization <= 0 {
			return raw
		}
		stabilized := maxOver(history, now.Add(-cfg.ScaleDownStabilization))
		// The mirror image: a DOWN decision must not become a scale-up.
		return min(stabilized, current)
	}

	return current
}

// minOver returns the smallest recommendation at or after cutoff.
func minOver(history []Recommendation, cutoff time.Time) int32 {
	out := int32(math.MaxInt32)
	found := false
	for _, r := range history {
		if r.Time.Before(cutoff) {
			continue
		}
		found = true
		out = min(out, r.Replicas)
	}
	if !found {
		return 0
	}
	return out
}

// maxOver returns the largest recommendation at or after cutoff.
func maxOver(history []Recommendation, cutoff time.Time) int32 {
	out := int32(0)
	found := false
	for _, r := range history {
		if r.Time.Before(cutoff) {
			continue
		}
		found = true
		out = max(out, r.Replicas)
	}
	if !found {
		return math.MaxInt32
	}
	return out
}

// prune drops history older than the longest stabilization window and caps the
// result.
//
// Both bounds are needed. The time bound is what makes stabilization mean what
// it says; the count bound is what stops a very long window plus a very short
// poll interval from writing an unbounded list into status.
func prune(history []Recommendation, now time.Time, cfg Config) []Recommendation {
	window := max(cfg.ScaleUpStabilization, cfg.ScaleDownStabilization)
	cutoff := now.Add(-window)

	out := make([]Recommendation, 0, len(history))
	for _, r := range history {
		// Entries stamped in the future are dropped rather than kept. They can
		// only come from a clock that moved backwards, and keeping one would
		// pin the stabilization window open until real time caught up.
		if r.Time.Before(cutoff) || r.Time.After(now) {
			continue
		}
		out = append(out, r)
	}

	if len(out) > maxHistory {
		out = out[len(out)-maxHistory:]
	}
	return out
}

// ceilToInt32 rounds up into an int32, saturating rather than overflowing.
//
// int32(math.Ceil(x)) is undefined in Go for a NaN or for anything outside the
// int32 range, and "undefined" here means an arbitrary replica count written to
// a live spec. Saturating keeps the value nonsensical-but-bounded, and the
// caller's clamp then brings it back inside the configured limits.
func ceilToInt32(v float64) int32 {
	switch {
	case math.IsNaN(v):
		return 0
	case v <= 0:
		return 0
	case v >= math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(math.Ceil(v))
	}
}

// clamp bounds n to [lo, hi], tolerating an inverted range.
func clamp(n, lo, hi int32) int32 {
	lo = max(lo, 1)
	if hi < lo {
		// CEL rejects maxReplicas < minReplicas, so this is defence in depth
		// for an object written before that rule existed. Preferring the floor
		// keeps capacity rather than removing it.
		hi = lo
	}
	return min(max(n, lo), hi)
}

// directionOf reports which way a change moves.
func directionOf(from, to int32) Direction {
	switch {
	case to > from:
		return DirectionUp
	case to < from:
		return DirectionDown
	default:
		return DirectionNone
	}
}

// itoa formats a small int32 for a message.
func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// formatFloat renders a measured value for a human-readable message, trimming
// the trailing noise that a raw %f produces.
func formatFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 0) {
		return "Inf"
	}
	s := trimZeros(v)
	return s
}

// trimZeros formats with two decimals and removes a trailing ".00" or "0".
func trimZeros(v float64) string {
	out := formatFixed(v)
	for len(out) > 0 && out[len(out)-1] == '0' {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && out[len(out)-1] == '.' {
		out = out[:len(out)-1]
	}
	if out == "" || out == "-" {
		return "0"
	}
	return out
}

// formatFixed renders v with two decimal places.
func formatFixed(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	whole := int64(v)
	frac := int64(math.Round((v - float64(whole)) * 100))
	if frac >= 100 {
		whole++
		frac -= 100
	}
	out := itoa64(whole) + "." + twoDigits(frac)
	if neg {
		out = "-" + out
	}
	return out
}

func itoa64(n int64) string {
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

func twoDigits(n int64) string {
	if n < 10 {
		return "0" + itoa64(n)
	}
	return itoa64(n)
}
