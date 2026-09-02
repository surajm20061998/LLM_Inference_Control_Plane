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

// Package engine is the seam between the operator's control logic and the
// inference servers it runs. Everything that is true of llama.cpp but not of
// vLLM — flag spellings, health endpoint, which model sources it can consume —
// lives behind the Profile interface, so that adding an engine is a new file in
// this package and no edit anywhere else.
//
// # Byte-stability
//
// The containers a Profile builds are applied with Server-Side Apply and the
// resulting Deployment is watched. If Build produced a different byte sequence
// for identical input — a re-ordered args slice, a map iterated into output —
// every reconcile would write a "change", the watch would fire, and the
// controller and the API server would fight in a hot loop that never converges.
//
// Therefore: a Profile must NEVER iterate a Go map to produce output. Map
// iteration order in Go is deliberately randomised per range statement, so such
// a bug is invisible in a single run and reliably fatal in production. Build
// args from ordered slices, copy caller slices preserving their order, and if a
// value must ever be derived from a map, sort the keys explicitly first.
// TestBuildIsDeterministic enforces this.
package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
)

// ModelVolumeName is the pod volume the model file is mounted from. The
// controller creates and populates this volume (from an image, a PVC, or an
// emptyDir an init container downloads into); a profile only mounts it, so that
// how weights arrive stays independent of which engine consumes them.
const ModelVolumeName = "model"

// ContainerName is the name of the engine container in every pod template.
//
// It is stable API: `kubectl logs -c engine` is in every runbook, and a
// Deployment's containers are matched by name during a Server-Side Apply, so
// renaming this would orphan the old container rather than update it.
const ContainerName = "engine"

// BuildContext is everything a profile needs to build its container. It is a
// value, not a pointer, because a profile must not mutate the caller's state:
// Build is expected to be a pure function of this input.
type BuildContext struct {
	// MD is the ModelDeployment being reconciled. Profiles read Spec; they must
	// not write to it.
	MD *v1alpha1.ModelDeployment

	// Variant says which side of a rollout this container serves. Profiles
	// generally ignore it — a canary must run the same engine configuration as
	// its primary or the comparison between them is meaningless — but it is
	// available for engines that genuinely need it.
	Variant v1alpha1.Variant

	// ModelPath is the absolute in-container path to the model file, or "" when
	// the engine fetches its own weights.
	ModelPath string

	// Threads is the resolved worker-thread count. It is computed once by the
	// controller via ThreadsFor and passed in; profiles must NOT re-derive it,
	// so that the number in the container's args is provably the same number
	// the controller reasoned about.
	Threads int32

	// Port is the container port the engine must listen on. Zero means
	// naming.EnginePort.
	Port int32
}

// BuildResult is a profile's output.
type BuildResult struct {
	// Container is the fully-formed engine container, minus probes.
	Container corev1.Container

	// HealthPath is the HTTP path that reports engine readiness, e.g.
	// "/health". Probes are constructed by the controller, not here: their
	// timings come from ServingSpec and their shape is uniform across engines,
	// so only the path varies.
	HealthPath string
}

// Profile builds the engine-specific parts of a serving pod.
//
// Implementations must be stateless and their Build must be deterministic; see
// the package comment on byte-stability.
type Profile interface {
	// Type is the EngineSpec.Type value this profile is selected by.
	Type() v1alpha1.EngineType

	// DefaultImage is the container image used when EngineSpec.Image is empty.
	// It must be a build-pinned reference, never a floating tag.
	DefaultImage() string

	// Validate reports spec/engine combinations that CEL cannot express.
	//
	// Returned errors are treated as terminal by the caller: the controller
	// will not retry until the spec changes. So return an error here only for
	// conditions no amount of waiting can fix, never for transient ones.
	Validate(spec *v1alpha1.ModelDeploymentSpec) error

	// Build produces the engine container for one variant.
	Build(bc BuildContext) (BuildResult, error)
}

// registry maps engine type to profile.
//
// It is guarded by a mutex rather than documented as init-only. Registration
// does happen in init() for the real profiles, but tests substitute fake
// profiles after init, and controller tests run reconciles from several
// goroutines at once — a data race there would surface as a flake in an
// unrelated test rather than as an obvious bug here. The lock is taken once per
// reconcile and is not on any hot path, so it costs nothing worth measuring.
var (
	registryMu sync.RWMutex
	registry   = map[v1alpha1.EngineType]Profile{}
)

// Register adds a profile to the registry, keyed by its Type.
//
// It panics if a type is registered twice or if the profile reports an empty
// type. Both are programming errors that can only be introduced at compile
// time, and both would otherwise show up as a silently wrong engine running in
// a cluster — failing at process start is strictly kinder.
func Register(p Profile) {
	if p == nil {
		panic("engine: Register called with a nil Profile")
	}
	t := p.Type()
	if t == "" {
		panic("engine: Register called with a Profile whose Type() is empty")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[t]; dup {
		panic(fmt.Sprintf("engine: duplicate Profile registration for type %q", t))
	}
	registry[t] = p
}

// Get returns the profile for an engine type.
//
// An unknown type is not merely a lookup miss: it means the CRD's enum and the
// compiled-in profiles have diverged, most likely because a cluster is running
// a newer CRD than its operator. The error names the supported types so that
// the resulting status condition says so out loud.
func Get(t v1alpha1.EngineType) (Profile, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	if p, ok := registry[t]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("engine: unknown engine type %q; supported types are: %s",
		t, strings.Join(registeredTypesLocked(), ", "))
}

// registeredTypesLocked returns the registered type names in sorted order.
// Callers must hold registryMu. Sorting is not cosmetic: this string reaches a
// status condition, and an unsorted (map-ordered) list would change on every
// reconcile and cause the condition to be rewritten forever.
func registeredTypesLocked() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, string(t))
	}
	slices.Sort(out)
	return out
}
