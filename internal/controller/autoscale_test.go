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
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// loadDial is a metric provider whose value a test turns like a dial.
//
// A ScriptedProvider replays a fixed sequence, which is right for a canary
// whose rounds are counted. An autoscaler is a closed loop instead: the test
// needs to hold a load level steady across several polls, then change it, and
// watch the fleet follow. A dial expresses that directly, and — like the
// scripted provider — it makes a load curve that no live Prometheus would
// produce on demand into three lines of test.
type loadDial struct {
	mu      sync.Mutex
	value   float64
	err     error
	queries []string
}

func (d *loadDial) set(v float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.value, d.err = v, nil
}

func (d *loadDial) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *loadDial) lastQuery() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queries) == 0 {
		return ""
	}
	return d.queries[len(d.queries)-1]
}

func (d *loadDial) Query(_ context.Context, query string) (analysis.Sample, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, query)
	if d.err != nil {
		return analysis.Sample{}, d.err
	}
	return analysis.Sample{Value: d.value, Count: 1}, nil
}

// scaleHarness drives the built-in autoscaler deterministically.
type scaleHarness struct {
	r     *ModelDeploymentReconciler
	clock *testingclock.FakePassiveClock
	dial  *loadDial

	namespace  string
	name       string
	mdKey      types.NamespacedName
	primaryKey types.NamespacedName
}

func newScaleHarness() *scaleHarness {
	GinkgoHelper()

	ns := mdtNewNamespace()
	name := "scale-" + mdtSuffix()

	dial := &loadDial{}
	clk := testingclock.NewFakePassiveClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))

	r := mdtNewReconciler()
	r.Clock = clk
	r.Provider = dial
	r.Discovery = proberWith(false)

	return &scaleHarness{
		r: r, clock: clk, dial: dial,
		namespace:  ns,
		name:       name,
		mdKey:      types.NamespacedName{Namespace: ns, Name: name},
		primaryKey: types.NamespacedName{Namespace: ns, Name: naming.PrimaryDeployment(name)},
	}
}

// create makes the ModelDeployment and reconciles it once.
func (h *scaleHarness) create(opts ...helpers.MDOption) {
	GinkgoHelper()
	mdtCreate(helpers.NewModelDeployment(h.name, h.namespace, opts...))
	mdtReconcile(h.r, h.mdKey)
}

// poll advances the clock by one interval and reconciles.
func (h *scaleHarness) poll() {
	GinkgoHelper()
	h.clock.SetTime(h.clock.Now().Add(20 * time.Second))
	h.settleReady()
	mdtReconcile(h.r, h.mdKey)
}

// settleReady makes every pod of the primary Deployment report ready, so the
// autoscaler always has a live denominator.
func (h *scaleHarness) settleReady() {
	GinkgoHelper()
	dep := mdtGetDeployment(h.primaryKey)
	Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.primaryKey, *dep.Spec.Replicas)).To(Succeed())
}

// replicas reads the current desired count off the spec.
func (h *scaleHarness) replicas() int32 {
	GinkgoHelper()
	md := mdtGet(h.mdKey)
	Expect(md.Spec.Replicas).NotTo(BeNil())
	return *md.Spec.Replicas
}

// withAutoscaling turns the built-in autoscaler on.
func withAutoscaling(mutate ...func(*inferencev1alpha1.AutoscalingSpec)) helpers.MDOption {
	return func(md *inferencev1alpha1.ModelDeployment) {
		target := resourceQuantity("2")
		tolerance := resourceQuantity("100m")

		md.Spec.Autoscaling = &inferencev1alpha1.AutoscalingSpec{
			Mode:        inferencev1alpha1.AutoscalingBuiltin,
			MinReplicas: 1,
			MaxReplicas: 10,
			Metric:      inferencev1alpha1.AutoscalingQueueDepth,
			Target:      &target,
			Tolerance:   &tolerance,
			// Instant up, five minutes down: adding capacity late costs latency
			// the user feels, removing it early costs a cold start.
			ScaleUpStabilization:   &metav1.Duration{Duration: 0},
			ScaleDownStabilization: &metav1.Duration{Duration: 5 * time.Minute},
			PollInterval:           &metav1.Duration{Duration: 15 * time.Second},
			Window:                 &metav1.Duration{Duration: 60 * time.Second},
			Provider: inferencev1alpha1.AnalysisProviderSpec{
				Type:    inferencev1alpha1.AnalysisProviderPrometheus,
				Address: "http://prometheus-operated.monitoring.svc:9090",
			},
		}
		for _, m := range mutate {
			m(md.Spec.Autoscaling)
		}
	}
}

var _ = Describe("The /scale subresource", func() {
	It("round-trips a replica count and drives the child Deployment", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2))

		By("reporting the current state through /scale")
		scale := &autoscalingv1.Scale{}
		Expect(k8sClient.SubResource("scale").Get(ctx, mdtGet(h.mdKey), scale)).To(Succeed())
		Expect(scale.Spec.Replicas).To(Equal(int32(2)))

		By("accepting a write, the way kubectl scale does")
		scale.Spec.Replicas = 5
		Expect(k8sClient.SubResource("scale").Update(ctx, mdtGet(h.mdKey), client.WithSubResourceBody(scale))).
			To(Succeed())

		Expect(h.replicas()).To(Equal(int32(5)))

		mdtReconcile(h.r, h.mdKey)
		Expect(*mdtGetDeployment(h.primaryKey).Spec.Replicas).To(Equal(int32(5)))
	})

	It("publishes a SERIALIZED selector that actually selects the pods", func() {
		// The single most common defect in a CRD /scale implementation is
		// putting a metav1.LabelSelector in status.selector instead of its
		// string form. The CRD still validates, kubectl scale still works, and
		// only an HPA reveals the bug — by silently matching no pods and
		// computing its target from nothing.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2))
		h.settleReady()
		mdtReconcile(h.r, h.mdKey)

		raw := mdtGet(h.mdKey).Status.Selector
		Expect(raw).NotTo(BeEmpty())
		Expect(raw).NotTo(ContainSubstring("matchLabels"),
			"status.selector must be a serialized selector string, not a marshalled LabelSelector")

		selector, err := labels.Parse(raw)
		Expect(err).NotTo(HaveOccurred(), "status.selector must parse as a label selector")

		By("matching the pods of BOTH variants")
		// It has to span the whole fleet: an HPA averages its metric over the
		// pods this selects, so a selector matching only the primary would
		// divide by the wrong pod count during a canary.
		podLabels := naming.PodLabels(h.name, inferencev1alpha1.VariantPrimary, "rev")
		Expect(selector.Matches(labels.Set(podLabels))).To(BeTrue())

		canaryLabels := naming.PodLabels(h.name, inferencev1alpha1.VariantCanary, "rev")
		Expect(selector.Matches(labels.Set(canaryLabels))).To(BeTrue())

		By("not matching an unrelated workload")
		Expect(selector.Matches(labels.Set{naming.LabelModelDeployment: "someone-else"})).To(BeFalse())
	})

	It("reports a replica count an HorizontalPodAutoscaler can divide by", func() {
		// envtest runs no HPA controller, so this asserts the CONTRACT the HPA
		// reads rather than the HPA's behaviour: a non-zero status.replicas and
		// a usable selector are exactly what turn `kubectl get hpa` from
		// <unknown> into a number. The cluster-tier proof is the Chainsaw suite.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(3))
		h.settleReady()
		mdtReconcile(h.r, h.mdKey)

		md := mdtGet(h.mdKey)
		Expect(md.Status.Replicas).To(Equal(int32(3)))
		Expect(md.Status.ReadyReplicas).To(Equal(int32(3)))
		Expect(md.Status.Selector).NotTo(BeEmpty())

		By("accepting a stock HPA that targets it")
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: h.namespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: inferencev1alpha1.GroupVersion.String(),
					Kind:       kindModelDeployment,
					Name:       h.name,
				},
				MinReplicas: ptr.To(int32(1)),
				MaxReplicas: 6,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())
	})
})

var _ = Describe("The built-in autoscaler", func() {
	It("is off, and says so, when spec.autoscaling is absent", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2))
		h.settleReady()
		mdtReconcile(h.r, h.mdKey)

		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionFalse)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonAutoscalingDisabled))
		Expect(h.replicas()).To(Equal(int32(2)), "an absent autoscaler must not touch the replica count")
	})

	It("scales up on load, holds inside the tolerance band, and scales down after the window", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2), withAutoscaling())
		h.settleReady()

		By("querying the fleet, not one variant")
		// An autoscaler decides how much capacity exists; a rollout decides how
		// it is split. Filtering by variant would size the fleet from a
		// fraction of its load that changes at every canary step.
		h.dial.set(4)
		mdtReconcile(h.r, h.mdKey)
		Expect(h.dial.lastQuery()).To(ContainSubstring("llmcp_inference_queue_depth"))
		Expect(h.dial.lastQuery()).NotTo(ContainSubstring("variant="))

		By("holding when the measurement is at target")
		Expect(h.replicas()).To(Equal(int32(2)), "4 queued over 2 pods is exactly the target of 2")
		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionTrue)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonAutoscalingActive))

		By("holding when the measurement moves inside the tolerance band")
		// Without a band, 2.05 per pod against a target of 2 is a scale event —
		// and so is 1.95 fifteen seconds later, forever.
		h.dial.set(4.1)
		h.poll()
		Expect(h.replicas()).To(Equal(int32(2)))

		By("scaling up when load genuinely climbs")
		h.dial.set(16)
		h.poll()
		Expect(h.replicas()).To(Equal(int32(8)), "16 queued at a target of 2 per pod wants 8 pods")

		md := mdtGet(h.mdKey)
		Expect(md.Status.Autoscaling).NotTo(BeNil())
		Expect(md.Status.Autoscaling.DesiredReplicas).To(Equal(int32(8)))
		Expect(md.Status.Autoscaling.LastScaleTime).NotTo(BeNil())
		Expect(mdtExpectCondition(md,
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionTrue).Reason).
			To(Equal(inferencev1alpha1.ReasonScaledUp))

		By("NOT scaling straight back down when the load drops")
		// A pod removed here has to load its weights again to come back, which
		// for a real model is minutes of unavailable capacity bought to save
		// seconds of idle capacity.
		h.dial.set(2)
		h.poll()
		Expect(h.replicas()).To(Equal(int32(8)), "the scale-down window has not cleared")
		Expect(mdtGet(h.mdKey).Status.Autoscaling.Message).To(ContainSubstring("stabilization"))

		By("scaling down once the window has passed")
		h.clock.SetTime(h.clock.Now().Add(6 * time.Minute))
		h.settleReady()
		mdtReconcile(h.r, h.mdKey)
		Expect(h.replicas()).To(Equal(int32(1)))
	})

	It("freezes the fleet and reports False when the metric cannot be read", func() {
		// The dangerous state, and the reason this condition is False rather
		// than Unknown: the replica count is pinned wherever it happened to be,
		// and demand can climb underneath it indefinitely.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(4), withAutoscaling())
		h.settleReady()

		h.dial.fail(analysis.ErrProviderDown)
		mdtReconcile(h.r, h.mdKey)

		Expect(h.replicas()).To(Equal(int32(4)),
			"an unreadable metric must never be treated as a measurement of zero")

		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionFalse)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonAutoscalingNoMetrics))
		Expect(cond.Message).To(ContainSubstring("provider"))
	})

	It("treats an empty query result as no measurement, not as zero load", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(4), withAutoscaling())
		h.settleReady()

		h.r.Provider = analysis.ProviderFunc(func(context.Context, string) (analysis.Sample, error) {
			return analysis.Sample{Count: 0}, nil
		})
		mdtReconcile(h.r, h.mdKey)

		Expect(h.replicas()).To(Equal(int32(4)),
			"an empty result would otherwise scale the fleet to its floor during a scrape outage")
		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionFalse)
		Expect(cond.Message).To(ContainSubstring("absence"))
	})

	It("respects minReplicas and maxReplicas", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(3), withAutoscaling(func(a *inferencev1alpha1.AutoscalingSpec) {
			a.MinReplicas = 2
			a.MaxReplicas = 5
			a.ScaleDownStabilization = &metav1.Duration{Duration: 0}
		}))
		h.settleReady()

		h.dial.set(1000)
		mdtReconcile(h.r, h.mdKey)
		Expect(h.replicas()).To(Equal(int32(5)))

		h.dial.set(0)
		h.poll()
		Expect(h.replicas()).To(Equal(int32(2)))
	})

	It("stands down when an external HorizontalPodAutoscaler also targets it", func() {
		// Two controllers writing .spec.replicas from different signals do not
		// average out; they fight, and the fleet oscillates at the rate of the
		// faster one. The symptom looks like a flapping workload rather than a
		// configuration mistake.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(3), withAutoscaling())
		h.settleReady()

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "rival", Namespace: h.namespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: inferencev1alpha1.GroupVersion.String(),
					Kind:       kindModelDeployment,
					Name:       h.name,
				},
				MinReplicas: ptr.To(int32(1)),
				MaxReplicas: 9,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		h.dial.set(100)
		mdtReconcile(h.r, h.mdKey)

		Expect(h.replicas()).To(Equal(int32(3)),
			"the built-in autoscaler must stand down rather than join the fight")

		cond := mdtExpectCondition(mdtGet(h.mdKey),
			inferencev1alpha1.ConditionAutoscalingReady, metav1.ConditionFalse)
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonAutoscalingConflict))
		Expect(cond.Message).To(ContainSubstring("rival"))
	})

	It("ignores an HPA that targets a different workload", func() {
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2), withAutoscaling())
		h.settleReady()

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: h.namespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "some-other-thing",
				},
				MaxReplicas: 9,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		h.dial.set(16)
		mdtReconcile(h.r, h.mdKey)
		Expect(h.replicas()).To(Equal(int32(8)),
			"an unrelated HPA must not disable the built-in autoscaler")
	})

	It("persists its stabilization history so a restart does not forget", func() {
		// An in-memory window silently forgets that the fleet was scaled up
		// ninety seconds ago, and the replacement process is free to scale it
		// straight back down. That is the failure a leader-election handover
		// produces, and it is invisible in a single-process test unless the
		// history is deliberately round-tripped.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(2), withAutoscaling())
		h.settleReady()

		h.dial.set(16)
		mdtReconcile(h.r, h.mdKey)
		Expect(h.replicas()).To(Equal(int32(8)))

		md := mdtGet(h.mdKey)
		Expect(md.Status.Autoscaling.Recommendations).NotTo(BeEmpty(),
			"the stabilization window lives in status precisely so it survives a restart")

		By("a fresh reconciler picking up where the old one left off")
		fresh := mdtNewReconciler()
		fresh.Clock = h.clock
		fresh.Provider = h.dial
		fresh.Discovery = proberWith(false)

		h.dial.set(2)
		h.clock.SetTime(h.clock.Now().Add(30 * time.Second))
		h.settleReady()
		mdtReconcile(fresh, h.mdKey)

		Expect(h.replicas()).To(Equal(int32(8)),
			"the replacement process must honour the window the previous one opened")
	})

	It("keeps a canary's weight ratio while the fleet grows underneath it", func() {
		// The replica-ownership contract, end to end: Recommend produces a
		// total and Split distributes it. Two functions, one input, no fight.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(4), withAutoscaling())

		// The dial is set BEFORE the fleet is measured. Left at its zero value
		// the autoscaler would — correctly — shrink an idle four-pod fleet to
		// one, and the canary would then have nothing to split.
		h.dial.set(8) // 8 queued over 4 pods is exactly the target
		h.settleReady()
		mdtReconcile(h.r, h.mdKey)
		Expect(h.replicas()).To(Equal(int32(4)))
		Expect(mdtGet(h.mdKey).Status.LastGoodRevision).NotTo(BeEmpty())

		By("starting a canary at 50%")
		md := mdtGet(h.mdKey)
		helpers.WithCanary(func(c *inferencev1alpha1.CanarySpec) {
			c.StepWeights = []int32{50}
		})(md)
		helpers.WithContextSize(8192)(md)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())

		mdtReconcile(h.r, h.mdKey)

		canaryKey := types.NamespacedName{Namespace: h.namespace, Name: naming.CanaryDeployment(h.name)}
		Expect(*mdtGetDeployment(canaryKey).Spec.Replicas).To(Equal(int32(2)))
		Expect(*mdtGetDeployment(h.primaryKey).Spec.Replicas).To(Equal(int32(2)))

		By("doubling the fleet and keeping the same 50/50 split")
		h.dial.set(16)
		h.clock.SetTime(h.clock.Now().Add(30 * time.Second))
		Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, h.primaryKey, 2)).To(Succeed())
		Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, canaryKey, 2)).To(Succeed())
		mdtReconcile(h.r, h.mdKey)

		Expect(h.replicas()).To(Equal(int32(8)))
		Expect(*mdtGetDeployment(h.primaryKey).Spec.Replicas).To(Equal(int32(4)))
		Expect(*mdtGetDeployment(canaryKey).Spec.Replicas).To(Equal(int32(4)))

		cs := mdtGet(h.mdKey).Status.Canary
		Expect(cs.CurrentWeight).To(Equal(int32(50)),
			"the canary's exposure must not drift because the fleet resized")
	})

	It("converges instead of flapping when the load sits between two replica counts", func() {
		// A closed loop with a demand that no integer replica count satisfies
		// exactly. Without the tolerance band this oscillates forever, and each
		// oscillation costs a model load.
		h := newScaleHarness()
		h.create(helpers.WithReplicas(1), withAutoscaling(func(a *inferencev1alpha1.AutoscalingSpec) {
			a.ScaleDownStabilization = &metav1.Duration{Duration: 0}
		}))
		h.settleReady()

		h.dial.set(13) // 6.5 pods' worth at a target of 2

		var last []int32
		for range 12 {
			h.poll()
			last = append(last, h.replicas())
			if len(last) > 4 {
				last = last[1:]
			}
		}

		for _, r := range last {
			Expect(r).To(Equal(last[0]),
				"the fleet never settled; the last recommendations were %v", last)
		}
	})
})

// resourceQuantity parses a canonical quantity literal for a fixture.
func resourceQuantity(s string) resource.Quantity { return resource.MustParse(s) }
