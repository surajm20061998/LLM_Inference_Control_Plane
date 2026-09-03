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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoscalingMode selects who decides how many replicas exist.
//
// There is deliberately no `HPA` value. An external HorizontalPodAutoscaler
// writes `.spec.replicas` through the /scale subresource and the controller
// reconciles that like any other spec change — so an `HPA` enum value would
// change exactly zero lines of behaviour while implying the operator does
// something special for it. A Chainsaw test proving a stock HPA drives this CR
// is worth more than the enum value, and it is what ships instead.
//
// +kubebuilder:validation:Enum=Off;Builtin
type AutoscalingMode string

const (
	// AutoscalingOff leaves .spec.replicas alone. Something else owns it:
	// a human with `kubectl scale`, a GitOps commit, or an external HPA.
	AutoscalingOff AutoscalingMode = "Off"

	// AutoscalingBuiltin lets this operator write .spec.replicas from a
	// measured signal.
	AutoscalingBuiltin AutoscalingMode = "Builtin"
)

// AutoscalingMetric is the signal the built-in autoscaler tracks.
//
// # Why not CPU
//
// This is the thesis of the whole project, so it is worth stating where the
// field is defined rather than only in a README. An inference server saturates
// its fixed number of decode slots long before it saturates the CPU: requests
// start queueing while utilisation still reads 45%, and by the time CPU is high
// enough to trip a utilisation target, tail latency has already been bad for
// minutes. Queue depth crosses its threshold at the moment work actually starts
// waiting, which is the moment more capacity would have helped.
//
// +kubebuilder:validation:Enum=queue-depth;concurrency
type AutoscalingMetric string

const (
	// AutoscalingQueueDepth targets the mean number of requests waiting for a
	// decode slot, per pod. A target of 0 is not achievable in practice and a
	// target of 1 means "tolerate one request queued per pod".
	AutoscalingQueueDepth AutoscalingMetric = "queue-depth"

	// AutoscalingConcurrency targets in-flight requests per pod, whether or not
	// they are queued.
	//
	// Useful when the engine's concurrency is high enough that queueing is rare
	// but latency still degrades with load — the difference between "work is
	// waiting" and "work is plentiful". Queue depth is the sharper signal and
	// is the default; this one is smoother and reacts earlier.
	AutoscalingConcurrency AutoscalingMetric = "concurrency"
)

// AutoscalingSpec configures the built-in autoscaler.
//
// The whole struct is a pointer on ModelDeploymentSpec, and nil means off. That
// is deliberate rather than a defaulted `mode: Off`: an absent block cannot
// carry a maxReplicas that is required-but-meaningless, so `maxReplicas` can be
// plainly required here instead of guarded by a CEL rule.
//
// That choice is itself a lesson from this codebase. A `+kubebuilder:default`
// on a field referenced by a CEL `has()` expression makes the rule
// unsatisfiable — the API server fills the field in, so `has()` is always true
// and an "exactly one of" or "required when" rule can never be satisfied. The
// failure is invisible until a manifest is rejected for setting a field nobody
// wrote. Modelling optionality with a nil struct sidesteps the whole class.
//
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type AutoscalingSpec struct {
	// Mode selects who owns .spec.replicas.
	// +optional
	// +kubebuilder:default=Builtin
	Mode AutoscalingMode `json:"mode,omitempty"`

	// MinReplicas is the floor.
	//
	// It is not 0: scale-to-zero is explicitly out of scope for this project,
	// and a floor of zero on an engine with a multi-minute cold start would
	// turn the first request after a quiet period into a timeout rather than a
	// slow response.
	//
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the ceiling. Required: an autoscaler without one is a
	// budget incident waiting for a traffic spike.
	// +required
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// Metric is the signal to track.
	// +optional
	// +kubebuilder:default=queue-depth
	Metric AutoscalingMetric `json:"metric,omitempty"`

	// Target is the desired value of Metric, PER POD.
	//
	// A Quantity rather than a float because the Kubernetes API convention
	// forbids floats in a spec: they serialise inconsistently across language
	// bindings and cannot round-trip through protobuf. Write "2" or "1500m".
	//
	// +optional
	// +kubebuilder:default="2"
	Target *resource.Quantity `json:"target,omitempty"`

	// Tolerance is the fraction by which the measured ratio may differ from 1.0
	// before any scaling happens.
	//
	// 0.1 is the HPA's own default and it is not decoration. Without a
	// tolerance band, a queue depth of 4.1 against a target of 4 is a scale
	// event — and so is 3.9 fifteen seconds later. The fleet then oscillates
	// forever, each change costing a model load, and the metric it is reacting
	// to is mostly the noise created by its own churn.
	//
	// +optional
	// +kubebuilder:default="100m"
	Tolerance *resource.Quantity `json:"tolerance,omitempty"`

	// PollInterval is how often the metric is sampled.
	// +optional
	// +kubebuilder:default="15s"
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// Window is the PromQL lookback the metric is averaged over.
	//
	// Averaged, not instantaneous: queue depth is spiky by nature — it is the
	// difference between arrivals and a fixed number of decode slots — and an
	// instant read samples whichever microsecond the scrape landed on.
	//
	// +optional
	// +kubebuilder:default="60s"
	Window *metav1.Duration `json:"window,omitempty"`

	// ScaleUpStabilization is how far back to look when scaling UP.
	//
	// Zero by default, deliberately asymmetric with ScaleDownStabilization.
	// Adding capacity late costs latency the user experiences; removing it
	// early costs a cold start on the next spike. The asymmetry says which
	// mistake is cheaper, and it is the same asymmetry the HPA ships with.
	//
	// +optional
	// +kubebuilder:default="0s"
	ScaleUpStabilization *metav1.Duration `json:"scaleUpStabilization,omitempty"`

	// ScaleDownStabilization is how far back to look when scaling DOWN.
	//
	// Five minutes, and for an inference workload that is if anything short: a
	// pod removed here has to load its weights again to come back, which for a
	// real model is minutes of unavailable capacity bought to save seconds of
	// idle capacity.
	//
	// +optional
	// +kubebuilder:default="300s"
	ScaleDownStabilization *metav1.Duration `json:"scaleDownStabilization,omitempty"`

	// Provider points at the metric backend.
	//
	// Separate from the canary's provider block even though both normally name
	// the same Prometheus. They fail independently and for different reasons —
	// a rollout can be over while autoscaling runs forever — and sharing one
	// field would mean a ModelDeployment with no canary could not configure an
	// autoscaler at all.
	//
	// +optional
	Provider AnalysisProviderSpec `json:"provider,omitempty"`
}

// AutoscalingEnabled reports whether the built-in autoscaler owns replicas.
func (a *AutoscalingSpec) AutoscalingEnabled() bool {
	return a != nil && a.Mode == AutoscalingBuiltin
}

// Resolved accessors. Each returns the effective value including defaults, so
// that no caller repeats a nil check and no two callers disagree about a
// default. They are correct for a hand-built spec that never passed through the
// API server's defaulting — a unit test, or a future dry-run path.
const (
	defaultAutoscaleTargetMilli    = int64(2000)
	defaultAutoscaleToleranceMilli = int64(100)
	defaultPollInterval            = 15 * time.Second
	defaultAutoscaleWindow         = 60 * time.Second
	defaultScaleUpStabilization    = time.Duration(0)
	defaultScaleDownStabilization  = 300 * time.Second
)

// ResolvedTarget returns the per-pod target value.
func (a *AutoscalingSpec) ResolvedTarget() float64 {
	if a == nil || a.Target == nil {
		return float64(defaultAutoscaleTargetMilli) / 1000
	}
	return float64(a.Target.MilliValue()) / 1000
}

// ResolvedTolerance returns the dead band around a ratio of 1.0.
func (a *AutoscalingSpec) ResolvedTolerance() float64 {
	if a == nil || a.Tolerance == nil {
		return float64(defaultAutoscaleToleranceMilli) / 1000
	}
	return float64(a.Tolerance.MilliValue()) / 1000
}

// ResolvedPollInterval returns how often the metric is sampled.
func (a *AutoscalingSpec) ResolvedPollInterval() time.Duration {
	if a == nil {
		return defaultPollInterval
	}
	return durationOr(a.PollInterval, defaultPollInterval).Duration
}

// ResolvedWindow returns the PromQL lookback.
func (a *AutoscalingSpec) ResolvedWindow() time.Duration {
	if a == nil {
		return defaultAutoscaleWindow
	}
	return durationOr(a.Window, defaultAutoscaleWindow).Duration
}

// ResolvedScaleUpStabilization returns the scale-up look-back window.
//
// Zero is a legitimate value here and must survive: durationOr treats a
// non-positive duration as unset, so this reads the pointer directly.
func (a *AutoscalingSpec) ResolvedScaleUpStabilization() time.Duration {
	if a == nil || a.ScaleUpStabilization == nil {
		return defaultScaleUpStabilization
	}
	if a.ScaleUpStabilization.Duration < 0 {
		return defaultScaleUpStabilization
	}
	return a.ScaleUpStabilization.Duration
}

// ResolvedScaleDownStabilization returns the scale-down look-back window.
func (a *AutoscalingSpec) ResolvedScaleDownStabilization() time.Duration {
	if a == nil || a.ScaleDownStabilization == nil {
		return defaultScaleDownStabilization
	}
	if a.ScaleDownStabilization.Duration < 0 {
		return defaultScaleDownStabilization
	}
	return a.ScaleDownStabilization.Duration
}

// ResolvedMetric returns the signal to track.
func (a *AutoscalingSpec) ResolvedMetric() AutoscalingMetric {
	if a == nil || a.Metric == "" {
		return AutoscalingQueueDepth
	}
	return a.Metric
}

// ResolvedProviderTimeout returns the per-query timeout.
func (a *AutoscalingSpec) ResolvedProviderTimeout() time.Duration {
	if a == nil {
		return defaultQueryTimeout
	}
	return durationOr(a.Provider.Timeout, defaultQueryTimeout).Duration
}

// ScaleRecommendation is one autoscaler decision, kept for stabilization.
type ScaleRecommendation struct {
	// Time is when the recommendation was computed.
	// +required
	Time metav1.Time `json:"time"`

	// Replicas is the raw recommendation, before stabilization and before
	// clamping to the current value.
	// +required
	Replicas int32 `json:"replicas"`
}

// AutoscalingStatus reports what the built-in autoscaler is doing.
type AutoscalingStatus struct {
	// DesiredReplicas is the count the autoscaler most recently asked for.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// CurrentMetric is the measured value, aggregated across the whole fleet,
	// rendered as a string.
	//
	// A string, not a Quantity: this is an observation rather than a
	// specification, so it is not bound by the no-floats convention, and it
	// must be able to stay empty when there was no measurement at all. A zero
	// would be indistinguishable from a measured zero — which is precisely the
	// confusion that makes an autoscaler freeze a fleet while load climbs.
	// +optional
	CurrentMetric string `json:"currentMetric,omitempty"`

	// CurrentMetricPerPod is CurrentMetric divided by the ready pods — the
	// number actually compared against the target.
	// +optional
	CurrentMetricPerPod string `json:"currentMetricPerPod,omitempty"`

	// LastScaleTime is when .spec.replicas last changed because of this
	// autoscaler.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// Recommendations is the recent decision history that stabilization reads.
	//
	// It lives in status rather than in memory for the same reason the canary's
	// position does: a leader-election handover, a crash or an operator upgrade
	// must not reset it. An in-memory window silently forgets that the fleet
	// was scaled up ninety seconds ago, and the replacement process is free to
	// scale it straight back down.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	Recommendations []ScaleRecommendation `json:"recommendations,omitempty"`

	// Message explains the most recent decision, including the decision not to
	// act.
	// +optional
	Message string `json:"message,omitempty"`
}
