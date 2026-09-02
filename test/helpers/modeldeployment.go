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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/surajmishra/llmcp/api/v1alpha1"
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
