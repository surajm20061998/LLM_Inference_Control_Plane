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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// canaryHarness drives a canary rollout deterministically.
//
// Every source of nondeterminism is pinned: the clock is fake and stepped
// explicitly, the metric provider is scripted, and the reconciler is invoked
// directly rather than through a manager. A full multi-step rollout therefore
// runs in milliseconds and produces the same result on every machine — which is
// the only way to assert "rollback fired on exactly the second failure" and
// mean it.
type canaryHarness struct {
	r        *ModelDeploymentReconciler
	clock    *testingclock.FakePassiveClock
	provider *analysis.ScriptedProvider

	namespace  string
	name       string
	mdKey      types.NamespacedName
	primaryKey types.NamespacedName
	canaryKey  types.NamespacedName
}

// newCanaryHarness creates a namespace and a reconciler wired to a script.
func newCanaryHarness(steps ...analysis.Step) *canaryHarness {
	GinkgoHelper()

	ns := mdtNewNamespace()
	name := "canary-" + mdtSuffix()

	provider := analysis.NewScripted(steps...)
	clk := testingclock.NewFakePassiveClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))

	r := mdtNewReconciler()
	r.Clock = clk
	r.Provider = provider
	r.Discovery = proberWith(false)

	return &canaryHarness{
		r: r, clock: clk, provider: provider,
		namespace:  ns,
		name:       name,
		mdKey:      types.NamespacedName{Namespace: ns, Name: name},
		primaryKey: types.NamespacedName{Namespace: ns, Name: naming.PrimaryDeployment(name)},
		canaryKey:  types.NamespacedName{Namespace: ns, Name: naming.CanaryDeployment(name)},
	}
}

// establishBaseline creates the ModelDeployment, rolls it out, and leaves it
// with a recorded lastGoodRevision.
//
// A canary needs something to roll back TO, so every scenario begins from a
// deployment that has already succeeded once. This also records the
// ControllerRevision the primary will be re-rendered from during the rollout.
func (h *canaryHarness) establishBaseline(opts ...helpers.MDOption) {
	GinkgoHelper()

	all := append([]helpers.MDOption{helpers.WithReplicas(5)}, opts...)
	md := mdtCreate(helpers.NewModelDeployment(h.name, h.namespace, all...))

	// The replica count comes from the fixture rather than a literal. The
	// rollout-complete test compares updated, ready and available against the
	// DESIRED count, so marking a different number leaves the resource stuck in
	// Progressing and lastGoodRevision never advances — a failure that presents
	// as "the canary refused to start" three assertions later.
	replicas := *md.Spec.Replicas

	mdtReconcile(h.r, h.mdKey)
	Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.primaryKey, replicas)).To(Succeed())
	mdtReconcile(h.r, h.mdKey)

	Expect(mdtGet(h.mdKey).Status.LastGoodRevision).NotTo(BeEmpty(),
		"a canary needs a known-good revision to roll back to")
}

// switchToCanary flips the strategy on and changes the pod template, producing
// a new target revision.
func (h *canaryHarness) switchToCanary(mutate ...func(*inferencev1alpha1.CanarySpec)) {
	GinkgoHelper()

	md := mdtGet(h.mdKey)
	helpers.WithCanary(mutate...)(md)
	// contextSize is part of the revision payload, so this is the cheapest way
	// to force a new revision without changing anything that matters.
	helpers.WithContextSize(8192)(md)
	Expect(k8sClient.Update(ctx, md)).To(Succeed())
}

// reconcile runs one pass.
func (h *canaryHarness) reconcile() {
	GinkgoHelper()
	mdtReconcile(h.r, h.mdKey)
}

// markCanaryAvailable makes the canary Deployment report ready pods.
func (h *canaryHarness) markCanaryAvailable() {
	GinkgoHelper()
	dep := mdtGetDeployment(h.canaryKey)
	Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.canaryKey, *dep.Spec.Replicas)).To(Succeed())
}

// analysisRound steps the clock past the next scheduled round and reconciles.
func (h *canaryHarness) analysisRound() {
	GinkgoHelper()
	h.clock.SetTime(h.clock.Now().Add(2 * time.Minute))
	h.reconcile()
}

// status returns the canary status, requiring it to exist.
func (h *canaryHarness) status() *inferencev1alpha1.CanaryStatus {
	GinkgoHelper()
	cs := mdtGet(h.mdKey).Status.Canary
	Expect(cs).NotTo(BeNil(), "status.canary is missing")
	return cs
}

// canaryExists reports whether the canary Deployment is present.
func (h *canaryHarness) canaryExists() bool {
	GinkgoHelper()
	err := k8sClient.Get(ctx, h.canaryKey, &appsv1.Deployment{})
	if apierrors.IsNotFound(err) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return true
}

// pass and fail are scripted answers keyed to the query they belong to.
//
// Keying on the query rather than on position is what keeps these tests from
// asserting an implementation detail: one round issues a traffic-gate query and
// then one per metric, and the order is free to change.
func trafficOK() analysis.Step { return analysis.Pass(5).For(llmcpmetrics.RequestsTotal) }
func ttft(v float64) analysis.Step {
	return analysis.Pass(v).For(llmcpmetrics.TTFTSeconds)
}

var _ = Describe("Canary rollout", func() {
	It("climbs the weight ladder and promotes when every check passes", func() {
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		By("creating a canary Deployment at the first rung")
		h.reconcile()

		canaryDep := mdtGetDeployment(h.canaryKey)
		Expect(*canaryDep.Spec.Replicas).To(Equal(int32(1)),
			"20% of 5 replicas rounds up to 1")

		primary := mdtGetDeployment(h.primaryKey)
		Expect(*primary.Spec.Replicas).To(Equal(int32(4)),
			"the split must sum to spec.replicas, never exceed it")

		By("running the primary on the OLD revision while the canary carries the new one")
		// The whole comparison rests on this. Rendering both from the live spec
		// would produce two identical variants and an analysis that compares a
		// revision against itself.
		Expect(primary.Labels[naming.LabelRevision]).NotTo(Equal(canaryDep.Labels[naming.LabelRevision]))
		Expect(primary.Labels[naming.LabelRevision]).To(Equal(mdtGet(h.mdKey).Status.LastGoodRevision))

		Expect(mdtGet(h.mdKey).Status.Phase).To(Equal(inferencev1alpha1.PhaseCanarying))

		By("holding through the warm-up window without analysing")
		h.markCanaryAvailable()
		h.reconcile()
		Expect(h.status().Step).To(Equal(int32(0)))

		By("advancing to the second rung on a passing round")
		h.analysisRound()
		Expect(h.status().Step).To(Equal(int32(1)))
		Expect(h.status().DesiredWeight).To(Equal(int32(50)))

		By("promoting after the final rung passes")
		h.markCanaryAvailable()
		h.analysisRound()

		md := mdtGet(h.mdKey)
		Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhasePromoting))
		Expect(h.canaryExists()).To(BeFalse(), "the canary Deployment must be removed on promotion")

		By("re-pointing the primary at the promoted revision")
		promoted := mdtGetDeployment(h.primaryKey)
		Expect(promoted.Labels[naming.LabelRevision]).To(Equal(md.Status.Canary.Revision))
		Expect(*promoted.Spec.Replicas).To(Equal(int32(5)))

		By("settling once the primary converges")
		Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.primaryKey, 5)).To(Succeed())
		h.reconcile()
		h.reconcile()

		md = mdtGet(h.mdKey)
		Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhaseAvailable))
		Expect(md.Status.LastGoodRevision).To(Equal(md.Status.StableRevision))
	})

	It("rolls back on exactly the second failure, not the first", func() {
		// The assertion the scripted provider exists for. Against a live
		// Prometheus it cannot be made at all: you cannot induce a metric that
		// fails precisely twice.
		h := newCanaryHarness(trafficOK(), ttft(9.0))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()

		By("holding at the current weight after ONE failure")
		h.analysisRound()
		cs := h.status()
		Expect(cs.FailedChecks).To(Equal(int32(1)))
		Expect(h.canaryExists()).To(BeTrue(), "one failure is below the threshold of 2")
		Expect(mdtGet(h.mdKey).Status.Phase).To(Equal(inferencev1alpha1.PhaseCanarying))

		By("rolling back on the SECOND failure")
		h.analysisRound()

		md := mdtGet(h.mdKey)
		Expect(md.Status.Canary.FailedChecks).To(Equal(int32(2)))
		Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhaseRollingBack))
		Expect(h.canaryExists()).To(BeFalse())

		mdtExpectCondition(md, inferencev1alpha1.ConditionCanaryHealthy, metav1.ConditionFalse)

		By("restoring the primary to the last known-good revision")
		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).
			To(Equal(md.Status.LastGoodRevision))
		Expect(*mdtGetDeployment(h.primaryKey).Spec.Replicas).To(Equal(int32(5)))

		By("not retrying the rejected revision")
		// Without the FailedRevision guard the very next pass would start an
		// identical canary — an infinite loop of failing rollouts, each costing
		// a full analysis window and a Deployment churn.
		h.reconcile()
		Expect(h.canaryExists()).To(BeFalse())
		Expect(mdtGet(h.mdKey).Status.Canary.FailedRevision).NotTo(BeEmpty())

		By("and not quietly re-pointing the primary AT the rejected revision")
		// The rollback is only real if it survives the next reconcile. The
		// state machine answers ActionNone for a rejected target — the same
		// answer it gives for "already stable" and "nothing to fall back to" —
		// so a planner that treats all three alike puts 100% of traffic back on
		// the revision the gate just rejected, while status still reports a
		// rollback.
		after := mdtGet(h.mdKey)
		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).
			To(Equal(after.Status.LastGoodRevision),
				"the primary must stay on the last known-good revision")
		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).
			NotTo(Equal(after.Status.Canary.FailedRevision),
				"the rejected revision must not come back")
	})

	It("reports Error, never a rollback, when the metric provider is down", func() {
		// The safety property that separates this from a naive canary
		// controller: a monitoring outage must not cause a production
		// rollback.
		h := newCanaryHarness(analysis.Fail(analysis.ErrProviderDown).For(llmcpmetrics.RequestsTotal))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()

		for range 2 {
			h.analysisRound()
			Expect(h.canaryExists()).To(BeTrue(),
				"an unreachable provider says nothing about the canary and must never roll it back")
			Expect(h.status().FailedChecks).To(Equal(int32(0)),
				"a provider error must not spend the rollback budget")
			Expect(h.status().ConsecutiveErrors).To(BeNumerically(">", 0))
		}

		cs := h.status()
		Expect(cs.Checks).NotTo(BeEmpty())
		Expect(cs.Checks[0].Verdict).To(Equal(inferencev1alpha1.VerdictError))

		mdtExpectCondition(mdtGet(h.mdKey), inferencev1alpha1.ConditionCanaryHealthy,
			metav1.ConditionUnknown)
	})

	It("holds, and does not promote, when the canary has too little traffic", func() {
		// The "healthy because empty" guard. With no traffic the error rate is
		// zero and every latency percentile is absent, so an ungated canary
		// passes every check having served nothing.
		h := newCanaryHarness(analysis.Pass(0.01).For(llmcpmetrics.RequestsTotal))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()

		for range 3 {
			h.analysisRound()
		}

		cs := h.status()
		Expect(cs.Step).To(Equal(int32(0)), "an inconclusive canary must not climb the ladder")
		Expect(cs.ConsecutiveInconclusive).To(BeNumerically(">", 0))
		Expect(h.canaryExists()).To(BeTrue())
		Expect(cs.Checks[0].Verdict).To(Equal(inferencev1alpha1.VerdictInconclusive))
	})

	It("waits for approval before the final promotion", func() {
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary(func(c *inferencev1alpha1.CanarySpec) {
			c.StepWeights = []int32{40}
			c.RequireApproval = boolPtr(true)
		})

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()
		h.analysisRound()

		By("pausing rather than promoting")
		md := mdtGet(h.mdKey)
		Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhasePaused))
		Expect(h.canaryExists()).To(BeTrue())
		cond := mdtExpectCondition(md, inferencev1alpha1.ConditionCanaryHealthy, metav1.ConditionTrue)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonAwaitingApproval))

		By("staying paused indefinitely with no signal")
		// A gate the machine can talk itself out of is advisory, and an
		// operator who stepped away would come back to a promotion they never
		// approved.
		h.clock.SetTime(h.clock.Now().Add(time.Hour))
		h.reconcile()
		Expect(mdtGet(h.mdKey).Status.Phase).To(Equal(inferencev1alpha1.PhasePaused))

		By("promoting once the annotation is set")
		md = mdtGet(h.mdKey)
		if md.Annotations == nil {
			md.Annotations = map[string]string{}
		}
		md.Annotations[naming.AnnoPromote] = annotationEnabled
		Expect(k8sClient.Update(ctx, md)).To(Succeed())

		h.reconcile()
		Expect(mdtGet(h.mdKey).Status.Phase).To(Equal(inferencev1alpha1.PhasePromoting))
		Expect(h.canaryExists()).To(BeFalse())

		By("consuming the annotation, so it cannot approve the next release too")
		Expect(mdtGet(h.mdKey).Annotations).NotTo(HaveKey(naming.AnnoPromote))
	})

	It("aborts immediately when the abort annotation is set", func() {
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()
		Expect(h.canaryExists()).To(BeTrue())

		md := mdtGet(h.mdKey)
		if md.Annotations == nil {
			md.Annotations = map[string]string{}
		}
		md.Annotations[naming.AnnoAbort] = annotationEnabled
		Expect(k8sClient.Update(ctx, md)).To(Succeed())

		h.reconcile()

		md = mdtGet(h.mdKey)
		Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhaseRollingBack))
		Expect(h.canaryExists()).To(BeFalse())
		Expect(md.Annotations).NotTo(HaveKey(naming.AnnoAbort))
	})

	It("restarts at step zero when the spec changes mid-rollout", func() {
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()

		h.reconcile()
		h.markCanaryAvailable()
		h.reconcile()
		h.analysisRound()
		Expect(h.status().Step).To(Equal(int32(1)))
		firstCanaryRevision := h.status().Revision

		By("changing the pod template again")
		md := mdtGet(h.mdKey)
		helpers.WithContextSize(16384)(md)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())

		h.reconcile()

		// Carrying the accumulated position across to a revision that has never
		// been measured would promote a new build on the strength of checks run
		// against a different one.
		cs := h.status()
		Expect(cs.Step).To(Equal(int32(0)))
		Expect(cs.DesiredWeight).To(Equal(int32(20)))
		Expect(cs.Revision).NotTo(Equal(firstCanaryRevision),
			"the canary must now carry the newest revision")
		Expect(mdtGetDeployment(h.canaryKey).Labels[naming.LabelRevision]).To(Equal(cs.Revision))
	})

	It("reports the quantized weight honestly", func() {
		// Replica splitting cannot express 20% with 3 pods; the nearest
		// achievable value is 33%. Showing only the requested figure would let
		// an operator believe the blast radius is smaller than it is.
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline(helpers.WithReplicas(3))
		h.switchToCanary()

		h.reconcile()

		cs := h.status()
		Expect(cs.DesiredWeight).To(Equal(int32(20)))
		Expect(cs.CurrentWeight).To(Equal(int32(33)))

		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionTrafficRoutingReady, metav1.ConditionTrue)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonWeightQuantized))
	})

	It("declines to canary a single replica instead of stalling", func() {
		// A canary Deployment with zero replicas never becomes available, and
		// the rollout would sit at "waiting for canary replicas" forever with
		// no hint that it never can.
		h := newCanaryHarness()
		h.establishBaseline(helpers.WithReplicas(1))
		h.switchToCanary()

		h.reconcile()

		Expect(h.canaryExists()).To(BeFalse())

		// The explanation lives on the condition rather than in status.canary:
		// there IS no canary, so a canary status describing one would be
		// misleading. What matters is that the refusal is stated somewhere a
		// user will look, instead of presenting as a rollout that never moves.
		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionCanaryHealthy, metav1.ConditionUnknown)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonInsufficientReplicas))
		Expect(cond.Message).To(ContainSubstring("at least 2 replicas"))

		By("still rolling the new revision out on the primary")
		md := mdtGet(h.mdKey)
		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).
			To(Equal(md.Status.StableRevision))
	})

	It("sums replica counts across both variants for the /scale contract", func() {
		// .spec.replicas is the total across variants, so .status.replicas must
		// be too — an HPA divides its metric by this number, and reporting only
		// the primary's would make it scale up to replace capacity that already
		// exists.
		h := newCanaryHarness(trafficOK(), ttft(0.4))
		h.provider.Repeat = true

		h.establishBaseline()
		h.switchToCanary()
		h.reconcile()

		Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.primaryKey, 4)).To(Succeed())
		h.markCanaryAvailable()
		h.reconcile()

		md := mdtGet(h.mdKey)
		Expect(md.Status.Replicas).To(Equal(int32(5)))
		Expect(md.Status.ReadyReplicas).To(Equal(int32(5)))
	})
})

var _ = Describe("Automatic rollback of a stalled rollout", func() {
	It("reverts a RollingUpdate that exceeded its progress deadline", func() {
		// The one thing a Deployment genuinely cannot do for itself: once it
		// sets ProgressDeadlineExceeded it stays there indefinitely, and
		// nothing in Kubernetes ever revisits the decision.
		h := newCanaryHarness()
		h.establishBaseline()

		good := mdtGet(h.mdKey).Status.LastGoodRevision

		By("rolling out a revision that never becomes available")
		md := mdtGet(h.mdKey)
		helpers.WithContextSize(8192)(md)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		h.reconcile()

		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).NotTo(Equal(good))

		By("reverting once the Deployment declares the rollout stalled")
		Expect(helpers.MarkDeploymentStalled(ctx, k8sClient, h.primaryKey)).To(Succeed())
		h.reconcile()

		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).To(Equal(good),
			"the primary must be restored to the last known-good revision")
	})

	It("leaves a stalled rollout alone when autoRollback is off", func() {
		h := newCanaryHarness()
		h.establishBaseline(func(md *inferencev1alpha1.ModelDeployment) {
			md.Spec.Rollout.AutoRollback = boolPtr(false)
		})

		md := mdtGet(h.mdKey)
		helpers.WithContextSize(8192)(md)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		h.reconcile()

		target := mdtGet(h.mdKey).Status.StableRevision
		Expect(helpers.MarkDeploymentStalled(ctx, k8sClient, h.primaryKey)).To(Succeed())
		h.reconcile()

		Expect(mdtGetDeployment(h.primaryKey).Labels[naming.LabelRevision]).To(Equal(target),
			"autoRollback: false means the operator does not intervene")
	})
})

// boolPtr returns a pointer to b.
func boolPtr(b bool) *bool { return &b }
