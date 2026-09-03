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
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// cuGeneration is the ModelDeployment generation used across these tests. It is
// deliberately not 1, so that a status which hard-codes a generation or leaves
// it at the zero value fails loudly.
const cuGeneration int64 = 7

// Hub coordinates shared by the huggingFace cases in this package. Real ones,
// so the fixtures name a reference that actually resolves.
const (
	testHFRepo = "unsloth/Qwen3-0.6B-GGUF"
	testHFFile = "Qwen3-0.6B-Q4_K_M.gguf"
)

// cuLastGood is the revision a previous reconcile banked as safe to roll back
// to; cuObserved's revision is the one being rolled out now. They differ so a
// test can tell "carried forward" from "just recorded".
const cuLastGood = "rev0001"

// cuStatusMD returns a ModelDeployment with a known generation. computeStatus
// only reads Generation from it, so nothing else needs filling in.
func cuStatusMD() *v1alpha1.ModelDeployment {
	return &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       cuMDName,
			Namespace:  cuNamespace,
			Generation: cuGeneration,
		},
	}
}

// cuDeployment builds a child Deployment whose status reports the given counts.
// generation/observedGeneration are equal by default, i.e. the Deployment
// controller has already seen the latest spec.
func cuDeployment(replicas, ready, updated, available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: cuMDName + "-primary", Namespace: cuNamespace, Generation: 2},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           replicas,
			ReadyReplicas:      ready,
			UpdatedReplicas:    updated,
			AvailableReplicas:  available,
		},
	}
}

// cuObserved is the standard observed state: a target revision, a serialized
// selector and the published endpoint.
func cuObserved(dep *appsv1.Deployment, desired int32) observed {
	return observed{
		Deployment:      dep,
		DesiredReplicas: desired,
		Revision:        "rev0002",
		Selector:        "llmcp.io/model-deployment=demo",
		Endpoint:        "http://demo.inference.svc:8080/v1",
	}
}

// cuCond returns the named condition, failing the test when it is absent.
// Every condition type this controller reports must be present from the very
// first reconcile, so that a consumer can tell "not yet evaluated" from
// "evaluated and false".
func cuCond(t *testing.T, status v1alpha1.ModelDeploymentStatus, condType string) metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(status.Conditions, condType)
	if c == nil {
		t.Fatalf("condition %q is missing; conditions present: %v", condType, cuCondTypes(status))
	}
	return *c
}

func cuCondTypes(status v1alpha1.ModelDeploymentStatus) []string {
	out := make([]string, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		out = append(out, c.Type)
	}
	return out
}

// cuAssertCondition checks a condition's status and reason in one line.
func cuAssertCondition(t *testing.T, status v1alpha1.ModelDeploymentStatus,
	condType string, want metav1.ConditionStatus, wantReason string) {
	t.Helper()
	c := cuCond(t, status, condType)
	if c.Status != want {
		t.Errorf("condition %s status = %q, want %q (reason %q, message %q)",
			condType, c.Status, want, c.Reason, c.Message)
	}
	if wantReason != "" && c.Reason != wantReason {
		t.Errorf("condition %s reason = %q, want %q", condType, c.Reason, wantReason)
	}
}

// cuAssertConditionsWellFormed enforces the invariants that hold in EVERY
// scenario, so each table case gets them for free.
//
// A condition with an empty Reason is REJECTED by the API server, which means
// the whole status write fails and the resource is left reporting stale state
// — a defect that is invisible in a unit test asserting only on the fields it
// cares about. observedGeneration must likewise match the ModelDeployment's
// generation, because anything asking "is the rollout finished?" compares those
// two numbers first and would otherwise read a stale answer as a fresh one.
//
// Every ModelDeployment in this file comes from cuStatusMD, so the expected
// generation is always cuGeneration rather than a parameter.
func cuAssertConditionsWellFormed(t *testing.T, status v1alpha1.ModelDeploymentStatus) {
	t.Helper()
	const generation = cuGeneration
	if len(status.Conditions) == 0 {
		t.Fatal("status carries no conditions at all")
	}
	for _, c := range status.Conditions {
		if c.Reason == "" {
			t.Errorf("condition %s has an empty Reason; the API server rejects such a condition and the "+
				"entire status write fails", c.Type)
		}
		if c.Status == "" {
			t.Errorf("condition %s has an empty Status", c.Type)
		}
		if c.ObservedGeneration != generation {
			t.Errorf("condition %s observedGeneration = %d, want %d", c.Type, c.ObservedGeneration, generation)
		}
	}
	if status.ObservedGeneration != generation {
		t.Errorf("status.observedGeneration = %d, want %d", status.ObservedGeneration, generation)
	}
}

func TestComputeStatusBeforeTheDeploymentExists(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	got := computeStatus(md, cuObserved(nil, 1), v1alpha1.ModelDeploymentStatus{})

	cuAssertConditionsWellFormed(t, got)
	cuAssertCondition(t, got, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonReconciling)
	cuAssertCondition(t, got, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonNewRevisionDetected)
	cuAssertCondition(t, got, v1alpha1.ConditionReady, metav1.ConditionFalse, "")
	// ModelLoading, not EngineUnhealthy: a rollout is in flight, so having no
	// ready replica yet is the normal path rather than a fault.
	cuAssertCondition(t, got, v1alpha1.ConditionModelReady, metav1.ConditionFalse, v1alpha1.ReasonModelLoading)

	// Nothing has ever served, so this is a first rollout rather than a
	// regression: Pending is normal and Degraded would warrant a page.
	if got.Phase != v1alpha1.PhasePending {
		t.Errorf("phase = %q, want %q", got.Phase, v1alpha1.PhasePending)
	}

	// Replica counts come from the Deployment's status; with no Deployment they
	// must stay zero rather than be invented, since an HPA divides by them.
	if got.Replicas != 0 || got.ReadyReplicas != 0 || got.UpdatedReplicas != 0 || got.AvailableReplicas != 0 {
		t.Errorf("replica counts = {replicas:%d ready:%d updated:%d available:%d}, want all zero",
			got.Replicas, got.ReadyReplicas, got.UpdatedReplicas, got.AvailableReplicas)
	}
	if got.LastGoodRevision != "" {
		t.Errorf("lastGoodRevision = %q, want empty before anything has ever served", got.LastGoodRevision)
	}
}

func TestComputeStatusWhenFullyRolledOut(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	obs := cuObserved(cuDeployment(3, 3, 3, 3), 3)
	got := computeStatus(md, obs, v1alpha1.ModelDeploymentStatus{LastGoodRevision: cuLastGood})

	cuAssertConditionsWellFormed(t, got)
	cuAssertCondition(t, got, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonMinimumReplicasAvailable)
	cuAssertCondition(t, got, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonRolloutComplete)
	cuAssertCondition(t, got, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonRolloutComplete)
	cuAssertCondition(t, got, v1alpha1.ConditionModelReady, metav1.ConditionTrue, v1alpha1.ReasonModelResolved)
	cuAssertCondition(t, got, v1alpha1.ConditionSpecValid, metav1.ConditionTrue, v1alpha1.ReasonSpecAccepted)

	if got.Phase != v1alpha1.PhaseAvailable {
		t.Errorf("phase = %q, want %q", got.Phase, v1alpha1.PhaseAvailable)
	}
	// The revision finished rolling out and is serving, so it is now a safe
	// rollback target and replaces the previous one.
	if got.LastGoodRevision != obs.Revision {
		t.Errorf("lastGoodRevision = %q, want %q once the revision is fully available",
			got.LastGoodRevision, obs.Revision)
	}
	if got.StableRevision != obs.Revision {
		t.Errorf("stableRevision = %q, want %q", got.StableRevision, obs.Revision)
	}
	if got.Replicas != 3 || got.ReadyReplicas != 3 || got.UpdatedReplicas != 3 || got.AvailableReplicas != 3 {
		t.Errorf("replica counts = {replicas:%d ready:%d updated:%d available:%d}, want all 3",
			got.Replicas, got.ReadyReplicas, got.UpdatedReplicas, got.AvailableReplicas)
	}
}

func TestComputeStatusMidRolloutDoesNotBankTheRevision(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	obs := cuObserved(cuDeployment(3, 1, 1, 1), 3)
	prev := v1alpha1.ModelDeploymentStatus{LastGoodRevision: cuLastGood}
	got := computeStatus(md, obs, prev)

	cuAssertConditionsWellFormed(t, got)

	// One replica IS serving traffic, so the workload is genuinely available
	// even though the rollout has not finished. Reporting Available=False here
	// would make every rolling update look like a partial outage.
	cuAssertCondition(t, got, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonMinimumReplicasAvailable)
	cuAssertCondition(t, got, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonRolloutInProgress)
	// Ready is the `kubectl wait` rollup and must stay False while a rollout is
	// in flight, or a CI pipeline would proceed against a half-updated fleet.
	cuAssertCondition(t, got, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonRolloutInProgress)

	if got.Phase != v1alpha1.PhaseProgressing {
		t.Errorf("phase = %q, want %q", got.Phase, v1alpha1.PhaseProgressing)
	}

	// The important assertion. lastGoodRevision is the target an automatic
	// rollback reverts TO. Banking the in-flight revision as "good" before it
	// has finished rolling out would make a broken revision the thing a future
	// rollback reverts to — the rollback would then restore the very failure it
	// was meant to escape.
	if got.LastGoodRevision != prev.LastGoodRevision {
		t.Errorf("lastGoodRevision = %q, want it left at %q: a revision that has not finished rolling out "+
			"is not a safe rollback target", got.LastGoodRevision, prev.LastGoodRevision)
	}
}

func TestComputeStatusWhenTheRolloutHasStalled(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	dep := cuDeployment(3, 1, 1, 1)
	// The stall verdict is read from the child Deployment rather than
	// recomputed here: the Deployment controller already tracks progress
	// deadlines, and a second implementation could disagree with the first.
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:    appsv1.DeploymentProgressing,
		Status:  corev1.ConditionFalse,
		Reason:  v1alpha1.ReasonProgressDeadlineExceeded,
		Message: "ReplicaSet \"demo-primary-abc\" has timed out progressing.",
	}}

	got := computeStatus(md, cuObserved(dep, 3), v1alpha1.ModelDeploymentStatus{LastGoodRevision: cuLastGood})

	cuAssertConditionsWellFormed(t, got)
	// The reason is propagated verbatim so that automation matching on
	// ProgressDeadlineExceeded works against this resource exactly as it does
	// against a Deployment.
	cuAssertCondition(t, got, v1alpha1.ConditionProgressing, metav1.ConditionFalse,
		v1alpha1.ReasonProgressDeadlineExceeded)

	if got.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("phase = %q, want %q for a stalled rollout", got.Phase, v1alpha1.PhaseDegraded)
	}

	// A stalled rollout must NOT become the rollback target.
	//
	// This is the subtle case: setProgressing returns (progressing=false,
	// stalled=true) when the child Deployment reports ProgressDeadlineExceeded,
	// and a stalled rollout that still has one old-revision replica passing
	// readiness also reports Available=True. So `available && !progressing`
	// alone is satisfied by a rollout that just FAILED, and banking it here
	// would leave auto-rollback "recovering" to the broken revision it was
	// trying to escape. rev0001 is the last revision that actually served, and
	// it must survive rev0002's failure.
	if got.LastGoodRevision != cuLastGood {
		t.Errorf("lastGoodRevision = %q, want %q: a stalled rollout must not overwrite the "+
			"last revision that actually served", got.LastGoodRevision, cuLastGood)
	}
}

func TestComputeStatusScaledToZeroIsNotAnOutage(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	got := computeStatus(md, cuObserved(cuDeployment(0, 0, 0, 0), 0), v1alpha1.ModelDeploymentStatus{})

	cuAssertConditionsWellFormed(t, got)
	// Zero replicas is a state the user (or their autoscaler) asked for, not a
	// failure. Reporting it as unavailable would fire an availability alert and
	// page somebody for a deliberate scale-down.
	cuAssertCondition(t, got, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonScaledToZero)

	if got.Phase != v1alpha1.PhaseAvailable {
		t.Errorf("phase = %q, want %q — a deliberate scale to zero is a steady state, not a fault",
			got.Phase, v1alpha1.PhaseAvailable)
	}
}

func TestDerivePhaseSeparatesFirstRolloutFromRegression(t *testing.T) {
	t.Parallel()

	// Pending and Degraded describe the same observable facts — nothing is
	// available — and are told apart solely by whether anything has EVER
	// served. That distinction is what separates "first rollout, be patient"
	// from "it was working and now it is not", i.e. a dashboard tile from a
	// page at 3am.
	tests := []struct {
		name        string
		available   bool
		progressing bool
		stalled     bool
		lastGood    string
		want        v1alpha1.Phase
	}{
		{
			name:        "nothing has ever served, so a slow first rollout is Pending",
			available:   false,
			progressing: true,
			lastGood:    "",
			want:        v1alpha1.PhasePending,
		},
		{
			name:      "nothing has ever served and nothing is in flight, still Pending",
			available: false,
			lastGood:  "",
			want:      v1alpha1.PhasePending,
		},
		{
			name:      "availability lost after a known-good revision is Degraded, never Pending",
			available: false,
			lastGood:  cuLastGood,
			want:      v1alpha1.PhaseDegraded,
		},
		{
			name:        "a known-good revision plus an in-flight rollout is Progressing",
			available:   false,
			progressing: true,
			lastGood:    cuLastGood,
			want:        v1alpha1.PhaseProgressing,
		},
		{
			name:      "serving with nothing in flight is Available",
			available: true,
			lastGood:  cuLastGood,
			want:      v1alpha1.PhaseAvailable,
		},
		{
			name:     "a stall outranks everything else",
			stalled:  true,
			lastGood: "",
			want:     v1alpha1.PhaseDegraded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := derivePhase(tc.available, tc.progressing, tc.stalled, tc.lastGood, rollout{})
			if got != tc.want {
				t.Errorf("derivePhase(available=%t, progressing=%t, stalled=%t, lastGood=%q) = %q, want %q",
					tc.available, tc.progressing, tc.stalled, tc.lastGood, got, tc.want)
			}
		})
	}

	t.Run("through computeStatus, a lost revision is not reported as Pending", func(t *testing.T) {
		t.Parallel()
		md := cuStatusMD()
		// Three pods exist and none is ready: availability was lost.
		got := computeStatus(md, cuObserved(cuDeployment(3, 0, 3, 0), 3),
			v1alpha1.ModelDeploymentStatus{LastGoodRevision: cuLastGood})
		cuAssertConditionsWellFormed(t, got)
		cuAssertCondition(t, got, v1alpha1.ConditionAvailable, metav1.ConditionFalse,
			v1alpha1.ReasonMinimumReplicasUnavailable)
		if got.Phase == v1alpha1.PhasePending {
			t.Errorf("phase = %q; a resource that has already served must never fall back to Pending, "+
				"which reads as 'be patient' rather than 'this regressed'", got.Phase)
		}
	})
}

func TestStatusConditionTransitionTimesDoNotChurn(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()
	obs := cuObserved(cuDeployment(3, 3, 3, 3), 3)

	first := computeStatus(md, obs, v1alpha1.ModelDeploymentStatus{})
	// Feeding the first result back in is exactly what a second reconcile with
	// unchanged cluster state does.
	second := computeStatus(md, obs, first)

	cuAssertConditionsWellFormed(t, second)
	if len(second.Conditions) != len(first.Conditions) {
		t.Fatalf("condition count changed between identical reconciles: %d then %d",
			len(first.Conditions), len(second.Conditions))
	}

	for _, want := range first.Conditions {
		got := cuCond(t, second, want.Type)
		// apimeta.SetStatusCondition only stamps a new lastTransitionTime when
		// the condition's status actually changes. If a timestamp moved on
		// every reconcile, every reconcile would produce a status write, the
		// write would wake the controller through its own watch, and the
		// controller would loop on itself forever writing nothing but clocks.
		if !got.LastTransitionTime.Equal(&want.LastTransitionTime) {
			t.Errorf("condition %s lastTransitionTime moved from %v to %v without an actual transition; "+
				"a timestamp that churns produces an endless status-write loop",
				want.Type, want.LastTransitionTime, got.LastTransitionTime)
		}
		if got.Status != want.Status {
			t.Errorf("condition %s status changed from %q to %q with identical inputs",
				want.Type, want.Status, got.Status)
		}
	}
}

func TestSetInvalidSpecMarksTheResourceDegraded(t *testing.T) {
	t.Parallel()

	status := v1alpha1.ModelDeploymentStatus{ObservedGeneration: cuGeneration}
	const msg = "llamacpp: .spec.model.source.image is required"
	setInvalidSpec(&status, cuGeneration, msg)

	cuAssertCondition(t, status, v1alpha1.ConditionSpecValid, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	// Ready is also driven false with the same reason, so that
	// `kubectl wait --for=condition=Ready` fails fast instead of blocking on a
	// spec no amount of waiting can fix.
	cuAssertCondition(t, status, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	cuAssertConditionsWellFormed(t, status)

	if status.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("phase = %q, want %q", status.Phase, v1alpha1.PhaseDegraded)
	}
	for _, condType := range []string{v1alpha1.ConditionSpecValid, v1alpha1.ConditionReady} {
		if got := cuCond(t, status, condType).Message; got != msg {
			t.Errorf("condition %s message = %q, want the validation error %q", condType, got, msg)
		}
	}
}

func TestStatusConditionsAccumulateWithoutDuplicating(t *testing.T) {
	t.Parallel()

	md := cuStatusMD()

	// Walk a realistic lifecycle: nothing yet, then rolling out, then serving,
	// then an invalid spec. Conditions are a listType=map keyed by type, so a
	// duplicate entry is rejected by the API server outright.
	status := computeStatus(md, cuObserved(nil, 3), v1alpha1.ModelDeploymentStatus{})
	status = computeStatus(md, cuObserved(cuDeployment(3, 1, 1, 1), 3), status)
	status = computeStatus(md, cuObserved(cuDeployment(3, 3, 3, 3), 3), status)
	status = computeStatus(md, cuObserved(cuDeployment(3, 3, 3, 3), 3), status)
	setInvalidSpec(&status, cuGeneration, "bad spec")

	seen := map[string]int{}
	for _, c := range status.Conditions {
		seen[c.Type]++
	}
	for condType, n := range seen {
		if n != 1 {
			t.Errorf("condition %s appears %d times; conditions are keyed by type and a duplicate is "+
				"rejected by the API server", condType, n)
		}
	}

	// The expected set is DERIVED from the API package rather than repeated
	// here. A hard-coded list has to be edited every time a condition is added
	// — it was, twice, while the canary work was being written — and each edit
	// is an opportunity to "fix" the test by loosening it. Deriving it means
	// declaring a condition type and never setting it fails immediately, which
	// is the bug worth catching.
	wantTypes := v1alpha1.AllConditionTypes()
	for _, condType := range wantTypes {
		if _, ok := seen[condType]; !ok {
			t.Errorf("condition %s is declared in AllConditionTypes but was never set "+
				"during a full lifecycle; present: %v", condType, cuCondTypes(status))
		}
	}
	for condType := range seen {
		if !slices.Contains(wantTypes, condType) {
			t.Errorf("condition %s is set but not declared in AllConditionTypes; "+
				"anything a consumer can observe belongs in that list", condType)
		}
	}
}

// TestModelReadySeparatesLoadingFromUnhealthy is the reason ModelReady has two
// distinct False reasons at all.
//
// With a real model these two states are minutes apart and call for opposite
// responses: one is "wait", the other is "investigate". Collapsing them — which
// is what a single "no replica is ready" reason does — is how a condition stops
// carrying information, because it spends the whole normal startup window
// claiming something is wrong.
func TestModelReadySeparatesLoadingFromUnhealthy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dep        *appsv1.Deployment
		desired    int32
		wantReason string
	}{
		{
			// A rollout is in flight and no replica is ready yet. Expected.
			name:       "mid rollout is loading",
			dep:        cuDeployment(2, 0, 2, 0),
			desired:    2,
			wantReason: v1alpha1.ReasonModelLoading,
		},
		{
			// Every replica exists and is up to date, yet none passes its
			// health check. Nothing is in flight to explain it, so this is the
			// state that warrants attention.
			name:       "settled but no replica healthy is unhealthy",
			dep:        cuDeploymentStalled(2, 0, 2, 0),
			desired:    2,
			wantReason: v1alpha1.ReasonEngineUnhealthy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := computeStatus(cuStatusMD(), cuObserved(tc.dep, tc.desired), v1alpha1.ModelDeploymentStatus{})
			cuAssertCondition(t, got, v1alpha1.ConditionModelReady, metav1.ConditionFalse, tc.wantReason)
		})
	}
}

// TestModelReadyMessageNamesTheModelSource proves the message points at what is
// actually being waited on. The two sources hang for different reasons — a
// registry or a wrong path for an image, egress or a rate limit for the Hub —
// and this message is usually the only thing distinguishing them.
func TestModelReadyMessageNamesTheModelSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		source   v1alpha1.ModelSourceSpec
		wantSubs []string
	}{
		{
			name: "image source names the image",
			source: v1alpha1.ModelSourceSpec{
				Image: &v1alpha1.ImageModelSource{Image: "localhost:5001/llmcp-model:qwen3", Path: "/weights/model.gguf"},
			},
			wantSubs: []string{"localhost:5001/llmcp-model:qwen3"},
		},
		{
			name: "hugging face source names the repository and file",
			source: v1alpha1.ModelSourceSpec{
				HuggingFace: &v1alpha1.HuggingFaceModelSource{
					Repo: testHFRepo,
					File: testHFFile,
				},
			},
			wantSubs: []string{testHFRepo, testHFFile},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md := cuStatusMD()
			md.Spec.Model.Source = tc.source

			// Mid-rollout: the state in which the loading message is reported.
			got := computeStatus(md, cuObserved(cuDeployment(2, 0, 2, 0), 2), v1alpha1.ModelDeploymentStatus{})

			msg := cuCond(t, got, v1alpha1.ConditionModelReady).Message
			for _, want := range tc.wantSubs {
				if !strings.Contains(msg, want) {
					t.Errorf("ModelReady message %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// cuDeploymentStalled is cuDeployment plus the Progressing=False/
// ProgressDeadlineExceeded condition the Deployment controller sets when a
// rollout stops making progress.
func cuDeploymentStalled(replicas, ready, updated, available int32) *appsv1.Deployment {
	dep := cuDeployment(replicas, ready, updated, available)
	dep.Status.Conditions = append(dep.Status.Conditions, appsv1.DeploymentCondition{
		Type:   appsv1.DeploymentProgressing,
		Status: corev1.ConditionFalse,
		Reason: v1alpha1.ReasonProgressDeadlineExceeded,
	})
	return dep
}
