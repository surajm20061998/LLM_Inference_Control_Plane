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

// Package revision defines what a ModelDeployment "revision" is and persists
// the history of those revisions as apps/v1 ControllerRevision objects — the
// same primitive StatefulSet and DaemonSet use for their own history.
//
// This exists so that a rollout can be undone. A plain Deployment whose rollout
// stalls stays stalled forever; this operator instead reverts to the last
// revision that was known good, which requires both knowing which revision that
// was (a stable identifier) and being able to reconstruct it (a stored payload).
//
// The identifier and payload are produced from the same canonical bytes. New
// records use an explicit versioned envelope; the original unversioned shape
// remains below solely so existing ControllerRevisions can be decoded by their
// stored identity.
package revision

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// revisionInput is the exact subset of ModelDeploymentSpec that defines a
// revision: the fields that affect the pod template, and nothing else.
//
// It is a DEDICATED struct rather than the spec itself for two reasons. First,
// determinism: the JSON encoding of a struct follows declaration order, so the
// bytes fed to the hash are fixed by this type and not by unrelated edits to
// the API types. Second, intent: adding a field to ModelDeploymentSpec must be
// a deliberate decision about whether it triggers a rollout, not an accident.
//
// What is DELIBERATELY excluded, because both exclusions look like bugs until
// you know why:
//
//   - Replicas. Scaling is not a new revision. Replicas backs the /scale
//     subresource, so an HorizontalPodAutoscaler writes to it; if it were
//     hashed, every autoscaler tick would compute a new revision hash and
//     trigger a rollout of pods that are otherwise identical.
//   - Rollout, in its entirety. Those fields describe HOW to move between
//     revisions (surge, unavailability, progress deadline, auto-rollback), not
//     what is being run. Raising a progress deadline while a rollout is in
//     flight must not restart that rollout — which is exactly what would happen
//     if the deadline were part of the revision identity.
//
// The legacy format included Serving.Port and omitted StartupTimeout. That
// classification was incorrect: Port changes the stable Service, while
// StartupTimeout changes each Pod's probe. workloadV2 fixes the boundary
// without reinterpreting these old bytes.
//
// This legacy wire type is frozen. Changing it would make old history
// undecodable; new identity changes belong in a new versioned workload type.
type revisionInput struct {
	// Model is included whole: name, source image, path and pull policy all
	// determine what the pod mounts and serves.
	Model inferencev1alpha1.ModelSpec `json:"model"`

	// Engine is included whole: image, context size, concurrency, threads,
	// extra args, env and resources all become container fields.
	//
	// EngineSpec.Env is a slice and its order is user-authored and significant
	// (later entries may reference earlier ones). It is encoded AS GIVEN and
	// must never be sorted or otherwise re-ordered here: doing so would make
	// two genuinely different pod templates hash the same.
	//
	// EngineSpec.Resources embeds corev1.ResourceList, which IS a Go map — the
	// usual reason a "deterministic" hash is not. It is safe here, for two
	// reasons that are worth stating because a reviewer will (correctly) flag
	// the map on sight: encoding/json sorts map keys when marshalling, so
	// iteration order cannot leak into the output; and resource.Quantity
	// implements MarshalJSON with a canonical form, so "1000m" and "1" produce
	// the same bytes only when they are the same quantity, and a given quantity
	// always produces the same bytes. See TestHashResourceMapOrdering.
	Engine inferencev1alpha1.EngineSpec `json:"engine"`

	// Serving carries only Port. The rest of ServingSpec is excluded: a nested
	// struct is used rather than a bare port field so that adding another
	// pod-template-affecting serving field later is a local edit here, and so
	// that the stored payload reads like the spec it came from.
	Serving revisionServing `json:"serving"`
}

// revisionServing is the revision-defining subset of ServingSpec.
type revisionServing struct {
	// Port was historically treated as workload identity. It is retained only
	// to decode the original wire format; v2 correctly classifies it as a
	// Service-only field.
	Port int32 `json:"port"`

	// Shim is included whole because every field of it lands in the pod
	// template: enabling it adds a container, its image and resources are that
	// container's, and its log level is one of its arguments.
	//
	// Leaving it out would be a genuine bug rather than an omission. Toggling
	// the shim changes the pods the Deployment runs, so if the revision hash did
	// not move, the operator would record no new revision, the rollout would
	// have no rollback target, and — worse — a canary triggered by "turn metrics
	// on" would compare two variants the controller believes are identical.
	Shim inferencev1alpha1.ShimSpec `json:"shim"`
}

const (
	// PayloadVersionV2 is the first explicitly versioned revision format. The
	// original payload had no version field and remains decodable as legacy v1.
	PayloadVersionV2 = "v2"
)

// workloadV2 is the effective, pod-template-affecting configuration recorded
// by the current controller. The caller resolves API and operator defaults
// before constructing it, so a later controller can render the same workload
// even when its compiled-in defaults have changed.
//
// Serving.Port is intentionally absent: it changes the stable Service, not a
// serving Pod. StartupTimeout is present because it changes the startup probe.
type workloadV2 struct {
	Model   inferencev1alpha1.ModelSpec  `json:"model"`
	Engine  inferencev1alpha1.EngineSpec `json:"engine"`
	Serving workloadServingV2            `json:"serving"`
}

type workloadServingV2 struct {
	StartupTimeout *metav1.Duration           `json:"startupTimeout"`
	Shim           inferencev1alpha1.ShimSpec `json:"shim"`
}

// versionedPayload is the durable ControllerRevision wire format. Version is
// part of the hashed bytes, preventing a future decoder from confusing two
// schemas that happen to contain similar fields.
type versionedPayload struct {
	Version  string     `json:"version"`
	Workload workloadV2 `json:"workload"`
}

// Snapshot couples the immutable bytes stored in ControllerRevision with the
// identifier derived from those exact bytes.
type Snapshot struct {
	Revision string
	Raw      []byte
}

// Decoded describes a stored revision without losing which migration rules
// apply to it.
type Decoded struct {
	Spec    inferencev1alpha1.ModelDeploymentSpec
	Version string
}

// Legacy reports whether this revision predates the versioned envelope.
func (d Decoded) Legacy() bool { return d.Version == "" }

// NewSnapshot encodes an already-resolved workload using the current payload
// version and hashes the exact stored bytes.
func NewSnapshot(spec *inferencev1alpha1.ModelDeploymentSpec) (Snapshot, error) {
	if spec == nil {
		return Snapshot{}, fmt.Errorf("revision: cannot snapshot a nil effective workload")
	}
	if err := validateEffectiveSpec(spec); err != nil {
		return Snapshot{}, err
	}
	payload := versionedPayload{
		Version: PayloadVersionV2,
		Workload: workloadV2{
			Model:  spec.Model,
			Engine: spec.Engine,
			Serving: workloadServingV2{
				StartupTimeout: spec.Serving.StartupTimeout,
				Shim:           spec.Serving.Shim,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Snapshot{}, fmt.Errorf("revision: encoding %s payload: %w", PayloadVersionV2, err)
	}
	return Snapshot{Revision: hashBytes(raw), Raw: raw}, nil
}

func validateEffectiveSpec(spec *inferencev1alpha1.ModelDeploymentSpec) error {
	switch {
	case spec.Engine.Type == "":
		return fmt.Errorf("revision: effective engine type is empty")
	case spec.Engine.Image == "":
		return fmt.Errorf("revision: effective engine image is empty")
	case spec.Engine.ImagePullPolicy == "":
		return fmt.Errorf("revision: effective engine image pull policy is empty")
	case spec.Engine.ContextSize == nil:
		return fmt.Errorf("revision: effective engine context size is unresolved")
	case spec.Engine.MaxConcurrency == nil:
		return fmt.Errorf("revision: effective engine concurrency is unresolved")
	case spec.Engine.Threads == nil:
		return fmt.Errorf("revision: effective engine thread count is unresolved")
	case spec.Serving.StartupTimeout == nil:
		return fmt.Errorf("revision: effective startup timeout is unresolved")
	case spec.Serving.Shim.Enabled == nil:
		return fmt.Errorf("revision: effective shim enablement is unresolved")
	}
	if image := spec.Model.Source.Image; image != nil {
		if image.Path == "" || image.PullPolicy == "" {
			return fmt.Errorf("revision: effective model image path or pull policy is unresolved")
		}
	}
	if spec.Serving.ShimEnabled() {
		shim := spec.Serving.Shim
		if shim.Image == "" || shim.LogLevel == "" ||
			(len(shim.Resources.Requests) == 0 && len(shim.Resources.Limits) == 0) {
			return fmt.Errorf("revision: effective shim image, log level, or resources are unresolved")
		}
	}
	return nil
}

// Decode reads both the current envelope and the original unversioned
// revisionInput. Unknown versions fail closed instead of being interpreted as
// the latest schema.
func Decode(raw []byte) (Decoded, error) {
	if len(raw) == 0 {
		return Decoded{}, fmt.Errorf("revision: empty payload")
	}
	var header struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return Decoded{}, fmt.Errorf("revision: decoding payload header: %w", err)
	}
	if header.Version == "" {
		var legacy revisionInput
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return Decoded{}, fmt.Errorf("revision: decoding legacy payload: %w", err)
		}
		return Decoded{Spec: inferencev1alpha1.ModelDeploymentSpec{
			Model:  legacy.Model,
			Engine: legacy.Engine,
			Serving: inferencev1alpha1.ServingSpec{
				Port: legacy.Serving.Port,
				Shim: legacy.Serving.Shim,
			},
		}}, nil
	}
	if header.Version != PayloadVersionV2 {
		return Decoded{}, fmt.Errorf("revision: unsupported payload version %q", header.Version)
	}
	var payload versionedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Decoded{}, fmt.Errorf("revision: decoding %s payload: %w", PayloadVersionV2, err)
	}
	return Decoded{Version: payload.Version, Spec: inferencev1alpha1.ModelDeploymentSpec{
		Model:  payload.Workload.Model,
		Engine: payload.Workload.Engine,
		Serving: inferencev1alpha1.ServingSpec{
			StartupTimeout: payload.Workload.Serving.StartupTimeout,
			Shim:           payload.Workload.Serving.Shim,
		},
	}}, nil
}

// revisionPayload projects a ModelDeploymentSpec onto the fields that define a
// revision.
//
// A nil spec is treated as the zero spec so that callers in a reconcile loop
// cannot panic on a partially-decoded object.
func revisionPayload(spec *inferencev1alpha1.ModelDeploymentSpec) revisionInput {
	if spec == nil {
		spec = &inferencev1alpha1.ModelDeploymentSpec{}
	}
	return revisionInput{
		Model:  spec.Model,
		Engine: spec.Engine,
		Serving: revisionServing{
			Port: spec.Serving.Port,
			Shim: spec.Serving.Shim,
		},
	}
}

// Encode returns the original unversioned encoding. It is retained for legacy
// fixtures and compatibility tooling; current controller code uses
// NewSnapshot.
//
// Hash hashes exactly these bytes. Recorder.Record without an explicit payload
// also stores them, allowing upgrade tests to construct history written by the
// pre-v2 controller.
func Encode(spec *inferencev1alpha1.ModelDeploymentSpec) ([]byte, error) {
	return json.Marshal(revisionPayload(spec))
}

// Hash returns a legacy revision identifier. Current controller code uses the
// identifier returned by NewSnapshot.
//
// The result is stable across processes and across restarts: it is FNV-1a/32
// over the canonical JSON from Encode, then encoded with
// rand.SafeEncodeString — the same construction upstream Kubernetes uses for
// pod-template-hash. That yields a short (at most 10 characters), lowercase
// alphanumeric string that is valid both as a label value and as a DNS-1123
// name suffix, which is required because it is used as both (see
// naming.LabelRevision and naming.ControllerRevision).
//
// Equal specs always hash equally; a change to any included field changes the
// hash. Changes to Replicas or Rollout do NOT change it — see revisionInput for
// why those are excluded.
//
// FNV-32 is a non-cryptographic hash chosen for stability and brevity, not
// collision resistance: this identifies revisions authored by the same trusted
// controller, it is not a defence against a crafted collision.
func Hash(spec *inferencev1alpha1.ModelDeploymentSpec) string {
	data, err := Encode(spec)
	if err != nil {
		// Unreachable with the current types: revisionInput contains only
		// JSON-representable values (no channels, funcs or cyclic pointers).
		// Degrade to a deterministic hash of the failure rather than panicking
		// inside a reconcile loop — a stable wrong answer is recoverable, a
		// crashing controller is not.
		data = []byte("llmcp: revision encode error: " + err.Error())
	}

	return hashBytes(data)
}

func hashBytes(data []byte) string {
	h := fnv.New32a()
	// hash.Hash's Write never returns an error.
	_, _ = h.Write(data)
	return rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
}
