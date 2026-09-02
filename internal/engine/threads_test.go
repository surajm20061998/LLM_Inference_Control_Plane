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
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
)

func TestThreadsFor(t *testing.T) {
	tests := []struct {
		name     string
		threads  *int32
		limits   corev1.ResourceList
		requests corev1.ResourceList
		fallback int32
		want     int32
	}{
		{
			name:     "explicit threads wins over everything",
			threads:  int32Ptr(3),
			limits:   cpu("16"),
			requests: cpu("8"),
			fallback: 99,
			want:     3,
		},
		{
			name:     "explicit threads wins with no resources at all",
			threads:  int32Ptr(12),
			fallback: 2,
			want:     12,
		},
		{
			name:     "limit wins over request",
			limits:   cpu("2"),
			requests: cpu("8"),
			fallback: 99,
			want:     2,
		},
		{
			name:     "request used when there is no limit",
			requests: cpu("6"),
			fallback: 99,
			want:     6,
		},
		{
			name:     "request used when limits exist but carry no cpu",
			limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
			requests: cpu("4"),
			fallback: 99,
			want:     4,
		},
		{
			name:     "fallback when neither limit nor request is set",
			fallback: 4,
			want:     4,
		},
		{
			name:     "fallback when resources carry only memory",
			limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
			requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			fallback: 7,
			want:     7,
		},
		{
			// The single most important row: half a core must still get one
			// thread. Zero threads makes llama.cpp fall back to the host core
			// count, which is the exact failure this derivation prevents.
			name:     "fractional cpu floors to 1 not 0",
			limits:   cpu("500m"),
			fallback: 99,
			want:     1,
		},
		{
			name:     "very small cpu still yields 1",
			limits:   cpu("10m"),
			fallback: 99,
			want:     1,
		},
		{
			name:     "2500m floors to 2",
			limits:   cpu("2500m"),
			fallback: 99,
			want:     2,
		},
		{
			name:     "1999m floors to 1",
			limits:   cpu("1999m"),
			fallback: 99,
			want:     1,
		},
		{
			name:     "whole cores pass through",
			limits:   cpu("8"),
			fallback: 99,
			want:     8,
		},
		{
			name:     "large value passes through",
			limits:   cpu("192"),
			fallback: 99,
			want:     192,
		},
		{
			// CEL's Minimum=1 keeps these out of a real cluster, but the value
			// lands on a command line, so "-t 0" must be unreachable.
			name:     "zero threads clamps to 1",
			threads:  int32Ptr(0),
			fallback: 8,
			want:     1,
		},
		{
			name:     "negative threads clamps to 1",
			threads:  int32Ptr(-4),
			fallback: 8,
			want:     1,
		},
		{
			name:     "zero cpu limit clamps to 1",
			limits:   cpu("0"),
			fallback: 99,
			want:     1,
		},
		{
			name:     "negative cpu limit clamps to 1",
			limits:   cpu("-2"),
			fallback: 99,
			want:     1,
		},
		{
			name:     "zero fallback clamps to 1",
			fallback: 0,
			want:     1,
		},
		{
			name:     "negative fallback clamps to 1",
			fallback: -3,
			want:     1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &v1alpha1.ModelDeploymentSpec{
				Engine: v1alpha1.EngineSpec{
					Threads: tc.threads,
					Resources: corev1.ResourceRequirements{
						Limits:   tc.limits,
						Requests: tc.requests,
					},
				},
			}

			if got := ThreadsFor(spec, tc.fallback); got != tc.want {
				t.Errorf("ThreadsFor() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestThreadsForNilSpec(t *testing.T) {
	if got := ThreadsFor(nil, 4); got != 4 {
		t.Errorf("ThreadsFor(nil, 4) = %d, want 4", got)
	}
	if got := ThreadsFor(nil, 0); got != 1 {
		t.Errorf("ThreadsFor(nil, 0) = %d, want 1", got)
	}
}

// TestThreadsForIsPure asserts ThreadsFor does not mutate the spec it reads.
// The spec it is handed in production comes straight out of the informer cache,
// which is shared with every other controller in the process.
func TestThreadsForIsPure(t *testing.T) {
	spec := &v1alpha1.ModelDeploymentSpec{
		Engine: v1alpha1.EngineSpec{
			Resources: corev1.ResourceRequirements{Limits: cpu("4")},
		},
	}
	before := spec.DeepCopy()

	for range 10 {
		if got := ThreadsFor(spec, 1); got != 4 {
			t.Fatalf("ThreadsFor() = %d, want 4", got)
		}
	}

	if !apiEqual(before, spec) {
		t.Errorf("ThreadsFor mutated the spec:\n before: %#v\n  after: %#v", before, spec)
	}
}

func TestCoresFromClampsOverflow(t *testing.T) {
	// A quantity far beyond any real node must not wrap the int32 conversion
	// into a negative thread count.
	huge := resource.MustParse("999999999999")
	if got := coresFrom(huge); got != math.MaxInt32 {
		t.Errorf("coresFrom(huge) = %d, want %d", got, int32(math.MaxInt32))
	}
}

// --- helpers ---------------------------------------------------------------

func cpu(q string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(q)}
}

func int32Ptr(v int32) *int32 { return &v }
