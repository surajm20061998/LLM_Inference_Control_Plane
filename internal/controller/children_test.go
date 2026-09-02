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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/engine"
	"github.com/surajmishra/llmcp/internal/naming"
)

// cuRevision is the revision hash used throughout these tests. It is a fixed
// literal so that assertions about which labels carry it are unambiguous.
const cuRevision = "abc123xy"

// The fixture ModelDeployment's identity and its model file, shared with the
// status tests in this package so both describe the same object.
const (
	cuMDName    = "demo"
	cuNamespace = "inference"
	cuModelPath = "/weights/qwen3.gguf"
)

// cuNewMD returns a minimally valid ModelDeployment: an image model source (the
// only implemented one) and the llama.cpp engine.
func cuNewMD() *v1alpha1.ModelDeployment {
	return &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cuMDName,
			Namespace: cuNamespace,
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model: v1alpha1.ModelSpec{
				Name: "qwen3",
				Source: v1alpha1.ModelSourceSpec{
					Image: &v1alpha1.ImageModelSource{
						Image:      "ghcr.io/example/qwen3:v1",
						Path:       cuModelPath,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
			Engine: v1alpha1.EngineSpec{Type: v1alpha1.EngineLlamaCPP},
		},
	}
}

// cuProfile returns the llama.cpp profile, failing the test if it is not
// registered (which would mean the engine package's init did not run).
func cuProfile(t *testing.T) engine.Profile {
	t.Helper()
	prof, err := engine.Get(v1alpha1.EngineLlamaCPP)
	if err != nil {
		t.Fatalf("engine.Get(%q): %v", v1alpha1.EngineLlamaCPP, err)
	}
	return prof
}

// cuBuild renders the children for md, failing the test on error.
func cuBuild(t *testing.T, md *v1alpha1.ModelDeployment) desiredChildren {
	t.Helper()
	children, err := buildChildren(md, cuProfile(t), cuRevision)
	if err != nil {
		t.Fatalf("buildChildren: %v", err)
	}
	return children
}

// cuDeref dereferences a pointer field of an apply configuration, failing the
// test when it is nil rather than panicking with an unhelpful stack.
func cuDeref[T any](t *testing.T, p *T, what string) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil", what)
	}
	return *p
}

func cuInt32Ptr(v int32) *int32 { return &v }

func cuDuration(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func cuIntOrStr(v intstr.IntOrString) *intstr.IntOrString { return &v }

func TestBuildDeploymentShape(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	md.Spec.Replicas = cuInt32Ptr(4)
	dep := cuBuild(t, md).Deployment

	if dep == nil {
		t.Fatal("buildChildren returned a nil Deployment")
	}
	if got, want := cuDeref(t, dep.Name, "deployment name"), "demo-primary"; got != want {
		t.Errorf("deployment name = %q, want %q", got, want)
	}
	if got, want := cuDeref(t, dep.Namespace, "deployment namespace"), cuNamespace; got != want {
		t.Errorf("deployment namespace = %q, want %q", got, want)
	}

	spec := dep.Spec
	if spec == nil {
		t.Fatal("deployment spec is nil")
	}
	if got := cuDeref(t, spec.Replicas, "spec.replicas"); got != 4 {
		t.Errorf("spec.replicas = %d, want 4", got)
	}
	if got := cuDeref(t, spec.RevisionHistoryLimit, "spec.revisionHistoryLimit"); got != defaultRevisionHistoryLimit {
		t.Errorf("spec.revisionHistoryLimit = %d, want %d", got, defaultRevisionHistoryLimit)
	}
	if got := cuDeref(t, spec.ProgressDeadlineSeconds, "spec.progressDeadlineSeconds"); got != 600 {
		t.Errorf("spec.progressDeadlineSeconds = %d, want 600 (the unset fallback)", got)
	}

	// spec.selector is IMMUTABLE once the Deployment exists. Every label in it
	// is therefore permanent: adding one later would require deleting and
	// recreating the Deployment in every cluster running this operator. So the
	// selector must contain exactly the two-label set and nothing else.
	if spec.Selector == nil {
		t.Fatal("spec.selector is nil")
	}
	selector := spec.Selector.MatchLabels
	if len(selector) != 2 {
		t.Fatalf("spec.selector.matchLabels has %d entries (%v), want exactly 2 — "+
			"the selector is immutable, so its size is a permanent API commitment", len(selector), selector)
	}
	if got, want := selector[naming.LabelModelDeployment], cuMDName; got != want {
		t.Errorf("selector[%s] = %q, want %q", naming.LabelModelDeployment, got, want)
	}
	if got, want := selector[naming.LabelVariant], string(v1alpha1.VariantPrimary); got != want {
		t.Errorf("selector[%s] = %q, want %q", naming.LabelVariant, got, want)
	}

	// The revision label changes on every spec change. If it were in the
	// immutable selector, every rollout would become a delete-and-recreate of
	// the whole Deployment rather than a rolling update.
	if v, ok := selector[naming.LabelRevision]; ok {
		t.Errorf("selector contains the revision label %s=%q; a per-revision label in an "+
			"immutable selector turns every rollout into a delete-and-recreate", naming.LabelRevision, v)
	}

	// Pods, unlike the selector, DO carry the revision label: that is what lets
	// a query or dashboard attribute a running pod to the revision that
	// produced it. The asymmetry between these two label sets is the point.
	if spec.Template == nil {
		t.Fatal("spec.template is nil")
	}
	podLabels := spec.Template.Labels
	if got, want := podLabels[naming.LabelRevision], cuRevision; got != want {
		t.Errorf("pod template label %s = %q, want %q — pods must be attributable to a revision",
			naming.LabelRevision, got, want)
	}
	for k, want := range selector {
		if got := podLabels[k]; got != want {
			t.Errorf("pod template label %s = %q, want %q — pods must match the selector", k, got, want)
		}
	}

	// Object metadata labels are free to grow (nothing immutable reads them),
	// and they carry the revision too.
	if got, want := dep.Labels[naming.LabelRevision], cuRevision; got != want {
		t.Errorf("deployment label %s = %q, want %q", naming.LabelRevision, got, want)
	}
}

func TestOwnerReferenceIsControllerAndBlocksDeletion(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	children := cuBuild(t, md)

	check := func(t *testing.T, what string, refs []metav1ac.OwnerReferenceApplyConfiguration) {
		t.Helper()
		if len(refs) != 1 {
			t.Fatalf("%s has %d owner references, want exactly 1", what, len(refs))
		}
		ref := refs[0]
		if got, want := cuDeref(t, ref.APIVersion, "ownerRef.apiVersion"), v1alpha1.GroupVersion.String(); got != want {
			t.Errorf("%s ownerRef.apiVersion = %q, want %q", what, got, want)
		}
		if got, want := cuDeref(t, ref.Kind, "ownerRef.kind"), "ModelDeployment"; got != want {
			t.Errorf("%s ownerRef.kind = %q, want %q", what, got, want)
		}
		if got, want := cuDeref(t, ref.Name, "ownerRef.name"), md.Name; got != want {
			t.Errorf("%s ownerRef.name = %q, want %q", what, got, want)
		}
		if got, want := cuDeref(t, ref.UID, "ownerRef.uid"), md.UID; got != want {
			t.Errorf("%s ownerRef.uid = %q, want %q", what, got, want)
		}
		// Controller:true is what makes garbage collection delete the child
		// when the ModelDeployment goes away — the reason this operator needs
		// no finalizer at all.
		if !cuDeref(t, ref.Controller, "ownerRef.controller") {
			t.Errorf("%s ownerRef.controller = false, want true", what)
		}
		// BlockOwnerDeletion keeps the owner around until the children are
		// gone, so a foreground delete cannot leave orphaned serving pods.
		if !cuDeref(t, ref.BlockOwnerDeletion, "ownerRef.blockOwnerDeletion") {
			t.Errorf("%s ownerRef.blockOwnerDeletion = false, want true", what)
		}
	}

	t.Run("deployment", func(t *testing.T) { check(t, "deployment", children.Deployment.OwnerReferences) })
	t.Run("service", func(t *testing.T) { check(t, "service", children.Service.OwnerReferences) })
}

func TestReplicasForTreatsExplicitZeroAsRequested(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		replicas *int32
		want     int32
	}{
		// Unset means "the user has no opinion", and one replica is the
		// sensible default.
		{name: "unset defaults to one", replicas: nil, want: 1},
		// Zero is a value the user (or an autoscaler) explicitly asked for.
		// Treating it as "unset" and silently scaling back up would override a
		// deliberate scale-to-zero, which is exactly why the field is a pointer.
		{name: "explicit zero is honoured", replicas: cuInt32Ptr(0), want: 0},
		{name: "explicit value passes through", replicas: cuInt32Ptr(5), want: 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := cuNewMD()
			md.Spec.Replicas = tc.replicas
			if got := replicasFor(md); got != tc.want {
				t.Errorf("replicasFor() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildStrategyMapsRolloutSpec(t *testing.T) {
	t.Parallel()

	t.Run("recreate has no rollingUpdate block", func(t *testing.T) {
		t.Parallel()
		md := cuNewMD()
		md.Spec.Rollout.Type = v1alpha1.RolloutRecreate
		// Surge and unavailability are meaningless for Recreate; carrying them
		// into the strategy would be rejected by the API server.
		md.Spec.Rollout.MaxSurge = cuIntOrStr(intstr.FromInt32(2))
		md.Spec.Rollout.MaxUnavailable = cuIntOrStr(intstr.FromString("25%"))

		strategy := buildStrategy(md)
		if got := cuDeref(t, strategy.Type, "strategy.type"); got != appsv1.RecreateDeploymentStrategyType {
			t.Errorf("strategy.type = %q, want %q", got, appsv1.RecreateDeploymentStrategyType)
		}
		if strategy.RollingUpdate != nil {
			t.Errorf("strategy.rollingUpdate = %+v, want nil for a Recreate strategy", *strategy.RollingUpdate)
		}
	})

	t.Run("unset type defaults to rolling update", func(t *testing.T) {
		t.Parallel()
		md := cuNewMD() // Rollout.Type left empty, as a spec that skipped defaulting would be.
		strategy := buildStrategy(md)
		if got := cuDeref(t, strategy.Type, "strategy.type"); got != appsv1.RollingUpdateDeploymentStrategyType {
			t.Errorf("strategy.type = %q, want %q", got, appsv1.RollingUpdateDeploymentStrategyType)
		}
		if strategy.RollingUpdate == nil {
			t.Fatal("strategy.rollingUpdate is nil, want a (possibly empty) rolling update block")
		}
		// Unset knobs are left absent rather than defaulted here, so the
		// Deployment's own defaults apply and Server-Side Apply does not claim
		// ownership of fields the user never set.
		if strategy.RollingUpdate.MaxSurge != nil {
			t.Errorf("rollingUpdate.maxSurge = %v, want nil when unset", *strategy.RollingUpdate.MaxSurge)
		}
		if strategy.RollingUpdate.MaxUnavailable != nil {
			t.Errorf("rollingUpdate.maxUnavailable = %v, want nil when unset", *strategy.RollingUpdate.MaxUnavailable)
		}
	})

	t.Run("rolling update passes surge and unavailable through", func(t *testing.T) {
		t.Parallel()
		md := cuNewMD()
		md.Spec.Rollout.Type = v1alpha1.RolloutRollingUpdate
		md.Spec.Rollout.MaxSurge = cuIntOrStr(intstr.FromInt32(2))
		md.Spec.Rollout.MaxUnavailable = cuIntOrStr(intstr.FromString("25%"))

		strategy := buildStrategy(md)
		if got := cuDeref(t, strategy.Type, "strategy.type"); got != appsv1.RollingUpdateDeploymentStrategyType {
			t.Errorf("strategy.type = %q, want %q", got, appsv1.RollingUpdateDeploymentStrategyType)
		}
		if strategy.RollingUpdate == nil {
			t.Fatal("strategy.rollingUpdate is nil")
		}
		if got, want := cuDeref(t, strategy.RollingUpdate.MaxSurge, "maxSurge"), intstr.FromInt32(2); got != want {
			t.Errorf("rollingUpdate.maxSurge = %v, want %v", got, want)
		}
		if got, want := cuDeref(t, strategy.RollingUpdate.MaxUnavailable, "maxUnavailable"), intstr.FromString("25%"); got != want {
			t.Errorf("rollingUpdate.maxUnavailable = %v, want %v", got, want)
		}
	})
}

func TestProgressDeadlineSecondsNeverReturnsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deadline *metav1.Duration
		want     int32
	}{
		{name: "unset falls back to the API default", deadline: nil, want: 600},
		{name: "whole seconds pass through", deadline: cuDuration(300 * time.Second), want: 300},
		// A sub-second duration truncates to zero seconds, and a Deployment
		// with progressDeadlineSeconds=0 reports ProgressDeadlineExceeded
		// immediately — every rollout would be born failed. Falling back is the
		// only safe answer.
		{name: "sub-second falls back rather than yielding zero", deadline: cuDuration(500 * time.Millisecond), want: 600},
		{name: "zero duration falls back", deadline: cuDuration(0), want: 600},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := cuNewMD()
			md.Spec.Rollout.ProgressDeadline = tc.deadline
			if got := progressDeadlineSeconds(md); got != tc.want {
				t.Errorf("progressDeadlineSeconds() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestStartupFailureThresholdRoundsUp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout *metav1.Duration
		want    int32
	}{
		{name: "unset uses the 300s default budget", timeout: nil, want: 300 / probePeriodSeconds},
		{name: "exact multiple of the probe period", timeout: cuDuration(120 * time.Second), want: 24},
		// Rounding DOWN here would silently shorten the user's configured
		// startup budget: 7s would become one 5s attempt and the pod would be
		// killed two seconds early. Always round up.
		{name: "non-multiple rounds up, never down", timeout: cuDuration(7 * time.Second), want: 2},
		// A probe with failureThreshold 0 is invalid; the floor keeps even an
		// absurdly small budget expressible.
		{name: "tiny budget still yields at least one attempt", timeout: cuDuration(1 * time.Second), want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := cuNewMD()
			md.Spec.Serving.StartupTimeout = tc.timeout
			got := startupFailureThreshold(md)
			if got != tc.want {
				t.Errorf("startupFailureThreshold() = %d, want %d", got, tc.want)
			}
			if got < 1 {
				t.Errorf("startupFailureThreshold() = %d; a threshold below 1 is rejected by the API server", got)
			}
		})
	}
}

func TestRuntimeModelPathPreservesBasename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string
	}{
		// The basename survives the move because engines sniff file extensions
		// (.gguf) and because the path in a log line should still be
		// recognisable as the file the user pointed at.
		{name: "nested source keeps its file name", src: cuModelPath, want: "/models/qwen3.gguf"},
		{name: "bare root-level file", src: "/model.bin", want: "/models/model.bin"},
		{name: "deeply nested source", src: "/a/b/c/llama-7b.Q4_K_M.gguf", want: "/models/llama-7b.Q4_K_M.gguf"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runtimeModelPath(&v1alpha1.ImageModelSource{Path: tc.src})
			if got != tc.want {
				t.Errorf("runtimeModelPath(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

func TestPodSpecDeliversModelThenRunsEngine(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	podSpec, err := buildPodSpec(md, cuProfile(t), v1alpha1.VariantPrimary)
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}

	t.Run("single init container copies the weights", func(t *testing.T) {
		if len(podSpec.InitContainers) != 1 {
			t.Fatalf("got %d init containers, want exactly 1", len(podSpec.InitContainers))
		}
		init := podSpec.InitContainers[0]
		if got := cuDeref(t, init.Name, "initContainer.name"); got != modelInitContainerName {
			t.Errorf("init container name = %q, want %q", got, modelInitContainerName)
		}
		// The weights ship as their own OCI image so that a model change and an
		// engine change stay independently deployable.
		if got, want := cuDeref(t, init.Image, "initContainer.image"), md.Spec.Model.Source.Image.Image; got != want {
			t.Errorf("init container image = %q, want the MODEL image %q", got, want)
		}
		// The destination is the STAGING path, not the engine's mount path.
		// A volume mount shadows whatever the image already has at that path,
		// so mounting the (empty) model volume over the image's own /models
		// would hide the very file being copied. Staging elsewhere keeps the
		// source visible to `cp`.
		wantCmd := []string{"cp", cuModelPath, "/mnt/model/qwen3.gguf"}
		if len(init.Command) != len(wantCmd) {
			t.Fatalf("init container command = %v, want %v", init.Command, wantCmd)
		}
		for i := range wantCmd {
			if init.Command[i] != wantCmd[i] {
				t.Errorf("init container command[%d] = %q, want %q", i, init.Command[i], wantCmd[i])
			}
		}
		// The init container mounts the shared volume somewhere OTHER than the
		// engine's mount path, for the shadowing reason above.
		if len(init.VolumeMounts) != 1 {
			t.Fatalf("got %d init volume mounts, want exactly 1", len(init.VolumeMounts))
		}
		if got := cuDeref(t, init.VolumeMounts[0].MountPath, "initContainer.volumeMount.mountPath"); got != modelStagingPath {
			t.Errorf("init container mount path = %q, want the staging path %q", got, modelStagingPath)
		}
		if modelStagingPath == modelMountPath {
			t.Fatal("staging and engine mount paths must differ, or the copy source is shadowed")
		}
	})

	t.Run("single engine container", func(t *testing.T) {
		if len(podSpec.Containers) != 1 {
			t.Fatalf("got %d containers, want exactly 1", len(podSpec.Containers))
		}
		// `kubectl logs -c engine` is in every runbook, and Server-Side Apply
		// matches containers BY NAME — a rename would orphan the old container
		// rather than update it.
		if got := cuDeref(t, podSpec.Containers[0].Name, "container.name"); got != engine.ContainerName {
			t.Errorf("container name = %q, want %q", got, engine.ContainerName)
		}
	})

	t.Run("one emptyDir model volume", func(t *testing.T) {
		if len(podSpec.Volumes) != 1 {
			t.Fatalf("got %d volumes, want exactly 1", len(podSpec.Volumes))
		}
		vol := podSpec.Volumes[0]
		if got := cuDeref(t, vol.Name, "volume.name"); got != engine.ModelVolumeName {
			t.Errorf("volume name = %q, want %q", got, engine.ModelVolumeName)
		}
		// An emptyDir, not a PVC: the weights are re-copied from the image on
		// every pod start, so a node failure never leaves stale weights behind.
		if vol.EmptyDir == nil {
			t.Errorf("volume %q is not an emptyDir", engine.ModelVolumeName)
		}
	})

	t.Run("pod security context satisfies the restricted profile", func(t *testing.T) {
		sc := podSpec.SecurityContext
		if sc == nil {
			t.Fatal("pod securityContext is nil")
		}
		// runAsNonRoot plus a RuntimeDefault seccomp profile is what makes this
		// pod admissible under the restricted Pod Security Standard, which many
		// clusters enforce at the namespace level.
		if !cuDeref(t, sc.RunAsNonRoot, "securityContext.runAsNonRoot") {
			t.Error("pod securityContext.runAsNonRoot = false, want true")
		}
		if sc.SeccompProfile == nil {
			t.Fatal("pod securityContext.seccompProfile is nil")
		}
		if got := cuDeref(t, sc.SeccompProfile.Type, "seccompProfile.type"); got != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("seccompProfile.type = %q, want %q", got, corev1.SeccompProfileTypeRuntimeDefault)
		}
	})
}

func TestProbesBudgetForSlowModelLoads(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	md.Spec.Serving.StartupTimeout = cuDuration(120 * time.Second)

	podSpec, err := buildPodSpec(md, cuProfile(t), v1alpha1.VariantPrimary)
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	if len(podSpec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(podSpec.Containers))
	}
	c := podSpec.Containers[0]

	probes := map[string]*corev1ac.ProbeApplyConfiguration{
		"startup":   c.StartupProbe,
		"readiness": c.ReadinessProbe,
		"liveness":  c.LivenessProbe,
	}
	for name, p := range probes {
		if p == nil {
			t.Fatalf("%s probe is missing", name)
		}
		if p.HTTPGet == nil {
			t.Fatalf("%s probe has no httpGet handler", name)
		}
		if got, want := cuDeref(t, p.HTTPGet.Path, "httpGet.path"), engine.LlamaCPPHealthPath; got != want {
			t.Errorf("%s probe httpGet.path = %q, want %q", name, got, want)
		}
		// Probes must hit the ENGINE port, not the Service port: kubelet talks
		// to the container directly and never goes through the Service.
		if got, want := cuDeref(t, p.HTTPGet.Port, "httpGet.port"), intstr.FromInt32(naming.EnginePort); got != want {
			t.Errorf("%s probe httpGet.port = %v, want %v", name, got, want)
		}
	}

	startup := probes["startup"]
	if got, want := cuDeref(t, startup.FailureThreshold, "startup.failureThreshold"), startupFailureThreshold(md); got != want {
		t.Errorf("startup probe failureThreshold = %d, want %d (spec.serving.startupTimeout / probe period)", got, want)
	}

	// Liveness must be strictly slacker than readiness. Readiness merely pulls
	// a pod out of the endpoint list, which is cheap and reversible; liveness
	// KILLS the container. A liveness probe as tight as readiness would restart
	// pods that are only slow under load, converting a latency blip into a
	// rolling outage.
	readiness, liveness := probes["readiness"], probes["liveness"]
	rFail := cuDeref(t, readiness.FailureThreshold, "readiness.failureThreshold")
	lFail := cuDeref(t, liveness.FailureThreshold, "liveness.failureThreshold")
	if lFail <= rFail {
		t.Errorf("liveness failureThreshold (%d) must exceed readiness (%d): a tight liveness probe "+
			"restarts merely-slow pods", lFail, rFail)
	}
	rPeriod := cuDeref(t, readiness.PeriodSeconds, "readiness.periodSeconds")
	lPeriod := cuDeref(t, liveness.PeriodSeconds, "liveness.periodSeconds")
	if lPeriod <= rPeriod {
		t.Errorf("liveness periodSeconds (%d) must exceed readiness (%d): liveness should sample less "+
			"often than readiness", lPeriod, rPeriod)
	}
}

func TestServiceFrontsEveryVariant(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	svc := buildService(md)

	if got, want := cuDeref(t, svc.Name, "service name"), md.Name; got != want {
		t.Errorf("service name = %q, want %q — the endpoint URL is derived from it", got, want)
	}
	if got, want := cuDeref(t, svc.Namespace, "service namespace"), md.Namespace; got != want {
		t.Errorf("service namespace = %q, want %q", got, want)
	}

	spec := svc.Spec
	if spec == nil {
		t.Fatal("service spec is nil")
	}
	if got := cuDeref(t, spec.Type, "service type"); got != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %q, want %q", got, corev1.ServiceTypeClusterIP)
	}

	// The Service selector deliberately OMITS the variant label so that one
	// endpoint spans primary and canary pods alike. Adding the variant here
	// would silently exclude a future canary's pods from the endpoint, and the
	// canary would receive no traffic at all — which reads as a healthy canary.
	if len(spec.Selector) != 1 {
		t.Fatalf("service selector has %d entries (%v), want exactly 1 — it must span every variant",
			len(spec.Selector), spec.Selector)
	}
	if got, want := spec.Selector[naming.LabelModelDeployment], md.Name; got != want {
		t.Errorf("service selector[%s] = %q, want %q", naming.LabelModelDeployment, got, want)
	}

	if len(spec.Ports) != 1 {
		t.Fatalf("service has %d ports, want exactly 1", len(spec.Ports))
	}
	port := spec.Ports[0]
	// ServiceMonitor selects endpoints by port NAME, so this string is as
	// stable as a field name.
	if got := cuDeref(t, port.Name, "port.name"); got != naming.PortNameHTTP {
		t.Errorf("service port name = %q, want %q", got, naming.PortNameHTTP)
	}
	if got := cuDeref(t, port.Protocol, "port.protocol"); got != corev1.ProtocolTCP {
		t.Errorf("service port protocol = %q, want TCP", got)
	}

	// targetPort must be the STRING "http", not the number 8000. Naming the
	// target port is what lets the metrics shim later take over the container's
	// "http" port without any change to this Service — a numeric targetPort
	// would have to be edited in lockstep with the pod spec.
	target := cuDeref(t, port.TargetPort, "port.targetPort")
	if target.Type != intstr.String {
		t.Errorf("service targetPort type = %v (value %v), want intstr.String — targeting by name is "+
			"what lets a sidecar take over the port later", target.Type, target)
	}
	if target.StrVal != naming.PortNameHTTP {
		t.Errorf("service targetPort = %q, want %q", target.StrVal, naming.PortNameHTTP)
	}

	t.Run("unset serving port falls back to the default", func(t *testing.T) {
		t.Parallel()
		md := cuNewMD()
		md.Spec.Serving.Port = 0
		got := cuDeref(t, buildService(md).Spec.Ports[0].Port, "port.port")
		if got != naming.ServicePort {
			t.Errorf("service port = %d, want %d", got, naming.ServicePort)
		}
	})

	t.Run("explicit serving port is honoured", func(t *testing.T) {
		t.Parallel()
		md := cuNewMD()
		md.Spec.Serving.Port = 9999
		got := cuDeref(t, buildService(md).Spec.Ports[0].Port, "port.port")
		if got != 9999 {
			t.Errorf("service port = %d, want 9999", got)
		}
	})
}

func TestBuildChildrenIsDeterministic(t *testing.T) {
	t.Parallel()

	// A ModelDeployment with the two shapes most likely to leak map iteration
	// order into the output: an env var list and a multi-entry resource map.
	md := cuNewMD()
	md.Spec.Replicas = cuInt32Ptr(3)
	md.Spec.Engine.ExtraArgs = []string{"--no-warmup", "--flash-attn"}
	md.Spec.Engine.Env = []corev1.EnvVar{
		{Name: "ZULU", Value: "1"},
		{Name: "ALPHA", Value: "2"},
		{Name: "MIKE", Value: "3"},
		{Name: "BRAVO", Value: "4"},
	}
	md.Spec.Engine.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("4"),
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("4Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
	}

	// Why 100 runs and not one: Go randomises map iteration order per range
	// statement, so a map-ordered output is invisible in a single execution and
	// reliably fatal in production. These objects are applied with Server-Side
	// Apply and the result is watched — a byte difference between two renders
	// of the same spec means the controller writes a "change" on every
	// reconcile, the watch fires, and the controller and the API server
	// hot-loop forever without ever converging.
	prof := cuProfile(t)
	first, err := buildChildren(md, prof, cuRevision)
	if err != nil {
		t.Fatalf("buildChildren: %v", err)
	}
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshalling first render: %v", err)
	}

	for i := 1; i < 100; i++ {
		got, err := buildChildren(md, prof, cuRevision)
		if err != nil {
			t.Fatalf("buildChildren (run %d): %v", i, err)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshalling run %d: %v", i, err)
		}
		if string(data) != string(want) {
			t.Fatalf("buildChildren is not byte-stable; run %d differs from run 0.\n run 0: %s\n run %d: %s",
				i, want, i, data)
		}
	}
}

func TestServiceSelectorStringIsSerialized(t *testing.T) {
	t.Parallel()

	md := cuNewMD()
	got, err := serviceSelectorString(md)
	if err != nil {
		t.Fatalf("serviceSelectorString: %v", err)
	}
	// status.selector is part of the /scale contract and MUST be a serialized
	// selector string. A structured selector still validates and `kubectl
	// scale` still works, so only an HPA reveals the mistake — by matching no
	// pods and computing its target from nothing.
	want := naming.LabelModelDeployment + "=" + md.Name
	if got != want {
		t.Errorf("serviceSelectorString() = %q, want %q", got, want)
	}
}

func TestApplyConfigFromRoundTripsAContainer(t *testing.T) {
	t.Parallel()

	in := corev1.Container{
		Name:  "engine",
		Image: "ghcr.io/example/engine:v1",
		// Argument ORDER is load-bearing for llama-server (later flags win), so
		// a conversion that reordered args would change engine behaviour.
		Args: []string{"--host", "0.0.0.0", "--port", "8000", "-m", "/models/m.gguf"},
		Ports: []corev1.ContainerPort{{
			Name:          naming.PortNameHTTP,
			ContainerPort: naming.EnginePort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{Name: "ZULU", Value: "1"},
			{Name: "ALPHA", Value: "2"},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      engine.ModelVolumeName,
			MountPath: modelMountPath,
			ReadOnly:  true,
		}},
	}

	out, err := applyConfigFrom[corev1ac.ContainerApplyConfiguration](in)
	if err != nil {
		t.Fatalf("applyConfigFrom: %v", err)
	}

	if got := cuDeref(t, out.Name, "name"); got != in.Name {
		t.Errorf("name = %q, want %q", got, in.Name)
	}
	if got := cuDeref(t, out.Image, "image"); got != in.Image {
		t.Errorf("image = %q, want %q", got, in.Image)
	}
	if len(out.Args) != len(in.Args) {
		t.Fatalf("args = %v, want %v", out.Args, in.Args)
	}
	for i := range in.Args {
		if out.Args[i] != in.Args[i] {
			t.Errorf("args[%d] = %q, want %q", i, out.Args[i], in.Args[i])
		}
	}
	if len(out.Ports) != 1 {
		t.Fatalf("got %d ports, want 1", len(out.Ports))
	}
	if got := cuDeref(t, out.Ports[0].Name, "port.name"); got != naming.PortNameHTTP {
		t.Errorf("port name = %q, want %q", got, naming.PortNameHTTP)
	}
	if got := cuDeref(t, out.Ports[0].ContainerPort, "port.containerPort"); got != naming.EnginePort {
		t.Errorf("containerPort = %d, want %d", got, naming.EnginePort)
	}
	if len(out.Env) != len(in.Env) {
		t.Fatalf("got %d env vars, want %d", len(out.Env), len(in.Env))
	}
	// Env order is preserved verbatim: later entries can reference earlier ones
	// via $(VAR) expansion, so reordering changes meaning.
	for i := range in.Env {
		if got := cuDeref(t, out.Env[i].Name, "env.name"); got != in.Env[i].Name {
			t.Errorf("env[%d].name = %q, want %q", i, got, in.Env[i].Name)
		}
		if got := cuDeref(t, out.Env[i].Value, "env.value"); got != in.Env[i].Value {
			t.Errorf("env[%d].value = %q, want %q", i, got, in.Env[i].Value)
		}
	}
	if len(out.VolumeMounts) != 1 {
		t.Fatalf("got %d volume mounts, want 1", len(out.VolumeMounts))
	}
	if got := cuDeref(t, out.VolumeMounts[0].Name, "volumeMount.name"); got != engine.ModelVolumeName {
		t.Errorf("volumeMount name = %q, want %q", got, engine.ModelVolumeName)
	}
	if got := cuDeref(t, out.VolumeMounts[0].MountPath, "volumeMount.mountPath"); got != modelMountPath {
		t.Errorf("volumeMount mountPath = %q, want %q", got, modelMountPath)
	}
}
