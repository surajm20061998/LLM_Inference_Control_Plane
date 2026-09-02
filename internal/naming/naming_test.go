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

// These are regression tests in the truest sense. Most of what this package
// returns is baked into objects that already exist in real clusters: an
// immutable Deployment selector, a Server-Side Apply field manager recorded in
// managedFields, object names other controllers already resolved. A test here
// failing is not a style disagreement — it means the change under review cannot
// be rolled out without operator intervention in every cluster. The failure
// messages spell out that consequence, because whoever trips one will be
// reading it a year from now without any of this context.
package naming

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	inferencev1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
)

// dns1123LabelRe is the pattern the API server applies to every object name
// this package generates. Names that fail it are rejected at admission, so the
// operator would fail to create children at all.
var dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ianaSvcNameRe is the pattern the API server applies to Service and container
// port names (IANA_SVC_NAME): at most 15 characters, lowercase alphanumerics
// and dashes, no leading/trailing dash, and at least one letter.
var ianaSvcNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxNameLenExpected mirrors the package's own budget. It is duplicated here on
// purpose: if someone raises maxNameLen, these tests must fail so the change is
// reviewed against the 63-character label-value cap that motivated it, rather
// than silently following along.
const maxNameLenExpected = 63

// mdName is the ModelDeployment name used wherever these tests do not care
// about the specific input, so every expectation below is anchored to one name.
const mdName = "demo"

// nsName is the namespace used wherever these tests do not care which one.
const nsName = "prod"

// k8sLabelPrefix is the prefix Kubernetes reserves for the well-known
// application labels. It is spelled out here rather than derived from the
// constants under test, so that a typo in one of them is caught.
const k8sLabelPrefix = "app.kubernetes.io/"

// The child Deployment names mdName must map to. Spelled out in full rather
// than composed from mdName, so that a change to how the suffix is joined
// fails here — these exact strings already name objects in live clusters.
const (
	wantPrimaryName = "demo-primary"
	wantCanaryName  = "demo-canary"
)

// Subtest labels for the name-producing functions. Naming them once means a
// renamed function is renamed in one place rather than in four tables.
const (
	fnPrimaryDeployment = "PrimaryDeployment"
	fnCanaryDeployment  = "CanaryDeployment"
	fnService           = "Service"
)

// longName builds a name of exactly n characters ending in tail, so tests can
// construct inputs that straddle the truncation boundary precisely.
func longName(n int, tail string) string {
	if len(tail) > n {
		panic("tail longer than requested name")
	}
	return strings.Repeat("a", n-len(tail)) + tail
}

func TestSelectorLabelsIsExactlyTwoLabelsForever(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mdName  string
		variant inferencev1alpha1.Variant
	}{
		{name: "primary", mdName: mdName, variant: inferencev1alpha1.VariantPrimary},
		{name: "canary", mdName: mdName, variant: inferencev1alpha1.VariantCanary},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := SelectorLabels(tc.mdName, tc.variant)

			// spec.selector is immutable. Every extra key here is a key that
			// existing Deployments do not have and can never be given.
			if len(got) != 2 {
				t.Errorf("SelectorLabels(%q, %q) has %d keys, want exactly 2: this map becomes the child Deployment's spec.selector, which Kubernetes treats as IMMUTABLE — adding or removing a key requires deleting and recreating every Deployment in every cluster. Got %v",
					tc.mdName, tc.variant, len(got), got)
			}

			want := map[string]string{
				LabelModelDeployment: tc.mdName,
				LabelVariant:         string(tc.variant),
			}
			for k, wantV := range want {
				gotV, ok := got[k]
				if !ok {
					t.Errorf("SelectorLabels(%q, %q) is missing key %q: dropping a selector key orphans every running Deployment, whose immutable selector still requires it — the operator would create a second Deployment alongside the old one and double the served replicas",
						tc.mdName, tc.variant, k)
					continue
				}
				if gotV != wantV {
					t.Errorf("SelectorLabels(%q, %q)[%q] = %q, want %q: the selector value must match the pod labels exactly, or the Deployment adopts zero pods and scales up forever",
						tc.mdName, tc.variant, k, gotV, wantV)
				}
			}
			for k := range got {
				if _, expected := want[k]; !expected {
					t.Errorf("SelectorLabels(%q, %q) contains unexpected key %q: spec.selector is immutable, so any new key here is a breaking change requiring recreation of every Deployment in every cluster",
						tc.mdName, tc.variant, k)
				}
			}

			// The revision hash changes on every spec edit. In an immutable
			// selector that turns each rollout into a delete-and-recreate,
			// which means downtime on every single spec change.
			if _, present := got[LabelRevision]; present {
				t.Errorf("SelectorLabels(%q, %q) contains %q: the revision hash must NEVER appear in spec.selector — it changes on every spec change, and because the selector is immutable that would turn every rollout into a delete-and-recreate of the Deployment (and therefore full downtime)",
					tc.mdName, tc.variant, LabelRevision)
			}
		})
	}
}

func TestPodLabelsIsSupersetOfSelectorLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		mdName       string
		variant      inferencev1alpha1.Variant
		revision     string
		wantRevision bool
	}{
		{name: "primary with revision", mdName: mdName, variant: inferencev1alpha1.VariantPrimary, revision: "abc123", wantRevision: true},
		{name: "canary with revision", mdName: mdName, variant: inferencev1alpha1.VariantCanary, revision: "def456", wantRevision: true},
		{name: "primary without revision", mdName: mdName, variant: inferencev1alpha1.VariantPrimary, revision: "", wantRevision: false},
		{name: "canary without revision", mdName: mdName, variant: inferencev1alpha1.VariantCanary, revision: "", wantRevision: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := PodLabels(tc.mdName, tc.variant, tc.revision)
			selector := SelectorLabels(tc.mdName, tc.variant)

			// A pod that does not match its own Deployment's selector is never
			// adopted: the Deployment sees zero ready replicas, scales up again,
			// and the rollout never completes.
			for k, v := range selector {
				got, ok := pod[k]
				if !ok {
					t.Errorf("PodLabels(%q, %q, %q) is missing selector key %q: a pod that does not carry its own Deployment's selector labels is never counted by that Deployment, so the rollout never converges and replicas are created without limit",
						tc.mdName, tc.variant, tc.revision, k)
					continue
				}
				if got != v {
					t.Errorf("PodLabels(%q, %q, %q)[%q] = %q, but SelectorLabels has %q: a mismatch means the pod does not match its own Deployment's selector and is never counted as ready",
						tc.mdName, tc.variant, tc.revision, k, got, v)
				}
			}

			// The revision label is what lets dashboards and analysis attribute
			// a pod to the spec it was created from.
			gotRev, present := pod[LabelRevision]
			switch {
			case tc.wantRevision && !present:
				t.Errorf("PodLabels(%q, %q, %q) is missing %q: without the revision label, no query can attribute a pod to the revision it was rolled out from, and canary analysis cannot separate old pods from new",
					tc.mdName, tc.variant, tc.revision, LabelRevision)
			case tc.wantRevision && gotRev != tc.revision:
				t.Errorf("PodLabels(%q, %q, %q)[%q] = %q, want %q: the revision label must be the revision it was given, or pods are attributed to the wrong revision",
					tc.mdName, tc.variant, tc.revision, LabelRevision, gotRev, tc.revision)
			case !tc.wantRevision && present:
				t.Errorf("PodLabels(%q, %q, %q) set %q to %q for an empty revision: an empty label value is not the same as an absent label, and it would make every pod match a query for revision \"\"",
					tc.mdName, tc.variant, tc.revision, LabelRevision, gotRev)
			}
		})
	}
}

func TestServiceSelectorSpansBothVariants(t *testing.T) {
	t.Parallel()

	got := ServiceSelector(mdName)

	if len(got) != 1 {
		t.Errorf("ServiceSelector(%q) has %d keys, want exactly 1: this selector must span BOTH variants. Any additional key narrows it to one variant, excluding canary pods from the Service endpoints — 100%% of traffic would silently go to primary and every canary comparison would be measuring the same workload twice. Got %v",
			mdName, len(got), got)
	}
	if got[LabelModelDeployment] != mdName {
		t.Errorf("ServiceSelector(%q)[%q] = %q, want %q: the Service would select no pods at all and the endpoint would return connection refused",
			mdName, LabelModelDeployment, got[LabelModelDeployment], mdName)
	}
	if _, present := got[LabelVariant]; present {
		t.Errorf("ServiceSelector(%q) contains %q: a variant key here excludes canary pods from the Service's endpoints, silently sending 100%% of traffic to primary and making any future canary comparison meaningless. It is also serialized as status.selector, which an HPA uses to average pod metrics — a narrowed selector makes the HPA divide by the wrong pod count",
			mdName, LabelVariant)
	}
}

func TestVariantServiceSelectorEqualsSelectorLabels(t *testing.T) {
	t.Parallel()

	variants := []inferencev1alpha1.Variant{
		inferencev1alpha1.VariantPrimary,
		inferencev1alpha1.VariantCanary,
	}

	for _, variant := range variants {
		t.Run(string(variant), func(t *testing.T) {
			t.Parallel()

			got := VariantServiceSelector(mdName, variant)
			want := SelectorLabels(mdName, variant)

			if len(got) != len(want) {
				t.Fatalf("VariantServiceSelector(%q, %q) has %d keys, SelectorLabels has %d: the per-variant Service must select exactly the pods of that variant's Deployment, or metric analysis scrapes the wrong fleet. Got %v, want %v",
					mdName, variant, len(got), len(want), got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("VariantServiceSelector(%q, %q)[%q] = %q, want %q: the per-variant Service must select exactly that variant's pods, or canary metrics are collected from primary pods and the promotion decision is made on the wrong data",
						mdName, variant, k, got[k], v)
				}
			}
		})
	}
}

func TestCommonLabelsCarriesRecommendedLabels(t *testing.T) {
	t.Parallel()

	got := CommonLabels(mdName)

	want := map[string]string{
		LabelName:            AppName,
		LabelInstance:        mdName,
		LabelManagedBy:       ManagedBy,
		LabelComponent:       ComponentInferenceServer,
		LabelModelDeployment: mdName,
	}

	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("CommonLabels(%q) is missing %q: the app.kubernetes.io/* recommended labels are what operators, dashboards and `kubectl get -l` use to find every object this controller owns; a missing one makes owned objects invisible to those queries",
				mdName, k)
			continue
		}
		if gotV != wantV {
			t.Errorf("CommonLabels(%q)[%q] = %q, want %q: tooling selects on these exact values, so changing one orphans every object already labelled with the old value",
				mdName, k, gotV, wantV)
		}
	}
	for k := range got {
		if _, expected := want[k]; !expected {
			t.Errorf("CommonLabels(%q) contains unexpected key %q: this set may grow, but every addition lands on every owned object — confirm the key is a valid qualified name and that no selector depends on the old set",
				mdName, k)
		}
	}
}

func TestCommonLabelsReturnsFreshMapEachCall(t *testing.T) {
	t.Parallel()

	first := CommonLabels(mdName)
	// Several callers add keys to the returned map (component overrides, extra
	// metadata). If the map were shared — a package-level var, say — those
	// additions would leak onto every subsequent object.
	first["mutated-by-caller"] = "leak"
	delete(first, LabelComponent)

	second := CommonLabels(mdName)

	if _, leaked := second["mutated-by-caller"]; leaked {
		t.Errorf("CommonLabels(%q) returned a map still carrying a key added by a previous caller: callers mutate this result to add per-object keys, so a shared map leaks one object's labels onto every other object the controller applies — including into label selectors that then match the wrong pods",
			mdName)
	}
	if _, ok := second[LabelComponent]; !ok {
		t.Errorf("CommonLabels(%q) lost %q after a previous caller deleted it: the returned map must be freshly allocated on every call, or one caller's edits silently rewrite the labels of every object applied afterwards",
			mdName, LabelComponent)
	}

	if fmt.Sprintf("%p", first) == fmt.Sprintf("%p", second) {
		t.Errorf("CommonLabels(%q) returned the same map on two calls: callers mutate the result, so every object would end up sharing one label map",
			mdName)
	}
}

func TestPodLabelsDoesNotAliasCommonOrSelectorLabels(t *testing.T) {
	t.Parallel()

	const revision = "abc123"
	variant := inferencev1alpha1.VariantCanary

	pod := PodLabels(mdName, variant, revision)
	// PodLabels is built from CommonLabels and SelectorLabels. If it handed back
	// either of their internals, mutating a pod's labels would rewrite the
	// Deployment's immutable selector on the very next call.
	pod["mutated-by-caller"] = "leak"
	pod[LabelVariant] = "tampered"
	delete(pod, LabelRevision)

	if got := SelectorLabels(mdName, variant); got[LabelVariant] != string(variant) {
		t.Errorf("SelectorLabels(%q, %q)[%q] = %q after a caller mutated a PodLabels result: PodLabels must not alias the selector map, or editing pod labels silently rewrites spec.selector — which is immutable, so the resulting apply is rejected and the Deployment can never be updated again",
			mdName, variant, LabelVariant, got[LabelVariant])
	}
	if got := CommonLabels(mdName); len(got) != 5 {
		t.Errorf("CommonLabels(%q) has %d keys after a caller mutated a PodLabels result (want 5, %v): PodLabels must copy, not alias, or pod-only labels such as the revision hash leak onto every other owned object",
			mdName, len(got), got)
	}

	fresh := PodLabels(mdName, variant, revision)
	if _, leaked := fresh["mutated-by-caller"]; leaked {
		t.Errorf("PodLabels(%q, %q, %q) returned a map still carrying a previous caller's key: pods of one ModelDeployment would be stamped with another's labels and adopted by the wrong Deployment",
			mdName, variant, revision)
	}
	if fresh[LabelRevision] != revision {
		t.Errorf("PodLabels(%q, %q, %q)[%q] = %q after a previous caller deleted it: each call must return a freshly built map",
			mdName, variant, revision, LabelRevision, fresh[LabelRevision])
	}
}

func TestShortNamesGetPlainSuffixes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: fnPrimaryDeployment, got: PrimaryDeployment(mdName), want: wantPrimaryName},
		{name: fnCanaryDeployment, got: CanaryDeployment(mdName), want: wantCanaryName},
		{name: fnService, got: Service(mdName), want: mdName},
		{name: "ControllerRevision", got: ControllerRevision(mdName, "abc123"), want: "demo-abc123"},
		{name: "DeploymentFor primary", got: DeploymentFor(mdName, inferencev1alpha1.VariantPrimary), want: wantPrimaryName},
		{name: "DeploymentFor canary", got: DeploymentFor(mdName, inferencev1alpha1.VariantCanary), want: wantCanaryName},
		{name: "DeploymentFor unknown variant falls back to primary", got: DeploymentFor(mdName, inferencev1alpha1.Variant("bogus")), want: wantPrimaryName},
		{name: "DeploymentFor empty variant falls back to primary", got: DeploymentFor(mdName, inferencev1alpha1.Variant("")), want: wantPrimaryName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q: these names are already recorded in live clusters and in other objects' references. Changing one orphans the existing object — the controller creates a second alongside it rather than updating it, and the old one is never garbage collected",
					tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestServiceNameEqualsModelDeploymentName(t *testing.T) {
	t.Parallel()

	// The whole point of the Service name is that users can predict the
	// in-cluster endpoint without looking anything up.
	if got := Service(mdName); got != mdName {
		t.Errorf("Service(%q) = %q, want %q: the Service name must equal the ModelDeployment name so that http://<name>.<ns>.svc:8080/v1 is predictable — any other name breaks every hard-coded client URL",
			mdName, got, mdName)
	}
}

// generatedNames is every name-producing function under one roof, so the
// truncation properties below are checked against all of them at once and a new
// function cannot quietly skip them.
type generatedName struct {
	fn         string
	value      string
	wantSuffix string // suffix that must survive truncation intact ("" = none)
	// isPrefix marks values that are deliberately not standalone object names.
	isPrefix bool
}

func namesFor(mdName string) []generatedName {
	return []generatedName{
		{fn: fnPrimaryDeployment, value: PrimaryDeployment(mdName), wantSuffix: "-primary"},
		{fn: fnCanaryDeployment, value: CanaryDeployment(mdName), wantSuffix: "-canary"},
		{fn: "DeploymentFor/primary", value: DeploymentFor(mdName, inferencev1alpha1.VariantPrimary), wantSuffix: "-primary"},
		{fn: "DeploymentFor/canary", value: DeploymentFor(mdName, inferencev1alpha1.VariantCanary), wantSuffix: "-canary"},
		{fn: fnService, value: Service(mdName)},
		{fn: "ControllerRevision", value: ControllerRevision(mdName, "abc123"), wantSuffix: "-abc123"},
	}
}

func TestGeneratedNamesRespectTheSixtyThreeCharacterLimit(t *testing.T) {
	t.Parallel()

	inputs := []struct {
		name   string
		mdName string
	}{
		{name: "short name", mdName: mdName},
		{name: "name one character under the limit", mdName: longName(maxNameLenExpected-1, "x")},
		{name: "name exactly at the limit", mdName: longName(maxNameLenExpected, "x")},
		{name: "name one character over the limit", mdName: longName(maxNameLenExpected+1, "x")},
		{name: "name well over the limit", mdName: longName(80, "x")},
		{name: "name far over the limit", mdName: longName(200, "x")},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			t.Parallel()

			for _, gen := range namesFor(in.mdName) {
				// A name over 63 characters is rejected outright when it is used
				// as a label value (app.kubernetes.io/instance), so the object is
				// never created and the ModelDeployment never becomes ready.
				if len(gen.value) > maxNameLenExpected {
					t.Errorf("%s(len %d input) = %q (%d chars), want at most %d: these names become label VALUES via app.kubernetes.io/instance, which the API server caps at 63 — an over-long name is rejected at admission and the child object is never created at all",
						gen.fn, len(in.mdName), gen.value, len(gen.value), maxNameLenExpected)
				}

				if gen.isPrefix {
					// A prefix is concatenated with a revision hash before use,
					// so it legitimately ends in "-". Validity is asserted on
					// ControllerRevision, which is the real object name.
					continue
				}

				if !dns1123LabelRe.MatchString(gen.value) {
					t.Errorf("%s(len %d input) = %q, which is not a valid DNS-1123 label: the API server rejects it, so the child object is never created and the ModelDeployment stays unready with an opaque validation error",
						gen.fn, len(in.mdName), gen.value)
				}
				if strings.HasSuffix(gen.value, "-") {
					t.Errorf("%s(len %d input) = %q, which ends in '-': trailing dashes are rejected by DNS-1123 validation, and this is exactly what a naive truncation produces when it cuts mid-token",
						gen.fn, len(in.mdName), gen.value)
				}
			}
		})
	}
}

func TestTruncationPreservesTheVariantSuffix(t *testing.T) {
	t.Parallel()

	inputs := []string{
		longName(maxNameLenExpected, "x"),
		longName(maxNameLenExpected+1, "x"),
		longName(80, "x"),
		longName(200, "x"),
	}

	for _, mdName := range inputs {
		t.Run(fmt.Sprintf("input of %d characters", len(mdName)), func(t *testing.T) {
			t.Parallel()

			primary := PrimaryDeployment(mdName)
			canary := CanaryDeployment(mdName)

			// The suffix is the ONLY thing distinguishing the two Deployments.
			if !strings.HasSuffix(primary, "-primary") {
				t.Errorf("PrimaryDeployment(%d-char name) = %q, which does not end in \"-primary\": truncation ate the suffix that distinguishes the two Deployments, so the primary and canary Deployments would collide on one name — the canary would overwrite the primary in place and the rollout would replace 100%% of traffic instead of a slice of it",
					len(mdName), primary)
			}
			if !strings.HasSuffix(canary, "-canary") {
				t.Errorf("CanaryDeployment(%d-char name) = %q, which does not end in \"-canary\": truncation ate the suffix that distinguishes the two Deployments, so the canary would be applied over the primary Deployment and take all production traffic",
					len(mdName), canary)
			}
			if primary == canary {
				t.Errorf("PrimaryDeployment and CanaryDeployment both returned %q for a %d-char name: the two variants must never share a name, or the canary apply overwrites the primary Deployment and every request goes to the untested workload",
					primary, len(mdName))
			}
		})
	}
}

func TestTruncationDoesNotCollideForNamesSharingALongPrefix(t *testing.T) {
	t.Parallel()

	// Two 80-character names identical except for their final characters. Their
	// truncated prefixes are byte-for-byte the same; only the inserted hash of
	// the FULL original keeps them apart. This is the entire reason the hash
	// exists.
	a := longName(80, "alpha")
	b := longName(80, "bravo")
	c := longName(80, "alphb")

	if a == b || a == c || b == c {
		t.Fatalf("test inputs are not distinct: %q %q %q", a, b, c)
	}

	fns := []struct {
		name string
		fn   func(string) string
	}{
		{name: fnPrimaryDeployment, fn: PrimaryDeployment},
		{name: fnCanaryDeployment, fn: CanaryDeployment},
		{name: fnService, fn: Service},
	}

	for _, f := range fns {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			results := map[string]string{}
			for _, in := range []string{a, b, c} {
				out := f.fn(in)
				if prev, clash := results[out]; clash {
					t.Errorf("%s collides: inputs %q and %q both produce %q. Two DIFFERENT ModelDeployments would own the same child object — whichever reconciles second overwrites the first's Deployment, taking down a workload that belongs to another team's resource. The inserted hash exists precisely to prevent this",
						f.name, prev, in, out)
				}
				results[out] = in
			}
		})
	}

	// Truncated names sharing a prefix must still differ from each other in the
	// hash segment, not merely by luck of the surviving prefix.
	if strings.HasPrefix(PrimaryDeployment(a), a[:40]) != strings.HasPrefix(PrimaryDeployment(b), b[:40]) {
		t.Errorf("PrimaryDeployment kept different amounts of the two inputs (%q vs %q): truncation must be uniform, or name length becomes an unpredictable function of the input",
			PrimaryDeployment(a), PrimaryDeployment(b))
	}
}

func TestTruncationIsDeterministic(t *testing.T) {
	t.Parallel()

	mdName := longName(120, "tail")

	fns := []struct {
		name string
		fn   func(string) string
	}{
		{name: fnPrimaryDeployment, fn: PrimaryDeployment},
		{name: fnCanaryDeployment, fn: CanaryDeployment},
		{name: fnService, fn: Service},
	}

	for _, f := range fns {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			want := f.fn(mdName)
			// A non-deterministic name (map iteration order, a random seed, a
			// process-lifetime salt) would make every controller restart create
			// a brand-new child object and abandon the previous one.
			for i := range 1000 {
				if got := f.fn(mdName); got != want {
					t.Fatalf("%s returned %q on call %d but %q on the first call: name generation must be a pure function of its input, or each controller restart (or each replica in an HA setup) creates a different child object, abandoning the running one and leaking Deployments that nothing ever cleans up",
						f.name, got, i, want)
				}
			}
		})
	}
}

func TestEndpointRendersThePublishedURL(t *testing.T) {
	t.Parallel()

	t.Run("short name", func(t *testing.T) {
		t.Parallel()

		got := Endpoint(mdName, nsName)
		want := "http://demo.prod.svc:8080/v1"
		if got != want {
			t.Errorf("Endpoint(%q, %q) = %q, want %q: this exact string is published in status and copied into client configuration by users — changing its shape breaks every client that already resolved it",
				mdName, nsName, got, want)
		}
	})

	t.Run("uses ServicePort", func(t *testing.T) {
		t.Parallel()

		got := Endpoint(mdName, nsName)
		if !strings.Contains(got, fmt.Sprintf(":%d/", ServicePort)) {
			t.Errorf("Endpoint(%q, %q) = %q, which does not contain the ServicePort (%d): the published endpoint must point at the port the Service actually listens on, or every client connection is refused",
				mdName, nsName, got, ServicePort)
		}
	})

	t.Run("reflects the truncated Service name", func(t *testing.T) {
		t.Parallel()

		mdName := longName(80, "x")
		got := Endpoint(mdName, nsName)
		want := fmt.Sprintf("http://%s.prod.svc:%d/v1", Service(mdName), ServicePort)

		if got != want {
			t.Errorf("Endpoint(%d-char name, %q) = %q, want %q: the endpoint must be built from Service(), not the raw ModelDeployment name — a published URL naming a Service that does not exist fails DNS resolution for every client",
				len(mdName), nsName, got, want)
		}
		if strings.Contains(got, mdName) {
			t.Errorf("Endpoint(%d-char name, %q) = %q, which embeds the untruncated name: the Service object is named %q, so this URL resolves to nothing",
				len(mdName), nsName, got, Service(mdName))
		}
	})
}

func TestFieldManagerIsStableAndWithinLimits(t *testing.T) {
	t.Parallel()

	if FieldManager == "" {
		t.Fatal("FieldManager is empty: Server-Side Apply requires a field manager, and an empty one is rejected by the API server on every apply, so the controller can write nothing at all")
	}
	// The API server caps the field manager at 128 characters.
	if len(FieldManager) > 128 {
		t.Errorf("FieldManager = %q (%d chars), want at most 128: the API server rejects longer field managers, so every Server-Side Apply fails",
			FieldManager, len(FieldManager))
	}
	if want := "llmcp-controller"; FieldManager != want {
		t.Errorf("FieldManager = %q, want %q: this string is recorded in the managedFields of every object the controller has ever applied. Renaming it strands that ownership — the new manager does not own the existing fields, so Server-Side Apply silently stops converging and stale values are never corrected, with no error anywhere",
			FieldManager, want)
	}
}

func TestLabelAndAnnotationKeysAreValidQualifiedNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		key        string
		wantPrefix string
	}{
		{name: "LabelName", key: LabelName, wantPrefix: k8sLabelPrefix},
		{name: "LabelInstance", key: LabelInstance, wantPrefix: k8sLabelPrefix},
		{name: "LabelManagedBy", key: LabelManagedBy, wantPrefix: k8sLabelPrefix},
		{name: "LabelComponent", key: LabelComponent, wantPrefix: k8sLabelPrefix},
		{name: "LabelModelDeployment", key: LabelModelDeployment, wantPrefix: Domain + "/"},
		{name: "LabelVariant", key: LabelVariant, wantPrefix: Domain + "/"},
		{name: "LabelRevision", key: LabelRevision, wantPrefix: Domain + "/"},
		{name: "AnnoPromote", key: AnnoPromote, wantPrefix: Domain + "/"},
		{name: "AnnoAbort", key: AnnoAbort, wantPrefix: Domain + "/"},
	}

	seen := map[string]string{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: the shared `seen` map below detects duplicate keys.
			if errs := validation.IsQualifiedName(tc.key); len(errs) > 0 {
				t.Errorf("%s = %q is not a valid Kubernetes qualified name (%v): the API server rejects the whole object, so the controller cannot create or apply anything carrying this key",
					tc.name, tc.key, errs)
			}
			if !strings.HasPrefix(tc.key, tc.wantPrefix) {
				t.Errorf("%s = %q, want the prefix %q: keys must stay under the operator's own domain so they never collide with another controller's keys — and every object already in a cluster carries the old key, which nothing would clean up",
					tc.name, tc.key, tc.wantPrefix)
			}
			// The part after the "/" is the name segment, which has its own
			// validation rules (alphanumeric start and end, at most 63 chars).
			segment := strings.TrimPrefix(tc.key, tc.wantPrefix)
			if errs := validation.IsQualifiedName(segment); len(errs) > 0 {
				t.Errorf("%s has the name segment %q, which is not itself a valid qualified name (%v): a malformed segment makes the whole key invalid and every object carrying it is rejected at admission",
					tc.name, segment, errs)
			}
			if strings.Contains(segment, "/") {
				t.Errorf("%s = %q has more than one '/': Kubernetes keys allow exactly one prefix separator, and the API server rejects the object outright",
					tc.name, tc.key)
			}
		})
	}

	for _, tc := range cases {
		if prev, dup := seen[tc.key]; dup {
			t.Errorf("%s and %s are both %q: two distinct concepts sharing one label key means writing one silently overwrites the other on every object",
				prev, tc.name, tc.key)
		}
		seen[tc.key] = tc.name
	}
}

func TestPortConstantsAgreeWhereTheCommentsSayTheyMust(t *testing.T) {
	t.Parallel()

	// The package comment states that the metrics shim fronts the engine on
	// ShimPort and that the Service's own port never changes. The Service
	// targets the shim, so these two must be the same number.
	if ShimPort != ServicePort {
		t.Errorf("ShimPort = %d but ServicePort = %d: the Service forwards to the shim, so if they diverge every request to the published endpoint reaches a closed port and the ModelDeployment serves nothing while still reporting ready",
			ShimPort, ServicePort)
	}
	// The engine must NOT share the shim's port, or the shim cannot bind in
	// front of it.
	if EnginePort == ShimPort {
		t.Errorf("EnginePort = %d equals ShimPort = %d: the shim fronts the engine in the same pod's network namespace, so identical ports make the second container fail to bind and the pod crash-loops",
			EnginePort, ShimPort)
	}
	if ShimMetricsPort == ServicePort || ShimMetricsPort == EnginePort {
		t.Errorf("ShimMetricsPort = %d collides with ServicePort = %d or EnginePort = %d: a port collision inside the pod means either metrics scraping or inference stops working, and only one of the two will be noticed",
			ShimMetricsPort, ServicePort, EnginePort)
	}

	for _, p := range []struct {
		name  string
		value int32
	}{
		{name: "ServicePort", value: ServicePort},
		{name: "EnginePort", value: EnginePort},
		{name: "ShimPort", value: ShimPort},
		{name: "ShimMetricsPort", value: ShimMetricsPort},
	} {
		if p.value < 1 || p.value > 65535 {
			t.Errorf("%s = %d is outside the valid port range 1-65535: the API server rejects the pod or Service spec outright",
				p.name, p.value)
		}
	}
}

func TestPortNamesAreDistinctValidIANAServiceNames(t *testing.T) {
	t.Parallel()

	names := []struct {
		constant string
		value    string
	}{
		{constant: "PortNameHTTP", value: PortNameHTTP},
		{constant: "PortNameEngine", value: PortNameEngine},
		{constant: "PortNameMetrics", value: PortNameMetrics},
	}

	seen := map[string]string{}
	for _, n := range names {
		t.Run(n.constant, func(t *testing.T) {
			// A Service port name over 15 characters is rejected by the API
			// server (IANA_SVC_NAME), so the Service is never created.
			if l := len(n.value); l == 0 || l > 15 {
				t.Errorf("%s = %q is %d characters, want 1-15: a Service port name longer than 15 characters is rejected by the API server as an invalid IANA_SVC_NAME, so the Service — and therefore the published endpoint — is never created",
					n.constant, n.value, l)
			}
			if !ianaSvcNameRe.MatchString(n.value) {
				t.Errorf("%s = %q is not a valid IANA_SVC_NAME (lowercase alphanumerics and '-', no leading or trailing '-'): the Service spec is rejected at admission",
					n.constant, n.value)
			}
			if strings.Contains(n.value, "--") {
				t.Errorf("%s = %q contains adjacent dashes, which IANA_SVC_NAME forbids: the Service spec is rejected at admission",
					n.constant, n.value)
			}
			if errs := validation.IsValidPortName(n.value); len(errs) > 0 {
				t.Errorf("%s = %q is rejected by Kubernetes port-name validation (%v): the Service or pod spec carrying it is rejected at admission",
					n.constant, n.value, errs)
			}
		})

		if prev, dup := seen[n.value]; dup {
			t.Errorf("%s and %s are both %q: port names must be unique within a pod and within a Service, and duplicates are rejected at admission — while ServiceMonitor selects endpoints BY NAME, so a rename or duplicate silently stops metric scraping with no error anywhere",
				prev, n.constant, n.value)
		}
		seen[n.value] = n.constant
	}
}

func TestIdentityConstantsAreStable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "Domain", got: Domain, want: "llmcp.io"},
		{name: "AppName", got: AppName, want: "llmcp"},
		{name: "ManagedBy", got: ManagedBy, want: "llmcp-controller"},
		{name: "ComponentInferenceServer", got: ComponentInferenceServer, want: "inference-server"},
		{name: "ComponentRouter", got: ComponentRouter, want: "router"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q: this value is baked into the label keys or label values of every object already in every cluster. Changing it orphans all of them — the controller stops recognising what it owns and creates a parallel set alongside the running one",
					tc.name, tc.got, tc.want)
			}
			// Label VALUES must themselves be valid, or every object carrying
			// them is rejected.
			if errs := validation.IsValidLabelValue(tc.got); len(errs) > 0 {
				t.Errorf("%s = %q is not a valid label value (%v): every object stamped with it is rejected at admission",
					tc.name, tc.got, errs)
			}
		})
	}
}

func TestControllerRevisionNamesStayInBoundsForRealisticRevisions(t *testing.T) {
	t.Parallel()

	// Revisions in this operator are short content hashes. The lengths below
	// cover every hash width anyone is likely to switch to (fnv, crc, a
	// truncated sha) against ModelDeployment names on both sides of the
	// truncation boundary.
	//
	// Note the asymmetry this exercises: withSuffix truncates the NAME but
	// never the SUFFIX, so the revision string's length is spent straight out
	// of the 63-character budget. Widening the hash silently eats the name.
	revisionLengths := []int{6, 8, 10, 16, 20}
	mdNames := []string{
		mdName,
		longName(maxNameLenExpected-1, "x"),
		longName(maxNameLenExpected, "x"),
		longName(120, "x"),
	}

	for _, revLen := range revisionLengths {
		revision := strings.Repeat("f", revLen)
		for _, mdName := range mdNames {
			t.Run(fmt.Sprintf("revision of %d chars on a %d-char name", revLen, len(mdName)), func(t *testing.T) {
				t.Parallel()

				got := ControllerRevision(mdName, revision)

				if len(got) > maxNameLenExpected {
					t.Errorf("ControllerRevision(%d-char name, %d-char revision) = %q (%d chars), want at most %d: an over-long ControllerRevision name is rejected at admission, so no revision history is recorded at all — and without history, rollback has nothing to roll back to",
						len(mdName), revLen, got, len(got), maxNameLenExpected)
				}
				if !dns1123LabelRe.MatchString(got) {
					t.Errorf("ControllerRevision(%d-char name, %d-char revision) = %q, which is not a valid DNS-1123 label: the revision object is rejected and rollback silently has no history to use",
						len(mdName), revLen, got)
				}
				if !strings.HasSuffix(got, revision) {
					t.Errorf("ControllerRevision(%d-char name, %d-char revision) = %q, which does not end in the revision %q: the revision hash is what makes each history entry unique, so losing it makes successive revisions collide on one object and each new revision overwrites the previous one",
						len(mdName), revLen, got, revision)
				}
			})
		}
	}
}

// TestControllerRevisionNameSurvivesALongRevisionIdentifier is a regression
// test for suffix-driven overflow in withSuffix.
//
// The truncation logic shortens the NAME to make room, but the suffix was
// previously spent straight out of the 63-character budget without ever being
// shortened itself. A revision identifier of 53 characters or more therefore
// produced a name longer than a DNS-1123 label allows, and the API server
// rejects such an object at admission — meaning no revision would be recorded
// at all, and a later rollback would have nothing to roll back to.
//
// Today's revision identifiers are short content hashes, so this is latent
// rather than live. It is exactly the kind of trap that a future switch to a
// longer identifier would spring silently.
func TestControllerRevisionNameSurvivesALongRevisionIdentifier(t *testing.T) {
	t.Parallel()

	dns1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	for _, revLen := range []int{8, 40, 52, 53, 60, 70, 120} {
		t.Run(fmt.Sprintf("revision-length-%d", revLen), func(t *testing.T) {
			t.Parallel()

			rev := strings.Repeat("a", revLen)
			for _, name := range []string{mdName, strings.Repeat("m", 63), strings.Repeat("m", 120)} {
				got := ControllerRevision(name, "-"+rev)

				if len(got) > maxNameLen {
					t.Errorf("ControllerRevision(len %d name, len %d revision) produced a %d-character name: %q\n"+
						"names longer than %d characters are rejected at admission, so no revision history "+
						"would be recorded and rollback would have no target",
						len(name), revLen, len(got), got, maxNameLen)
				}
				if !dns1123.Match([]byte(got)) {
					t.Errorf("ControllerRevision(len %d name, len %d revision) = %q, which is not a valid "+
						"DNS-1123 label", len(name), revLen, got)
				}
			}
		})
	}
}
