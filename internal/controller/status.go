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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/naming"
)

// observed is the cluster state one reconcile pass acted on.
//
// Collecting it into a struct keeps computeStatus a pure function of its
// inputs, which is what lets the whole status surface be table-tested without
// an API server.
type observed struct {
	// Deployment is the primary variant's Deployment, or nil if it does not
	// exist yet.
	Deployment *appsv1.Deployment
	// DesiredReplicas is the total desired across all variants.
	DesiredReplicas int32
	// Revision is the target revision hash for the current spec.
	Revision string
	// Selector is the serialized /scale label selector.
	Selector string
	// Endpoint is the in-cluster OpenAI-compatible base URL.
	Endpoint string
}

// computeStatus derives the full status from observed cluster state.
//
// It takes the previous status so that fields which are only advanced on
// success — notably lastGoodRevision — are carried forward rather than
// recomputed. It never reads the clock: condition timestamps are managed by
// apimeta.SetStatusCondition, which only stamps a new time when a condition
// actually transitions.
func computeStatus(
	md *inferencev1alpha1.ModelDeployment,
	obs observed,
	prev inferencev1alpha1.ModelDeploymentStatus,
) inferencev1alpha1.ModelDeploymentStatus {
	status := inferencev1alpha1.ModelDeploymentStatus{
		ObservedGeneration: md.Generation,
		Selector:           obs.Selector,
		Endpoint:           obs.Endpoint,
		StableRevision:     obs.Revision,
		LastGoodRevision:   prev.LastGoodRevision,
		Conditions:         prev.Conditions,
	}

	if obs.Deployment != nil {
		ds := obs.Deployment.Status
		status.Replicas = ds.Replicas
		status.ReadyReplicas = ds.ReadyReplicas
		status.UpdatedReplicas = ds.UpdatedReplicas
		status.AvailableReplicas = ds.AvailableReplicas
	}

	setSpecValid(&status, md.Generation)
	available := setAvailable(&status, obs, md.Generation)
	progressing, stalled := setProgressing(&status, obs, md.Generation)
	setModelReady(&status, available, md.Generation)

	// Only a revision that actually reached availability is a safe rollback
	// target. Recording it any earlier would let a broken revision become the
	// thing a future rollback reverts *to*.
	//
	// The !stalled term is not redundant with !progressing. A rollout that
	// blew its deadline reports Progressing=False, and if any old-revision
	// replica is still passing readiness it also reports Available=True — so
	// without this term a stalled rollout would bank the very revision that
	// failed, and an automatic rollback would then "recover" to the broken
	// version it was trying to escape.
	if available && !progressing && !stalled && obs.Revision != "" {
		status.LastGoodRevision = obs.Revision
	}

	setReady(&status, available, progressing, md.Generation)
	status.Phase = derivePhase(available, progressing, stalled, status.LastGoodRevision)

	return status
}

// setSpecValid marks the spec accepted. Rejection is handled on the error path
// in Reconcile, which sets this condition false and stops.
func setSpecValid(status *inferencev1alpha1.ModelDeploymentStatus, generation int64) {
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionSpecValid,
		Status:             metav1.ConditionTrue,
		Reason:             inferencev1alpha1.ReasonSpecAccepted,
		Message:            "Specification accepted",
		ObservedGeneration: generation,
	})
}

// setAvailable reports whether the workload is serving, and returns that answer.
//
// Zero replicas is deliberately Available: it is a state the user asked for,
// not a failure, and reporting it as unavailable would make every scale-to-zero
// look like an outage.
func setAvailable(status *inferencev1alpha1.ModelDeploymentStatus, obs observed, generation int64) bool {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionAvailable,
		ObservedGeneration: generation,
	}

	switch {
	case obs.DesiredReplicas == 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonScaledToZero
		cond.Message = "Scaled to zero replicas"
	case obs.Deployment == nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonReconciling
		cond.Message = "Waiting for the serving Deployment to be created"
	case status.ReadyReplicas > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasAvailable
		cond.Message = pluralReplicas(status.ReadyReplicas) + " ready"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasUnavailable
		cond.Message = "No replicas are ready"
	}

	setCondition(status, cond)
	return cond.Status == metav1.ConditionTrue
}

// setProgressing reports whether a rollout is in flight, and whether it has
// stalled past its deadline.
//
// The stall verdict is read from the child Deployment rather than recomputed.
// The Deployment controller already tracks progress deadlines correctly, and
// duplicating that logic here would mean two implementations that can disagree.
func setProgressing(
	status *inferencev1alpha1.ModelDeploymentStatus,
	obs observed,
	generation int64,
) (progressing, stalled bool) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionProgressing,
		ObservedGeneration: generation,
	}

	if obs.Deployment == nil {
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonNewRevisionDetected
		cond.Message = "Creating the serving Deployment"
		setCondition(status, cond)
		return true, false
	}

	if depCond := findDeploymentCondition(obs.Deployment, appsv1.DeploymentProgressing); depCond != nil {
		if depCond.Status == corev1.ConditionFalse &&
			depCond.Reason == inferencev1alpha1.ReasonProgressDeadlineExceeded {
			cond.Status = metav1.ConditionFalse
			cond.Reason = inferencev1alpha1.ReasonProgressDeadlineExceeded
			cond.Message = "Rollout has not progressed within spec.rollout.progressDeadline"
			setCondition(status, cond)
			return false, true
		}
	}

	rolloutComplete := status.UpdatedReplicas == obs.DesiredReplicas &&
		status.Replicas == obs.DesiredReplicas &&
		status.AvailableReplicas == obs.DesiredReplicas &&
		obs.Deployment.Status.ObservedGeneration >= obs.Deployment.Generation

	if rolloutComplete {
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonRolloutComplete
		cond.Message = "All replicas are running the target revision"
		setCondition(status, cond)
		return false, false
	}

	cond.Status = metav1.ConditionTrue
	cond.Reason = inferencev1alpha1.ReasonRolloutInProgress
	cond.Message = "Rolling out the target revision"
	setCondition(status, cond)
	return true, false
}

// setModelReady reports whether the engine loaded its model and reported
// healthy. Readiness is the signal: the engine's own /health returns 503 until
// the model is resident, so a ready replica is proof the model loaded.
func setModelReady(status *inferencev1alpha1.ModelDeploymentStatus, available bool, generation int64) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelReady,
		ObservedGeneration: generation,
	}
	if available {
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonModelResolved
		cond.Message = "Model loaded and the engine is serving"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonEngineUnhealthy
		cond.Message = "No engine replica has reported healthy"
	}
	setCondition(status, cond)
}

// setReady is the top-level rollup that `kubectl wait --for=condition=Ready`
// targets: valid spec, serving, and nothing in flight.
func setReady(status *inferencev1alpha1.ModelDeploymentStatus, available, progressing bool, generation int64) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionReady,
		ObservedGeneration: generation,
	}
	switch {
	case available && !progressing:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonRolloutComplete
		cond.Message = "ModelDeployment is serving the target revision"
	case progressing:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonRolloutInProgress
		cond.Message = "Rollout in progress"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasUnavailable
		cond.Message = "No replicas are ready"
	}
	setCondition(status, cond)
}

// derivePhase collapses the conditions into the coarse summary shown in
// `kubectl get`. Phase is for humans; automation should read conditions.
//
// It deliberately takes only the condition verdicts, not the observed state
// they were derived from: phase must be a function of the conditions alone, or
// `kubectl get` and the conditions can disagree about the same reconcile.
func derivePhase(available, progressing, stalled bool, lastGood string) inferencev1alpha1.Phase {
	switch {
	case stalled:
		return inferencev1alpha1.PhaseDegraded
	case available && !progressing:
		return inferencev1alpha1.PhaseAvailable
	case lastGood == "" && !available:
		// Nothing has ever served, so this is a first rollout rather than a
		// regression. Distinguishing the two matters: Pending is normal,
		// Degraded warrants a page.
		return inferencev1alpha1.PhasePending
	case progressing:
		return inferencev1alpha1.PhaseProgressing
	default:
		return inferencev1alpha1.PhaseDegraded
	}
}

// setInvalidSpec marks the spec unusable. Used on the terminal error path, so
// the rest of the status is left untouched.
func setInvalidSpec(status *inferencev1alpha1.ModelDeploymentStatus, generation int64, msg string) {
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionSpecValid,
		Status:             metav1.ConditionFalse,
		Reason:             inferencev1alpha1.ReasonInvalidSpec,
		Message:            msg,
		ObservedGeneration: generation,
	})
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             inferencev1alpha1.ReasonInvalidSpec,
		Message:            msg,
		ObservedGeneration: generation,
	})
	status.Phase = inferencev1alpha1.PhaseDegraded
}

// setCondition wraps apimeta.SetStatusCondition, which handles the
// lastTransitionTime contract: the timestamp only moves when the status
// actually changes, so an unchanged condition does not churn on every
// reconcile.
func setCondition(status *inferencev1alpha1.ModelDeploymentStatus, cond metav1.Condition) {
	apimeta.SetStatusCondition(&status.Conditions, cond)
}

// findDeploymentCondition returns a condition from a Deployment's status, or
// nil when absent.
func findDeploymentCondition(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == t {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// pluralReplicas renders a replica count for a human-readable message.
func pluralReplicas(n int32) string {
	if n == 1 {
		return "1 replica"
	}
	return strconv.Itoa(int(n)) + " replicas"
}

// endpointFor renders the published in-cluster endpoint.
func endpointFor(md *inferencev1alpha1.ModelDeployment) string {
	return naming.Endpoint(md.Name, md.Namespace)
}
