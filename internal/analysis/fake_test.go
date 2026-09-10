package analysis

import (
	"context"
	"testing"
)

func TestRepeatedStandingAnswersRejectUnmatchedQuery(t *testing.T) {
	p := NewScripted(Pass(1).For("decision_metric"))
	p.Repeat = true
	if _, err := p.Query(context.Background(), "observation_metric"); err == nil {
		t.Fatal("unmatched query must report exhaustion")
	}
	if sample, err := p.Query(context.Background(), "decision_metric"); err != nil || sample.Value != 1 {
		t.Fatalf("unmatched query broke standing answer: %+v, %v", sample, err)
	}
}

func TestRepeatedPositionalAnswersSkipStandingRules(t *testing.T) {
	p := NewScripted(Pass(10).For("standing"), Pass(1), Pass(2), Pass(20).For("other"))
	p.Repeat = true
	for _, want := range []float64{1, 2, 1, 2, 1} {
		sample, err := p.Query(context.Background(), "positional")
		if err != nil || sample.Value != want {
			t.Fatalf("got %+v, %v; want %v", sample, err, want)
		}
	}
}
