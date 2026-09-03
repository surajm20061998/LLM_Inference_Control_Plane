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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Variant identifies which side of a progressive rollout a workload belongs to.
// It is a label value on every pod and a metric label on every request, so it
// must stay short and stable.
type Variant string

const (
	// VariantPrimary is the stable, promoted workload.
	VariantPrimary Variant = "primary"
	// VariantCanary is the workload under evaluation.
	VariantCanary Variant = "canary"
)

// ModelDeploymentSpec defines the desired state of a ModelDeployment.
type ModelDeploymentSpec struct {
	// Replicas is the TOTAL number of serving pods across all variants
	// (primary + canary combined).
	//
	// This field backs the /scale subresource, so `kubectl scale` and any
	// HorizontalPodAutoscaler targeting this resource write here. During a
	// canary the controller distributes this total between the two variants
	// rather than adding to it — an autoscaler decides how much capacity
	// exists, the rollout decides how it is split.
	//
	// It is a pointer so that "unset" is distinguishable from zero, which
	// matters when an external autoscaler owns the field.
	//
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// Model identifies what to serve and where its weights come from.
	// +required
	Model ModelSpec `json:"model"`

	// Engine selects and configures the inference server.
	// +required
	Engine EngineSpec `json:"engine"`

	// Serving configures the network surface in front of the engine.
	// +optional
	Serving ServingSpec `json:"serving,omitempty"`

	// Rollout controls how replacements are rolled out when the pod template
	// changes.
	// +optional
	Rollout RolloutSpec `json:"rollout,omitempty"`

	// Observability configures what the operator publishes for Prometheus and
	// Grafana to consume.
	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`

	// Autoscaling configures the built-in autoscaler.
	//
	// Nil means off, and something else owns .spec.replicas: a human with
	// `kubectl scale`, a GitOps commit, or an external HorizontalPodAutoscaler
	// writing through the /scale subresource. All three work without any
	// configuration here, which is why this is a pointer rather than a struct
	// with a defaulted `mode: Off`.
	//
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

// ModelSpec describes the model being served.
type ModelSpec struct {
	// Name is the model identifier clients pass as "model" in OpenAI-compatible
	// requests, echoed at GET /v1/models. It is also the `model` label on every
	// metric this project emits, so keep it stable and low-cardinality.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Source is where the model weights come from.
	// +required
	Source ModelSourceSpec `json:"source"`
}

// ModelSourceSpec is a discriminated union: exactly one member must be set.
//
// The union is modelled in full from the first release even though only Image
// is implemented today. Adding a member to an existing union is a backward
// compatible change; turning a scalar field into a union is not.
//
// +kubebuilder:validation:XValidation:rule="[has(self.image), has(self.huggingFace), has(self.persistentVolumeClaim)].filter(x, x).size() == 1",message="exactly one of image, huggingFace or persistentVolumeClaim must be set"
type ModelSourceSpec struct {
	// Image mounts model weights from an OCI image.
	//
	// This is the recommended source: it is deterministic, layer-cached by the
	// container runtime, works fully offline, and decouples the model's version
	// from the engine's — which is what allows a model change and an engine
	// change to be rolled out independently.
	//
	// +optional
	Image *ImageModelSource `json:"image,omitempty"`

	// HuggingFace downloads weights from the Hugging Face Hub at pod start.
	//
	// Convenient, but startup is non-deterministic, every replica re-downloads
	// on every restart, and it is subject to upstream availability and rate
	// limits. Not implemented yet.
	//
	// +optional
	HuggingFace *HuggingFaceModelSource `json:"huggingFace,omitempty"`

	// PersistentVolumeClaim mounts weights from an existing PVC.
	// Not implemented yet.
	// +optional
	PersistentVolumeClaim *PVCModelSource `json:"persistentVolumeClaim,omitempty"`
}

// ImageModelSource mounts model weights from an OCI image.
type ImageModelSource struct {
	// Image is the OCI reference containing the model file.
	// +required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Path is the absolute path to the model file inside that image.
	//
	// This is where the file lives in the MODEL image, which is a different
	// place from where the engine will read it: the operator copies it onto a
	// shared volume and mounts that at a fixed location. The default matches
	// the convention used by this project's own Dockerfile.model.
	//
	// +optional
	// +kubebuilder:default="/weights/model.gguf"
	// +kubebuilder:validation:Pattern=`^/.*`
	Path string `json:"path,omitempty"`

	// PullPolicy for the model image.
	// +optional
	// +kubebuilder:default=IfNotPresent
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// HuggingFaceModelSource downloads weights from the Hugging Face Hub.
type HuggingFaceModelSource struct {
	// Repo is the Hub repository, e.g. "unsloth/Qwen3-0.6B-GGUF".
	// +required
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`
	Repo string `json:"repo"`

	// File is the specific file to fetch, e.g. "Qwen3-0.6B-Q4_K_M.gguf".
	// Required for GGUF engines; omit for engines that consume a whole repo.
	// +optional
	File string `json:"file,omitempty"`

	// TokenSecretRef references a Hugging Face token for gated repositories.
	// +optional
	TokenSecretRef *corev1.SecretKeySelector `json:"tokenSecretRef,omitempty"`
}

// PVCModelSource mounts weights from a PersistentVolumeClaim.
type PVCModelSource struct {
	// ClaimName is the PVC to mount.
	// +required
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// Path is the absolute path to the model file within the volume.
	// +required
	// +kubebuilder:validation:Pattern=`^/.*`
	Path string `json:"path"`

	// ReadOnly mounts the volume read-only.
	// +optional
	// +kubebuilder:default=true
	ReadOnly *bool `json:"readOnly,omitempty"`
}

// ServingSpec configures the network surface in front of the engine.
type ServingSpec struct {
	// Port is the port the ModelDeployment's Service listens on.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// StartupTimeout bounds how long the engine may take to load its model and
	// report healthy before the pod is restarted.
	//
	// Model load dominates startup and varies by orders of magnitude between
	// engines — seconds for a quantized GGUF, minutes for a framework that
	// compiles kernels first — so this is generous by default and is enforced
	// with a startupProbe rather than a livenessProbe.
	//
	// +optional
	// +kubebuilder:default="300s"
	StartupTimeout *metav1.Duration `json:"startupTimeout,omitempty"`

	// Shim configures the llmcp metrics sidecar.
	// +optional
	Shim ShimSpec `json:"shim,omitempty"`
}

// ShimSpec configures the llmcp metrics sidecar.
//
// # Why a sidecar exists at all
//
// It is not a convenience layer. llama.cpp exposes GAUGES only — no
// histograms, no request duration, no time-to-first-token, no HTTP status-code
// counter. p95 latency and success rate are therefore not merely inconvenient
// to compute from the engine's own metrics, they are impossible, and canary
// analysis would have nothing to gate a promotion on. The shim reverse-proxies
// the OpenAI surface and emits the missing signals.
//
// The second reason is engine independence. `llmcp_inference_ttft_seconds`
// means exactly the same thing whether the engine underneath is llama.cpp,
// vLLM or the deterministic test stub, because one binary measures it in one
// place. That is what makes the EngineSpec abstraction real rather than
// decorative: swapping engines does not invalidate a single alert, dashboard
// or rollout gate.
type ShimSpec struct {
	// Enabled injects the shim. Defaults to true.
	//
	// Turning it off leaves the engine serving directly and is supported, but
	// it disables every llmcp_* metric — which means canary analysis, the
	// built-in autoscaler and the SLO alerts all lose their inputs. The
	// MetricsRegistered condition says so out loud rather than leaving an
	// operator to discover it from empty graphs.
	//
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Image overrides the shim container image.
	//
	// The operator's own build-pinned shim image is used when this is empty,
	// which is almost always what is wanted: the shim's metric names are the
	// contract the operator's analysis code reads, so running a shim from a
	// different build than the controller is a version skew with silent
	// consequences.
	//
	// +optional
	Image string `json:"image,omitempty"`

	// Resources are the shim container's compute resources.
	//
	// The shim is a streaming byte pump with a histogram attached; it is
	// deliberately given a small, explicit default rather than left
	// unconstrained, because an unbounded sidecar sharing a pod with a
	// CPU-starved inference engine can steal exactly the cycles whose absence
	// the metrics are supposed to be measuring.
	//
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// LogLevel sets the shim's log verbosity.
	// +optional
	// +kubebuilder:default=info
	// +kubebuilder:validation:Enum=debug;info;warn;error
	LogLevel string `json:"logLevel,omitempty"`
}

// ShimEnabled reports whether the metrics sidecar should be injected.
func (s *ServingSpec) ShimEnabled() bool {
	if s == nil || s.Shim.Enabled == nil {
		return true
	}
	return *s.Shim.Enabled
}

// Phase is a coarse, human-facing summary of where a ModelDeployment is.
// Machine logic should read Conditions, not Phase.
//
// The state machine is:
//
//	Pending -> Progressing -> Available
//	Available -> Canarying -> Promoting   -> Available
//	                       -> RollingBack -> Degraded
//	                       -> Paused      (awaiting a human)
//
// Three invariants are enforced by unit tests rather than by comments:
// Promoting and RollingBack always make progress; Paused never leaves without
// external input; and entering Canarying requires a non-empty lastGoodRevision,
// because a canary with nothing to roll back TO is not a canary, it is a
// deploy with extra steps.
//
// +kubebuilder:validation:Enum=Pending;Progressing;Available;Canarying;Promoting;RollingBack;Paused;Degraded
type Phase string

const (
	// PhasePending means no revision has ever become available.
	PhasePending Phase = "Pending"
	// PhaseProgressing means a rollout is in flight.
	PhaseProgressing Phase = "Progressing"
	// PhaseAvailable is the steady state: desired replicas are ready.
	PhaseAvailable Phase = "Available"
	// PhaseCanarying means a canary is running and being analysed.
	PhaseCanarying Phase = "Canarying"
	// PhasePromoting means the canary passed and is becoming the primary.
	PhasePromoting Phase = "Promoting"
	// PhaseRollingBack means the canary failed and is being torn down.
	PhaseRollingBack Phase = "RollingBack"
	// PhasePaused means the rollout is waiting for an operator to approve or
	// abort it. Nothing moves out of this phase without external input.
	PhasePaused Phase = "Paused"
	// PhaseDegraded means the rollout failed or availability was lost.
	PhaseDegraded Phase = "Degraded"
)

// ModelDeploymentStatus reports the observed state of a ModelDeployment.
type ModelDeploymentStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	// Anything asking "is the rollout finished?" must compare this against
	// .metadata.generation first, or it will read a stale answer.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse summary for humans and printer columns.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Replicas is the TOTAL actual pod count across all variants.
	//
	// Part of the /scale subresource contract. An HPA divides by this number,
	// so an inaccurate value does not merely look wrong, it makes the
	// autoscaler compute the wrong target.
	//
	// +optional
	Replicas int32 `json:"replicas"`

	// Selector is a SERIALIZED label selector string (e.g.
	// "llmcp.io/model-deployment=demo"), not a metav1.LabelSelector.
	//
	// Part of the /scale subresource contract: an HPA uses it to find the pods
	// whose metrics it averages. It must select every variant, so that scaling
	// decisions are made over the whole fleet rather than half of it.
	//
	// +optional
	Selector string `json:"selector,omitempty"`

	// ReadyReplicas is the number of pods passing their readiness probe.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// UpdatedReplicas is the number of pods running the target revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// AvailableReplicas is the number of pods that have been ready long enough
	// to count as available.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// StableRevision is the revision hash currently serving as primary.
	// +optional
	StableRevision string `json:"stableRevision,omitempty"`

	// LastGoodRevision is the most recent revision that reached availability.
	// It is the target an automatic rollback reverts to.
	// +optional
	LastGoodRevision string `json:"lastGoodRevision,omitempty"`

	// Endpoint is the in-cluster OpenAI-compatible base URL.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Canary reports the state of an in-flight or just-finished canary. It is
	// nil when no canary has ever run.
	// +optional
	Canary *CanaryStatus `json:"canary,omitempty"`

	// Autoscaling reports what the built-in autoscaler is doing. Nil when it is
	// not enabled.
	// +optional
	Autoscaling *AutoscalingStatus `json:"autoscaling,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=md;mdep,categories=inference
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Weight",type=integer,JSONPath=`.status.canary.currentWeight`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.canary.failedChecks`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.stableRevision`,priority=1
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1

// ModelDeployment is a declarative deployment of an LLM inference server:
// it owns the serving workload, health-checks it, and reports a status other
// controllers (and an HPA, via the /scale subresource) can act on.
type ModelDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state.
	// +required
	Spec ModelDeploymentSpec `json:"spec"`

	// status is the observed state.
	// +optional
	Status ModelDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelDeploymentList contains a list of ModelDeployment.
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &ModelDeployment{}, &ModelDeploymentList{})
		return nil
	})
}
