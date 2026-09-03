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

	// ConditionMetricsRegistered is true when this ModelDeployment's metrics
	// are wired up for collection: the shim is injected and a ServiceMonitor
	// exists for Prometheus to act on.
	//
	// It is reported separately from Ready on purpose. A ModelDeployment with
	// no metrics still serves traffic perfectly well, so failing Ready over it
	// would be wrong — but every rollout decision made about it would be made
	// blind, so silently succeeding would be worse. A distinct condition says
	// "serving, but not observable" in one place a human or a test can read.
	ConditionMetricsRegistered = "MetricsRegistered"

	// ConditionCanaryHealthy reports the verdict of the most recent analysis
	// round. It is Unknown when no canary is in flight, True while checks are
	// passing, and False once a rollback has been triggered.
	//
	// `kubectl wait --for=condition=CanaryHealthy=False` is the supported way
	// to block on a rollback in a test or a demo script — which is why the
	// condition exists at all rather than the information living only in
	// status.canary.
	ConditionCanaryHealthy = "CanaryHealthy"

	// ConditionTrafficRoutingReady reports whether the traffic split the canary
	// asked for is actually in place.
	//
	// Separate from CanaryHealthy because the two fail for unrelated reasons
	// and have unrelated remedies: analysis can be perfectly healthy while the
	// canary is receiving no traffic, and that combination — which looks like
	// success — is exactly the one worth surfacing.
	ConditionTrafficRoutingReady = "TrafficRoutingReady"

	// ConditionAutoscalingReady reports whether the built-in autoscaler is
	// able to act.
	//
	// It is False, not Unknown, when the autoscaler is configured but cannot
	// read its metric — because that state is actively dangerous in a way
	// "we haven't looked" is not: the fleet is frozen at whatever size it
	// happened to be when the metrics stopped, and load can climb underneath it
	// indefinitely with nothing to say so.
	ConditionAutoscalingReady = "AutoscalingReady"
)

// AllConditionTypes returns every condition type this operator sets.
//
// It exists so that the list has ONE definition. The alternative — a test that
// hard-codes the set, or worse a count — churns every time a condition is
// added, and it churned twice while Sprints 3 and 4 were being written. Worse
// than the churn, a hard-coded list silently stops asserting anything about a
// condition nobody remembered to add to it.
//
// Deriving the test's expectations from here inverts that: adding a condition
// type without ever setting it becomes an immediate test failure, which is the
// bug actually worth catching.
func AllConditionTypes() []string {
	return []string{
		ConditionReady,
		ConditionAvailable,
		ConditionProgressing,
		ConditionModelReady,
		ConditionSpecValid,
		ConditionMetricsRegistered,
		ConditionCanaryHealthy,
		ConditionTrafficRoutingReady,
		ConditionAutoscalingReady,
	}
}

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

	// ReasonModelLoading indicates the model is still being fetched or loaded.
	// It is distinct from EngineUnhealthy on purpose: for a real model these
	// are minutes apart and mean opposite things — one is the normal path, the
	// other warrants investigation — and a single reason covering both trains
	// operators to ignore it.
	ReasonModelLoading = "ModelLoading"

	// ReasonEngineUnhealthy indicates no replica is passing its health check
	// and no rollout is in flight to explain it.
	ReasonEngineUnhealthy = "EngineUnhealthy"

	// ReasonInvalidSpec indicates a specification the controller cannot act on.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonUnsupportedEngineOption indicates an option the selected engine
	// does not support.
	ReasonUnsupportedEngineOption = "UnsupportedEngineOption"

	// ReasonSpecAccepted indicates the specification validated successfully.
	ReasonSpecAccepted = "SpecAccepted"

	// ReasonMetricsRegistered indicates the shim is injected and a
	// ServiceMonitor was applied.
	ReasonMetricsRegistered = "MetricsRegistered"

	// ReasonPrometheusOperatorCRDsAbsent indicates monitoring.coreos.com is not
	// served by this cluster, so no ServiceMonitor could be created.
	//
	// This is a statement of fact about the cluster, not a failure of the
	// ModelDeployment: a cluster without prometheus-operator is a legitimate
	// configuration and the workload runs normally. Naming the cause precisely
	// is what stops it being diagnosed as a broken operator.
	ReasonPrometheusOperatorCRDsAbsent = "PrometheusOperatorCRDsAbsent"

	// ReasonServiceMonitorDisabled indicates the user turned scraping off in
	// spec.observability.
	ReasonServiceMonitorDisabled = "ServiceMonitorDisabled"

	// ReasonShimDisabled indicates spec.serving.shim.enabled is false, so no
	// llmcp_* metric is produced for this ModelDeployment at all.
	ReasonShimDisabled = "ShimDisabled"

	// Canary reasons.

	// ReasonCanaryNotRunning indicates no canary is in flight.
	ReasonCanaryNotRunning = "CanaryNotRunning"

	// ReasonCanaryProgressing indicates the canary is climbing its weight
	// ladder with checks passing.
	ReasonCanaryProgressing = "CanaryProgressing"

	// ReasonCanaryChecksPassed indicates the most recent analysis round passed
	// every metric.
	ReasonCanaryChecksPassed = "ChecksPassed"

	// ReasonCanaryChecksFailed indicates the most recent round failed at least
	// one metric, but the failure threshold has not been reached.
	ReasonCanaryChecksFailed = "ChecksFailed"

	// ReasonCanaryRolledBack indicates the failure threshold was reached and
	// the canary was torn down.
	ReasonCanaryRolledBack = "CanaryRolledBack"

	// ReasonCanaryPromoted indicates the canary became the primary.
	ReasonCanaryPromoted = "CanaryPromoted"

	// ReasonAnalysisError indicates the metric provider could not be queried.
	//
	// Kept strictly distinct from ChecksFailed. A Prometheus outage says
	// nothing about the canary, and a controller that could not tell the two
	// apart would let a monitoring failure cause a production rollback.
	ReasonAnalysisError = "AnalysisError"

	// ReasonAnalysisInconclusive indicates the queries succeeded but the
	// answers are unusable — most often too little traffic to judge.
	ReasonAnalysisInconclusive = "AnalysisInconclusive"

	// ReasonAwaitingApproval indicates the canary passed every check and is
	// waiting for llmcp.io/promote.
	ReasonAwaitingApproval = "AwaitingApproval"

	// ReasonCanaryAborted indicates an operator set llmcp.io/abort.
	ReasonCanaryAborted = "CanaryAborted"

	// ReasonWeightQuantized indicates the requested traffic weight is not
	// expressible with the available replica count, so a nearby one is in
	// force.
	ReasonWeightQuantized = "WeightQuantized"

	// ReasonTrafficSplit indicates the requested split is in place.
	ReasonTrafficSplit = "TrafficSplit"

	// ReasonNoLastGoodRevision indicates a canary was requested but no revision
	// has ever reached availability, so there is nothing to roll back to.
	ReasonNoLastGoodRevision = "NoLastGoodRevision"

	// ReasonInsufficientReplicas indicates spec.replicas is too small to divide
	// between two variants, so the revision is rolled out directly.
	//
	// Reported explicitly rather than allowed to become a stall. A canary
	// Deployment created with zero replicas never becomes available, and the
	// rollout would sit at "waiting for canary replicas" indefinitely with no
	// indication that it never can.
	ReasonInsufficientReplicas = "InsufficientReplicas"

	// Autoscaling reasons.

	// ReasonAutoscalingDisabled indicates spec.autoscaling is absent or Off, so
	// something outside this operator owns .spec.replicas.
	ReasonAutoscalingDisabled = "AutoscalingDisabled"

	// ReasonAutoscalingActive indicates the autoscaler has a usable metric and
	// is tracking it.
	ReasonAutoscalingActive = "AutoscalingActive"

	// ReasonAutoscalingNoMetrics indicates the metric could not be read, so the
	// fleet is frozen at its current size.
	//
	// Reported as a FALSE condition rather than Unknown. "We cannot measure the
	// load" is not a neutral state for an autoscaler: the replica count is
	// pinned wherever it happened to be, and demand can climb underneath it
	// indefinitely with nothing else to say so.
	ReasonAutoscalingNoMetrics = "AutoscalingNoMetrics"

	// ReasonAutoscalingConflict indicates an external HorizontalPodAutoscaler
	// also targets this ModelDeployment while mode is Builtin.
	//
	// Two controllers writing .spec.replicas from different signals do not
	// average out; they fight, and the fleet oscillates at the rate of the
	// faster one. Detecting it is worth the one cached List it costs, because
	// the symptom otherwise looks like a flapping workload rather than a
	// configuration mistake.
	ReasonAutoscalingConflict = "AutoscalingConflict"

	// ReasonScaledUp and ReasonScaledDown record the direction of the most
	// recent change.
	ReasonScaledUp   = "ScaledUp"
	ReasonScaledDown = "ScaledDown"
)

// Kubernetes event reasons emitted by the controller.
const (
	EventReasonRevisionCreated = "RevisionCreated"
	EventReasonRolloutStarted  = "RolloutStarted"
	EventReasonRolloutComplete = "RolloutComplete"
	EventReasonInvalidSpec     = "InvalidSpec"

	// EventReasonMetricsUnavailable is emitted once when metric collection
	// cannot be set up — most often because prometheus-operator is absent.
	EventReasonMetricsUnavailable = "MetricsUnavailable"

	// Canary lifecycle events. These are the audit trail: the canary dashboard
	// sources its annotations from them, and the annotated rollback moment is
	// the single most useful thing on it.
	EventReasonCanaryStarted    = "CanaryStarted"
	EventReasonCanaryAdvanced   = "CanaryAdvanced"
	EventReasonCanaryPromoted   = "CanaryPromoted"
	EventReasonCanaryRolledBack = "CanaryRolledBack"
	EventReasonCanaryPaused     = "CanaryPaused"
	EventReasonCanaryAborted    = "CanaryAborted"
	EventReasonAnalysisFailed   = "AnalysisFailed"
	EventReasonAnalysisError    = "AnalysisError"
	EventReasonWeightQuantized  = "WeightQuantized"
	EventReasonRolloutStalled   = "RolloutStalled"

	// Autoscaling events.
	EventReasonScaledUp             = "ScaledUp"
	EventReasonScaledDown           = "ScaledDown"
	EventReasonAutoscalingConflict  = "AutoscalingConflict"
	EventReasonAutoscalingNoMetrics = "AutoscalingNoMetrics"
)
