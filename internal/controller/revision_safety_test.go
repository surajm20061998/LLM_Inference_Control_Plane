package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision"
)

const candidateEngineImage = "engine:candidate"

func revisionSafetyReconciler(t *testing.T) (*ModelDeploymentReconciler, *inferencev1alpha1.ModelDeployment) {
	t.Helper()
	s := runtime.NewScheme()
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	md := &inferencev1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{Name: "history", Namespace: "test", UID: "history-uid"}}
	md.Spec.Engine.Image = "engine:stable"
	r := &ModelDeploymentReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	r.revisions = &revision.Recorder{Client: r.Client, Scheme: s}
	return r, md
}

func seedLegacyPrimary(t *testing.T, r *ModelDeploymentReconciler, md *inferencev1alpha1.ModelDeployment,
	revisionID, engineImage, shimImage string, startup time.Duration,
) {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: naming.PrimaryDeployment(md.Name), Namespace: md.Namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{naming.LabelRevision: revisionID}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: engine.ContainerName, Image: engineImage, ImagePullPolicy: corev1.PullIfNotPresent,
					StartupProbe: &corev1.Probe{
						PeriodSeconds: probePeriodSeconds, FailureThreshold: int32(startup / (time.Duration(probePeriodSeconds) * time.Second)),
					},
				}},
				InitContainers: []corev1.Container{{
					Name: naming.ShimContainerName, Image: shimImage, Args: []string{"--log-level=info"},
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("50m"),
					}},
				}},
			},
		}},
	}
	if err := r.Create(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionSafetyMissingHistoryFailsClosed(t *testing.T) {
	r, md := revisionSafetyReconciler(t)
	md.Spec.Engine.Image = candidateEngineImage
	got, err := r.specForRevision(context.Background(), md, "candidate", "missing-stable")
	if err == nil || !reflect.DeepEqual(got, inferencev1alpha1.ModelDeploymentSpec{}) {
		t.Fatalf("missing history returned a renderable spec: %#v, %v", got, err)
	}
	got, err = r.specForRevision(context.Background(), md, "candidate", "candidate")
	if err != nil || got.Engine.Image != candidateEngineImage {
		t.Fatalf("current target should not need historical lookup: %#v, %v", got, err)
	}
}

func TestRevisionSafetyHistoricalMergePreservesPolicy(t *testing.T) {
	r, md := revisionSafetyReconciler(t)
	stable := revision.Hash(&md.Spec)
	if _, _, err := r.revisions.Record(context.Background(), md, stable); err != nil {
		t.Fatal(err)
	}
	seedLegacyPrimary(t, r, md, stable, "engine:stable", DefaultShimImage, 10*time.Minute)
	md.Spec.Engine.Image = candidateEngineImage
	n := int32(8)
	md.Spec.Replicas = &n
	before := md.DeepCopy()
	got, err := r.specForRevision(context.Background(), md, revision.Hash(&md.Spec), stable)
	if err != nil || got.Engine.Image != "engine:stable" || got.Replicas == nil || *got.Replicas != n {
		t.Fatalf("historical merge did not retain stable image and live replicas: %#v, %v", got, err)
	}
	if got.Serving.Shim.Image != DefaultShimImage {
		t.Fatalf("legacy shim image = %q, want %q", got.Serving.Shim.Image, DefaultShimImage)
	}
	if got.Serving.StartupTimeout == nil || got.Serving.StartupTimeout.Duration != 10*time.Minute {
		t.Fatalf("legacy startup timeout = %v, want exact live probe budget", got.Serving.StartupTimeout)
	}
	got.Engine.Image = "changed"
	*got.Replicas = 1
	if !reflect.DeepEqual(md, before) {
		t.Fatal("historical spec aliases live state")
	}
}

func TestLegacyRevisionWithoutMatchingDeploymentFailsClosed(t *testing.T) {
	r, md := revisionSafetyReconciler(t)
	stable := revision.Hash(&md.Spec)
	if _, _, err := r.revisions.Record(context.Background(), md, stable); err != nil {
		t.Fatal(err)
	}
	md.Spec.Engine.Image = candidateEngineImage
	if _, err := r.specForRevision(context.Background(), md, "candidate", stable); err == nil ||
		!strings.Contains(err.Error(), "no matching primary Deployment") {
		t.Fatalf("legacy migration error = %v", err)
	}
}

func TestVersionedRevisionFreezesDefaultsAndRestoresOnlyWorkload(t *testing.T) {
	r, md := revisionSafetyReconciler(t)
	md.Spec.Engine = inferencev1alpha1.EngineSpec{Type: inferencev1alpha1.EngineLlamaCPP}
	md.Spec.Serving.Port = 8080
	profile, err := engine.Get(md.Spec.Engine.Type)
	if err != nil {
		t.Fatal(err)
	}

	const stableShimImage = "shim:stable"
	stable := resolvedRevisionSpec(md, profile, renderOptions{ShimImage: stableShimImage})
	snapshot, err := revision.NewSnapshot(&stable)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.revisions.Record(context.Background(), md, snapshot.Revision, snapshot.Raw); err != nil {
		t.Fatal(err)
	}

	md.Spec.Engine.Image = candidateEngineImage
	md.Spec.Serving.Shim.Image = "shim:candidate"
	md.Spec.Serving.Port = 9090
	wantReplicas := int32(6)
	md.Spec.Replicas = &wantReplicas
	got, err := r.specForRevision(context.Background(), md, "candidate", snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine.Image != engine.LlamaCPPDefaultImage {
		t.Errorf("engine image = %q, want frozen %q", got.Engine.Image, engine.LlamaCPPDefaultImage)
	}
	if got.Serving.Shim.Image != stableShimImage {
		t.Errorf("shim image = %q, want frozen %q", got.Serving.Shim.Image, stableShimImage)
	}
	if got.Serving.StartupTimeout == nil || got.Serving.StartupTimeout.Duration != defaultStartupTimeout {
		t.Errorf("startup timeout = %v, want %s", got.Serving.StartupTimeout, defaultStartupTimeout)
	}
	if got.Engine.Threads == nil || *got.Engine.Threads != 1 {
		t.Errorf("threads = %v, want resolved 1", got.Engine.Threads)
	}
	if got.Serving.Port != 9090 {
		t.Errorf("Service port = %d, want current 9090", got.Serving.Port)
	}
	if got.Replicas == nil || *got.Replicas != wantReplicas {
		t.Errorf("replicas = %v, want current %d", got.Replicas, wantReplicas)
	}
}

func TestResolvedRevisionSpecDoesNotMutateInformerObject(t *testing.T) {
	_, md := revisionSafetyReconciler(t)
	md.Spec.Engine = inferencev1alpha1.EngineSpec{Type: inferencev1alpha1.EngineLlamaCPP}
	before := md.DeepCopy()
	profile, err := engine.Get(md.Spec.Engine.Type)
	if err != nil {
		t.Fatal(err)
	}

	got := resolvedRevisionSpec(md, profile, renderOptions{})
	if !reflect.DeepEqual(md, before) {
		t.Fatal("resolvedRevisionSpec mutated its informer object")
	}
	if got.Engine.Image == "" || got.Serving.Shim.Image == "" || got.Serving.StartupTimeout == nil {
		t.Fatalf("resolved defaults are incomplete: %#v", got)
	}
	if got.Serving.StartupTimeout.Duration != 300*time.Second {
		t.Fatalf("startup timeout = %s", got.Serving.StartupTimeout.Duration)
	}
}

func TestRevisionSafetyRetainsActiveHistory(t *testing.T) {
	r, md := revisionSafetyReconciler(t)
	hashes := make([]string, 0, 15)
	for i := range 15 {
		md.Spec.Engine.Image = fmt.Sprintf("engine:v%d", i)
		hash := revision.Hash(&md.Spec)
		hashes = append(hashes, hash)
		if _, _, err := r.revisions.Record(context.Background(), md, hash); err != nil {
			t.Fatal(err)
		}
	}
	md.Status.StableRevision = hashes[0]
	md.Status.LastGoodRevision = hashes[1]
	md.Status.Canary = &inferencev1alpha1.CanaryStatus{StableRevision: hashes[2], Revision: hashes[3], FailedRevision: hashes[4]}
	if err := r.recordRevision(context.Background(), md, hashes[14]); err != nil {
		t.Fatal(err)
	}
	for _, hash := range append(hashes[:5:5], hashes[14]) {
		if _, err := r.revisions.Get(context.Background(), md, hash); err != nil {
			t.Errorf("required history %s was pruned: %v", hash, err)
		}
	}
	remaining, err := r.revisions.List(context.Background(), md)
	if err != nil || len(remaining) != int(revisionHistoryLimit) {
		t.Fatalf("history limit not enforced: count=%d err=%v", len(remaining), err)
	}
}
