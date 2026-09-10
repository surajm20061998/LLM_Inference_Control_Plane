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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// These are upgrade tests, not encoder tests. Each fixture is written through
// the original unversioned Recorder path and rendered as a real Deployment in
// envtest, then reconciled by the current controller. That exercises the same
// API defaulting, status persistence and Server-Side Apply path as an operator
// upgrade in a cluster.
var _ = Describe("Legacy revision migration", func() {
	It("adopts a semantically equivalent steady workload without starting a rollout", func() {
		ns := mdtNewNamespace()
		name := "legacy-steady"
		key := types.NamespacedName{Namespace: ns, Name: name}
		md := mdtCreate(helpers.NewModelDeployment(name, ns, helpers.WithReplicas(3)))

		legacy := migrationRecordLegacy(md, md.Spec)
		migrationCreateDeployment(md, md.Spec, legacy, inferencev1alpha1.VariantPrimary, 3)
		migrationSetStatus(md, inferencev1alpha1.ModelDeploymentStatus{
			Phase:              inferencev1alpha1.PhaseAvailable,
			StableRevision:     legacy,
			LastGoodRevision:   legacy,
			ObservedGeneration: md.Generation,
		})

		// A Service-only edit is deliberately not a workload revision in v2.
		md = mdtGet(key)
		md.Spec.Serving.Port = 9090
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		migrationExpectTarget(md, legacy, inferencev1alpha1.VariantPrimary, workloadLegacyTelemetry)

		mdtReconcile(mdtNewReconciler(), key)

		got := mdtGet(key)
		Expect(got.Status.StableRevision).To(Equal(legacy))
		Expect(got.Status.LastGoodRevision).To(Equal(legacy))
		Expect(mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.PrimaryDeployment(name),
		}).Spec.Template.Labels[naming.LabelRevision]).To(Equal(legacy))
		Expect(mdtRevisions(ns, name)).To(HaveLen(1),
			"a schema-only v2 identity must not be recorded as a release")
	})

	It("resumes an active legacy rung without resetting evidence", func() {
		ns := mdtNewNamespace()
		name := "legacy-active"
		key := types.NamespacedName{Namespace: ns, Name: name}
		md := mdtCreate(helpers.NewModelDeployment(name, ns,
			helpers.WithReplicas(5), helpers.WithCanary(), helpers.WithContextSize(8192)))

		candidateSpec := *md.Spec.DeepCopy()
		stableSpec := *md.Spec.DeepCopy()
		*stableSpec.Engine.ContextSize = 4096
		stable := migrationRecordLegacy(md, stableSpec)
		candidate := migrationRecordLegacy(md, candidateSpec)
		migrationCreateDeployment(md, stableSpec, stable, inferencev1alpha1.VariantPrimary, 3)
		canary := migrationCreateDeployment(md, candidateSpec, candidate, inferencev1alpha1.VariantCanary, 2)
		Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient,
			types.NamespacedName{Namespace: ns, Name: canary.Name}, 2)).To(Succeed())
		canary = mdtGetDeployment(types.NamespacedName{Namespace: ns, Name: canary.Name})

		now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
		started := metav1.NewTime(now.Add(-10 * time.Minute))
		available := metav1.NewTime(now.Add(-5 * time.Minute))
		lastAnalysis := metav1.NewTime(now)
		migrationSetStatus(md, inferencev1alpha1.ModelDeploymentStatus{
			Phase:            inferencev1alpha1.PhaseCanarying,
			StableRevision:   stable,
			LastGoodRevision: stable,
			Canary: &inferencev1alpha1.CanaryStatus{
				Revision:                candidate,
				StableRevision:          stable,
				Step:                    1,
				FailedChecks:            2,
				ConsecutiveErrors:       1,
				ConsecutiveInconclusive: 1,
				MetricScopeReady:        true,
				StartTime:               &started,
				AvailableSince:          &available,
				LastAnalysisTime:        &lastAnalysis,
				ReadinessTarget: inferencev1alpha1.CanaryReadinessTarget{
					Step: 1, Weight: 50, TotalReplicas: 5, CanaryReplicas: 2,
					Generation: canary.Generation, DeploymentUID: string(canary.UID),
				},
			},
		})
		migrationExpectTarget(md, candidate, inferencev1alpha1.VariantCanary, workloadLegacyTelemetry)

		r := mdtNewReconciler()
		r.Clock = testingclock.NewFakePassiveClock(now)
		before := mdtGet(key)
		beforePrimary := mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.PrimaryDeployment(name),
		})
		beforeCanary := mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.CanaryDeployment(name),
		})
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		got := mdtGet(key)
		Expect(got.Status.Phase).To(Equal(inferencev1alpha1.PhaseCanarying))
		Expect(got.Status.Canary.Revision).To(Equal(candidate))
		Expect(got.Status.Canary.Step).To(Equal(int32(1)))
		Expect(got.Status.Canary.FailedChecks).To(Equal(int32(2)))
		Expect(got.Status.Canary.ConsecutiveErrors).To(Equal(int32(1)))
		Expect(got.Status.Canary.ConsecutiveInconclusive).To(Equal(int32(1)))
		Expect(got.Status.Canary.StartTime).NotTo(BeNil())
		Expect(got.Status.Canary.StartTime.Time.Equal(started.Time)).To(BeTrue())
		Expect(got.Status.Canary.AvailableSince).NotTo(BeNil())
		Expect(got.Status.Canary.AvailableSince.Time.Equal(available.Time)).To(BeTrue())
		Expect(got.Status.Canary.LastAnalysisTime).NotTo(BeNil())
		Expect(got.Status.Canary.LastAnalysisTime.Time.Equal(lastAnalysis.Time)).To(BeTrue())
		Expect(got.Status.Canary.ReadinessTarget).To(Equal(before.Status.Canary.ReadinessTarget))
		Expect(got.Status.Canary.MetricScopeReady).To(BeFalse())
		Expect(got.Status.Canary.Message).To(ContainSubstring(naming.AnnoAbort + "=true"))
		condition := mdtExpectCondition(got,
			inferencev1alpha1.ConditionMetricScopeReady, metav1.ConditionFalse)
		Expect(condition.Reason).To(Equal(inferencev1alpha1.ReasonMetricsScopePending))
		Expect(condition.Message).To(ContainSubstring("pre-migration telemetry"))
		Expect(got.Status.StableRevision).To(Equal(before.Status.StableRevision))
		Expect(got.Status.LastGoodRevision).To(Equal(before.Status.LastGoodRevision))
		Expect(mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.PrimaryDeployment(name),
		}).ResourceVersion).To(Equal(beforePrimary.ResourceVersion))
		Expect(mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.CanaryDeployment(name),
		}).ResourceVersion).To(Equal(beforeCanary.ResourceVersion))
		Expect(mdtRevisions(ns, name)).To(HaveLen(2),
			"resuming a legacy rung must not create a v2 candidate")

		By("allowing an explicit abort without ever creating the v2 schema candidate")
		got.Annotations = map[string]string{naming.AnnoAbort: annotationEnabled}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		mdtReconcile(r, key)
		Expect(mdtGet(key).Status.Phase).To(Equal(inferencev1alpha1.PhaseRollingBack))
		Expect(mdtRevisions(ns, name)).To(HaveLen(2))
		removed := &appsv1.Deployment{}
		err = k8sClient.Get(ctx, types.NamespacedName{
			Namespace: ns, Name: naming.CanaryDeployment(name),
		}, removed)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("keeps a rejected legacy target sticky after the candidate was removed", func() {
		ns := mdtNewNamespace()
		name := "legacy-failed"
		key := types.NamespacedName{Namespace: ns, Name: name}
		md := mdtCreate(helpers.NewModelDeployment(name, ns,
			helpers.WithReplicas(5), helpers.WithCanary(), helpers.WithContextSize(8192)))

		candidateSpec := *md.Spec.DeepCopy()
		stableSpec := *md.Spec.DeepCopy()
		*stableSpec.Engine.ContextSize = 4096
		stable := migrationRecordLegacy(md, stableSpec)
		failed := migrationRecordLegacy(md, candidateSpec)
		migrationCreateDeployment(md, stableSpec, stable, inferencev1alpha1.VariantPrimary, 5)
		migrationSetStatus(md, inferencev1alpha1.ModelDeploymentStatus{
			Phase:            inferencev1alpha1.PhaseAvailable,
			StableRevision:   stable,
			LastGoodRevision: stable,
			Canary: &inferencev1alpha1.CanaryStatus{
				FailedRevision: failed,
				FailedChecks:   2,
			},
		})

		mdtReconcile(mdtNewReconciler(), key)

		got := mdtGet(key)
		Expect(got.Status.Canary).NotTo(BeNil())
		Expect(got.Status.Canary.FailedRevision).To(Equal(failed))
		Expect(mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.PrimaryDeployment(name),
		}).Spec.Template.Labels[naming.LabelRevision]).To(Equal(stable))
		Expect(mdtRevisions(ns, name)).To(HaveLen(2),
			"a rejected target must not return under a new schema hash")
	})

	It("uses a v2 candidate for a genuine workload change", func() {
		ns := mdtNewNamespace()
		name := "legacy-changed"
		key := types.NamespacedName{Namespace: ns, Name: name}
		md := mdtCreate(helpers.NewModelDeployment(name, ns, helpers.WithReplicas(3)))

		legacy := migrationRecordLegacy(md, md.Spec)
		migrationCreateDeployment(md, md.Spec, legacy, inferencev1alpha1.VariantPrimary, 3)
		migrationSetStatus(md, inferencev1alpha1.ModelDeploymentStatus{
			Phase:            inferencev1alpha1.PhaseAvailable,
			StableRevision:   legacy,
			LastGoodRevision: legacy,
		})

		md = mdtGet(key)
		md.Spec.Engine.Image = "engine:genuine-change"
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		profile, err := engine.Get(md.Spec.Engine.Type)
		Expect(err).NotTo(HaveOccurred())
		resolved := resolvedRevisionSpec(md, profile, renderOptions{})
		v2, err := revision.NewSnapshot(&resolved)
		Expect(err).NotTo(HaveOccurred())

		mdtReconcile(mdtNewReconciler(), key)

		Expect(mdtGetDeployment(types.NamespacedName{
			Namespace: ns, Name: naming.PrimaryDeployment(name),
		}).Spec.Template.Labels[naming.LabelRevision]).To(Equal(v2.Revision))
		Expect(v2.Revision).NotTo(Equal(legacy))
		Expect(mdtRevisions(ns, name)).To(HaveLen(2))
	})
})

func migrationRecordLegacy(
	md *inferencev1alpha1.ModelDeployment,
	spec inferencev1alpha1.ModelDeploymentSpec,
) string {
	GinkgoHelper()
	fixture := md.DeepCopy()
	fixture.Spec = *spec.DeepCopy()
	rev := revision.Hash(&fixture.Spec)
	recorder := revision.Recorder{Client: k8sClient, Scheme: scheme}
	_, created, err := recorder.Record(ctx, fixture, rev)
	Expect(err).NotTo(HaveOccurred())
	Expect(created).To(BeTrue())
	return rev
}

func migrationCreateDeployment(
	md *inferencev1alpha1.ModelDeployment,
	spec inferencev1alpha1.ModelDeploymentSpec,
	rev string,
	variant inferencev1alpha1.Variant,
	replicas int32,
) *appsv1.Deployment {
	GinkgoHelper()
	profile, err := engine.Get(spec.Engine.Type)
	Expect(err).NotTo(HaveOccurred())
	desired, err := buildDeployment(withSpec(md, spec), profile, rev, variant, replicas, renderOptions{})
	Expect(err).NotTo(HaveOccurred())
	dep, err := applyConfigFrom[appsv1.Deployment](desired)
	Expect(err).NotTo(HaveOccurred())
	for i := range dep.Spec.Template.Spec.InitContainers {
		if dep.Spec.Template.Spec.InitContainers[i].Name == naming.ShimContainerName {
			dep.Spec.Template.Spec.InitContainers[i].Args = withoutMetricScopeArgs(
				dep.Spec.Template.Spec.InitContainers[i].Args)
		}
	}
	Expect(k8sClient.Create(ctx, dep)).To(Succeed())
	return dep
}

func migrationSetStatus(
	md *inferencev1alpha1.ModelDeployment,
	status inferencev1alpha1.ModelDeploymentStatus,
) {
	GinkgoHelper()
	latest := mdtGet(types.NamespacedName{Namespace: md.Namespace, Name: md.Name})
	latest.Status = status
	Expect(k8sClient.Status().Update(ctx, latest)).To(Succeed())
}

func migrationExpectTarget(
	md *inferencev1alpha1.ModelDeployment,
	rev string,
	variant inferencev1alpha1.Variant,
	want workloadMigrationMatch,
) {
	GinkgoHelper()
	md = mdtGet(types.NamespacedName{Namespace: md.Namespace, Name: md.Name})
	profile, err := engine.Get(md.Spec.Engine.Type)
	Expect(err).NotTo(HaveOccurred())
	resolved := resolvedRevisionSpec(md, profile, renderOptions{})
	dep := mdtGetDeployment(types.NamespacedName{
		Namespace: md.Namespace, Name: naming.DeploymentFor(md.Name, variant),
	})
	match, err := currentWorkloadMatchesDeployment(md, profile, resolved, legacyMigrationReference{
		revision: rev, deployment: dep, variant: variant,
	}, renderOptions{})
	Expect(err).NotTo(HaveOccurred())
	if match != want {
		desired, buildErr := buildDeployment(withSpec(md, resolved), profile, rev, variant, replicasFor(md), renderOptions{})
		Expect(buildErr).NotTo(HaveOccurred())
		typed, convertErr := applyConfigFrom[appsv1.Deployment](desired)
		Expect(convertErr).NotTo(HaveOccurred())
		for i := range typed.Spec.Template.Spec.InitContainers {
			if typed.Spec.Template.Spec.InitContainers[i].Name == naming.ShimContainerName {
				typed.Spec.Template.Spec.InitContainers[i].Args = withoutMetricScopeArgs(
					typed.Spec.Template.Spec.InitContainers[i].Args)
			}
		}
		wantJSON, _ := json.MarshalIndent(typed.Spec.Template, "", "  ")
		gotJSON, _ := json.MarshalIndent(dep.Spec.Template, "", "  ")
		GinkgoWriter.Printf("desired legacy template:\n%s\nlive legacy template:\n%s\n", wantJSON, gotJSON)
	}
	Expect(match).To(Equal(want))
}
