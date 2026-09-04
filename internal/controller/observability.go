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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
)

// metricsVerdict is the outcome of wiring up metric collection for one
// ModelDeployment.
//
// It is a value rather than an error because most of its outcomes are not
// failures. "This cluster has no prometheus-operator" and "the user turned
// scraping off" are both correct, expected states in which the workload serves
// traffic normally — they just need saying out loud, because every rollout
// decision made about such a resource is made blind.
type metricsVerdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// The monitoring.coreos.com rules above are granted UNCONDITIONALLY, including
// on clusters where those CRDs are not installed.
//
// Granting a permission on a nonexistent API group is legal and inert — RBAC
// does not validate group names. Omitting it, on the other hand, produces a
// forbidden error at the exact moment someone installs prometheus-operator and
// expects metrics to start flowing, which is the worst possible time to
// discover a missing rule. The asymmetry is entirely one-sided, so the rule
// always ships.

// reconcileObservability applies the ServiceMonitor and reports what happened.
//
// It returns an error only for conditions worth RETRYING: a discovery failure
// that says nothing about whether the CRD exists, or an apply that was
// rejected. Everything else is a verdict, because a reconcile that failed over
// a missing optional CRD would stop the controller from managing the workload
// that CRD has nothing to do with.
func (r *ModelDeploymentReconciler) reconcileObservability(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) (metricsVerdict, error) {
	// Reconciled FIRST, and on EVERY path below.
	//
	// The SLO rules are not a sub-object of the ServiceMonitor. They are
	// generated from spec.observability.slo, they are applied to a different
	// kind, and they page on their own. Reaching them only after a successful
	// ServiceMonitor apply meant that turning scraping off — or simply running
	// on a cluster with no ServiceMonitor CRD — left the PrometheusRule frozen
	// at whatever it last was: still loaded, still alerting, and with nothing
	// in the controller ever revisiting it again.
	rulesMsg, rulesStatus, err := r.reconcilePrometheusRule(ctx, md)
	if err != nil {
		// rulesStatus carries the DISTINCTION the message cannot: a failed
		// probe means nothing was learned about the cluster (Unknown), while a
		// rejected apply means something definite went wrong (False). Reporting
		// a probe failure as False would tell a user their SLO rules are
		// missing because the API server hiccuped.
		return metricsVerdict{
			Status:  rulesStatus,
			Reason:  inferencev1alpha1.ReasonReconciling,
			Message: "Reconciling the PrometheusRule failed: " + err.Error(),
		}, err
	}

	if !md.Spec.Observability.ServiceMonitorEnabled() {
		// Disabling has to REMOVE the object, not merely stop creating it.
		// Turning the field off on a deployment that already had a
		// ServiceMonitor otherwise leaves the old one in place and Prometheus
		// still scraping, while the condition below says in plain words that
		// nothing does — a status that contradicts the cluster, and the
		// hardest kind of discrepancy to notice.
		if err := r.deleteObservabilityChild(
			ctx, md, observability.ServiceMonitorGVK, naming.ServiceMonitor(md.Name),
		); err != nil {
			return metricsVerdict{
				Status:  metav1.ConditionUnknown,
				Reason:  inferencev1alpha1.ReasonReconciling,
				Message: "Could not remove the disabled ServiceMonitor: " + err.Error(),
			}, err
		}
		return metricsVerdict{
			Status: metav1.ConditionFalse,
			Reason: inferencev1alpha1.ReasonServiceMonitorDisabled,
			Message: "spec.observability.serviceMonitor.enabled is false; no ServiceMonitor is created, " +
				"so nothing scrapes this deployment",
		}, nil
	}

	if r.Discovery == nil {
		// A reconciler constructed without a prober — a unit test, or a
		// deliberately offline mode. Reporting Unknown rather than guessing is
		// the honest answer: nothing was checked, so nothing is known.
		return metricsVerdict{
			Status:  metav1.ConditionUnknown,
			Reason:  inferencev1alpha1.ReasonReconciling,
			Message: "Metric collection has not been probed",
		}, nil
	}

	present, probeErr := r.Discovery.Has(ctx, observability.ServiceMonitorGVK)
	if probeErr != nil {
		// Discovery itself failed. This is NOT evidence that the CRD is absent,
		// and reporting it as such would tell a user their monitoring stack is
		// uninstalled because the API server hiccuped. Unknown, and retry.
		return metricsVerdict{
			Status:  metav1.ConditionUnknown,
			Reason:  inferencev1alpha1.ReasonReconciling,
			Message: "Could not determine whether the cluster serves ServiceMonitor: " + probeErr.Error(),
		}, fmt.Errorf("probing for %s: %w", observability.ServiceMonitorGVK.Kind, probeErr)
	}

	if !present {
		return metricsVerdict{
			Status: metav1.ConditionFalse,
			Reason: inferencev1alpha1.ReasonPrometheusOperatorCRDsAbsent,
			Message: "The cluster does not serve " + observability.MonitoringGroup +
				"/" + observability.MonitoringVersion + " " + observability.KindServiceMonitor +
				"; install prometheus-operator to collect metrics. The workload is serving normally.",
		}, nil
	}

	sm := observability.BuildServiceMonitor(observability.ServiceMonitorInput{
		Name:        md.Name,
		Namespace:   md.Namespace,
		OwnerRef:    ownerReferenceFor(md),
		Spec:        &md.Spec.Observability,
		ShimEnabled: md.Spec.Serving.ShimEnabled(),
	})

	if applyErr := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(sm),
		client.FieldOwner(naming.FieldManager),
		client.ForceOwnership,
	); err != nil {
		return metricsVerdict{
			Status:  metav1.ConditionFalse,
			Reason:  inferencev1alpha1.ReasonReconciling,
			Message: "Applying the ServiceMonitor failed: " + applyErr.Error(),
		}, fmt.Errorf("applying ServiceMonitor: %w", applyErr)
	}

	if !md.Spec.Serving.ShimEnabled() {
		// The ServiceMonitor exists and the engine's own metrics are scraped,
		// but the canonical llmcp_* SLIs are not being produced at all. That is
		// a materially different state from "fully wired", and calling it True
		// would let a canary be configured against metrics that will never
		// arrive — which stalls the rollout rather than failing it, and is far
		// harder to diagnose than a condition that says so.
		return metricsVerdict{
			Status: metav1.ConditionFalse,
			Reason: inferencev1alpha1.ReasonShimDisabled,
			Message: "spec.serving.shim.enabled is false, so no " +
				"llmcp_* metric is produced; canary analysis and the built-in autoscaler have no inputs",
		}, nil
	}

	return metricsVerdict{
		Status:  metav1.ConditionTrue,
		Reason:  inferencev1alpha1.ReasonMetricsRegistered,
		Message: "Shim injected and ServiceMonitor " + naming.ServiceMonitor(md.Name) + " applied" + rulesMsg,
	}, nil
}

// reconcilePrometheusRule applies the SLO recording rules and burn-rate alerts.
//
// It returns a message FRAGMENT rather than its own condition. Adding a
// SLORegistered condition would be the obvious move and is the wrong one: a
// consumer already has to read MetricsRegistered to know whether this
// deployment is observable at all, and splitting "is it scraped" from "are its
// SLOs defined" into two conditions means two things to check for one question.
// The rule's outcome belongs in the same sentence.
//
// The PrometheusRule kind is probed separately from ServiceMonitor. They ship
// together in practice, but "in practice" is doing a lot of work there — a
// cluster running a cut-down prometheus-operator install, or one mid-upgrade,
// can genuinely serve one and not the other, and assuming otherwise turns into
// a failed apply rather than a clear message.
//
// The second return value is the condition status to report if the third is
// non-nil: Unknown when the cluster could not be probed, False when an apply
// was rejected.
func (r *ModelDeploymentReconciler) reconcilePrometheusRule(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) (string, metav1.ConditionStatus, error) {
	if !md.Spec.Observability.PrometheusRuleEnabled() {
		// Same reason as the ServiceMonitor above: a PrometheusRule left behind
		// keeps its burn-rate alerts loaded and paging, on an SLO the spec says
		// is switched off.
		if err := r.deleteObservabilityChild(
			ctx, md, observability.PrometheusRuleGVK, naming.PrometheusRule(md.Name),
		); err != nil {
			return "", metav1.ConditionFalse, err
		}
		return "; SLO rules disabled", metav1.ConditionTrue, nil
	}

	if r.Discovery == nil {
		// No prober: a unit test, or a deliberately offline mode. Nothing was
		// checked, so nothing is claimed.
		return "; SLO rules not probed", metav1.ConditionUnknown, nil
	}

	present, err := r.Discovery.Has(ctx, observability.PrometheusRuleGVK)
	if err != nil {
		// A probe that failed says NOTHING about whether the CRD exists.
		return "", metav1.ConditionUnknown,
			fmt.Errorf("probing for %s: %w", observability.KindPrometheusRule, err)
	}
	if !present {
		return "; no PrometheusRule CRD in this cluster, so no SLO alerts",
			metav1.ConditionTrue, nil
	}

	rule := observability.BuildPrometheusRule(observability.PrometheusRuleInput{
		Name:      md.Name,
		Namespace: md.Namespace,
		Model:     md.Spec.Model.Name,
		OwnerRef:  ownerReferenceFor(md),
		Spec:      &md.Spec.Observability,
	})

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(rule),
		client.FieldOwner(naming.FieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", metav1.ConditionFalse, fmt.Errorf("applying PrometheusRule: %w", err)
	}

	msg := "; SLO rules in " + naming.PrometheusRule(md.Name)

	// A snapped threshold is reported rather than applied silently. Quietly
	// changing a number someone wrote in a spec is the kind of helpfulness that
	// costs an afternoon the first time an SLO does not mean what its author
	// believes it means.
	if snapped, changed := observability.SnappedThreshold(&md.Spec.Observability); changed {
		msg += fmt.Sprintf(
			" (the TTFT threshold was snapped up to %gs, the nearest histogram bucket boundary "+
				"at or above the configured value — a threshold between boundaries would select "+
				"no bucket and the alert could never fire)", snapped)
	}

	return msg, metav1.ConditionTrue, nil
}

// kindModelDeployment is this operator's own kind, as it appears in an owner
// reference and in a decoded manifest.
const kindModelDeployment = "ModelDeployment"

// ownerReferenceFor builds a typed controller owner reference.
//
// The apply-configuration variant used for the built-in children cannot be
// reused here: the ServiceMonitor is unstructured, so its owner reference has
// to be rendered as plain map content.
func ownerReferenceFor(md *inferencev1alpha1.ModelDeployment) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion:         inferencev1alpha1.GroupVersion.String(),
		Kind:               kindModelDeployment,
		Name:               md.Name,
		UID:                md.UID,
		Controller:         &t,
		BlockOwnerDeletion: &t,
	}
}

// deleteObservabilityChild removes a generated monitoring object that the spec
// has since disabled.
//
// Both classes of "it is not there" are tolerated, and they are different
// things: IsNotFound means the object is already gone, which is the goal;
// IsNoMatchError means the cluster does not serve the KIND at all, so there was
// never anything to delete and probing for it would be a wasted round trip.
// Neither is a reason to fail a reconcile of the workload itself.
func (r *ModelDeploymentReconciler) deleteObservabilityChild(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	gvk schema.GroupVersionKind,
	name string,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(md.Namespace)
	obj.SetName(name)

	err := r.Delete(ctx, obj)
	switch {
	case err == nil, apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil
	default:
		return fmt.Errorf("deleting disabled %s %s: %w", gvk.Kind, name, err)
	}
}
