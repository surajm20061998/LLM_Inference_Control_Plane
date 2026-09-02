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
	"k8s.io/apimachinery/pkg/util/intstr"
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
}

// RolloutStrategyType selects how pod template changes are rolled out.
// +kubebuilder:validation:Enum=RollingUpdate;Recreate
type RolloutStrategyType string

const (
	// RolloutRollingUpdate replaces pods incrementally, keeping the service up.
	RolloutRollingUpdate RolloutStrategyType = "RollingUpdate"
	// RolloutRecreate tears down all pods before creating replacements.
	RolloutRecreate RolloutStrategyType = "Recreate"
)

// RolloutSpec controls rollout behaviour.
//
// The mechanics of a rolling update are delegated to the child Deployment,
// which already implements them correctly. What this operator adds on top is
// the behaviour a Deployment does NOT have: a Deployment whose rollout stalls
// stays stalled forever, whereas exceeding ProgressDeadline here reverts to the
// last revision that was known good.
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
	// exceeded. Consumed from the sprint that implements rollback; declared
	// here so the field is stable.
	// +optional
	// +kubebuilder:default=true
	AutoRollback *bool `json:"autoRollback,omitempty"`
}

// Phase is a coarse, human-facing summary of where a ModelDeployment is.
// Machine logic should read Conditions, not Phase.
// +kubebuilder:validation:Enum=Pending;Progressing;Available;Degraded
type Phase string

const (
	// PhasePending means no revision has ever become available.
	PhasePending Phase = "Pending"
	// PhaseProgressing means a rollout is in flight.
	PhaseProgressing Phase = "Progressing"
	// PhaseAvailable is the steady state: desired replicas are ready.
	PhaseAvailable Phase = "Available"
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
