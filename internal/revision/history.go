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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// Recorder persists ModelDeployment revision history as ControllerRevisions.
//
// ControllerRevision is reused rather than reinvented because it already solves
// the problem: it is an immutable, namespaced, owner-referenced record with an
// opaque payload and a monotonic sequence number, and cluster operators already
// know how to inspect it (`kubectl get controllerrevision`).
//
// Every object it writes is owner-referenced to its ModelDeployment, so history
// is garbage-collected with the parent. There is deliberately no finalizer:
// history has no value once the ModelDeployment it describes is gone, and a
// finalizer would only add a way for deletion to get stuck.
type Recorder struct {
	// Client reads and writes ControllerRevisions.
	Client client.Client

	// Scheme resolves the ModelDeployment's GroupVersionKind when setting owner
	// references. It must have both the project's v1alpha1 types and appsv1
	// registered.
	Scheme *runtime.Scheme
}

// Record ensures a ControllerRevision exists for the given spec revision.
// It is idempotent: recording an unchanged revision is a no-op.
//
// created reports whether this call actually created the object, as opposed to
// finding or adopting an existing one. It comes from the API server's response
// to the Create, which is the only trustworthy source: a caller that instead
// inferred novelty from a prior read would be reading an informer cache that
// routinely lags its own writes, and would conclude "new revision" on every
// reconcile until the cache caught up.
//
// revision is the identifier the caller computed with Hash for md.Spec; it
// names the object (via naming.ControllerRevision) and is stamped on it as
// naming.LabelRevision. The stored payload comes from Encode, so the recorded
// bytes are by construction the bytes the hash was taken over.
//
// The .Revision sequence number is assigned as max(existing) + 1, starting at
// 1. An already-recorded revision keeps the number it was first given: it is
// not "re-recorded" and does not move to the head of history. That is what
// makes a rollback to an older revision observable as a rollback rather than as
// a brand new revision.
func (r *Recorder) Record(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	revision string,
) (cr *appsv1.ControllerRevision, created bool, err error) {
	if md == nil {
		return nil, false, fmt.Errorf("revision: cannot record a nil ModelDeployment")
	}
	if revision == "" {
		return nil, false, fmt.Errorf("revision: cannot record an empty revision for %s/%s",
			md.Namespace, md.Name)
	}

	// Fast path: already recorded. Returning the existing object unchanged is
	// what makes a steady-state reconcile free of writes.
	existing, err := r.Get(ctx, md, revision)
	if err == nil {
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}

	data, err := Encode(&md.Spec)
	if err != nil {
		return nil, false, fmt.Errorf("revision: encoding spec of %s/%s: %w", md.Namespace, md.Name, err)
	}

	next, err := r.nextRevisionNumber(ctx, md)
	if err != nil {
		return nil, false, err
	}

	labels := naming.CommonLabels(md.Name)
	labels[naming.LabelRevision] = revision

	cr = &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.ControllerRevision(md.Name, revision),
			Namespace: md.Namespace,
			Labels:    labels,
		},
		Data:     runtime.RawExtension{Raw: data},
		Revision: next,
	}

	if err := controllerutil.SetControllerReference(md, cr, r.Scheme); err != nil {
		return nil, false, fmt.Errorf("revision: setting owner reference on %s: %w", cr.Name, err)
	}

	if err := r.Client.Create(ctx, cr, client.FieldOwner(naming.FieldManager)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race with another writer — or, far more commonly, with our
			// own earlier reconcile, because the Get above reads through an
			// informer cache that has not yet observed our own Create. The
			// object is immutable and content-addressed by its name, so
			// whichever write won is equally correct: adopt it.
			//
			// This is also why `created` is reported from the Create call
			// rather than from the Get: only the API server's answer is
			// authoritative about whether anything actually happened, and a
			// caller that keys an event off a stale cache read will emit that
			// event once per reconcile instead of once per revision.
			adopted, getErr := r.Get(ctx, md, revision)
			return adopted, false, getErr
		}
		return nil, false, fmt.Errorf("revision: creating %s/%s: %w", cr.Namespace, cr.Name, err)
	}

	return cr, true, nil
}

// Get returns the ControllerRevision for a revision hash.
//
// The returned error satisfies apierrors.IsNotFound when no such revision has
// been recorded, so callers can distinguish "never recorded" (a rollback target
// that no longer exists) from a genuine API failure.
func (r *Recorder) Get(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	revision string,
) (*appsv1.ControllerRevision, error) {
	if md == nil {
		return nil, fmt.Errorf("revision: cannot get a revision of a nil ModelDeployment")
	}

	cr := &appsv1.ControllerRevision{}
	key := types.NamespacedName{
		Namespace: md.Namespace,
		Name:      naming.ControllerRevision(md.Name, revision),
	}
	if err := r.Client.Get(ctx, key, cr); err != nil {
		// Returned unwrapped: apierrors.IsNotFound must keep working for
		// callers, and wrapping would change the value they inspect.
		return nil, err
	}
	return cr, nil
}

// List returns this ModelDeployment's revisions, ordered oldest first by
// .revision.
//
// Filtering is by label rather than by owner reference so it can be served from
// an indexed cache. Ties on .revision (which the API server does not prevent)
// are broken by name so the order is total and stable.
func (r *Recorder) List(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) ([]appsv1.ControllerRevision, error) {
	if md == nil {
		return nil, fmt.Errorf("revision: cannot list revisions of a nil ModelDeployment")
	}

	list := &appsv1.ControllerRevisionList{}
	if err := r.Client.List(ctx, list,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{naming.LabelModelDeployment: md.Name},
	); err != nil {
		return nil, fmt.Errorf("revision: listing revisions of %s/%s: %w", md.Namespace, md.Name, err)
	}

	slices.SortFunc(list.Items, func(a, b appsv1.ControllerRevision) int {
		if c := cmp.Compare(a.Revision, b.Revision); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})

	return list.Items, nil
}

// SpecFrom reconstructs the recorded spec subset from a ControllerRevision.
//
// IMPORTANT: only the revision-defining fields are populated — Model, Engine,
// Serving.Port and Serving.Shim. Everything excluded from the revision is returned ZERO:
// Replicas is nil, Rollout is the empty struct, Serving.StartupTimeout is nil.
// That is not a gap to be filled in later, it is the point: those fields are
// not part of the revision, so history has no opinion about them.
//
// A caller rolling back must therefore MERGE this into the live spec — copy the
// returned fields over — and must never assign it wholesale. Replacing the spec
// with the return value would reset the replica count to nil (undoing whatever
// an autoscaler had decided) and discard the current rollout settings, turning
// a rollback into an unintended scale-down.
func SpecFrom(cr *appsv1.ControllerRevision) (*inferencev1alpha1.ModelDeploymentSpec, error) {
	if cr == nil {
		return nil, fmt.Errorf("revision: cannot decode a nil ControllerRevision")
	}
	if len(cr.Data.Raw) == 0 {
		return nil, fmt.Errorf("revision: ControllerRevision %s/%s has an empty payload", cr.Namespace, cr.Name)
	}

	var in revisionInput
	if err := json.Unmarshal(cr.Data.Raw, &in); err != nil {
		return nil, fmt.Errorf("revision: decoding ControllerRevision %s/%s: %w", cr.Namespace, cr.Name, err)
	}

	return &inferencev1alpha1.ModelDeploymentSpec{
		Model:  in.Model,
		Engine: in.Engine,
		Serving: inferencev1alpha1.ServingSpec{
			Port: in.Serving.Port,
			Shim: in.Serving.Shim,
		},
	}, nil
}

// Prune deletes the oldest revisions beyond limit, never deleting the
// revisions named in keep.
//
// keep holds revision HASHES (the same values passed to Record), typically
// status.stableRevision and status.lastGoodRevision — deleting either would
// destroy the only thing an automatic rollback can aim at, so they are pinned
// regardless of age. Kept revisions still COUNT towards limit; they are simply
// skipped when choosing victims, so pinning an old revision costs a newer one
// rather than growing history without bound.
//
// A negative limit disables pruning entirely. A limit of 0 is honoured
// literally: everything not in keep is deleted.
//
// Deleting a revision that is already gone is not an error — Kubernetes garbage
// collection may have removed it concurrently, and the desired end state has
// been reached either way.
func (r *Recorder) Prune(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	limit int32,
	keep ...string,
) error {
	if md == nil {
		return fmt.Errorf("revision: cannot prune revisions of a nil ModelDeployment")
	}
	if limit < 0 {
		return nil
	}

	revisions, err := r.List(ctx, md)
	if err != nil {
		return err
	}

	excess := len(revisions) - int(limit)
	if excess <= 0 {
		return nil
	}

	// Pin by both the revision label and the derived object name: callers
	// naturally hold hashes, and both identify the same object.
	pinned := make(map[string]struct{}, len(keep)*2)
	for _, k := range keep {
		if k == "" {
			continue
		}
		pinned[k] = struct{}{}
		pinned[naming.ControllerRevision(md.Name, k)] = struct{}{}
	}

	// revisions is already oldest-first, so this deletes in age order.
	for i := range revisions {
		if excess <= 0 {
			break
		}
		cr := &revisions[i]
		if _, ok := pinned[cr.Name]; ok {
			continue
		}
		if _, ok := pinned[cr.Labels[naming.LabelRevision]]; ok {
			continue
		}

		if err := r.Client.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("revision: deleting %s/%s: %w", cr.Namespace, cr.Name, err)
		}
		excess--
	}

	return nil
}

// nextRevisionNumber returns max(existing .revision) + 1, or 1 when no history
// exists.
//
// This is a read-then-write and so is racy under concurrent reconciles; the
// consequence is a duplicate sequence number, which only affects ordering
// between two revisions recorded in the same instant. The name — derived from
// the content hash — remains the unique key, so no history can be lost this
// way.
func (r *Recorder) nextRevisionNumber(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) (int64, error) {
	revisions, err := r.List(ctx, md)
	if err != nil {
		return 0, err
	}

	var maxRevision int64
	for i := range revisions {
		if revisions[i].Revision > maxRevision {
			maxRevision = revisions[i].Revision
		}
	}

	return maxRevision + 1, nil
}
