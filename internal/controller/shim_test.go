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
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
)

// flagShimImage stands in for the operator's --shim-image value.
const flagShimImage = "reg/shim:v2"

// shimFrom returns the shim init container from a rendered pod spec.
func shimFrom(t *testing.T, pod *corev1ac.PodSpecApplyConfiguration) *corev1ac.ContainerApplyConfiguration {
	t.Helper()
	for i := range pod.InitContainers {
		c := &pod.InitContainers[i]
		if c.Name != nil && *c.Name == naming.ShimContainerName {
			return c
		}
	}
	return nil
}

// shimPod renders the primary pod spec for md.
func shimPod(t *testing.T, md *v1alpha1.ModelDeployment, opts renderOptions) *corev1ac.PodSpecApplyConfiguration {
	t.Helper()
	pod, err := buildPodSpec(md, cuProfile(t), v1alpha1.VariantPrimary, opts)
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	return pod
}

func TestShimIsInjectedAsANativeSidecar(t *testing.T) {
	t.Parallel()

	pod := shimPod(t, cuNewMD(), renderOptions{})
	shim := shimFrom(t, pod)
	if shim == nil {
		t.Fatal("no shim container was injected; the shim is on by default")
	}

	// restartPolicy: Always on an INIT container is what makes it a native
	// sidecar. Without it the kubelet waits for the shim to exit before
	// starting the engine — and since the shim is a server that never exits,
	// the pod hangs in Init forever.
	if shim.RestartPolicy == nil {
		t.Fatal("the shim has no restartPolicy; as a plain init container the pod would never leave Init")
	}
	if got := *shim.RestartPolicy; got != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("restartPolicy = %q, want %q", got, corev1.ContainerRestartPolicyAlways)
	}

	// It must be an INIT container, not a regular one. Only init containers get
	// the start-before / terminate-after ordering that keeps traffic from being
	// served unmeasured at either end of a pod's life.
	for i := range pod.Containers {
		if pod.Containers[i].Name != nil && *pod.Containers[i].Name == naming.ShimContainerName {
			t.Fatal("the shim is a regular container; it would start after the engine and stop before it")
		}
	}
}

func TestShimStartsAfterTheModelIsStaged(t *testing.T) {
	t.Parallel()

	pod := shimPod(t, cuNewMD(), renderOptions{})
	if len(pod.InitContainers) != 2 {
		t.Fatalf("got %d init containers, want 2", len(pod.InitContainers))
	}
	if got := cuDeref(t, pod.InitContainers[0].Name, "init[0].name"); got != modelInitContainerName {
		t.Fatalf("init[0] = %q, want the model init container first", got)
	}
	if got := cuDeref(t, pod.InitContainers[1].Name, "init[1].name"); got != naming.ShimContainerName {
		t.Fatalf("init[1] = %q, want the shim second", got)
	}
}

func TestShimStartupProbeDoesNotDependOnTheEngine(t *testing.T) {
	t.Parallel()

	// The deadlock this prevents: the kubelet starts the engine only after the
	// sidecar's STARTUP probe passes, so a startup probe that proxied through
	// to the engine would be waiting for a container that has not been started
	// yet. Startup targets the shim's own listener; readiness targets the
	// proxied engine health endpoint.
	shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{}))

	if shim.StartupProbe == nil || shim.StartupProbe.HTTPGet == nil {
		t.Fatal("the shim has no HTTP startup probe")
	}
	if got := cuDeref(t, shim.StartupProbe.HTTPGet.Path, "startup path"); got != naming.ShimLivePath {
		t.Errorf("startup probe path = %q, want %q — anything that reaches the engine deadlocks the pod",
			got, naming.ShimLivePath)
	}

	if shim.ReadinessProbe == nil || shim.ReadinessProbe.HTTPGet == nil {
		t.Fatal("the shim has no HTTP readiness probe")
	}
	if got := cuDeref(t, shim.ReadinessProbe.HTTPGet.Path, "readiness path"); got != naming.ShimHealthPath {
		t.Errorf("readiness probe path = %q, want %q — the pod must not be Ready before the engine is",
			got, naming.ShimHealthPath)
	}
}

func TestShimArgsCarryTheMetricIdentity(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	md.Spec.Engine.MaxConcurrency = ptr.To(int32(6))

	for _, variant := range []v1alpha1.Variant{v1alpha1.VariantPrimary, v1alpha1.VariantCanary} {
		pod, err := buildPodSpec(md, cuProfile(t), variant, renderOptions{})
		if err != nil {
			t.Fatalf("buildPodSpec: %v", err)
		}
		args := shimFrom(t, pod).Args

		want := []string{
			"--model=qwen3",
			"--variant=" + string(variant),
			// Queue depth is derived from this: in-flight requests beyond the
			// engine's concurrency are, by definition, waiting.
			"--max-concurrency=6",
			// Loopback, never a Service. Routing through kube-proxy could land
			// on a DIFFERENT pod, which would make every per-pod metric a lie.
			"--upstream=http://127.0.0.1:8000",
			"--health-path=/health",
		}
		for _, w := range want {
			if !slices.Contains(args, w) {
				t.Errorf("variant %s: args %v missing %q", variant, args, w)
			}
		}
	}
}

func TestShimArgsAreOrderStable(t *testing.T) {
	t.Parallel()

	// The args slice is applied with Server-Side Apply. A reordered slice reads
	// as a change on every reconcile, the watch fires, and the controller loops
	// against itself — a failure that is invisible until it is saturating the
	// API server.
	md := cuNewMD()
	first := shimArgs(md, v1alpha1.VariantPrimary, "/health", "info")
	for range 200 {
		got := shimArgs(md, v1alpha1.VariantPrimary, "/health", "info")
		if !slices.Equal(got, first) {
			t.Fatalf("shimArgs is not deterministic:\nfirst: %v\ngot:   %v", first, got)
		}
	}
}

func TestShimImageResolution(t *testing.T) {
	t.Parallel()

	t.Run("falls back to the build-pinned default", func(t *testing.T) {
		shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{}))
		if got := cuDeref(t, shim.Image, "shim image"); got != DefaultShimImage {
			t.Errorf("image = %q, want %q", got, DefaultShimImage)
		}
	})

	t.Run("the operator flag wins over the default", func(t *testing.T) {
		shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{ShimImage: flagShimImage}))
		if got := cuDeref(t, shim.Image, "shim image"); got != flagShimImage {
			t.Errorf("image = %q, want %q", got, flagShimImage)
		}
	})

	t.Run("the spec wins over the operator flag", func(t *testing.T) {
		md := cuNewMD()
		md.Spec.Serving.Shim.Image = "reg/shim:experiment"
		shim := shimFrom(t, shimPod(t, md, renderOptions{ShimImage: flagShimImage}))
		if got := cuDeref(t, shim.Image, "shim image"); got != "reg/shim:experiment" {
			t.Errorf("image = %q, want reg/shim:experiment", got)
		}
	})
}

func TestShimHasNoCPULimit(t *testing.T) {
	t.Parallel()

	// A CPU limit would let the kubelet throttle the process sitting in the
	// request path of every inference call, and the time lost to that
	// throttling is indistinguishable, in the resulting histogram, from the
	// engine being slow. A canary gate would then roll back a release because
	// its sidecar was squeezed.
	shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{}))

	if shim.Resources == nil {
		t.Fatal("the shim has no resources at all")
	}
	if _, ok := (*shim.Resources.Limits)[corev1.ResourceCPU]; ok {
		t.Error("the shim has a CPU limit; throttling the measurement path corrupts the measurement")
	}
	if _, ok := (*shim.Resources.Limits)[corev1.ResourceMemory]; !ok {
		t.Error("the shim has no memory limit; an unbounded sidecar can OOM-kill the engine beside it")
	}
	if _, ok := (*shim.Resources.Requests)[corev1.ResourceCPU]; !ok {
		t.Error("the shim has no CPU request; it needs a scheduling floor to stay responsive under load")
	}
}

func TestShimResourcesAreTakenWholeFromTheSpec(t *testing.T) {
	t.Parallel()

	// Taken whole rather than merged field by field: a partial merge would let
	// someone who set only a memory limit silently inherit a CPU limit they
	// never asked for.
	md := cuNewMD()
	md.Spec.Serving.Shim.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}

	shim := shimFrom(t, shimPod(t, md, renderOptions{}))
	if shim.Resources.Requests != nil && len(*shim.Resources.Requests) != 0 {
		t.Errorf("requests = %v, want none: the user's block replaces the default entirely",
			*shim.Resources.Requests)
	}
	got := (*shim.Resources.Limits)[corev1.ResourceMemory]
	if got.String() != "256Mi" {
		t.Errorf("memory limit = %s, want 256Mi", got.String())
	}
}

func TestShimDisabledLeavesTheEngineServingDirectly(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	md.Spec.Serving.Shim.Enabled = ptr.To(false)

	children := cuBuild(t, md)

	pod := children.Deployment.Spec.Template.Spec
	if shimFrom(t, pod) != nil {
		t.Fatal("the shim was injected although it is disabled")
	}

	// With no shim, "http" resolves to nothing, so the Service must target the
	// engine's own port name instead. Getting this wrong produces a Service
	// with no endpoints and a deployment that looks healthy but answers nothing.
	tp := children.Service.Spec.Ports[0].TargetPort
	if tp == nil || tp.StrVal != naming.PortNameEngine {
		t.Fatalf("service targetPort = %v, want %q", tp, naming.PortNameEngine)
	}
}

func TestServiceTargetsTheShimWhenEnabled(t *testing.T) {
	t.Parallel()

	children := cuBuild(t, cuNewMD())
	tp := children.Service.Spec.Ports[0].TargetPort
	if tp == nil || tp.StrVal != naming.PortNameHTTP {
		t.Fatalf("service targetPort = %v, want %q", tp, naming.PortNameHTTP)
	}
}

func TestMetricsServiceIsHeadlessAndSeparate(t *testing.T) {
	t.Parallel()

	children := cuBuild(t, cuNewMD())
	svc := children.MetricsService
	if svc == nil {
		t.Fatal("no metrics Service was rendered")
	}

	if got := cuDeref(t, svc.Name, "metrics service name"); got != "demo-metrics" {
		t.Errorf("name = %q, want demo-metrics", got)
	}

	// Headless: a scraper wants one target per pod. A normal ClusterIP would
	// give prometheus-operator a single VIP that load-balances to a different
	// pod on every scrape, producing one series whose value jumps between pods —
	// which for a per-pod gauge like queue depth is meaningless.
	if got := cuDeref(t, svc.Spec.ClusterIP, "clusterIP"); got != corev1.ClusterIPNone {
		t.Errorf("clusterIP = %q, want None", got)
	}

	// Scraped even while loading, so the first minutes of a rollout are not a
	// hole in the data.
	if !cuDeref(t, svc.Spec.PublishNotReadyAddresses, "publishNotReadyAddresses") {
		t.Error("publishNotReadyAddresses is false; the model-load window would produce no samples")
	}

	// The component label is what the ServiceMonitor selects on, and the only
	// thing distinguishing this Service from the serving one.
	if got := svc.Labels[naming.LabelComponent]; got != observability.ComponentMetrics {
		t.Errorf("component label = %q, want %q", got, observability.ComponentMetrics)
	}

	names := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		names = append(names, cuDeref(t, p.Name, "port name"))
	}
	if !slices.Contains(names, naming.PortNameMetrics) || !slices.Contains(names, naming.PortNameEngine) {
		t.Errorf("metrics service ports = %v, want both %q and %q",
			names, naming.PortNameMetrics, naming.PortNameEngine)
	}
}

func TestServingServiceNeverExposesTheEnginePort(t *testing.T) {
	t.Parallel()

	// If the engine's port were on the serving Service, any in-cluster client
	// could address the engine directly and bypass the shim — at which point
	// that traffic contributes to no llmcp_* series, and canary analysis is
	// reasoning about a subset of the load while believing it sees all of it.
	children := cuBuild(t, cuNewMD())
	for _, p := range children.Service.Spec.Ports {
		if p.Name != nil && *p.Name == naming.PortNameEngine {
			t.Fatal("the serving Service exposes the engine port; clients could bypass the metrics shim")
		}
		if p.TargetPort != nil && p.TargetPort.StrVal == naming.PortNameEngine {
			t.Fatal("the serving Service targets the engine directly although the shim is enabled")
		}
	}
}

func TestShimInjectionIsByteStable(t *testing.T) {
	t.Parallel()

	// Server-Side Apply idempotency: identical input must render identical
	// bytes, every time. Go randomises map iteration per range statement, so a
	// single comparison would pass by luck.
	md := cuNewMD()
	md.Spec.Serving.Shim.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}

	first, err := json.Marshal(cuBuild(t, md))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 100 {
		got, err := json.Marshal(cuBuild(t, md))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d differs; SSA would rewrite the Deployment on every reconcile", i)
		}
	}
}

func TestEngineKeepsItsOwnPortName(t *testing.T) {
	t.Parallel()

	// Container port names are unique within a pod, so the engine cannot be
	// called "http" once the shim takes that name. It keeps "engine" in BOTH
	// configurations rather than swapping, so the pod template does not depend
	// on whether a sidecar happens to be present.
	for _, shimOn := range []bool{true, false} {
		md := cuNewMD()
		md.Spec.Serving.Shim.Enabled = ptr.To(shimOn)

		pod := shimPod(t, md, renderOptions{})
		var engineC *corev1ac.ContainerApplyConfiguration
		for i := range pod.Containers {
			if pod.Containers[i].Name != nil && *pod.Containers[i].Name == "engine" {
				engineC = &pod.Containers[i]
			}
		}
		if engineC == nil {
			t.Fatalf("shim=%v: no engine container", shimOn)
		}
		if got := cuDeref(t, engineC.Ports[0].Name, "engine port name"); got != naming.PortNameEngine {
			t.Errorf("shim=%v: engine port name = %q, want %q", shimOn, got, naming.PortNameEngine)
		}
	}
}

func TestShimSecurityContextMatchesTheEngine(t *testing.T) {
	t.Parallel()

	// The shim terminates untrusted client connections, so there is no reason
	// for it to be less constrained than the process behind it.
	shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{}))
	sc := shim.SecurityContext
	if sc == nil {
		t.Fatal("the shim has no security context")
	}
	if !cuDeref(t, sc.ReadOnlyRootFilesystem, "readOnlyRootFilesystem") {
		t.Error("the shim's root filesystem is writable")
	}
	if cuDeref(t, sc.AllowPrivilegeEscalation, "allowPrivilegeEscalation") {
		t.Error("the shim allows privilege escalation")
	}
	if !cuDeref(t, sc.RunAsNonRoot, "runAsNonRoot") {
		t.Error("the shim may run as root")
	}
	if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
		t.Error("the shim does not drop all capabilities")
	}
}

func TestShimLogLevelDefaultsToInfo(t *testing.T) {
	t.Parallel()

	shim := shimFrom(t, shimPod(t, cuNewMD(), renderOptions{}))
	found := false
	for _, a := range shim.Args {
		if strings.HasPrefix(a, "--log-level=") {
			found = true
			if a != "--log-level=info" {
				t.Errorf("log level arg = %q, want --log-level=info", a)
			}
		}
	}
	if !found {
		t.Error("no --log-level argument was rendered")
	}
}
