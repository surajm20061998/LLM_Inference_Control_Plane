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
	corev1 "k8s.io/api/core/v1"
)

// EngineType selects the inference server implementation.
//
// New values are added only in the release that implements them. An enum value
// whose only behaviour is to fail validation is worse than no enum value:
// adding one later is backward compatible, but shipping a broken one is not.
//
// +kubebuilder:validation:Enum=llamacpp
type EngineType string

const (
	// EngineLlamaCPP runs llama.cpp's llama-server.
	EngineLlamaCPP EngineType = "llamacpp"
)

// EngineSpec selects and configures the inference server.
type EngineSpec struct {
	// Type selects the engine implementation.
	// +optional
	// +kubebuilder:default=llamacpp
	Type EngineType `json:"type,omitempty"`

	// Image overrides the engine profile's default container image.
	//
	// This is also the supported way to substitute a deterministic stub server
	// in tests and CI: the stub is selected here rather than through a
	// dedicated enum value, so the real profile's argument-building code path
	// is exercised rather than bypassed.
	//
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy for the engine image.
	// +optional
	// +kubebuilder:default=IfNotPresent
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ContextSize is the maximum context window, in tokens.
	// +optional
	// +kubebuilder:default=4096
	// +kubebuilder:validation:Minimum=256
	ContextSize *int32 `json:"contextSize,omitempty"`

	// MaxConcurrency is how many requests the engine may process concurrently
	// (llama.cpp's --parallel).
	//
	// It also determines when the engine starts queueing, which is the signal
	// autoscaling is driven from — so it is a capacity knob, not just a
	// performance one.
	//
	// +optional
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	MaxConcurrency *int32 `json:"maxConcurrency,omitempty"`

	// Threads is the engine's worker thread count (llama.cpp's -t).
	//
	// When unset it is DERIVED from the container's CPU limit. This matters
	// more than it looks: llama.cpp reads the host's /proc and is unaware of
	// cgroup limits, so left alone every replica spawns one thread per host
	// core. Several replicas on one node then oversubscribe the CPU badly
	// enough that latency percentiles become noise — which in turn makes
	// metric-driven canary analysis produce false rollbacks.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	Threads *int32 `json:"threads,omitempty"`

	// ExtraArgs are appended verbatim to the engine's command line, after every
	// argument the profile generates.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Env are additional environment variables for the engine container.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources are the engine container's compute resources. Setting a CPU
	// limit is strongly recommended: it is what Threads is derived from.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// APIKeySecretRef supplies an API key the engine will require on requests.
	// +optional
	APIKeySecretRef *corev1.SecretKeySelector `json:"apiKeySecretRef,omitempty"`
}
