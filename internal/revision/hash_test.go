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

package revision

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// ptrTo is a local generic pointer helper so the tests do not depend on any
// particular utility package.
func ptrTo[T any](v T) *T { return &v }

// baseSpec is a fully-populated spec: every field the hash is meant to cover is
// non-zero, so a test that mutates one field is genuinely changing something.
//
// Resources deliberately carries MULTIPLE map keys in both Limits and Requests,
// because a single-key map cannot expose a key-ordering bug.
func baseSpec() *inferencev1alpha1.ModelDeploymentSpec {
	return &inferencev1alpha1.ModelDeploymentSpec{
		Replicas: ptrTo(int32(3)),
		Model: inferencev1alpha1.ModelSpec{
			Name: "qwen3-0.6b",
			Source: inferencev1alpha1.ModelSourceSpec{
				Image: &inferencev1alpha1.ImageModelSource{
					Image:      "ghcr.io/example/qwen3-0.6b:v1",
					Path:       "/models/model.gguf",
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
		Engine: inferencev1alpha1.EngineSpec{
			Type:            inferencev1alpha1.EngineLlamaCPP,
			Image:           "ghcr.io/ggml-org/llama.cpp:server",
			ImagePullPolicy: corev1.PullIfNotPresent,
			ContextSize:     ptrTo(int32(4096)),
			MaxConcurrency:  ptrTo(int32(4)),
			Threads:         ptrTo(int32(2)),
			ExtraArgs:       []string{"--flash-attn", "--mlock"},
			Env: []corev1.EnvVar{
				{Name: "LLAMA_LOG_LEVEL", Value: "info"},
				{Name: "OMP_NUM_THREADS", Value: "2"},
			},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
		},
		Serving: inferencev1alpha1.ServingSpec{
			Port:           8080,
			StartupTimeout: &metav1.Duration{Duration: 300_000_000_000},
		},
		Rollout: inferencev1alpha1.RolloutSpec{
			Type:             inferencev1alpha1.RolloutRollingUpdate,
			MaxSurge:         ptrTo(intstr.FromInt32(1)),
			MaxUnavailable:   ptrTo(intstr.FromInt32(0)),
			ProgressDeadline: &metav1.Duration{Duration: 600_000_000_000},
			AutoRollback:     ptrTo(true),
		},
	}
}

// TestHashIsDeterministic is the property the whole package rests on: the same
// input must produce the same identifier, forever and in any process. It is
// repeated many times because the risk being guarded against — Go's randomized
// map iteration order reaching the hash input — is intermittent by nature and
// would pass a single-shot test roughly always.
func TestHashIsDeterministic(t *testing.T) {
	spec := baseSpec()

	seen := make(map[string]struct{})
	for range 1000 {
		seen[Hash(spec)] = struct{}{}
	}

	if len(seen) != 1 {
		t.Fatalf("Hash produced %d distinct values over 1000 calls, want 1: %v", len(seen), keysOf(seen))
	}
}

// TestHashEqualSpecsHashEqually checks that equality of VALUE, not identity of
// pointer, is what the hash observes. The two specs share no memory.
func TestHashEqualSpecsHashEqually(t *testing.T) {
	a, b := baseSpec(), baseSpec()

	if Hash(a) != Hash(b) {
		t.Fatalf("separately constructed equal specs hashed differently: %q vs %q", Hash(a), Hash(b))
	}
}

// TestHashChangesOnIncludedFields walks every field that is part of the
// revision and asserts a single mutation moves the hash. A field that fails
// here would silently pin a ModelDeployment to a stale pod template: the spec
// changes, the hash does not, and no rollout is ever triggered.
func TestHashChangesOnIncludedFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inferencev1alpha1.ModelDeploymentSpec)
	}{
		{"model name", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Model.Name = "qwen3-1.7b"
		}},
		{"model image", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Model.Source.Image.Image = "ghcr.io/example/qwen3-0.6b:v2"
		}},
		{"model path", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Model.Source.Image.Path = "/models/other.gguf"
		}},
		{"model pull policy", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Model.Source.Image.PullPolicy = corev1.PullAlways
		}},
		{"engine type", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Type = inferencev1alpha1.EngineType("vllm")
		}},
		{"engine image", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Image = "ghcr.io/ggml-org/llama.cpp:server-b1234"
		}},
		{"context size", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.ContextSize = ptrTo(int32(8192))
		}},
		{"max concurrency", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.MaxConcurrency = ptrTo(int32(8))
		}},
		{"threads", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Threads = ptrTo(int32(4))
		}},
		{"extra arg appended", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.ExtraArgs = append(s.Engine.ExtraArgs, "--verbose")
		}},
		{"extra arg reordered", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.ExtraArgs = []string{"--mlock", "--flash-attn"}
		}},
		{"env var value", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Env[0].Value = "debug"
		}},
		{"env var order", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Env[0], s.Engine.Env[1] = s.Engine.Env[1], s.Engine.Env[0]
		}},
		{"cpu limit", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("4")
		}},
		{"memory request", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Engine.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("3Gi")
		}},
		{"serving port", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Serving.Port = 9090
		}},
	}

	original := Hash(baseSpec())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := baseSpec()
			tc.mutate(mutated)

			if got := Hash(mutated); got == original {
				t.Fatalf("mutating %s did not change the hash (still %q)", tc.name, got)
			}
		})
	}
}

// TestHashIgnoresExcludedFields is the counterpart, and the more consequential
// half. A hash that moved with Replicas would make every autoscaler tick roll
// the fleet; a hash that moved with Rollout would restart an in-flight rollout
// the moment someone widened its deadline to give it more time.
func TestHashIgnoresExcludedFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inferencev1alpha1.ModelDeploymentSpec)
	}{
		{"replicas increased", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Replicas = ptrTo(int32(42))
		}},
		{"replicas unset", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Replicas = nil
		}},
		{"replicas zero", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Replicas = ptrTo(int32(0))
		}},
		{"rollout type", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout.Type = inferencev1alpha1.RolloutRecreate
		}},
		{"max surge", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout.MaxSurge = ptrTo(intstr.FromString("50%"))
		}},
		{"max unavailable", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout.MaxUnavailable = ptrTo(intstr.FromInt32(2))
		}},
		{"progress deadline", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout.ProgressDeadline = &metav1.Duration{Duration: 1_200_000_000_000}
		}},
		{"auto rollback disabled", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout.AutoRollback = ptrTo(false)
		}},
		{"rollout emptied", func(s *inferencev1alpha1.ModelDeploymentSpec) {
			s.Rollout = inferencev1alpha1.RolloutSpec{}
		}},
	}

	original := Hash(baseSpec())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := baseSpec()
			tc.mutate(mutated)

			if got := Hash(mutated); got != original {
				t.Fatalf("mutating %s changed the hash: %q -> %q", tc.name, original, got)
			}
		})
	}
}

// TestHashFormat pins the shape of the output. The value is used both as a
// label value and as a DNS-1123 name suffix (naming.ControllerRevision), so a
// hash that is empty, long, or contains an unexpected character does not
// produce a wrong revision — it produces an object the API server rejects.
func TestHashFormat(t *testing.T) {
	dns1123Label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	specs := map[string]*inferencev1alpha1.ModelDeploymentSpec{
		"populated": baseSpec(),
		"zero":      {},
		"nil-safe":  nil,
	}
	// A spread of inputs, so the assertion covers many distinct hash values
	// rather than one lucky one.
	for i := range 200 {
		s := baseSpec()
		s.Serving.Port = int32(1000 + i)
		specs[fmt.Sprintf("port-%d", s.Serving.Port)] = s
	}

	for name, spec := range specs {
		got := Hash(spec)

		if got == "" {
			t.Fatalf("%s: Hash returned an empty string", name)
		}
		if len(got) > 10 {
			t.Fatalf("%s: Hash returned %q (%d chars), want at most 10", name, got, len(got))
		}
		if !dns1123Label.MatchString(got) {
			t.Fatalf("%s: Hash returned %q, which is not a valid DNS-1123 label", name, got)
		}
	}
}

// TestHashResourceMapOrdering is the test that makes the "maps are safe here"
// comment in hash.go load-bearing rather than aspirational.
//
// corev1.ResourceList is a Go map, and the two specs below populate it with the
// same content in opposite insertion orders. If the hash ever observed
// iteration order — for example by hashing a fmt-printed map, or by switching
// to an encoder that does not sort keys — this test fails.
func TestHashResourceMapOrdering(t *testing.T) {
	forward := baseSpec()
	forward.Engine.Resources.Limits = corev1.ResourceList{}
	forward.Engine.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
	forward.Engine.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	forward.Engine.Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("10Gi")

	reverse := baseSpec()
	reverse.Engine.Resources.Limits = corev1.ResourceList{}
	reverse.Engine.Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("10Gi")
	reverse.Engine.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	reverse.Engine.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")

	// Repeated, because map iteration order is randomized per range statement:
	// a single comparison could agree by chance.
	for i := range 200 {
		if a, b := Hash(forward), Hash(reverse); a != b {
			t.Fatalf("iteration %d: identical resource maps built in different orders hashed differently: %q vs %q", i, a, b)
		}
	}
}

// TestEncodeExcludesNonRevisionFields asserts on the stored bytes directly, not
// just on the hash: Encode's output is what lands in a ControllerRevision, so a
// leaked field would be persisted in cluster history and could not be taken
// back without invalidating every recorded revision.
func TestEncodeExcludesNonRevisionFields(t *testing.T) {
	data, err := Encode(baseSpec())
	if err != nil {
		t.Fatalf("Encode returned an error: %v", err)
	}

	payload := string(data)
	for _, forbidden := range []string{"replicas", "rollout", "maxSurge", "progressDeadline", "startupTimeout"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("Encode payload contains excluded field %q: %s", forbidden, payload)
		}
	}
	for _, required := range []string{"model", "engine", "serving", "port"} {
		if !strings.Contains(payload, required) {
			t.Fatalf("Encode payload is missing required field %q: %s", required, payload)
		}
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
