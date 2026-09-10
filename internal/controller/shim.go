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

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// DefaultShimImage is the metrics sidecar image used when neither the operator
// flag nor the ModelDeployment overrides it.
//
// Pinned, never floating. The shim's metric NAMES are the contract the
// controller's own analysis code queries, so running a shim from a different
// build than the controller is a version skew whose symptom is an empty query
// result — which analysis is obliged to read as "no data" and which therefore
// stalls a canary rather than failing it. A pinned tag makes that skew a
// deliberate act.
// It is deliberately a published reference rather than the local development
// registry: a binary's compiled-in default should be something that exists for
// anyone who installs it. The local kind workflow overrides it with
// --shim-image, which config/dev sets.
const DefaultShimImage = "ghcr.io/surajm20061998/llmcp-shim:v0.1.0"

// Shim probe timings.
//
// These are much tighter than the engine's, and that is the point: the shim is
// a small Go binary with nothing to load, so it either starts in a second or it
// is broken. Waiting on it with the engine's generous budget would delay every
// pod start by the difference.
const (
	shimStartupPeriodSeconds   int32 = 1
	shimStartupFailureThresh   int32 = 30
	shimReadinessPeriodSeconds int32 = 5
	shimReadinessFailureThresh int32 = 3
	shimLivenessPeriodSeconds  int32 = 10
	shimLivenessFailureThresh  int32 = 6
)

// Default shim resources.
//
// A memory limit but deliberately NO CPU limit. A CPU limit on the shim would
// let the kubelet throttle the process that sits in the request path of every
// inference call — and the time lost to that throttling is indistinguishable,
// in the resulting histogram, from the engine being slow. The metric would
// then move because the sidecar was squeezed, and a canary gate reading it
// would roll back a release for a reason that has nothing to do with the
// release. A request reserves the shim a floor of CPU; a limit would put a
// ceiling on the measurement itself.
var (
	shimCPURequest    = resource.MustParse("50m")
	shimMemoryRequest = resource.MustParse("64Mi")
	shimMemoryLimit   = resource.MustParse("128Mi")
)

// buildShimContainer renders the metrics sidecar for one variant.
//
// It is returned as a NATIVE SIDECAR — an entry in initContainers carrying
// restartPolicy: Always — rather than as an ordinary container. Three
// properties follow from that, and all three are needed here:
//
//   - It is STARTED BEFORE the engine, so there is no window in which the
//     engine is serving and nothing is measuring it.
//   - It is TERMINATED AFTER the engine, so requests in flight during a pod
//     shutdown are still counted rather than vanishing from the error rate.
//   - Its readiness counts toward the POD's readiness, so a pod cannot join a
//     Service with a dead proxy in front of a healthy engine.
//
// As an ordinary container none of the three would hold, and the failure they
// prevent — traffic served without being measured — is silent.
func buildShimContainer(
	md *inferencev1alpha1.ModelDeployment,
	variant inferencev1alpha1.Variant,
	healthPath string,
	image string,
) corev1.Container {
	shim := md.Spec.Serving.Shim

	if shim.Image != "" {
		image = shim.Image
	}

	logLevel := shim.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	return corev1.Container{
		Name:  naming.ShimContainerName,
		Image: image,

		// Always: the shim ships alongside the operator under a mutable
		// development tag, and a node that has cached an older layer would keep
		// running a shim whose metric names no longer match what the controller
		// queries.
		ImagePullPolicy: md.Spec.Engine.ImagePullPolicy,

		// restartPolicy: Always on an init container is what MAKES it a native
		// sidecar. Without this field the entry is an ordinary init container:
		// the kubelet waits for the shim to EXIT before starting the engine, and
		// since the shim is a server that never exits, the pod hangs in Init
		// forever.
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),

		Args: shimArgs(md, variant, healthPath, logLevel),

		Ports: []corev1.ContainerPort{
			{
				// The shim takes the "http" name, which is what the serving
				// Service targets. That is the whole traffic redirection: no
				// Service change, no client change, the proxy simply becomes
				// what "http" resolves to.
				Name:          naming.PortNameHTTP,
				ContainerPort: naming.ShimPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          naming.PortNameMetrics,
				ContainerPort: naming.ShimMetricsPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},

		StartupProbe:   shimProbe(naming.ShimLivePath, shimStartupPeriodSeconds, shimStartupFailureThresh),
		ReadinessProbe: shimProbe(naming.ShimHealthPath, shimReadinessPeriodSeconds, shimReadinessFailureThresh),
		LivenessProbe:  shimProbe(naming.ShimLivePath, shimLivenessPeriodSeconds, shimLivenessFailureThresh),

		Resources:       shimResources(shim),
		SecurityContext: shimSecurityContext(),
	}
}

// shimArgs renders the sidecar's command line.
//
// Configuration is by flag, not environment, so that `kubectl describe pod`
// shows which upstream a shim is proxying and which variant it will label its
// metrics with. When a canary's numbers look wrong that is the first thing to
// check, and needing to exec into a container to read it makes the check slower
// than the question deserves.
//
// The order is fixed. These arguments are applied with Server-Side Apply and
// the resulting Deployment is watched; a reordered slice reads as a change on
// every reconcile and the controller loops against itself.
func shimArgs(
	md *inferencev1alpha1.ModelDeployment,
	variant inferencev1alpha1.Variant,
	healthPath, logLevel string,
) []string {
	return []string{
		fmt.Sprintf("--listen=:%d", naming.ShimPort),
		fmt.Sprintf("--metrics-listen=:%d", naming.ShimMetricsPort),
		// Loopback, not the pod IP: the engine and the shim share a network
		// namespace, so this hop never leaves the pod. Addressing the engine
		// through a Service instead would put kube-proxy in the measurement
		// path and could route to a DIFFERENT pod, which would make every
		// per-pod metric a lie.
		fmt.Sprintf("--upstream=http://127.0.0.1:%d", naming.EnginePort),
		"--health-path=" + healthPath,
		"--model=" + md.Spec.Model.Name,
		"--namespace=" + md.Namespace,
		"--model-deployment=" + md.Name,
		"--variant=" + string(variant),
		// The engine's concurrency, so the shim can derive queue depth:
		// in-flight requests beyond this many are, by definition, waiting.
		fmt.Sprintf("--max-concurrency=%d", maxConcurrencyFor(md)),
		"--log-level=" + logLevel,
	}
}

// maxConcurrencyFor mirrors the engine's concurrency setting.
func maxConcurrencyFor(md *inferencev1alpha1.ModelDeployment) int32 {
	if n := md.Spec.Engine.MaxConcurrency; n != nil && *n > 0 {
		return *n
	}
	return 4
}

// shimProbe builds one HTTP probe against the shim's own listener.
func shimProbe(path string, period, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(naming.ShimPort),
			},
		},
		PeriodSeconds:    period,
		FailureThreshold: failures,
		SuccessThreshold: 1,
		TimeoutSeconds:   2,
	}
}

// shimResources resolves the sidecar's compute resources, defaulting when the
// user set none.
//
// The user's value is taken WHOLE when present rather than merged field by
// field. A partial merge here would let someone who set only a memory limit
// silently inherit a CPU limit they never asked for — and per the note on
// shimCPURequest, a CPU limit on this container distorts the metric it exists
// to produce.
func shimResources(shim inferencev1alpha1.ShimSpec) corev1.ResourceRequirements {
	if len(shim.Resources.Requests) > 0 || len(shim.Resources.Limits) > 0 {
		return *shim.Resources.DeepCopy()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    shimCPURequest.DeepCopy(),
			corev1.ResourceMemory: shimMemoryRequest.DeepCopy(),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: shimMemoryLimit.DeepCopy(),
		},
	}
}

// shimSecurityContext returns the restricted-profile context the sidecar runs
// with. It matches the engine's: this container terminates untrusted client
// connections, so there is no reason for it to be less constrained than the
// process behind it.
func shimSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		RunAsNonRoot:             ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
