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
// The identifier and the payload are produced from ONE definition — see
// revisionInput — so that the bytes that were hashed and the bytes that were
// stored can never drift apart.
package revision

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

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
// Serving contributes only Port: it is the port the container listens on and so
// is part of the pod template. ServingSpec.StartupTimeout is currently probe
// tuning that the controller applies to the pod template as well, but it is
// left out on purpose — see the Serving comment below.
//
// Changing this struct changes every existing revision hash in every cluster,
// which orphans recorded history and forces a one-time rollout of every
// ModelDeployment. Treat it as effectively permanent.
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
	// Port is the port the engine's Service — and therefore its container —
	// listens on.
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

// Encode returns the canonical JSON encoding of the revision-defining subset of
// spec.
//
// This is the single source of truth for "what a revision is": Hash hashes
// exactly these bytes, and Recorder.Record stores exactly these bytes in the
// ControllerRevision's Data.Raw. Keeping one function means the recorded
// payload can never describe a different revision than the hash that names it —
// if the two were computed independently, a change to one would silently
// produce history that cannot be verified against its own identifier.
//
// SpecFrom is the inverse.
func Encode(spec *inferencev1alpha1.ModelDeploymentSpec) ([]byte, error) {
	return json.Marshal(revisionPayload(spec))
}

// Hash returns the revision identifier for the pod-template-affecting subset
// of spec.
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

	h := fnv.New32a()
	// hash.Hash's Write never returns an error.
	_, _ = h.Write(data)

	return rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
}
