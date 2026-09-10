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

package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/fake"
	testingclient "k8s.io/client-go/testing"
	testclock "k8s.io/utils/clock/testing"
)

// The optional CRD these tests probe for. It is the real thing the operator
// looks up, not a placeholder: the point of the suite is that a cluster without
// prometheus-operator is handled correctly.
const (
	monitoringGV       = "monitoring.coreos.com/v1"
	kindServiceMonitor = "ServiceMonitor"
	pluralSMs          = "servicemonitors"
)

var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    kindServiceMonitor,
}

// countingDiscovery wraps a fake discovery client and counts group-version
// lookups, so the cache can be observed rather than assumed.
type countingDiscovery struct {
	discovery.DiscoveryInterface

	calls int
	err   error
	lists map[string]*metav1.APIResourceList
}

func (c *countingDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	list, ok := c.lists[gv]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gv}, "")
	}
	return list, nil
}

func newCounting(lists map[string]*metav1.APIResourceList) *countingDiscovery {
	return &countingDiscovery{
		DiscoveryInterface: &fake.FakeDiscovery{Fake: &testingclient.Fake{}},
		lists:              lists,
	}
}

func TestHasReportsPresentKind(t *testing.T) {
	dc := newCounting(map[string]*metav1.APIResourceList{
		monitoringGV: {
			GroupVersion: monitoringGV,
			APIResources: []metav1.APIResource{
				{Name: "prometheusrules", Kind: "PrometheusRule"},
				{Name: pluralSMs, Kind: kindServiceMonitor},
			},
		},
	})

	p := NewFor(dc)
	present, err := p.Has(context.Background(), serviceMonitorGVK)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !present {
		t.Fatal("ServiceMonitor reported absent though the group serves it")
	}
}

func TestHasReportsAbsentGroupWithoutError(t *testing.T) {
	// "This cluster does not run prometheus-operator" is an ANSWER, not a
	// failure. Returning an error would force the caller to string-match to
	// tell a missing CRD from a broken API server, which is exactly the
	// distinction that must not be got wrong.
	p := NewFor(newCounting(nil))

	present, err := p.Has(context.Background(), serviceMonitorGVK)
	if err != nil {
		t.Fatalf("a missing API group must not be an error, got: %v", err)
	}
	if present {
		t.Fatal("reported present against an empty discovery")
	}
}

func TestHasReportsMissingKindInPresentGroup(t *testing.T) {
	// The group exists (someone installed a partial CRD set) but the kind does
	// not. Distinct from a missing group and equally not an error.
	dc := newCounting(map[string]*metav1.APIResourceList{
		monitoringGV: {
			GroupVersion: monitoringGV,
			APIResources: []metav1.APIResource{{Name: "prometheusrules", Kind: "PrometheusRule"}},
		},
	})

	present, err := NewFor(dc).Has(context.Background(), serviceMonitorGVK)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if present {
		t.Fatal("reported ServiceMonitor present when only PrometheusRule is served")
	}
}

func TestHasSurfacesTransportErrors(t *testing.T) {
	// A transport or authorisation failure says NOTHING about whether the kind
	// exists. Reporting it as an absence would tell a user their monitoring
	// stack is uninstalled because the API server hiccuped.
	dc := newCounting(nil)
	dc.err = errors.New("connection refused")

	prober := NewFor(dc)
	if _, err := prober.Has(context.Background(), serviceMonitorGVK); err == nil {
		t.Fatal("expected a transport failure to be returned as an error")
	}

	// And it must not be cached: the next call re-queries, so a brief blip does
	// not become thirty seconds of a wrong answer.
	before := dc.calls
	_, _ = prober.Has(context.Background(), serviceMonitorGVK)
	if dc.calls == before {
		t.Fatal("a failed probe was cached; a transient error must not become a sticky absence")
	}
}

func TestHasCachesPositiveAnswers(t *testing.T) {
	dc := newCounting(map[string]*metav1.APIResourceList{
		monitoringGV: {
			GroupVersion: monitoringGV,
			APIResources: []metav1.APIResource{{Name: pluralSMs, Kind: kindServiceMonitor}},
		},
	})
	clk := testclock.NewFakePassiveClock(time.Unix(0, 0))
	prober := NewFor(dc, WithClock(clk))

	for range 5 {
		if _, err := prober.Has(context.Background(), serviceMonitorGVK); err != nil {
			t.Fatalf("Has: %v", err)
		}
	}
	if dc.calls != 1 {
		t.Fatalf("discovery called %d times, want 1: repeated probes must be served from cache", dc.calls)
	}

	// Past the positive TTL, it re-queries.
	clk.SetTime(clk.Now().Add(PresentTTL + time.Second))
	if _, err := prober.Has(context.Background(), serviceMonitorGVK); err != nil {
		t.Fatalf("Has: %v", err)
	}
	if dc.calls != 2 {
		t.Fatalf("discovery called %d times after the TTL expired, want 2", dc.calls)
	}
}

func TestNegativeAnswerExpiresQuickly(t *testing.T) {
	// The asymmetry is the whole design. A negative answer is the transient
	// state of a cluster whose monitoring stack is still installing, and
	// holding it means a ModelDeployment created during that window never gets
	// a ServiceMonitor.
	if AbsentTTL >= PresentTTL {
		t.Fatalf("AbsentTTL (%v) must be shorter than PresentTTL (%v)", AbsentTTL, PresentTTL)
	}

	dc := newCounting(nil)
	clk := testclock.NewFakePassiveClock(time.Unix(0, 0))
	prober := NewFor(dc, WithClock(clk))

	if _, err := prober.Has(context.Background(), serviceMonitorGVK); err != nil {
		t.Fatalf("Has: %v", err)
	}
	if dc.calls != 1 {
		t.Fatalf("calls = %d, want 1", dc.calls)
	}

	// Still cached just before expiry.
	clk.SetTime(clk.Now().Add(AbsentTTL - time.Second))
	_, _ = prober.Has(context.Background(), serviceMonitorGVK)
	if dc.calls != 1 {
		t.Fatalf("calls = %d before the absent TTL expired, want 1", dc.calls)
	}

	// Re-queried after it, so a CRD installed in the meantime is picked up.
	clk.SetTime(clk.Now().Add(2 * time.Second))
	_, _ = prober.Has(context.Background(), serviceMonitorGVK)
	if dc.calls != 2 {
		t.Fatalf("calls = %d after the absent TTL expired, want 2", dc.calls)
	}
}

func TestCRDInstalledLaterIsNoticed(t *testing.T) {
	// The scenario the TTL exists for: the operator boots thirty seconds before
	// the monitoring stack does.
	dc := newCounting(nil)
	clk := testclock.NewFakePassiveClock(time.Unix(0, 0))
	prober := NewFor(dc, WithClock(clk))

	present, _ := prober.Has(context.Background(), serviceMonitorGVK)
	if present {
		t.Fatal("reported present before installation")
	}

	dc.lists = map[string]*metav1.APIResourceList{
		monitoringGV: {
			GroupVersion: monitoringGV,
			APIResources: []metav1.APIResource{{Name: pluralSMs, Kind: kindServiceMonitor}},
		},
	}
	clk.SetTime(clk.Now().Add(AbsentTTL + time.Second))

	present, err := prober.Has(context.Background(), serviceMonitorGVK)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !present {
		t.Fatal("a CRD installed after start-up was never noticed; the operator would stay permanently blind")
	}
}

func TestInvalidateForcesRequery(t *testing.T) {
	dc := newCounting(nil)
	prober := NewFor(dc)

	_, _ = prober.Has(context.Background(), serviceMonitorGVK)
	prober.Invalidate(serviceMonitorGVK)
	_, _ = prober.Has(context.Background(), serviceMonitorGVK)

	if dc.calls != 2 {
		t.Fatalf("calls = %d, want 2 after Invalidate", dc.calls)
	}
}

func TestStaticProber(t *testing.T) {
	s := &Static{Present: map[schema.GroupVersionKind]bool{serviceMonitorGVK: true}}
	if ok, err := s.Has(context.Background(), serviceMonitorGVK); err != nil || !ok {
		t.Fatalf("Has = (%v, %v), want (true, nil)", ok, err)
	}

	boom := errors.New("discovery down")
	s = &Static{Err: boom}
	if _, err := s.Has(context.Background(), serviceMonitorGVK); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestAggregatedDiscoveryDoesNotHideFailuresAsAbsence(t *testing.T) {
	resource := schema.GroupResource{Group: serviceMonitorGVK.Group, Resource: pluralSMs}
	for _, tc := range []struct {
		name   string
		cause  error
		absent bool
	}{
		{"not found", apierrors.NewNotFound(resource, ""), true},
		{"forbidden", apierrors.NewForbidden(resource, "", errors.New("denied")), false},
		{"unavailable", apierrors.NewServiceUnavailable("discovery unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc := newCounting(nil)
			dc.err = &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
				serviceMonitorGVK.GroupVersion(): tc.cause,
			}}
			prober := NewFor(dc)
			for range 2 {
				present, err := prober.Has(context.Background(), serviceMonitorGVK)
				if present || (err == nil) != tc.absent {
					t.Fatalf("Has = (%v, %v), want absent=%v", present, err, tc.absent)
				}
			}
			wantCalls := 2
			if tc.absent {
				wantCalls = 1
			}
			if dc.calls != wantCalls {
				t.Fatalf("discovery calls = %d, want %d", dc.calls, wantCalls)
			}
		})
	}
}
