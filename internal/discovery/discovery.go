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

// Package discovery answers one question: does this cluster serve a given API
// kind right now?
//
// # Why the operator needs to ask
//
// It creates prometheus-operator resources (ServiceMonitor, and later
// PrometheusRule) which are CRDs that may simply not be installed. A cluster
// without Prometheus is a legitimate configuration, and the operator must serve
// models on it normally rather than failing every reconcile against an API
// group that does not exist.
//
// # Why this is not the RESTMapper
//
// controller-runtime's manager already carries a RESTMapper, and it is tempting
// to probe with it. The trap is watches: a manager asked to WATCH a type whose
// CRD is absent fails at Start() with meta.NoKindMatchError, and — the part
// that bites — it does not recover when the CRD is installed later, because the
// mapper's discovery is not re-run for an established informer. That is why
// this operator never watches ServiceMonitors at all; it reads and applies
// them during reconciliation, with a periodic timer to repair drift.
//
// For plain client calls the dynamic RESTMapper does reload on a miss, so the
// remaining problem is only one of REPORTING: without a probe, the first
// evidence that Prometheus is absent is an obscure "no matches for kind" error
// attached to an unrelated reconcile. Probing first turns that into an accurate
// status condition.
//
// # Why results are cached with a TTL rather than resolved once
//
// Resolving once at start-up is how an operator ends up permanently convinced
// that Prometheus is missing because it happened to boot thirty seconds before
// the monitoring stack did. Discovery is also not free — it is a round trip per
// API group — so calling it on every reconcile of every ModelDeployment is not
// an option either. A short TTL on the negative answer and a long one on the
// positive answer resolves both: an installation is noticed within a minute,
// and a steady-state cluster pays almost nothing.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
)

// Cache lifetimes.
//
// The asymmetry is deliberate and is the whole design. A NEGATIVE answer is the
// one that must expire quickly: it is the transient state of a cluster whose
// monitoring stack is still installing, and holding it for long means a
// ModelDeployment created during that window never gets a ServiceMonitor until
// something else nudges it. A POSITIVE answer is close to permanent — CRDs are
// rarely uninstalled — and if one is wrong the apply simply fails and is
// retried, which is a self-correcting error rather than a silent absence.
const (
	// PresentTTL is how long a "yes, this kind exists" answer is reused.
	PresentTTL = 10 * time.Minute

	// AbsentTTL is how long a "no such kind" answer is reused.
	AbsentTTL = 30 * time.Second
)

// Interface is the probe surface the controller depends on. Declaring it here
// rather than taking a concrete type is what lets tests supply a fake without
// an API server.
type Interface interface {
	// Has reports whether the cluster currently serves gvk.
	Has(ctx context.Context, gvk schema.GroupVersionKind) (bool, error)
}

// Prober answers Has against a live cluster, with caching.
type Prober struct {
	client discovery.DiscoveryInterface
	clock  clock.PassiveClock

	presentTTL time.Duration
	absentTTL  time.Duration

	mu    sync.Mutex
	cache map[schema.GroupVersionKind]entry
}

// entry is one cached answer.
type entry struct {
	present   bool
	expiresAt time.Time
}

// Option configures a Prober.
type Option func(*Prober)

// WithClock injects a clock, so cache expiry is testable without sleeping.
func WithClock(c clock.PassiveClock) Option {
	return func(p *Prober) { p.clock = c }
}

// WithTTLs overrides the cache lifetimes.
func WithTTLs(present, absent time.Duration) Option {
	return func(p *Prober) { p.presentTTL, p.absentTTL = present, absent }
}

// New builds a Prober from a REST config.
func New(cfg *rest.Config, opts ...Option) (*Prober, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	return NewFor(dc, opts...), nil
}

// NewFor builds a Prober around an existing discovery client.
func NewFor(dc discovery.DiscoveryInterface, opts ...Option) *Prober {
	p := &Prober{
		client:     dc,
		clock:      clock.RealClock{},
		presentTTL: PresentTTL,
		absentTTL:  AbsentTTL,
		cache:      map[schema.GroupVersionKind]entry{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Has reports whether the cluster currently serves gvk.
//
// A missing API GROUP is reported as (false, nil), not as an error: "this
// cluster does not run prometheus-operator" is an answer, not a failure, and
// making the caller distinguish it from a genuine outage by string-matching an
// error would be exactly the kind of thing that gets it wrong. A transport
// failure, by contrast, IS returned as an error, so the caller can say
// "unknown" rather than assert an absence it did not verify.
func (p *Prober) Has(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	if cached, ok := p.lookup(gvk); ok {
		return cached, nil
	}

	gv := gvk.GroupVersion().String()
	resources, err := p.client.ServerResourcesForGroupVersion(gv)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err), isGroupDiscoveryFailure(err, gv):
		// The group/version is not served. Definitive, and cacheable.
		p.store(gvk, false)
		return false, nil
	default:
		// A transport or authorisation problem. Deliberately NOT cached: this
		// says nothing about whether the kind exists, and caching "unknown" as
		// "absent" is how a brief API-server blip turns into thirty seconds of
		// ModelDeployments being told Prometheus is uninstalled.
		return false, fmt.Errorf("discovering %s: %w", gv, err)
	}

	for i := range resources.APIResources {
		if resources.APIResources[i].Kind == gvk.Kind {
			p.store(gvk, true)
			return true, nil
		}
	}

	p.store(gvk, false)
	return false, nil
}

// Invalidate drops any cached answer for gvk, so the next Has re-queries.
func (p *Prober) Invalidate(gvk schema.GroupVersionKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, gvk)
}

// lookup returns a cached, unexpired answer.
func (p *Prober) lookup(gvk schema.GroupVersionKind) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.cache[gvk]
	if !ok || !p.clock.Now().Before(e.expiresAt) {
		return false, false
	}
	return e.present, true
}

// store caches an answer with the TTL appropriate to its polarity.
func (p *Prober) store(gvk schema.GroupVersionKind, present bool) {
	ttl := p.absentTTL
	if present {
		ttl = p.presentTTL
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[gvk] = entry{present: present, expiresAt: p.clock.Now().Add(ttl)}
}

// isGroupDiscoveryFailure reports a definitive absence of gv within an
// aggregated discovery error. Authorization and transport failures remain errors.
//
// The API server returns a 404 for an unknown group/version, but the aggregated
// discovery path wraps that in ErrGroupDiscoveryFailed instead — so checking
// only for NotFound works on some clusters and not others. Both are handled.
func isGroupDiscoveryFailure(err error, gv string) bool {
	var groupErr *discovery.ErrGroupDiscoveryFailed
	if !errors.As(err, &groupErr) {
		return false
	}
	for failed, cause := range groupErr.Groups {
		if failed.String() == gv {
			return apierrors.IsNotFound(cause)
		}
	}
	return false
}

// Static is a Prober that always gives the same answers. It exists for tests
// and for the "assume nothing is installed" path in offline tooling.
type Static struct {
	// Present lists the kinds this prober reports as available.
	Present map[schema.GroupVersionKind]bool

	// Err, when set, is returned from every call — the "discovery is broken"
	// case, which must produce an Unknown condition rather than a false
	// absence.
	Err error
}

var _ Interface = (*Static)(nil)

// Has implements Interface.
func (s *Static) Has(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	if s.Err != nil {
		return false, s.Err
	}
	return s.Present[gvk], nil
}
