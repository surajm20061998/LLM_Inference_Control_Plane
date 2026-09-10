package controller

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
)

func TestMetricScopeHandshake(t *testing.T) {
	now := time.Unix(1700000000, 0)
	md := &inferencev1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "chat"}}
	r := &ModelDeploymentReconciler{Clock: testingclock.NewFakePassiveClock(now)}
	for _, tc := range []struct {
		name               string
		primary, candidate analysis.Sample
		err                error
		ready              bool
	}{
		{"ready", analysis.Sample{Count: 1, Value: 3}, analysis.Sample{Count: 1, Value: 1}, nil, true},
		{"legacy primary", analysis.Sample{}, analysis.Sample{Count: 1, Value: 1}, nil, false},
		{"partial primary", analysis.Sample{Count: 1, Value: 2}, analysis.Sample{Count: 1, Value: 1}, nil, false},
		{"missing candidate", analysis.Sample{Count: 1, Value: 3}, analysis.Sample{}, nil, false},
		{"nan", analysis.Sample{Count: 1, Value: math.NaN()}, analysis.Sample{}, nil, false},
		{"backend error", analysis.Sample{}, analysis.Sample{}, errors.New("unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := analysis.ProviderFunc(func(_ context.Context, q string) (analysis.Sample, error) {
				for _, want := range []string{`namespace="team-a"`, `model_deployment="chat"`, "offset 60s", "@ 1700000000.000", "timestamp("} {
					if !strings.Contains(q, want) {
						t.Errorf("missing %q in %s", want, q)
					}
				}
				if strings.Contains(q, `variant="primary"`) {
					return tc.primary, tc.err
				}
				return tc.candidate, tc.err
			})
			ready, message := r.metricsScopeReady(context.Background(), p, md, time.Minute, 3, 1)
			if ready != tc.ready || message == "" {
				t.Fatalf("ready=%v message=%q", ready, message)
			}
		})
	}
}
