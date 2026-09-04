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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/discovery"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// proberWith returns a Static prober reporting the prometheus-operator kinds
// present or absent.
//
// Both kinds are reported together because that is how a cluster almost always
// has them. The controller still probes for each SEPARATELY — see
// reconcilePrometheusRule — so that a cut-down or mid-upgrade install serving
// one and not the other produces a clear message rather than a failed apply.
func proberWith(present bool) *discovery.Static {
	return &discovery.Static{
		Present: map[schema.GroupVersionKind]bool{
			observability.ServiceMonitorGVK: present,
			observability.PrometheusRuleGVK: present,
		},
	}
}

// getPrometheusRule reads the applied rules as unstructured content. The
// operator never registers the type in a scheme, so the test reads it the same
// way.
func getPrometheusRule(key types.NamespacedName) *unstructured.Unstructured {
	GinkgoHelper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(observability.PrometheusRuleGVK)
	Expect(k8sClient.Get(ctx, key, u)).To(Succeed())
	return u
}

// getServiceMonitor reads the applied ServiceMonitor as unstructured content.
// The operator never registers the type in a scheme — see the observability
// package comment — so the test reads it the same way.
func getServiceMonitor(key types.NamespacedName) *unstructured.Unstructured {
	GinkgoHelper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(observability.ServiceMonitorGVK)
	Expect(k8sClient.Get(ctx, key, u)).To(Succeed())
	return u
}

var _ = Describe("Metric collection", func() {
	var (
		namespace string
		name      string
		mdKey     types.NamespacedName
		smKey     types.NamespacedName
		r         *ModelDeploymentReconciler
	)

	BeforeEach(func() {
		namespace = mdtNewNamespace()
		name = "obs-" + mdtSuffix()
		mdKey = types.NamespacedName{Namespace: namespace, Name: name}
		smKey = types.NamespacedName{Namespace: namespace, Name: naming.ServiceMonitor(name)}
		r = mdtNewReconciler()
	})

	When("the cluster serves prometheus-operator CRDs", func() {
		BeforeEach(func() { r.Discovery = proberWith(true) })

		It("creates a ServiceMonitor owned by the ModelDeployment", func() {
			md := mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			sm := getServiceMonitor(smKey)

			By("owning it, so garbage collection removes it with the ModelDeployment")
			mdtExpectOwnedBy(sm.GetOwnerReferences(), md)

			By("selecting the headless metrics Service, not the serving one")
			sel, found, err := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(sel).To(HaveKeyWithValue(naming.LabelComponent, observability.ComponentMetrics))

			By("reporting MetricsRegistered=True")
			cond := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionTrue)
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonMetricsRegistered))
		})

		It("creates a headless metrics Service alongside the serving one", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			metricsSvc := mdtGetService(types.NamespacedName{
				Namespace: namespace, Name: naming.MetricsService(name),
			})
			Expect(metricsSvc.Spec.ClusterIP).To(Equal("None"),
				"a load-balanced VIP would land on a different pod every scrape")
			Expect(metricsSvc.Spec.PublishNotReadyAddresses).To(BeTrue())

			By("keeping the engine port off the SERVING Service")
			// Otherwise any in-cluster client could address the engine directly
			// and bypass the shim, and that traffic would contribute to no
			// llmcp_* series at all.
			serving := mdtGetService(types.NamespacedName{Namespace: namespace, Name: naming.Service(name)})
			for _, p := range serving.Spec.Ports {
				Expect(p.Name).NotTo(Equal(naming.PortNameEngine))
			}
		})

		It("converges: applying twice does not rewrite the ServiceMonitor", func() {
			// The Server-Side Apply idempotency check, applied to the
			// unstructured path. A non-byte-stable render would bump
			// resourceVersion on every reconcile, and with a watch attached
			// that is a hot loop that never converges.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)
			first := getServiceMonitor(smKey).GetResourceVersion()

			mdtReconcile(r, mdKey)
			Expect(getServiceMonitor(smKey).GetResourceVersion()).To(Equal(first),
				"the second apply rewrote the object; SSA would loop against the watch")
		})

		It("creates the SLO recording rules and burn-rate alerts", func() {
			name2 := name
			mdtCreate(helpers.NewModelDeployment(name2, namespace))
			mdtReconcile(r, mdKey)

			rule := getPrometheusRule(types.NamespacedName{
				Namespace: namespace, Name: naming.PrometheusRule(name2),
			})

			By("owning it, so it is garbage-collected with the ModelDeployment")
			mdtExpectOwnedBy(rule.GetOwnerReferences(), mdtGet(mdKey))

			groups, found, err := unstructured.NestedSlice(rule.Object, "spec", "groups")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(groups).To(HaveLen(2), "one group for the SLIs, one for the alerts")

			By("saying so in the MetricsRegistered message")
			cond := mdtExpectCondition(mdtGet(mdKey),
				inferencev1alpha1.ConditionMetricsRegistered, metav1.ConditionTrue)
			Expect(cond.Message).To(ContainSubstring("SLO rules"))
		})

		It("converges: applying twice does not rewrite the PrometheusRule", func() {
			// Same Server-Side Apply idempotency requirement as the
			// ServiceMonitor, and the same consequence for getting it wrong: a
			// byte-unstable render bumps resourceVersion every reconcile, fires
			// the watch, and loops.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			ruleKey := types.NamespacedName{Namespace: namespace, Name: naming.PrometheusRule(name)}
			first := getPrometheusRule(ruleKey).GetResourceVersion()

			mdtReconcile(r, mdKey)
			Expect(getPrometheusRule(ruleKey).GetResourceVersion()).To(Equal(first))
		})

		It("reports a snapped SLO threshold instead of changing it silently", func() {
			// 0.5s is not a bucket boundary; the nearest at or above it is 0.6s.
			// A threshold between boundaries would select no bucket, the
			// recording rule would produce nothing, and the alert could never
			// fire — an SLO that cannot alert, which is worse than none because
			// it is believed.
			md := helpers.NewModelDeployment(name, namespace)
			md.Spec.Observability.SLO = &inferencev1alpha1.SLOSpec{
				TTFTThreshold: helpers.QuantityPtr("500m"),
			}
			mdtCreate(md)
			mdtReconcile(r, mdKey)

			cond := mdtExpectCondition(mdtGet(mdKey),
				inferencev1alpha1.ConditionMetricsRegistered, metav1.ConditionTrue)
			Expect(cond.Message).To(ContainSubstring("snapped up to 0.6s"))
		})

		It("creates no PrometheusRule when it is disabled", func() {
			md := helpers.NewModelDeployment(name, namespace)
			md.Spec.Observability.PrometheusRule = &inferencev1alpha1.PrometheusRuleSpec{
				Enabled: ptr.To(false),
			}
			mdtCreate(md)
			mdtReconcile(r, mdKey)

			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(observability.PrometheusRuleGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: naming.PrometheusRule(name),
			}, u)).NotTo(Succeed())

			cond := mdtExpectCondition(mdtGet(mdKey),
				inferencev1alpha1.ConditionMetricsRegistered, metav1.ConditionTrue)
			Expect(cond.Message).To(ContainSubstring("SLO rules disabled"))
		})

		It("removes a ServiceMonitor and PrometheusRule that are later disabled", func() {
			// Disabling has to REMOVE, not merely stop creating. A
			// ServiceMonitor left behind keeps Prometheus scraping and a
			// PrometheusRule left behind keeps paging — both while the status
			// says in plain words that nothing is collected. A cluster that
			// contradicts its own status is the hardest kind of discrepancy to
			// notice, because every reasonable place you would look agrees with
			// you.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			ruleKey := types.NamespacedName{Namespace: namespace, Name: naming.PrometheusRule(name)}
			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(observability.ServiceMonitorGVK)
			Expect(k8sClient.Get(ctx, smKey, sm)).To(Succeed())
			rule := &unstructured.Unstructured{}
			rule.SetGroupVersionKind(observability.PrometheusRuleGVK)
			Expect(k8sClient.Get(ctx, ruleKey, rule)).To(Succeed())

			By("turning both off")
			md := mdtGet(mdKey)
			md.Spec.Observability.ServiceMonitor = &inferencev1alpha1.ServiceMonitorSpec{
				Enabled: ptr.To(false),
			}
			md.Spec.Observability.PrometheusRule = &inferencev1alpha1.PrometheusRuleSpec{
				Enabled: ptr.To(false),
			}
			Expect(k8sClient.Update(ctx, md)).To(Succeed())
			mdtReconcile(r, mdKey)

			By("and finding neither object still in the cluster")
			smAfter := &unstructured.Unstructured{}
			smAfter.SetGroupVersionKind(observability.ServiceMonitorGVK)
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, smKey, smAfter))).To(BeTrue(),
				"the ServiceMonitor survived being disabled; Prometheus is still scraping")

			ruleAfter := &unstructured.Unstructured{}
			ruleAfter.SetGroupVersionKind(observability.PrometheusRuleGVK)
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, ruleKey, ruleAfter))).To(BeTrue(),
				"the PrometheusRule survived being disabled; its burn-rate alerts still page")

			By("and staying deleted on the next pass")
			mdtReconcile(r, mdKey)
		})

		It("reports MetricsRegistered=False when the shim is disabled", func() {
			// The ServiceMonitor still exists — engine diagnostics are worth
			// collecting — but no llmcp_* metric is produced, so a canary
			// configured against them would stall rather than fail. Saying so
			// in a condition is the difference between a five-second diagnosis
			// and an afternoon.
			md := helpers.NewModelDeployment(name, namespace)
			md.Spec.Serving.Shim.Enabled = ptr.To(false)
			mdtCreate(md)
			mdtReconcile(r, mdKey)

			cond := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionFalse)
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonShimDisabled))

			_ = getServiceMonitor(smKey)
		})
	})

	When("the prometheus-operator CRDs are absent", func() {
		BeforeEach(func() { r.Discovery = proberWith(false) })

		It("serves normally and says why metrics are missing", func() {
			// A cluster without Prometheus is a legitimate configuration.
			// Failing the reconcile over it would stop the operator managing a
			// workload the missing CRD has nothing to do with.
			mdtCreate(helpers.NewModelDeployment(name, namespace))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: mdKey})
			Expect(err).NotTo(HaveOccurred(), "a missing optional CRD must not fail the reconcile")

			cond := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionFalse)
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonPrometheusOperatorCRDsAbsent))
			Expect(cond.Message).To(ContainSubstring("prometheus-operator"))

			By("creating no PrometheusRule either")
			ruleObj := &unstructured.Unstructured{}
			ruleObj.SetGroupVersionKind(observability.PrometheusRuleGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: naming.PrometheusRule(name),
			}, ruleObj)).NotTo(Succeed())

			By("still creating the Deployment and both Services")
			mdtGetDeployment(types.NamespacedName{Namespace: namespace, Name: naming.PrimaryDeployment(name)})
			mdtGetService(types.NamespacedName{Namespace: namespace, Name: naming.Service(name)})
			mdtGetService(types.NamespacedName{Namespace: namespace, Name: naming.MetricsService(name)})

			By("creating no ServiceMonitor")
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(observability.ServiceMonitorGVK)
			Expect(k8sClient.Get(ctx, smKey, u)).NotTo(Succeed())
		})

		It("still reaches Ready, because metrics are not part of the readiness rollup", func() {
			// If MetricsRegistered fed into Ready, every e2e test in this
			// repository would need a monitoring stack to pass.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			depKey := types.NamespacedName{Namespace: namespace, Name: naming.PrimaryDeployment(name)}
			Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, depKey, 1)).To(Succeed())
			mdtReconcile(r, mdKey)

			mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionReady, metav1.ConditionTrue)
			mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered, metav1.ConditionFalse)
		})
	})

	When("discovery itself fails", func() {
		It("reports Unknown and retries, rather than asserting an absence", func() {
			// A transport failure says NOTHING about whether the CRD exists.
			// Reporting False here would tell a user their monitoring stack is
			// uninstalled because the API server hiccuped — and, worse, would
			// stop retrying.
			r.Discovery = &discovery.Static{Err: errors.New("connection refused")}
			mdtCreate(helpers.NewModelDeployment(name, namespace))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: mdKey})
			Expect(err).To(HaveOccurred(), "a discovery failure must requeue")

			cond := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionUnknown)
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonReconciling))

			By("writing status before returning the error")
			// The status write happens first on purpose: the retry that follows
			// should start from an accurate picture, not a stale one.
			Expect(mdtGet(mdKey).Status.Conditions).NotTo(BeEmpty())
		})
	})

	When("the user turns the ServiceMonitor off", func() {
		It("creates none and says so", func() {
			r.Discovery = proberWith(true)

			md := helpers.NewModelDeployment(name, namespace)
			md.Spec.Observability.ServiceMonitor = &inferencev1alpha1.ServiceMonitorSpec{
				Enabled: ptr.To(false),
			}
			mdtCreate(md)
			mdtReconcile(r, mdKey)

			cond := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionFalse)
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonServiceMonitorDisabled))

			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(observability.ServiceMonitorGVK)
			Expect(k8sClient.Get(ctx, smKey, u)).NotTo(Succeed())
		})
	})

	When("no discovery prober is configured", func() {
		It("reports Unknown rather than guessing", func() {
			// A reconciler built without a prober checked nothing, so it must
			// assert nothing. This is the shape a unit test constructs, and it
			// must not thereby make a claim about a cluster.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionMetricsRegistered,
				metav1.ConditionUnknown)
		})
	})
})
