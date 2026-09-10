package canary

import (
	"testing"
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

func TestWholeRungReadinessRejectsStaleEvidence(t *testing.T) {
	plan := testPlan(func(p *Plan) { p.Weights = []int32{25, 50, 75}; p.Window = time.Minute })
	for _, tc := range []struct {
		name   string
		mutate func(*DeploymentObservation)
	}{
		{"one of three ready", func(d *DeploymentObservation) { d.ReadyReplicas = 1 }},
		{"one of three available", func(d *DeploymentObservation) { d.AvailableReplicas = 1 }},
		{"old replicas remain", func(d *DeploymentObservation) { d.UpdatedReplicas = 1 }},
		{"stale observed generation", func(d *DeploymentObservation) { d.ObservedGeneration = 0 }},
		{"old template revision", func(d *DeploymentObservation) { d.Revision = revStable }},
		{"old capacity", func(d *DeploymentObservation) { d.Replicas = 2 }},
		{"missing generation", func(d *DeploymentObservation) { d.Generation = 0 }},
		{"missing deployment", func(d *DeploymentObservation) { *d = DeploymentObservation{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := warmState(2)
			st.ReadinessTarget = readinessTarget(plan, 2, 4)
			st.LastAnalysis = t0
			st.SetCounters(1, 2, 3)
			dep := readyDeployment(plan, 2, 4)
			tc.mutate(&dep)
			in := Observe(Input{Now: t0.Add(time.Hour), Plan: plan, State: st,
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 4,
				Deployment: dep, Round: round(inferencev1alpha1.VerdictPass)})
			if DueForAnalysis(in.Now, plan, in.State) || in.Round != nil {
				t.Fatal("stale readiness permitted a provider call or verdict")
			}
			out := Next(in)
			if out.Action != ActionWait || out.State.Phase != PhaseWaiting || !out.State.AvailableSince.IsZero() ||
				!out.State.LastAnalysis.IsZero() || out.State.FailedChecks() != 1 || out.State.ConsecutiveErrors() != 0 || out.State.ConsecutiveInconclusive() != 0 {
				t.Fatalf("readiness loss did not invalidate pending evidence and preserve failures: %+v", out)
			}
		})
	}
}

func TestReadinessChangesRequireFreshWarmupAndFullWindow(t *testing.T) {
	plan := testPlan(func(p *Plan) { p.Window = 90 * time.Second })
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"rung change", func(in *Input) { in.State.Step = 1; in.Deployment = readyDeployment(plan, 1, 10) }},
		{"capacity change", func(in *Input) { in.TotalReplicas = 20; in.Deployment = readyDeployment(plan, 0, 20) }},
		{"deployment generation", func(in *Input) { in.Deployment.Generation = 2; in.Deployment.ObservedGeneration = 2 }},
		{"deployment replacement", func(in *Input) { in.Deployment.UID = "replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := analysed(plan, warmState(0), inferencev1alpha1.VerdictPass)
			tc.mutate(&in)
			in = Observe(in)
			if in.Round != nil || !in.State.AvailableSince.Equal(in.Now) || DueForAnalysis(in.Now, plan, in.State) {
				t.Fatalf("changed target reused evidence: %+v", in)
			}
			readyAt := in.Now
			// The input's value-only status is exactly what survives a restart.
			restored := in.State
			for _, elapsed := range []time.Duration{plan.InitialDelay, plan.InitialDelay + plan.Window - time.Nanosecond} {
				in.State, in.Now = restored, readyAt.Add(elapsed)
				in.Round = round(inferencev1alpha1.VerdictPass)
				if out := Next(in); out.Action != ActionWait {
					t.Fatalf("early verdict at %v: %+v", elapsed, out)
				}
			}
			in.Now = readyAt.Add(plan.InitialDelay + plan.Window)
			if !DueForAnalysis(in.Now, plan, restored) {
				t.Fatal("full fresh window not eligible")
			}
			out := Next(in)
			if out.Action != ActionAdvance {
				t.Fatalf("eligible rung did not advance: %+v", out)
			}
			if !out.State.AvailableSince.IsZero() || !out.State.LastAnalysis.IsZero() {
				t.Fatal("next rung inherited evidence")
			}
		})
	}
}

func TestReadinessFlapsCannotResetProgressDeadline(t *testing.T) {
	plan := testPlan(func(p *Plan) { p.Window = time.Minute; p.ProgressDeadline = 5 * time.Minute })
	in := analysed(plan, warmState(0), inferencev1alpha1.VerdictPass)
	in.Now = t0.Add(4 * time.Minute)
	in.Deployment.ReadyReplicas = 0
	in = Observe(in)
	in.Deployment = readyDeployment(plan, 0, 10)
	in.Now = t0.Add(5 * time.Minute)
	out := Next(in)
	if out.Action != ActionRollback || out.Reason != inferencev1alpha1.ReasonCanaryProgressDeadlineExceeded {
		t.Fatalf("readiness recovery reset the original deadline: %+v", out)
	}
}

func TestCandidateReplacementResetsMetricScopeHandshake(t *testing.T) {
	plan := testPlan(func(p *Plan) { p.Window = time.Minute })
	st := warmState(0)
	st.ReadinessTarget = readinessTarget(plan, 0, 10)
	st.MetricScopeReady = true
	in := Input{Now: t0.Add(time.Hour), Plan: plan, State: st,
		TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
		Deployment: readyDeployment(plan, 0, 10)}

	in = Observe(in)
	if !in.State.MetricScopeReady {
		t.Fatal("an unchanged candidate forgot its completed metric handshake")
	}
	in.Deployment.UID = "replacement"
	in = Observe(in)
	if in.State.MetricScopeReady {
		t.Fatal("a replacement candidate inherited another object's metric window")
	}
}

func TestSingleReplicaHoldCanRecoverCapacity(t *testing.T) {
	plan := testPlan(func(p *Plan) { p.ProgressDeadline = 5 * time.Minute })
	in := Input{Now: t0, Plan: plan, TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 1}
	out := Next(in)
	if out.Action != ActionWait || out.Primary != 1 || out.Canary != 0 || out.State.StableRevision != revStable {
		t.Fatalf("unexpected hold: %+v", out)
	}
	if !out.State.StartedAt.IsZero() || out.State.FailedRevision != "" {
		t.Fatalf("capacity hold consumed the rollout deadline or recorded a failure: %+v", out.State)
	}

	// Remaining below viable capacity beyond progressDeadline must still hold:
	// there is no candidate whose progress can time out.
	in.State, in.Now = out.State, t0.Add(30*time.Minute)
	out = Next(in)
	if out.Action != ActionWait || out.Reason != inferencev1alpha1.ReasonInsufficientReplicas ||
		!out.State.StartedAt.IsZero() || out.State.FailedRevision != "" {
		t.Fatalf("expired capacity hold was treated as a failed rollout: %+v", out)
	}

	// Capacity recovery starts a fresh candidate and only now begins its
	// progress deadline.
	in.State, in.TotalReplicas = out.State, 4
	out = Next(in)
	if out.Primary != 3 || out.Canary != 1 || out.Action != ActionStart {
		t.Fatalf("capacity recovery: %+v", out)
	}
	if out.State.StartedAt != in.Now || out.State.FailedRevision != "" {
		t.Fatal("capacity recovery did not begin a fresh progress deadline")
	}
}
