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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// # Why this suite does not start a manager
//
// The kubebuilder scaffold's instinct is to start a manager in BeforeSuite and
// let the controller run in the background. That turns a deterministic state
// machine into a race: the manager reconciles off an informer cache which is
// updated asynchronously, so every assertion has to be wrapped in Eventually
// and every failure has two candidate explanations — the controller is wrong,
// or the test looked too early. Worse, the two are indistinguishable from the
// failure output, so the usual repair is to raise the timeout, which converts a
// real bug into an intermittent one.
//
// So: no manager. Each spec constructs a reconciler over the direct (uncached)
// client and calls Reconcile synchronously, asserting between calls. Reconcile
// is a pure "one pass over the world" function; running it explicitly means the
// test states exactly how many passes happened, which is precisely what several
// of the properties below (idempotency, revision counting) are about. There is
// deliberately not a single Eventually in this file, and the whole suite
// finishes in milliseconds.

var _ = Describe("ModelDeployment controller", func() {
	var (
		namespace string
		name      string
		mdKey     types.NamespacedName
		depKey    types.NamespacedName
		svcKey    types.NamespacedName
		r         *ModelDeploymentReconciler
	)

	BeforeEach(func() {
		// A fresh namespace per spec: specs stay independent, may run in any
		// order, and nothing has to be torn down (envtest is discarded whole).
		namespace = mdtNewNamespace()
		name = "demo"
		mdKey = types.NamespacedName{Name: name, Namespace: namespace}
		depKey = types.NamespacedName{Name: naming.PrimaryDeployment(name), Namespace: namespace}
		svcKey = types.NamespacedName{Name: naming.Service(name), Namespace: namespace}
		r = mdtNewReconciler()
	})

	Context("when reconciling a newly created ModelDeployment", func() {
		It("creates an owned Deployment and Service with the immutable selector", func() {
			md := mdtCreate(helpers.NewModelDeployment(name, namespace, helpers.WithReplicas(3)))
			mdtReconcile(r, mdKey)

			By("creating the primary Deployment")
			dep := mdtGetDeployment(depKey)
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)))

			By("creating the Service")
			svc := mdtGetService(svcKey)
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))

			By("owning both children with a controller reference")
			mdtExpectOwnedBy(dep.OwnerReferences, md)
			mdtExpectOwnedBy(svc.OwnerReferences, md)

			By("selecting on exactly the two immutable labels")
			// spec.selector is immutable for the life of the Deployment. If the
			// revision label ever leaked into it, every spec change would need a
			// delete-and-recreate of the workload rather than a rolling update —
			// which is a full outage, not a rollout.
			Expect(dep.Spec.Selector).NotTo(BeNil())
			Expect(dep.Spec.Selector.MatchLabels).To(Equal(map[string]string{
				naming.LabelModelDeployment: name,
				naming.LabelVariant:         string(inferencev1alpha1.VariantPrimary),
			}))
			Expect(dep.Spec.Selector.MatchLabels).NotTo(HaveKey(naming.LabelRevision))
			Expect(dep.Spec.Selector.MatchExpressions).To(BeEmpty())

			By("still stamping the revision on pods, where it is safe")
			Expect(dep.Spec.Template.Labels).To(HaveKey(naming.LabelRevision))
		})
	})

	Context("when reconciling repeatedly with an unchanged spec", func() {
		// This is the most important test in the file.
		//
		// The controller writes its children with Server-Side Apply and watches
		// the result. SSA is only a no-op when the applied bytes are identical:
		// a slice built in a different order, a map iterated into output, or an
		// empty struct emitted where the API server omits one all read as a
		// change. Each such "change" bumps resourceVersion, which fires the
		// watch, which schedules another reconcile, which applies again —
		// a hot loop between the controller and the API server that converges
		// on nothing and is completely invisible until it is saturating the
		// apiserver of a production cluster.
		//
		// resourceVersion is the sensitive half of the assertion (it moves on
		// ANY write, including a metadata-only one); generation is the
		// coarse half (it moves only on a spec write). Both must hold still.
		It("is byte-stable: repeated applies do not write", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace, helpers.WithReplicas(2)))
			mdtReconcile(r, mdKey)

			depRV, depGen := mdtVersion(mdtGetDeployment(depKey))
			svcRV, svcGen := mdtVersion(mdtGetService(svcKey))

			By("reconciling a second time with no spec change")
			mdtReconcile(r, mdKey)

			dep := mdtGetDeployment(depKey)
			Expect(dep.ResourceVersion).To(Equal(depRV),
				"the child Deployment was rewritten by an identical apply; the controller will hot-loop")
			Expect(dep.Generation).To(Equal(depGen))

			svc := mdtGetService(svcKey)
			Expect(svc.ResourceVersion).To(Equal(svcRV),
				"the child Service was rewritten by an identical apply; the controller will hot-loop")
			Expect(svc.Generation).To(Equal(svcGen))

			By("reconciling a third time, for good measure")
			mdtReconcile(r, mdKey)

			dep = mdtGetDeployment(depKey)
			Expect(dep.ResourceVersion).To(Equal(depRV))
			Expect(dep.Generation).To(Equal(depGen))

			svc = mdtGetService(svcKey)
			Expect(svc.ResourceVersion).To(Equal(svcRV))
			Expect(svc.Generation).To(Equal(svcGen))
		})
	})

	Context("when computing status", func() {
		It("reports Pending before any replica is ready, then Available once they are", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace, helpers.WithReplicas(2)))
			mdtReconcile(r, mdKey)

			By("reporting the first-rollout state")
			// envtest runs no Deployment controller, so the child's status is
			// still all zeroes here. That is exactly the "nothing has started
			// yet" case Pending exists for.
			md := mdtGet(mdKey)
			Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhasePending))
			Expect(md.Status.ObservedGeneration).To(Equal(md.Generation))

			mdtExpectCondition(md, inferencev1alpha1.ConditionAvailable, metav1.ConditionFalse)
			mdtExpectCondition(md, inferencev1alpha1.ConditionProgressing, metav1.ConditionTrue)
			mdtExpectCondition(md, inferencev1alpha1.ConditionReady, metav1.ConditionFalse)

			By("giving every condition a reason and the observed generation")
			// A condition without a Reason is unusable by automation, and one
			// carrying a stale observedGeneration silently answers a question
			// about a spec that is no longer current.
			Expect(md.Status.Conditions).NotTo(BeEmpty())
			for _, cond := range md.Status.Conditions {
				Expect(cond.Reason).NotTo(BeEmpty(), "condition %s has no reason", cond.Type)
				Expect(cond.ObservedGeneration).To(Equal(md.Generation),
					"condition %s reports a stale generation", cond.Type)
			}

			By("faking the rollout completing, since nothing in envtest will")
			Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, depKey, 2)).To(Succeed())
			mdtReconcile(r, mdKey)

			md = mdtGet(mdKey)
			Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhaseAvailable))
			Expect(md.Status.ReadyReplicas).To(Equal(int32(2)))

			mdtExpectCondition(md, inferencev1alpha1.ConditionAvailable, metav1.ConditionTrue)
			mdtExpectCondition(md, inferencev1alpha1.ConditionProgressing, metav1.ConditionFalse)
			mdtExpectCondition(md, inferencev1alpha1.ConditionReady, metav1.ConditionTrue)
			mdtExpectCondition(md, inferencev1alpha1.ConditionModelReady, metav1.ConditionTrue)

			By("promoting the serving revision to the rollback target")
			// lastGoodRevision may only advance to a revision that actually
			// reached availability; recording it any earlier would let a broken
			// revision become the thing a future rollback reverts TO.
			Expect(md.Status.StableRevision).NotTo(BeEmpty())
			Expect(md.Status.LastGoodRevision).To(Equal(md.Status.StableRevision))
		})
	})

	Context("when publishing the /scale contract", func() {
		It("serializes status.selector as a selector STRING", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			// The single most common defect in CRD /scale implementations is
			// publishing a structured metav1.LabelSelector (or its JSON) here
			// instead of the serialized form. Nothing complains: the CRD still
			// validates, `kubectl scale` still works, and status looks
			// plausible. Only an HPA reveals it — by parsing garbage, matching
			// zero pods, and computing its scaling target from no metrics at
			// all, which presents as an autoscaler that simply never acts.
			md := mdtGet(mdKey)
			Expect(md.Status.Selector).To(Equal(naming.LabelModelDeployment + "=" + name))
		})

		It("round-trips the scale subresource and propagates the new count", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace, helpers.WithReplicas(1)))
			mdtReconcile(r, mdKey)

			md := mdtGet(mdKey)

			By("reading the scale subresource")
			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, md, scale)).To(Succeed())
			Expect(scale.Spec.Replicas).To(Equal(int32(1)))
			Expect(scale.Status.Selector).To(Equal(naming.LabelModelDeployment + "=" + name))

			By("writing the scale subresource, as `kubectl scale` and an HPA do")
			scale.Spec.Replicas = 4
			Expect(k8sClient.SubResource("scale").Update(ctx, md, client.WithSubResourceBody(scale))).To(Succeed())

			md = mdtGet(mdKey)
			Expect(md.Spec.Replicas).NotTo(BeNil())
			Expect(*md.Spec.Replicas).To(Equal(int32(4)))

			By("propagating the count to the child Deployment on the next pass")
			mdtReconcile(r, mdKey)
			Expect(*mdtGetDeployment(depKey).Spec.Replicas).To(Equal(int32(4)))
		})
	})

	Context("when the spec changes", func() {
		It("records a revision per pod-template change, and none for scaling", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace, helpers.WithContextSize(4096)))
			mdtReconcile(r, mdKey)

			By("recording the first revision")
			Expect(mdtRevisions(namespace, name)).To(HaveLen(1))
			firstRevision := mdtGet(mdKey).Status.StableRevision
			Expect(firstRevision).NotTo(BeEmpty())

			By("changing a pod-template-affecting field")
			md := mdtGet(mdKey)
			md.Spec.Engine.ContextSize = ptr.To(int32(8192))
			Expect(k8sClient.Update(ctx, md)).To(Succeed())
			mdtReconcile(r, mdKey)

			Expect(mdtRevisions(namespace, name)).To(HaveLen(2))
			secondRevision := mdtGet(mdKey).Status.StableRevision
			Expect(secondRevision).NotTo(Equal(firstRevision))

			By("changing only the replica count")
			// Scaling must NOT be a revision. spec.replicas backs /scale, so an
			// HPA writes it; if it were part of the revision identity every
			// autoscaler tick would mint a new revision and roll the whole
			// fleet of otherwise-identical pods.
			md = mdtGet(mdKey)
			md.Spec.Replicas = ptr.To(int32(5))
			Expect(k8sClient.Update(ctx, md)).To(Succeed())
			mdtReconcile(r, mdKey)

			Expect(mdtRevisions(namespace, name)).To(HaveLen(2),
				"scaling minted a new ControllerRevision")
			Expect(mdtGet(mdKey).Status.StableRevision).To(Equal(secondRevision))
			Expect(*mdtGetDeployment(depKey).Spec.Replicas).To(Equal(int32(5)))
		})
	})

	Context("when a rollout stalls", func() {
		It("reports ProgressDeadlineExceeded and goes Degraded", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			// The stall verdict is read from the child Deployment rather than
			// recomputed here, so this fixture is the whole input.
			Expect(helpers.MarkDeploymentStalled(ctx, k8sClient, depKey)).To(Succeed())
			mdtReconcile(r, mdKey)

			md := mdtGet(mdKey)
			progressing := mdtExpectCondition(md, inferencev1alpha1.ConditionProgressing, metav1.ConditionFalse)
			Expect(progressing.Reason).To(Equal(inferencev1alpha1.ReasonProgressDeadlineExceeded))
			Expect(md.Status.Phase).To(Equal(inferencev1alpha1.PhaseDegraded))
		})

		It("keeps the revert once the stall condition clears", func() {
			// The revert is driven by the child Deployment's
			// ProgressDeadlineExceeded condition, and that condition goes away
			// as soon as the reverted template starts rolling out. If the
			// rejection is not remembered independently, the next reconcile
			// re-applies the failed revision, the rollout stalls again, and the
			// operator settles into an endless bad-revision -> stall -> revert
			// loop with a period of spec.rollout.progressDeadline — churning
			// ReplicaSets and pods forever while status flaps.
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			By("banking a known-good revision")
			Expect(helpers.MarkDeploymentAvailable(ctx, k8sClient, depKey, 1)).To(Succeed())
			mdtReconcile(r, mdKey)
			good := mdtGet(mdKey).Status.LastGoodRevision
			Expect(good).NotTo(BeEmpty())

			By("rolling out a revision that stalls")
			md := mdtGet(mdKey)
			md.Spec.Engine.ContextSize = ptr.To(int32(2048))
			Expect(k8sClient.Update(ctx, md)).To(Succeed())
			mdtReconcile(r, mdKey)
			bad := mdtGetDeployment(depKey).Labels[naming.LabelRevision]
			Expect(bad).NotTo(Equal(good))

			Expect(helpers.MarkDeploymentStalled(ctx, k8sClient, depKey)).To(Succeed())
			mdtReconcile(r, mdKey)

			By("reverting to the last known-good revision")
			Expect(mdtGetDeployment(depKey).Labels[naming.LabelRevision]).To(Equal(good))

			By("and STAYING there after the stall condition clears")
			Expect(helpers.MarkDeploymentProgressing(ctx, k8sClient, depKey, 1, 1)).To(Succeed())
			mdtReconcile(r, mdKey)
			Expect(mdtGetDeployment(depKey).Labels[naming.LabelRevision]).To(Equal(good),
				"the rejected revision was re-applied once the Deployment stopped reporting a stall")

			mdtReconcile(r, mdKey)
			Expect(mdtGetDeployment(depKey).Labels[naming.LabelRevision]).To(Equal(good))
		})
	})

	Context("when the spec is one the engine cannot serve", func() {
		It("fails terminally without creating children", func() {
			md := helpers.NewModelDeployment(name, namespace)
			// Valid per the CEL union rule (exactly one source is set), but the
			// llamacpp profile rejects it: PVC-mounted weights are not
			// implemented. This is precisely the class of error CEL cannot
			// express — it depends on which engine was selected.
			md.Spec.Model.Source.Image = nil
			md.Spec.Model.Source.PersistentVolumeClaim = &inferencev1alpha1.PVCModelSource{
				ClaimName: "weights",
				Path:      "/weights/model.gguf",
			}
			mdtCreate(md)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: mdKey})
			Expect(err).To(HaveOccurred())

			By("returning a terminal error so controller-runtime stops retrying")
			// Without this the controller would requeue with exponential
			// backoff forever on input that no amount of retrying can fix.
			Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeTrue(),
				"expected a terminal error, got %T: %v", err, err)

			By("saying so in the status")
			specValid := mdtExpectCondition(mdtGet(mdKey), inferencev1alpha1.ConditionSpecValid, metav1.ConditionFalse)
			Expect(specValid.Reason).To(Equal(inferencev1alpha1.ReasonInvalidSpec))
			Expect(specValid.Message).To(ContainSubstring("persistentVolumeClaim"))

			By("still reporting every condition, so nothing waits on one that never appears")
			// The API contract is that every condition is present from the
			// first reconcile, as Unknown when nothing is known — that is what
			// lets a consumer tell "not yet evaluated" from "evaluated and
			// false". A terminal failure on the FIRST reconcile is the one case
			// where nothing else runs, so it is also the one case where the
			// contract can silently lapse: a client polling for ModelReady
			// would block forever on a condition that is never written.
			final := mdtGet(mdKey)
			for _, tp := range inferencev1alpha1.AllConditionTypes() {
				Expect(apimeta.FindStatusCondition(final.Status.Conditions, tp)).NotTo(BeNil(),
					"condition %s was never reported", tp)
			}

			By("creating no children")
			err = k8sClient.Get(ctx, depKey, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Deployment was created for an invalid spec")
		})
	})

	Context("when the model source is huggingFace", func() {
		// The two implemented sources deliver weights in opposite directions,
		// and the whole difference lives in buildModelDelivery. This asserts
		// the shape of the pod that comes out, because getting it wrong
		// produces a pod that starts and then fails minutes later on a
		// permission error rather than anything that points at the cause.
		It("builds a pod with no init container and a writable model volume", func() {
			md := helpers.NewModelDeployment(name, namespace)
			md.Spec.Model.Source.Image = nil
			md.Spec.Model.Source.HuggingFace = &inferencev1alpha1.HuggingFaceModelSource{
				Repo: testHFRepo,
				File: testHFFile,
			}
			mdtCreate(md)
			mdtReconcile(r, mdKey)

			pod := mdtGetDeployment(depKey).Spec.Template.Spec

			By("omitting the MODEL init container, because the engine is the downloader")
			// The shim is still there: it is a native sidecar, which lives in
			// initContainers regardless of how weights arrive.
			Expect(pod.InitContainers).To(HaveLen(1))
			Expect(pod.InitContainers[0].Name).To(Equal(naming.ShimContainerName))

			By("mounting the model volume writable, because it is the download target")
			Expect(pod.Containers).To(HaveLen(1))
			mounts := pod.Containers[0].VolumeMounts
			Expect(mounts).To(HaveLen(1))
			Expect(mounts[0].Name).To(Equal(engine.ModelVolumeName))
			Expect(mounts[0].ReadOnly).To(BeFalse(),
				"a read-only mount would fail the download with a permission error")

			By("pointing llama.cpp's cache at that volume rather than at $HOME")
			// $HOME is on the root filesystem, which is mounted read-only.
			Expect(pod.Containers[0].Env).To(ContainElement(HaveField("Name", "LLAMA_CACHE")))

			By("keeping the root filesystem read-only all the same")
			Expect(*pod.Containers[0].SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
		})
	})

	Context("when the API server validates the model source union", func() {
		// This proves the union is enforced without any admission webhook: the
		// CEL rule ships in the CRD, so it holds for every client, including
		// ones that never go near this controller.
		DescribeTable("CEL rejects anything but exactly one source",
			func(mutate func(*inferencev1alpha1.ModelDeployment), accepted bool) {
				md := helpers.NewModelDeployment("union-"+mdtSuffix(), namespace)
				mutate(md)

				err := k8sClient.Create(ctx, md)
				if accepted {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
				Expect(err.Error()).To(ContainSubstring(
					"exactly one of image, huggingFace or persistentVolumeClaim must be set"))
			},
			Entry("zero sources", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Model.Source = inferencev1alpha1.ModelSourceSpec{}
			}, false),
			Entry("two sources", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Model.Source.HuggingFace = &inferencev1alpha1.HuggingFaceModelSource{
					Repo: testHFRepo,
					File: testHFFile,
				}
			}, false),
			Entry("exactly one source", func(_ *inferencev1alpha1.ModelDeployment) {}, true),
		)
	})

	Context("when the ModelDeployment is deleted", func() {
		It("adds no finalizer, leaving cleanup to owner-reference garbage collection", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			// A finalizer that is not strictly required is a liability: a bug in
			// its removal path wedges deletion permanently, and this operator
			// needs none — every child carries a controller owner reference, so
			// Kubernetes garbage collection removes them with the parent.
			md := mdtGet(mdKey)
			Expect(md.Finalizers).To(BeEmpty())

			Expect(k8sClient.Delete(ctx, md)).To(Succeed())

			// Deleting is not a reconcile error: the object is simply gone.
			mdtReconcile(r, mdKey)
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, mdKey, &inferencev1alpha1.ModelDeployment{}))).To(BeTrue())

			// NOTE: the children are deliberately NOT asserted gone here.
			// envtest runs no garbage collector, so owner-reference cleanup
			// never happens in this environment — asserting it would either
			// fail or, worse, be written as an Eventually that passes only
			// because it also matches "nothing happened". Child deletion is an
			// end-to-end concern and belongs in test/e2e against a real
			// cluster.
		})
	})

	Context("when rendering the serving pod", func() {
		It("gives the engine all three probes and the model an init container", func() {
			mdtCreate(helpers.NewModelDeployment(name, namespace))
			mdtReconcile(r, mdKey)

			dep := mdtGetDeployment(depKey)
			pod := dep.Spec.Template.Spec

			By("finding the engine container")
			var engineC *corev1.Container
			for i := range pod.Containers {
				if pod.Containers[i].Name == engine.ContainerName {
					engineC = &pod.Containers[i]
				}
			}
			Expect(engineC).NotTo(BeNil(), "no container named %q", engine.ContainerName)
			Expect(engineC.Image).To(Equal(helpers.FixtureEngineImage))

			By("probing the engine's health path on the engine port")
			// The startup probe is the load-bearing one: model load dominates
			// startup and a liveness probe alone would kill the container
			// mid-load, producing a crash loop that looks like a broken image.
			for _, probe := range []*corev1.Probe{engineC.StartupProbe, engineC.ReadinessProbe, engineC.LivenessProbe} {
				Expect(probe).NotTo(BeNil())
				Expect(probe.HTTPGet).NotTo(BeNil())
				Expect(probe.HTTPGet.Path).To(Equal("/health"))
				Expect(probe.HTTPGet.Port.IntValue()).To(Equal(int(naming.EnginePort)))
			}

			By("budgeting the startup probe from spec.serving.startupTimeout")
			md := mdtGet(mdKey)
			Expect(md.Spec.Serving.StartupTimeout).NotTo(BeNil())
			timeout := int32(md.Spec.Serving.StartupTimeout.Seconds())
			period := engineC.StartupProbe.PeriodSeconds
			Expect(period).To(BeNumerically(">", 0))
			// Rounded UP, so the configured budget is never under-honoured.
			Expect(engineC.StartupProbe.FailureThreshold).To(Equal((timeout + period - 1) / period))

			By("delivering the model with an init container, then starting the shim sidecar")
			// Order matters: weights are staged first, then the sidecar starts,
			// then the engine. Reversing the first two would have the shim
			// answering health checks for the minutes a model image takes to
			// copy — reporting a variant as "not ready yet" when it has not
			// begun to exist.
			Expect(pod.InitContainers).To(HaveLen(2))
			Expect(pod.InitContainers[1].Name).To(Equal(naming.ShimContainerName))
			Expect(pod.InitContainers[1].RestartPolicy).NotTo(BeNil(),
				"restartPolicy: Always is what makes this a native sidecar rather than "+
					"an init container the kubelet waits forever for")
			Expect(*pod.InitContainers[1].RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))

			initC := pod.InitContainers[0]
			Expect(initC.Name).To(Equal(modelInitContainerName))
			Expect(initC.Image).To(Equal(helpers.FixtureModelImage))
			Expect(initC.Command).To(HaveLen(3))
			Expect(initC.Command[0]).To(Equal("cp"))
			Expect(initC.Command[1]).To(Equal(helpers.FixtureModelPath))

			By("targeting the Service port BY NAME")
			// A numeric targetPort would have to be edited in lockstep with the
			// container port. Targeting "http" means the metrics shim can take
			// over that container port later with no change to the Service.
			svc := mdtGetService(svcKey)
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].TargetPort.Type).To(Equal(intstr.String),
				"targetPort is a number; it must be the string %q", naming.PortNameHTTP)
			Expect(svc.Spec.Ports[0].TargetPort.StrVal).To(Equal(naming.PortNameHTTP))
		})
	})
})

// ---------------------------------------------------------------------------
// Local helpers. All prefixed `mdt` so they cannot collide with fixtures
// defined by other test files in this package.
// ---------------------------------------------------------------------------

// mdtSuffixCounter makes generated names unique within a namespace.
var mdtSuffixCounter int

// mdtSuffix returns a short unique suffix for a generated object name.
func mdtSuffix() string {
	mdtSuffixCounter++
	return fmt.Sprintf("%d", mdtSuffixCounter)
}

// mdtNewReconciler builds a reconciler over the direct client, with a fake
// recorder (so events are captured, not dropped on a nil check) and a fake
// clock (so nothing in the suite depends on wall time).
func mdtNewReconciler() *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(100),
		Clock:    testingclock.NewFakePassiveClock(time.Now()),
	}
}

// mdtNewNamespace creates a uniquely named namespace and returns its name.
func mdtNewNamespace() string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mdt-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// mdtCreate persists a ModelDeployment and returns it with server-assigned
// fields (UID, generation) populated.
func mdtCreate(md *inferencev1alpha1.ModelDeployment) *inferencev1alpha1.ModelDeployment {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, md)).To(Succeed())
	Expect(md.UID).NotTo(BeEmpty())
	return md
}

// mdtReconcile runs exactly one reconcile pass and requires it to succeed.
func mdtReconcile(r *ModelDeploymentReconciler, key types.NamespacedName) ctrl.Result {
	GinkgoHelper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func mdtGet(key types.NamespacedName) *inferencev1alpha1.ModelDeployment {
	GinkgoHelper()
	md := &inferencev1alpha1.ModelDeployment{}
	Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
	return md
}

func mdtGetDeployment(key types.NamespacedName) *appsv1.Deployment {
	GinkgoHelper()
	dep := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
	return dep
}

func mdtGetService(key types.NamespacedName) *corev1.Service {
	GinkgoHelper()
	svc := &corev1.Service{}
	Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
	return svc
}

// mdtRevisions lists the ControllerRevisions recorded for one ModelDeployment.
func mdtRevisions(namespace, mdName string) []appsv1.ControllerRevision {
	GinkgoHelper()
	list := &appsv1.ControllerRevisionList{}
	Expect(k8sClient.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{naming.LabelModelDeployment: mdName},
	)).To(Succeed())
	return list.Items
}

// mdtVersion returns an object's resourceVersion and generation.
func mdtVersion(obj client.Object) (string, int64) {
	return obj.GetResourceVersion(), obj.GetGeneration()
}

// mdtExpectCondition asserts a condition exists with the given status and
// returns it for further assertions.
func mdtExpectCondition(
	md *inferencev1alpha1.ModelDeployment,
	condType string,
	status metav1.ConditionStatus,
) *metav1.Condition {
	GinkgoHelper()
	cond := apimeta.FindStatusCondition(md.Status.Conditions, condType)
	Expect(cond).NotTo(BeNil(), "condition %s is missing", condType)
	Expect(cond.Status).To(Equal(status), "condition %s has the wrong status", condType)
	return cond
}

// mdtExpectOwnedBy asserts the owner references contain a controller reference
// to md — the mechanism the whole no-finalizer cleanup story rests on.
func mdtExpectOwnedBy(refs []metav1.OwnerReference, md *inferencev1alpha1.ModelDeployment) {
	GinkgoHelper()
	var controller *metav1.OwnerReference
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			controller = &refs[i]
		}
	}
	Expect(controller).NotTo(BeNil(), "no controller owner reference")
	Expect(controller.Kind).To(Equal(kindModelDeployment))
	Expect(controller.APIVersion).To(Equal(inferencev1alpha1.GroupVersion.String()))
	Expect(controller.Name).To(Equal(md.Name))
	// The UID is what makes the reference resolvable. A stale or empty one
	// makes garbage collection delete the child immediately, or never.
	Expect(controller.UID).To(Equal(md.UID))
}
