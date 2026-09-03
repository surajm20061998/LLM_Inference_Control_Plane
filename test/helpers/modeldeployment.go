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

package helpers

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Fixture constants. They are exported so a test can assert on the value it
// asked for rather than re-typing a literal that can silently drift.
const (
	// FixtureModelName is the served model identity (llama-server's --alias).
	FixtureModelName = "test-model"

	// FixtureModelImage is the OCI image the model weights are copied out of.
	FixtureModelImage = "ghcr.io/example/test-model:v1"

	// FixtureModelPath is where the weights live INSIDE FixtureModelImage.
	//
	// Deliberately not the CRD's "/models/model.gguf" default: the controller
	// mounts the shared model volume at /models, so a source path under /models
	// is shadowed by that mount and the init container's `cp src dst` degenerates
	// to copying a file onto itself. Using a distinct source directory keeps the
	// fixture representative of a working deployment and makes the init
	// container's command assertable.
	FixtureModelPath = "/weights/model.gguf"

	// FixtureEngineImage is a pinned stub engine, selected the supported way —
	// through spec.engine.image rather than a dedicated enum value — so the real
	// llamacpp profile's argument building is exercised, not bypassed.
	FixtureEngineImage = "ghcr.io/example/fake-engine:v1"
)

// MDOption mutates a ModelDeployment fixture.
type MDOption func(*v1alpha1.ModelDeployment)

// NewModelDeployment returns a valid minimal ModelDeployment for tests.
//
// Every field the CRD defaults is set explicitly. That costs a few lines and
// buys determinism: a test that asserts on a value the API server filled in is
// really asserting on the CRD's defaulting, and it starts failing for reasons
// unrelated to the controller the moment a default is retuned.
func NewModelDeployment(name, namespace string, opts ...MDOption) *v1alpha1.ModelDeployment {
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.ModelDeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Model: v1alpha1.ModelSpec{
				Name: FixtureModelName,
				Source: v1alpha1.ModelSourceSpec{
					Image: &v1alpha1.ImageModelSource{
						Image:      FixtureModelImage,
						Path:       FixtureModelPath,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
			Engine: v1alpha1.EngineSpec{
				Type:            v1alpha1.EngineLlamaCPP,
				Image:           FixtureEngineImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				ContextSize:     ptr.To(int32(4096)),
				MaxConcurrency:  ptr.To(int32(4)),
			},
			Serving: v1alpha1.ServingSpec{
				Port:           8080,
				StartupTimeout: &metav1.Duration{Duration: 300 * time.Second},
			},
			Rollout: v1alpha1.RolloutSpec{
				Type:             v1alpha1.RolloutRollingUpdate,
				ProgressDeadline: &metav1.Duration{Duration: 600 * time.Second},
				AutoRollback:     ptr.To(true),
			},
		},
	}

	for _, opt := range opts {
		opt(md)
	}
	return md
}

// WithReplicas sets spec.replicas, the total across all variants.
func WithReplicas(n int32) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Replicas = ptr.To(n)
	}
}

// WithEngineImage overrides the engine container image.
func WithEngineImage(image string) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Engine.Image = image
	}
}

// WithModelImage overrides the OCI image the model weights come from.
func WithModelImage(image string) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		if md.Spec.Model.Source.Image == nil {
			md.Spec.Model.Source.Image = &v1alpha1.ImageModelSource{
				Path:       FixtureModelPath,
				PullPolicy: corev1.PullIfNotPresent,
			}
		}
		md.Spec.Model.Source.Image.Image = image
	}
}

// WithContextSize sets spec.engine.contextSize. It is part of the revision
// payload, so changing it is the cheapest way for a test to force a new
// revision.
func WithContextSize(n int32) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Engine.ContextSize = ptr.To(n)
	}
}

// WithServingPort sets spec.serving.port, the port the Service listens on.
func WithServingPort(port int32) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Serving.Port = port
	}
}

// WithResources sets the engine container's compute resources. The CPU limit is
// what the engine's thread count is derived from.
func WithResources(res corev1.ResourceRequirements) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Engine.Resources = res
	}
}

// WithCanary switches the ModelDeployment to the canary strategy.
//
// The defaults here are deliberately tighter than the API's: a two-rung ladder
// and a failure threshold of 2, so a test spends four analysis rounds rather
// than a dozen. The clock is fake, so this costs nothing in wall time — it
// costs READING time, and a test whose intent is "rollback fires on the second
// failure" should not need twelve steps of scrollback to show it.
func WithCanary(mutate ...func(*v1alpha1.CanarySpec)) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		md.Spec.Rollout.Type = v1alpha1.RolloutCanary
		md.Spec.Rollout.Canary = &v1alpha1.CanarySpec{
			StepWeights: []int32{20, 50},
			Analysis: v1alpha1.AnalysisSpec{
				Interval:              &metav1.Duration{Duration: 30 * time.Second},
				Window:                &metav1.Duration{Duration: 60 * time.Second},
				InitialDelay:          &metav1.Duration{Duration: 60 * time.Second},
				FailureThreshold:      ptr.To(int32(2)),
				ConsecutiveErrorLimit: ptr.To(int32(3)),
				InconclusiveLimit:     ptr.To(int32(3)),
				OnInconclusive:        v1alpha1.InconclusiveWait,
				MinRequestRate:        quantityPtr("500m"),
				Provider: v1alpha1.AnalysisProviderSpec{
					Type: v1alpha1.AnalysisProviderPrometheus,
					// Set even though the tests inject a scripted provider, so
					// the fixture is a spec that would work in a real cluster.
					Address: "http://prometheus-operated.monitoring.svc:9090",
					Timeout: &metav1.Duration{Duration: 10 * time.Second},
				},
				Metrics: []v1alpha1.AnalysisMetric{{
					Name:           "ttft-p95",
					Builtin:        builtinPtr(v1alpha1.MetricTTFTP95),
					ThresholdRange: v1alpha1.ThresholdRange{Max: quantityPtr("1500m")},
				}},
			},
			TrafficRouting: v1alpha1.TrafficRoutingSpec{Mode: v1alpha1.TrafficRoutingReplica},
		}
		for _, m := range mutate {
			m(md.Spec.Rollout.Canary)
		}
	}
}

// WithProviderAddress sets the metric backend URL.
func WithProviderAddress(address string) MDOption {
	return func(md *v1alpha1.ModelDeployment) {
		if md.Spec.Rollout.Canary == nil {
			return
		}
		md.Spec.Rollout.Canary.Analysis.Provider.Address = address
	}
}

// builtinPtr returns a pointer to a built-in metric name.
func builtinPtr(b v1alpha1.BuiltinMetric) *v1alpha1.BuiltinMetric { return &b }

// QuantityPtr parses a canonical quantity literal for a fixture. Exported so
// controller tests can build threshold values without re-importing resource.
func QuantityPtr(s string) *resource.Quantity { return quantityPtr(s) }

// quantityPtr parses a canonical quantity literal for a fixture.
//
// It panics on a malformed input, which is correct here: every caller passes a
// compile-time constant, so a failure can only be a typo introduced while
// editing this file, and the first test to run catches it.
func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
