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

// Package helpers holds fixtures shared by the operator's integration tests.
//
// # Why these exist
//
// envtest runs a real API server and a real etcd, and NOTHING else. There is no
// kubelet, no scheduler, and none of the built-in controllers — no Deployment
// controller, no ReplicaSet controller, no garbage collector. A Deployment
// created against envtest therefore never creates a ReplicaSet, never schedules
// a pod, and its `.status` stays at the zero value forever.
//
// Every assertion about availability consequently has to fake the child
// Deployment's status by hand. Doing that inline in each test is how a suite
// ends up with six subtly different definitions of "rolled out", one of which
// forgets observedGeneration and produces a test that passes for the wrong
// reason. It is written once, here.
package helpers

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// MarkDeploymentAvailable patches a Deployment's status so it appears fully
// rolled out with `ready` ready replicas.
//
// All four replica counters are set to the same value on purpose: the
// controller's rollout-complete test compares updatedReplicas, replicas and
// availableReplicas against the DESIRED count, so a fixture that moved only
// readyReplicas would report Available=True and Progressing=True forever.
func MarkDeploymentAvailable(ctx context.Context, c client.Client, key types.NamespacedName, ready int32) error {
	return patchDeploymentStatus(ctx, c, key, func(dep *appsv1.Deployment) {
		dep.Status.Replicas = ready
		dep.Status.ReadyReplicas = ready
		dep.Status.UpdatedReplicas = ready
		dep.Status.AvailableReplicas = ready
		dep.Status.UnavailableReplicas = 0

		setDeploymentCondition(dep, appsv1.DeploymentAvailable, corev1.ConditionTrue,
			"MinimumReplicasAvailable", "Deployment has minimum availability.")
		setDeploymentCondition(dep, appsv1.DeploymentProgressing, corev1.ConditionTrue,
			"NewReplicaSetAvailable", "ReplicaSet has successfully progressed.")
	})
}

// MarkDeploymentProgressing patches a Deployment's status to mid-rollout:
// `updated` pods carry the new revision and `ready` of them are serving.
func MarkDeploymentProgressing(
	ctx context.Context,
	c client.Client,
	key types.NamespacedName,
	ready, updated int32,
) error {
	return patchDeploymentStatus(ctx, c, key, func(dep *appsv1.Deployment) {
		dep.Status.Replicas = updated
		dep.Status.ReadyReplicas = ready
		dep.Status.UpdatedReplicas = updated
		dep.Status.AvailableReplicas = ready
		if updated > ready {
			dep.Status.UnavailableReplicas = updated - ready
		} else {
			dep.Status.UnavailableReplicas = 0
		}

		availability := corev1.ConditionFalse
		if ready > 0 {
			availability = corev1.ConditionTrue
		}
		setDeploymentCondition(dep, appsv1.DeploymentAvailable, availability,
			"MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
		setDeploymentCondition(dep, appsv1.DeploymentProgressing, corev1.ConditionTrue,
			"ReplicaSetUpdated", "ReplicaSet is progressing.")
	})
}

// MarkDeploymentStalled patches a Deployment's status to report
// Progressing=False with reason ProgressDeadlineExceeded.
//
// This is the exact shape the real Deployment controller produces when a
// rollout blows its progressDeadlineSeconds, and it is the only signal this
// operator reads to decide a rollout has failed — it deliberately does not
// re-derive the verdict itself.
func MarkDeploymentStalled(ctx context.Context, c client.Client, key types.NamespacedName) error {
	return patchDeploymentStatus(ctx, c, key, func(dep *appsv1.Deployment) {
		setDeploymentCondition(dep, appsv1.DeploymentProgressing, corev1.ConditionFalse,
			v1alpha1.ReasonProgressDeadlineExceeded,
			fmt.Sprintf("ReplicaSet %q has timed out progressing.", dep.Name))
	})
}

// patchDeploymentStatus applies mutate to a Deployment's status and writes it
// through the status subresource, retrying on conflict.
//
// It always stamps status.observedGeneration from metadata.generation. That is
// not incidental bookkeeping: computeStatus refuses to call a rollout complete
// while status.observedGeneration lags metadata.generation, so a fixture that
// skipped this would leave every "available" test stuck in Progressing.
func patchDeploymentStatus(
	ctx context.Context,
	c client.Client,
	key types.NamespacedName,
	mutate func(*appsv1.Deployment),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var dep appsv1.Deployment
		if err := c.Get(ctx, key, &dep); err != nil {
			return err
		}

		mutate(&dep)
		dep.Status.ObservedGeneration = dep.Generation

		return c.Status().Update(ctx, &dep)
	})
}

// setDeploymentCondition upserts a condition on a Deployment's status,
// preserving lastTransitionTime when the status value is unchanged.
func setDeploymentCondition(
	dep *appsv1.Deployment,
	condType appsv1.DeploymentConditionType,
	status corev1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	next := appsv1.DeploymentCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastUpdateTime:     now,
		LastTransitionTime: now,
	}

	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type != condType {
			continue
		}
		if dep.Status.Conditions[i].Status == status {
			next.LastTransitionTime = dep.Status.Conditions[i].LastTransitionTime
		}
		dep.Status.Conditions[i] = next
		return
	}

	dep.Status.Conditions = append(dep.Status.Conditions, next)
}
