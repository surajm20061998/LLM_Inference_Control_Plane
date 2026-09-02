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

package v1alpha1

// Condition types reported on a ModelDeployment.
//
// Types are adjectives or past-tense verbs, per the Kubernetes API conventions
// — "Available", not "Deploying". Every condition carries a Reason, and every
// condition is reported from the first reconcile (as Unknown if nothing is
// known yet) so that a consumer can distinguish "not yet evaluated" from
// "evaluated and false".
const (
	// ConditionReady is the top-level rollup and the condition
	// `kubectl wait --for=condition=Ready` targets. True when the spec is
	// valid, the workload is available, and no rollout is in flight.
	ConditionReady = "Ready"

	// ConditionAvailable is true when at least the minimum number of replicas
	// are ready and serving.
	ConditionAvailable = "Available"

	// ConditionProgressing is true while a revision is being rolled out. It
	// goes False with reason ProgressDeadlineExceeded when a rollout stalls.
	ConditionProgressing = "Progressing"

	// ConditionModelReady is true when the model artifact resolved and the
	// engine reported itself healthy.
	ConditionModelReady = "ModelReady"

	// ConditionSpecValid is false for specifications the API server could not
	// reject on its own — combinations that CEL cannot express. A false
	// SpecValid is terminal: the controller will not retry until the spec
	// changes, because retrying cannot fix it.
	ConditionSpecValid = "SpecValid"
)

// Condition reasons.
//
// Reasons are single-word CamelCase programmatic identifiers. They are part of
// the API surface: automation matches on them, so they are as stable as field
// names.
const (
	// ReasonReconciling indicates work is in progress and no verdict is known.
	ReasonReconciling = "Reconciling"

	// ReasonMinimumReplicasAvailable indicates enough replicas are ready.
	ReasonMinimumReplicasAvailable = "MinimumReplicasAvailable"

	// ReasonMinimumReplicasUnavailable indicates too few replicas are ready.
	ReasonMinimumReplicasUnavailable = "MinimumReplicasUnavailable"

	// ReasonNewRevisionDetected indicates the pod template changed.
	ReasonNewRevisionDetected = "NewRevisionDetected"

	// ReasonRolloutInProgress indicates a rollout is advancing normally.
	ReasonRolloutInProgress = "RolloutInProgress"

	// ReasonRolloutComplete indicates every replica runs the target revision.
	ReasonRolloutComplete = "RolloutComplete"

	// ReasonProgressDeadlineExceeded indicates a rollout stopped making
	// progress within spec.rollout.progressDeadline.
	ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"

	// ReasonScaledToZero indicates the resource is intentionally at zero
	// replicas, which is not a failure.
	ReasonScaledToZero = "ScaledToZero"

	// ReasonModelResolved indicates the model artifact was located and mounted.
	ReasonModelResolved = "ModelResolved"

	// ReasonEngineUnhealthy indicates no replica is passing its health check.
	ReasonEngineUnhealthy = "EngineUnhealthy"

	// ReasonInvalidSpec indicates a specification the controller cannot act on.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonUnsupportedEngineOption indicates an option the selected engine
	// does not support.
	ReasonUnsupportedEngineOption = "UnsupportedEngineOption"

	// ReasonSpecAccepted indicates the specification validated successfully.
	ReasonSpecAccepted = "SpecAccepted"
)

// Kubernetes event reasons emitted by the controller.
const (
	EventReasonRevisionCreated = "RevisionCreated"
	EventReasonRolloutStarted  = "RolloutStarted"
	EventReasonRolloutComplete = "RolloutComplete"
	EventReasonInvalidSpec     = "InvalidSpec"
)
