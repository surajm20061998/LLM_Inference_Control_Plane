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

package engine

import (
	"maps"
	"strings"
	"testing"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
)

func TestGetReturnsRegisteredProfiles(t *testing.T) {
	tests := []struct {
		name      string
		typ       v1alpha1.EngineType
		wantImage string
	}{
		{
			name:      "llamacpp",
			typ:       v1alpha1.EngineLlamaCPP,
			wantImage: LlamaCPPDefaultImage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Get(tc.typ)
			if err != nil {
				t.Fatalf("Get(%q) returned error: %v", tc.typ, err)
			}
			if p == nil {
				t.Fatalf("Get(%q) returned a nil Profile with no error", tc.typ)
			}
			if got := p.Type(); got != tc.typ {
				t.Errorf("Type() = %q, want %q", got, tc.typ)
			}
			if got := p.DefaultImage(); got != tc.wantImage {
				t.Errorf("DefaultImage() = %q, want %q", got, tc.wantImage)
			}
		})
	}
}

func TestGetUnknownTypeErrorsClearly(t *testing.T) {
	tests := []struct {
		name string
		typ  v1alpha1.EngineType
	}{
		{name: "not yet implemented engine", typ: v1alpha1.EngineType("vllm")},
		{name: "empty type", typ: v1alpha1.EngineType("")},
		{name: "typo", typ: v1alpha1.EngineType("llama.cpp")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Get(tc.typ)
			if err == nil {
				t.Fatalf("Get(%q) = %v, want an error", tc.typ, p)
			}
			if p != nil {
				t.Errorf("Get(%q) returned a non-nil Profile alongside an error", tc.typ)
			}

			msg := err.Error()
			// The message reaches a status condition, so it has to name both
			// the bad value and what would have been acceptable.
			if !strings.Contains(msg, string(tc.typ)) {
				t.Errorf("error %q does not mention the offending type %q", msg, tc.typ)
			}
			if !strings.Contains(msg, string(v1alpha1.EngineLlamaCPP)) {
				t.Errorf("error %q does not list the supported types", msg)
			}
		})
	}
}

// TestGetErrorListsTypesInStableOrder guards the same hot-loop hazard as
// TestBuildIsDeterministic: this string is written into a status condition, and
// a map-ordered list would rewrite that condition on every reconcile.
func TestGetErrorListsTypesInStableOrder(t *testing.T) {
	restore := withFakeProfiles(t,
		fakeProfile{typ: "zebra"},
		fakeProfile{typ: "aardvark"},
	)
	defer restore()

	_, err := Get("nope")
	if err == nil {
		t.Fatal("Get on an unknown type returned no error")
	}
	first := err.Error()

	for i := range 50 {
		_, err := Get("nope")
		if err == nil {
			t.Fatal("Get on an unknown type returned no error")
		}
		if err.Error() != first {
			t.Fatalf("error message is not stable:\n first: %s\n  iter %d: %s", first, i, err.Error())
		}
	}

	// Sorted, not insertion-ordered.
	if !strings.Contains(first, "aardvark, llamacpp, zebra") {
		t.Errorf("supported types are not sorted in %q", first)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
	}{
		{name: "nil profile", profile: nil},
		{name: "empty type", profile: fakeProfile{typ: ""}},
		{name: "duplicate type", profile: fakeProfile{typ: v1alpha1.EngineLlamaCPP}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%#v) did not panic", tc.profile)
				}
			}()
			Register(tc.profile)
		})
	}
}

func TestRegisterAddsProfile(t *testing.T) {
	restore := withFakeProfiles(t, fakeProfile{typ: "stub"})
	defer restore()

	p, err := Get("stub")
	if err != nil {
		t.Fatalf("Get after Register returned error: %v", err)
	}
	if p.Type() != "stub" {
		t.Errorf("Type() = %q, want %q", p.Type(), "stub")
	}
}

// TestModelVolumeNameIsStable pins the constant the controller and the profile
// both depend on: the controller creates this volume, the profile mounts it,
// and a mismatch is a pod stuck in CreateContainerConfigError.
func TestModelVolumeNameIsStable(t *testing.T) {
	if ModelVolumeName != "model" {
		t.Errorf("ModelVolumeName = %q, want %q", ModelVolumeName, "model")
	}
	if ContainerName != "engine" {
		t.Errorf("ContainerName = %q, want %q", ContainerName, "engine")
	}
}

// --- helpers ---------------------------------------------------------------

// fakeProfile is a minimal Profile used to exercise the registry without
// depending on a real engine's behaviour.
type fakeProfile struct {
	typ v1alpha1.EngineType
}

func (f fakeProfile) Type() v1alpha1.EngineType                    { return f.typ }
func (f fakeProfile) DefaultImage() string                         { return "example.invalid/fake:v0" }
func (f fakeProfile) Validate(*v1alpha1.ModelDeploymentSpec) error { return nil }
func (f fakeProfile) Build(BuildContext) (BuildResult, error)      { return BuildResult{}, nil }

// withFakeProfiles registers extra profiles and returns a function restoring
// the registry to exactly what it was. The registry is package-level state, so
// leaving a fake behind would make an unrelated test fail depending on
// execution order.
func withFakeProfiles(t *testing.T, profiles ...fakeProfile) func() {
	t.Helper()

	registryMu.Lock()
	saved := make(map[v1alpha1.EngineType]Profile, len(registry))
	maps.Copy(saved, registry)
	registryMu.Unlock()

	for _, p := range profiles {
		Register(p)
	}

	return func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		registry = saved
	}
}
