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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// RolloutStrategyType selects how pod template changes are rolled out.
// +kubebuilder:validation:Enum=RollingUpdate;Recreate;Canary
type RolloutStrategyType string

const (
	// RolloutRollingUpdate replaces pods incrementally, keeping the service up.
	RolloutRollingUpdate RolloutStrategyType = "RollingUpdate"

	// RolloutRecreate tears down all pods before creating replacements.
	RolloutRecreate RolloutStrategyType = "Recreate"

	// RolloutCanary runs the new revision alongside the old one, shifts a
	// growing share of traffic to it, and promotes or rolls back based on
	// measured metrics.
	//
	// This is the strategy the project exists to demonstrate. Kubernetes
	// already does rolling updates correctly and this operator delegates them
	// to the Deployment controller unchanged; what a Deployment cannot do is
	// notice that the new pods are HEALTHY BUT WORSE — passing their readiness
	// probes while serving 3x the latency — and undo itself.
	RolloutCanary RolloutStrategyType = "Canary"
)

// RolloutSpec controls rollout behaviour.
//
// The mechanics of a rolling update are delegated to the child Deployment,
// which already implements them correctly. What this operator adds on top is
// the behaviour a Deployment does NOT have: a Deployment whose rollout stalls
// stays stalled forever, whereas exceeding ProgressDeadline here reverts to the
// last revision that was known good — and, under the Canary strategy, a
// revision that rolls out perfectly well but degrades a measured SLI is
// reverted too.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'Canary' || has(self.canary)",message="spec.rollout.canary is required when spec.rollout.type is Canary"
type RolloutSpec struct {
	// Type selects the update strategy.
	// +optional
	// +kubebuilder:default=RollingUpdate
	Type RolloutStrategyType `json:"type,omitempty"`

	// MaxSurge is the number or percentage of pods that may exist above the
	// desired count during an update. Ignored when Type is Recreate.
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// MaxUnavailable is the number or percentage of pods that may be
	// unavailable during an update. Ignored when Type is Recreate.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// ProgressDeadline is how long a rollout may make no progress before it is
	// declared failed.
	// +optional
	// +kubebuilder:default="600s"
	ProgressDeadline *metav1.Duration `json:"progressDeadline,omitempty"`

	// AutoRollback reverts to the last known-good revision when the deadline is
	// exceeded.
	//
	// This applies to EVERY strategy, not just Canary. A stalled RollingUpdate
	// is the more common failure in practice — a bad image reference, a missing
	// secret, a model file that will not parse — and a Deployment left in that
	// state stays there indefinitely with no further signal.
	//
	// +optional
	// +kubebuilder:default=true
	AutoRollback *bool `json:"autoRollback,omitempty"`

	// Canary configures progressive delivery. Required when Type is Canary and
	// ignored otherwise.
	// +optional
	Canary *CanarySpec `json:"canary,omitempty"`
}

// CanarySpec configures a metric-gated progressive rollout.
//
// The vocabulary is deliberately Flagger's — stepWeight, maxWeight,
// failureThreshold, thresholdRange — because it is the vocabulary the
// progressive-delivery ecosystem converged on and inventing a synonym for each
// term would make every existing runbook wrong. What differs is what the
// operator does with them; see AnalysisSpec on why analysis has four verdicts
// rather than two.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.stepWeight) && has(self.stepWeights))",message="set at most one of stepWeight and stepWeights"
// +kubebuilder:validation:XValidation:rule="!has(self.stepWeights) || self.stepWeights.all(w, w >= 1 && w <= 100)",message="every entry in stepWeights must be between 1 and 100"
type CanarySpec struct {
	// StepWeight is the traffic share added at each step: 20 produces the
	// ladder 20, 40, 60, ... up to MaxWeight. Defaults to 20 when unset.
	//
	// Deliberately NOT given a kubebuilder default, even though every other
	// optional field here has one. A defaulted field is always "present" to
	// CEL, which would make the mutual-exclusion rule above unsatisfiable: a
	// user who set only stepWeights would be rejected for also setting a
	// stepWeight they never wrote. The default lives in StepLadder instead,
	// where it can coexist with the validation that matters more.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	StepWeight *int32 `json:"stepWeight,omitempty"`

	// StepWeights is an explicit, strictly increasing ladder, e.g. [5, 20, 50].
	// It overrides StepWeight and MaxWeight entirely.
	//
	// An explicit ladder exists because a uniform step is the wrong shape for
	// most real rollouts: the interesting information arrives in the first few
	// percent of traffic, so a small first step followed by larger ones spends
	// the risk budget where it buys the most.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=20
	StepWeights []int32 `json:"stepWeights,omitempty"`

	// MaxWeight is the traffic share at which the canary is promoted rather
	// than stepped further. Ignored when StepWeights is set.
	//
	// It is deliberately below 100 by default. Once a canary is carrying half
	// the traffic and every check has passed, additional steps buy very little
	// information and cost real time; promotion at that point is a decision,
	// not a gamble.
	//
	// +optional
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxWeight *int32 `json:"maxWeight,omitempty"`

	// Analysis configures the metric checks that gate each step.
	// +optional
	Analysis AnalysisSpec `json:"analysis,omitempty"`

	// TrafficRouting selects how traffic is divided between the variants.
	// +optional
	TrafficRouting TrafficRoutingSpec `json:"trafficRouting,omitempty"`

	// Scale decouples the canary's replica count from its traffic weight.
	//
	// Borrowed from Argo Rollouts' canaryScale, and it earns its place for
	// inference specifically: a model server has a long, expensive warm-up, so
	// a canary that is scaled up in lockstep with its weight spends the first
	// analysis window of every step loading a model rather than serving. Fixing
	// the replica count up front means the canary is warm before it is
	// measured.
	//
	// +optional
	Scale *CanaryScaleSpec `json:"scale,omitempty"`

	// RequireApproval pauses before the final promotion until an operator sets
	// the llmcp.io/promote annotation to "true".
	//
	// The gate is on PROMOTION, not on each step. A human asked to approve
	// every 20% increment stops reading and starts clicking, which is worse
	// than no gate at all; asked once, at the point of no return, they actually
	// look.
	//
	// +optional
	// +kubebuilder:default=false
	RequireApproval *bool `json:"requireApproval,omitempty"`
}

// TrafficRoutingMode selects the traffic-splitting mechanism.
// +kubebuilder:validation:Enum=Replica
type TrafficRoutingMode string

const (
	// TrafficRoutingReplica splits traffic by REPLICA COUNT: both variants sit
	// behind one Service, and the share each receives is approximately its
	// share of the ready pods.
	//
	// The approximation is real and is reported honestly. kube-proxy
	// load-balances per CONNECTION, not per request, so a client using HTTP
	// keep-alive — which every OpenAI SDK does by default — pins itself to one
	// pod for its whole session. At a requested 20% with 8 concurrent clients,
	// the realised split is some multiple of 1/8, stable and wrong for the
	// entire analysis window.
	//
	// This project's feasibility spike measured the effect at a skew of 1.02
	// with keep-alive on and 1.09 with it off, which is well inside the noise —
	// so replica-based splitting stands, and the load generator disables
	// keep-alive as the documented mitigation. status.canary.currentWeight
	// always reports the QUANTIZED weight actually achieved, never the one that
	// was asked for.
	TrafficRoutingReplica TrafficRoutingMode = "Replica"
)

// TrafficRoutingSpec configures how traffic is divided between variants.
type TrafficRoutingSpec struct {
	// Mode selects the splitting mechanism.
	// +optional
	// +kubebuilder:default=Replica
	Mode TrafficRoutingMode `json:"mode,omitempty"`
}

// CanaryScaleSpec decouples canary replicas from canary traffic weight.
//
// Exactly one member may be set.
//
// +kubebuilder:validation:XValidation:rule="[has(self.replicas), has(self.matchTrafficWeight)].filter(x, x).size() <= 1",message="set at most one of replicas and matchTrafficWeight"
type CanaryScaleSpec struct {
	// Replicas pins the canary to a fixed number of pods for the whole
	// rollout, regardless of its traffic weight.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// MatchTrafficWeight sizes the canary in proportion to its weight. This is
	// the default behaviour.
	// +optional
	MatchTrafficWeight *bool `json:"matchTrafficWeight,omitempty"`
}

// Resolved accessors. Each returns the effective value including defaults, so
// that callers never repeat a nil check and never disagree about a default.

// StepLadder returns the strictly increasing weight ladder this canary climbs.
//
// It is computed rather than stored so that a spec edited mid-rollout produces
// a ladder consistent with the new spec, and so the defaulting rule lives in
// exactly one place.
func (c *CanarySpec) StepLadder() []int32 {
	if c == nil {
		return nil
	}

	if len(c.StepWeights) > 0 {
		out := make([]int32, 0, len(c.StepWeights))
		var prev int32
		for _, w := range c.StepWeights {
			// Non-increasing entries are dropped rather than rejected here:
			// CEL validates the field at admission, and a controller that
			// panicked or stalled on a spec that slipped through would be a
			// worse failure than one that climbs a slightly shorter ladder.
			if w > prev && w <= 100 {
				out = append(out, w)
				prev = w
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	step := int32(20)
	if c.StepWeight != nil && *c.StepWeight > 0 {
		step = *c.StepWeight
	}
	maxW := int32(50)
	if c.MaxWeight != nil && *c.MaxWeight > 0 {
		maxW = *c.MaxWeight
	}

	var out []int32
	for w := step; w < maxW; w += step {
		out = append(out, w)
	}
	// The ladder always ENDS at maxWeight, even when the step does not divide
	// it evenly. Otherwise a stepWeight of 30 against a maxWeight of 50 would
	// promote straight from 30%, skipping the largest exposure the user
	// explicitly asked to observe before committing.
	return append(out, maxW)
}

// ApprovalRequired reports whether promotion waits for a human.
func (c *CanarySpec) ApprovalRequired() bool {
	return c != nil && c.RequireApproval != nil && *c.RequireApproval
}

// AutoRollbackEnabled reports whether a stalled or failed rollout reverts.
func (r *RolloutSpec) AutoRollbackEnabled() bool {
	if r == nil || r.AutoRollback == nil {
		return true
	}
	return *r.AutoRollback
}

// IsCanary reports whether this rollout uses the canary strategy.
func (r *RolloutSpec) IsCanary() bool {
	return r != nil && r.Type == RolloutCanary && r.Canary != nil
}
