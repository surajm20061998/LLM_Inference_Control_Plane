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

package revision

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/naming"
)

const testNamespace = "llmcp-test"

// newScheme registers both the project's API group and appsv1: the former so
// owner references can resolve a ModelDeployment's GroupVersionKind, the latter
// because ControllerRevision is the object being written.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering v1alpha1 in scheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("registering appsv1 in scheme: %v", err)
	}
	return s
}

// newRecorder returns a Recorder over a fake client seeded with objs.
//
// The fake client is sufficient here precisely because every operation in this
// package is a plain CRUD round-trip: nothing depends on the features the fake
// lacks (no garbage collection, no defaulting, no admission).
func newRecorder(t *testing.T, objs ...client.Object) *Recorder {
	t.Helper()

	s := newScheme(t)
	return &Recorder{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

// newModelDeployment builds a ModelDeployment carrying baseSpec. A UID is set
// because owner references require one.
func newModelDeployment(name string) *inferencev1alpha1.ModelDeployment {
	return &inferencev1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			// Owner references require a UID; make it distinct per object.
			UID: k8stypes.UID(name + "-uid"),
		},
		Spec: *baseSpec(),
	}
}

func TestRecordCreatesControllerRevision(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)
	hash := Hash(&md.Spec)

	cr, _, err := rec.Record(ctx, md, hash)
	if err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	if want := naming.ControllerRevision(md.Name, hash); cr.Name != want {
		t.Errorf("name = %q, want %q", cr.Name, want)
	}
	if cr.Namespace != md.Namespace {
		t.Errorf("namespace = %q, want %q", cr.Namespace, md.Namespace)
	}
	if cr.Revision != 1 {
		t.Errorf("revision = %d, want 1 for the first recorded revision", cr.Revision)
	}
	if cr.Labels[naming.LabelRevision] != hash {
		t.Errorf("label %s = %q, want %q", naming.LabelRevision, cr.Labels[naming.LabelRevision], hash)
	}
	for k, v := range naming.CommonLabels(md.Name) {
		if cr.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, cr.Labels[k], v)
		}
	}
	if len(cr.Data.Raw) == 0 {
		t.Error("Data.Raw is empty; the revision payload was not stored")
	}

	// The owner reference is what makes history garbage-collected with its
	// parent, which is the entire reason this package needs no finalizer.
	owners := cr.GetOwnerReferences()
	if len(owners) != 1 {
		t.Fatalf("owner references = %d, want exactly 1: %+v", len(owners), owners)
	}
	owner := owners[0]
	if owner.Kind != "ModelDeployment" || owner.Name != md.Name || owner.UID != md.UID {
		t.Errorf("owner = %+v, want a controller reference to ModelDeployment %s", owner, md.Name)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("owner reference is not marked as the controller")
	}

	// It really is in the API, not just in the returned value.
	persisted, err := rec.Get(ctx, md, hash)
	if err != nil {
		t.Fatalf("Get after Record returned an error: %v", err)
	}
	if persisted.Name != cr.Name {
		t.Errorf("persisted name = %q, want %q", persisted.Name, cr.Name)
	}
}

// TestRecordIsIdempotent is the property a reconcile loop depends on: it will
// call Record on every pass, and an unchanged spec must not churn the API or
// inflate the sequence number.
func TestRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)
	hash := Hash(&md.Spec)

	first, _, err := rec.Record(ctx, md, hash)
	if err != nil {
		t.Fatalf("first Record returned an error: %v", err)
	}

	for i := range 5 {
		again, _, err := rec.Record(ctx, md, hash)
		if err != nil {
			t.Fatalf("Record call %d returned an error: %v", i+2, err)
		}
		if again.Name != first.Name {
			t.Fatalf("Record call %d returned %q, want the existing %q", i+2, again.Name, first.Name)
		}
		if again.Revision != first.Revision {
			t.Fatalf("Record call %d bumped .Revision to %d, want it to stay %d", i+2, again.Revision, first.Revision)
		}
	}

	all, err := rec.List(ctx, md)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("history has %d entries after repeated recording of one revision, want 1", len(all))
	}
}

// TestRecordReportsCreatedExactlyOnce pins the `created` return.
//
// The reconcile loop emits a "RevisionCreated" Kubernetes event off this
// boolean, and it calls Record on every single pass. If `created` were derived
// from a read rather than from the API server's response to the Create, an
// informer cache lagging its own write would report true repeatedly and fill
// the object's event history with duplicates for one revision — noise in
// exactly the audit trail an operator reaches for when a rollout goes wrong.
func TestRecordReportsCreatedExactlyOnce(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)
	hash := Hash(&md.Spec)

	_, created, err := rec.Record(ctx, md, hash)
	if err != nil {
		t.Fatalf("first Record returned an error: %v", err)
	}
	if !created {
		t.Fatal("first Record reported created=false, want true: it did create the object")
	}

	for i := range 5 {
		_, created, err := rec.Record(ctx, md, hash)
		if err != nil {
			t.Fatalf("Record call %d returned an error: %v", i+2, err)
		}
		if created {
			t.Errorf("Record call %d reported created=true for an already-recorded revision", i+2)
		}
	}

	// A genuinely new revision must report created=true again, or the loop
	// would stop announcing real rollouts.
	md.Spec.Engine.ContextSize = ptrTo(int32(8192))
	_, created, err = rec.Record(ctx, md, Hash(&md.Spec))
	if err != nil {
		t.Fatalf("Record of a changed spec returned an error: %v", err)
	}
	if !created {
		t.Error("Record reported created=false for a new revision, want true")
	}
}

// TestRecordAssignsIncreasingRevisionNumbers covers the ordering that Prune and
// any "previous revision" lookup rely on.
func TestRecordAssignsIncreasingRevisionNumbers(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)

	first, _, err := rec.Record(ctx, md, Hash(&md.Spec))
	if err != nil {
		t.Fatalf("recording the first revision: %v", err)
	}
	if first.Revision != 1 {
		t.Fatalf("first revision number = %d, want 1", first.Revision)
	}

	md.Spec.Engine.Image = "ghcr.io/ggml-org/llama.cpp:server-next"
	second, _, err := rec.Record(ctx, md, Hash(&md.Spec))
	if err != nil {
		t.Fatalf("recording the second revision: %v", err)
	}
	if second.Revision != 2 {
		t.Fatalf("second revision number = %d, want 2", second.Revision)
	}
	if second.Name == first.Name {
		t.Fatalf("a changed spec reused the revision name %q", second.Name)
	}

	md.Spec.Serving.Port = 9090
	third, _, err := rec.Record(ctx, md, Hash(&md.Spec))
	if err != nil {
		t.Fatalf("recording the third revision: %v", err)
	}
	if third.Revision != 3 {
		t.Fatalf("third revision number = %d, want 3", third.Revision)
	}
}

// TestListOrdersOldestFirstAndFiltersByOwner asserts both halves of List's
// contract. The decoy matters: ControllerRevisions from different
// ModelDeployments share a namespace, and an unfiltered list would let one
// ModelDeployment prune — or roll back to — another's history.
func TestListOrdersOldestFirstAndFiltersByOwner(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	decoy := newModelDeployment("other")
	rec := newRecorder(t, md, decoy)

	ports := []int32{8080, 8081, 8082}
	wantNames := make([]string, 0, len(ports))
	for _, port := range ports {
		md.Spec.Serving.Port = port
		cr, _, err := rec.Record(ctx, md, Hash(&md.Spec))
		if err != nil {
			t.Fatalf("recording revision for port %d: %v", port, err)
		}
		wantNames = append(wantNames, cr.Name)
	}

	decoyCR, _, err := rec.Record(ctx, decoy, Hash(&decoy.Spec))
	if err != nil {
		t.Fatalf("recording the decoy's revision: %v", err)
	}

	got, err := rec.List(ctx, md)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != len(wantNames) {
		t.Fatalf("List returned %d revisions, want %d", len(got), len(wantNames))
	}

	for i, cr := range got {
		if cr.Name != wantNames[i] {
			t.Errorf("revision %d = %q, want %q (oldest first)", i, cr.Name, wantNames[i])
		}
		if want := int64(i + 1); cr.Revision != want {
			t.Errorf("revision %d has .Revision = %d, want %d", i, cr.Revision, want)
		}
		if cr.Name == decoyCR.Name {
			t.Errorf("List leaked another ModelDeployment's revision %q", cr.Name)
		}
	}

	// And the decoy's own history is intact and separate.
	decoyList, err := rec.List(ctx, decoy)
	if err != nil {
		t.Fatalf("listing the decoy's revisions: %v", err)
	}
	if len(decoyList) != 1 || decoyList[0].Name != decoyCR.Name {
		t.Fatalf("decoy history = %+v, want exactly its own revision %q", decoyList, decoyCR.Name)
	}
}

// TestSpecFromRoundTrips closes the loop: a revision recorded today must be
// reconstructible into something that hashes back to the same identifier, or a
// rollback would land on a spec that immediately looks like a new revision and
// roll forward again.
func TestSpecFromRoundTrips(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)
	hash := Hash(&md.Spec)

	cr, _, err := rec.Record(ctx, md, hash)
	if err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	got, err := SpecFrom(cr)
	if err != nil {
		t.Fatalf("SpecFrom returned an error: %v", err)
	}

	if Hash(got) != hash {
		t.Fatalf("round-tripped spec hashes to %q, want %q", Hash(got), hash)
	}
	if got.Model.Name != md.Spec.Model.Name {
		t.Errorf("model name = %q, want %q", got.Model.Name, md.Spec.Model.Name)
	}
	if got.Model.Source.Image == nil || got.Model.Source.Image.Image != md.Spec.Model.Source.Image.Image {
		t.Errorf("model source = %+v, want image %q", got.Model.Source, md.Spec.Model.Source.Image.Image)
	}
	if got.Serving.Port != md.Spec.Serving.Port {
		t.Errorf("serving port = %d, want %d", got.Serving.Port, md.Spec.Serving.Port)
	}
	if len(got.Engine.Env) != len(md.Spec.Engine.Env) {
		t.Errorf("engine env = %+v, want %+v", got.Engine.Env, md.Spec.Engine.Env)
	}

	// The excluded fields come back ZERO. This is asserted rather than merely
	// documented because a caller that assigns the result wholesale would
	// silently scale the workload to nil and drop its rollout settings.
	if got.Replicas != nil {
		t.Errorf("Replicas = %v, want nil: it is not part of a revision", *got.Replicas)
	}
	if got.Rollout != (inferencev1alpha1.RolloutSpec{}) {
		t.Errorf("Rollout = %+v, want the zero value: it is not part of a revision", got.Rollout)
	}
	if got.Serving.StartupTimeout != nil {
		t.Errorf("Serving.StartupTimeout = %v, want nil: it is not part of a revision", got.Serving.StartupTimeout)
	}
}

func TestSpecFromRejectsUnusableRevisions(t *testing.T) {
	if _, err := SpecFrom(nil); err == nil {
		t.Error("SpecFrom(nil) returned no error")
	}
	if _, err := SpecFrom(&appsv1.ControllerRevision{}); err == nil {
		t.Error("SpecFrom on an empty payload returned no error")
	}
	bad := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte("{not json")}}
	if _, err := SpecFrom(bad); err == nil {
		t.Error("SpecFrom on malformed JSON returned no error")
	}
}

// TestPruneKeepsNewestWithinLimit is the plain case: no pins, just age.
func TestPruneKeepsNewestWithinLimit(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)

	names := recordSeries(t, rec, md, 5)

	if err := rec.Prune(ctx, md, 2); err != nil {
		t.Fatalf("Prune returned an error: %v", err)
	}

	got, err := rec.List(ctx, md)
	if err != nil {
		t.Fatalf("List after Prune returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("history has %d entries after pruning to 2: %+v", len(got), revisionNames(got))
	}
	if got[0].Name != names[3] || got[1].Name != names[4] {
		t.Fatalf("Prune kept %v, want the two newest %v", revisionNames(got), names[3:])
	}
}

// TestPruneNeverDeletesKept is the case that matters operationally: the pinned
// revision is the OLDEST, so it is exactly what age-based pruning would remove
// first — and it is typically status.lastGoodRevision, the only thing an
// automatic rollback can aim at.
func TestPruneNeverDeletesKept(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)

	names := recordSeries(t, rec, md, 5)
	oldestHash := hashSeries(5)[0]

	if err := rec.Prune(ctx, md, 2, oldestHash); err != nil {
		t.Fatalf("Prune returned an error: %v", err)
	}

	got, err := rec.List(ctx, md)
	if err != nil {
		t.Fatalf("List after Prune returned an error: %v", err)
	}

	gotNames := revisionNames(got)
	if len(got) != 2 {
		t.Fatalf("history has %d entries after pruning to 2: %v", len(got), gotNames)
	}
	if gotNames[0] != names[0] {
		t.Fatalf("Prune deleted the pinned oldest revision %q; history is %v", names[0], gotNames)
	}
	if gotNames[1] != names[4] {
		t.Fatalf("Prune kept %v, want the pinned oldest plus the newest", gotNames)
	}
}

// TestPruneKeepsMultipleAndTolerates checks the remaining edges: several pins,
// a limit that pins alone exceed, a no-op limit, and pruning to nothing.
func TestPruneEdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("limit above history size is a no-op", func(t *testing.T) {
		md := newModelDeployment("demo")
		rec := newRecorder(t, md)
		recordSeries(t, rec, md, 3)

		if err := rec.Prune(ctx, md, 10); err != nil {
			t.Fatalf("Prune returned an error: %v", err)
		}
		assertHistoryLen(t, rec, md, 3)
	})

	t.Run("negative limit disables pruning", func(t *testing.T) {
		md := newModelDeployment("demo")
		rec := newRecorder(t, md)
		recordSeries(t, rec, md, 3)

		if err := rec.Prune(ctx, md, -1); err != nil {
			t.Fatalf("Prune returned an error: %v", err)
		}
		assertHistoryLen(t, rec, md, 3)
	})

	t.Run("pins survive a zero limit", func(t *testing.T) {
		md := newModelDeployment("demo")
		rec := newRecorder(t, md)
		names := recordSeries(t, rec, md, 4)
		hashes := hashSeries(4)

		if err := rec.Prune(ctx, md, 0, hashes[1], hashes[2]); err != nil {
			t.Fatalf("Prune returned an error: %v", err)
		}

		got, err := rec.List(ctx, md)
		if err != nil {
			t.Fatalf("List after Prune returned an error: %v", err)
		}
		if want := []string{names[1], names[2]}; !slices.Equal(revisionNames(got), want) {
			t.Fatalf("history = %v, want only the pinned %v", revisionNames(got), want)
		}
	})

	t.Run("zero limit with no pins empties history", func(t *testing.T) {
		md := newModelDeployment("demo")
		rec := newRecorder(t, md)
		recordSeries(t, rec, md, 3)

		if err := rec.Prune(ctx, md, 0); err != nil {
			t.Fatalf("Prune returned an error: %v", err)
		}
		assertHistoryLen(t, rec, md, 0)
	})

	t.Run("pruning empty history is a no-op", func(t *testing.T) {
		md := newModelDeployment("demo")
		rec := newRecorder(t, md)

		if err := rec.Prune(ctx, md, 2); err != nil {
			t.Fatalf("Prune returned an error: %v", err)
		}
		assertHistoryLen(t, rec, md, 0)
	})
}

// TestGetMissingRevisionIsNotFound: callers distinguish "the rollback target was
// pruned" from "the API is broken" via apierrors.IsNotFound, so the error value
// must survive Get unwrapped.
func TestGetMissingRevisionIsNotFound(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)

	_, err := rec.Get(ctx, md, "doesnotexist")
	if err == nil {
		t.Fatal("Get for an unrecorded revision returned no error")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get returned %v, want an IsNotFound error", err)
	}
}

func TestRecordRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	md := newModelDeployment("demo")
	rec := newRecorder(t, md)

	if _, _, err := rec.Record(ctx, nil, "abc"); err == nil {
		t.Error("Record with a nil ModelDeployment returned no error")
	}
	if _, _, err := rec.Record(ctx, md, ""); err == nil {
		t.Error("Record with an empty revision returned no error")
	}
}

// recordSeries records n distinct revisions in order and returns their object
// names, oldest first. Revisions are made distinct by port, which is an
// included field.
func recordSeries(
	t *testing.T,
	rec *Recorder,
	md *inferencev1alpha1.ModelDeployment,
	n int,
) []string {
	t.Helper()

	names := make([]string, 0, n)
	for i := range n {
		md.Spec.Serving.Port = int32(8080 + i)
		cr, _, err := rec.Record(context.Background(), md, Hash(&md.Spec))
		if err != nil {
			t.Fatalf("recording revision %d: %v", i, err)
		}
		names = append(names, cr.Name)
	}
	return names
}

// hashSeries recomputes the hashes recordSeries produced, so a test can pin one
// of them by hash without threading the values through.
func hashSeries(n int) []string {
	hashes := make([]string, 0, n)
	for i := range n {
		s := baseSpec()
		s.Serving.Port = int32(8080 + i)
		hashes = append(hashes, Hash(s))
	}
	return hashes
}

func revisionNames(revisions []appsv1.ControllerRevision) []string {
	names := make([]string, 0, len(revisions))
	for i := range revisions {
		names = append(names, revisions[i].Name)
	}
	return names
}

func assertHistoryLen(
	t *testing.T,
	rec *Recorder,
	md *inferencev1alpha1.ModelDeployment,
	want int,
) {
	t.Helper()

	got, err := rec.List(context.Background(), md)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != want {
		t.Fatalf("history has %d entries, want %d: %v", len(got), want, revisionNames(got))
	}
}
