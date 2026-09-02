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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
)

// ThreadsFor resolves the engine's worker-thread count.
//
// # Why this exists at all
//
// llama.cpp sizes its thread pool from the HOST's CPU topology, which it reads
// out of /proc. It has no idea it is in a container, and a cgroup CPU limit is
// not visible there: a pod limited to 2 cores on a 64-core node still starts 64
// worker threads. Put four such replicas on that node and there are 256 runnable
// threads competing for 8 cores' worth of quota. Throughput barely moves, but
// tail latency stops being a property of the model and becomes a property of
// whichever pods happened to land together.
//
// That is not a micro-optimisation, it is a correctness problem for this whole
// project. The canary analysis in a later sprint promotes or rolls back on
// latency percentiles. If p95 is dominated by CPU-scheduler noise, the analysis
// compares noise to noise and rolls back healthy revisions at random — and an
// intermittent false rollback is far more expensive to debug than a slow pod.
// So the operator always passes an explicit -t derived from the limit.
//
// # Resolution order
//
//  1. spec.Engine.Threads, if the user set it. An explicit value always wins:
//     someone tuning threads against a specific model knows something we don't.
//  2. The CPU LIMIT. This is the number that actually bounds the container.
//  3. The CPU REQUEST, when there is no limit — a Burstable pod is scheduled
//     against its request, so that is the capacity it can rely on.
//  4. The fallback argument, when the pod is BestEffort and neither is set.
//
// The quantity is floored to whole cores (2500m -> 2), because a fractional
// thread does not exist and rounding up would reintroduce the oversubscription
// this function exists to prevent. The result is clamped to a minimum of 1, so
// that a sub-core limit such as 500m yields one thread rather than zero — an
// engine told to run zero threads either refuses to start or picks the host
// default, and both outcomes defeat the point.
func ThreadsFor(spec *v1alpha1.ModelDeploymentSpec, fallback int32) int32 {
	if spec == nil {
		return atLeastOneThread(fallback)
	}

	// 1. Explicit user value.
	if spec.Engine.Threads != nil {
		return atLeastOneThread(*spec.Engine.Threads)
	}

	// 2. CPU limit, then 3. CPU request. Note the map lookups here read a
	// fixed, known key rather than ranging over the ResourceList, so no map
	// iteration order leaks into the output.
	res := spec.Engine.Resources
	if q, ok := res.Limits[corev1.ResourceCPU]; ok {
		return coresFrom(q)
	}
	if q, ok := res.Requests[corev1.ResourceCPU]; ok {
		return coresFrom(q)
	}

	// 4. Nothing to derive from.
	return atLeastOneThread(fallback)
}

// coresFrom floors a CPU quantity to whole cores, with a minimum of one.
//
// MilliValue is used rather than Value because Value already rounds UP for
// sub-core quantities (500m reports 1 core), which hides the distinction this
// function is meant to make explicit.
func coresFrom(q resource.Quantity) int32 {
	cores := q.MilliValue() / 1000
	if cores < 1 {
		// Covers 500m, and also the zero/negative quantities that CEL's
		// Minimum validation should already prevent. Defence in depth: this
		// value goes straight onto a command line, and "-t 0" is worse than
		// any wrong-but-positive answer.
		return 1
	}
	if cores > math.MaxInt32 {
		// Unreachable with any real node, but the conversion below is only
		// safe because of this clamp.
		return math.MaxInt32
	}
	return int32(cores)
}

// atLeastOneThread clamps a thread count to a minimum of 1, so that a caller's
// zero-valued fallback can never become "-t 0".
func atLeastOneThread(n int32) int32 {
	if n < 1 {
		return 1
	}
	return n
}
