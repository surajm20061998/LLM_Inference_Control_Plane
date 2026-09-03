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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

// The CEL acceptance matrix.
//
// The structural check in cel_test.go proves no rule is UNSATISFIABLE. This one
// proves each rule actually rejects what it is supposed to, and accepts what it
// is supposed to — against a real API server, where the defaulting and the
// validation happen in the order they will in a cluster.
//
// Both are needed. A rule can be perfectly satisfiable and still guard the
// wrong thing, and a rule with a beautiful message can be one nothing reaches.
var _ = Describe("CEL validation", func() {
	var namespace string

	BeforeEach(func() { namespace = mdtNewNamespace() })

	// apply attempts to create a ModelDeployment and returns the error, if any.
	apply := func(name string, opts ...helpers.MDOption) error {
		md := helpers.NewModelDeployment(name, namespace, opts...)
		return k8sClient.Create(ctx, md)
	}

	Describe("the model source union", func() {
		It("accepts exactly one member", func() {
			Expect(apply("src-one")).To(Succeed())
		})

		It("rejects two members", func() {
			err := apply("src-two", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Model.Source.HuggingFace = &inferencev1alpha1.HuggingFaceModelSource{
					Repo: "unsloth/Qwen3-0.6B-GGUF", File: "Qwen3-0.6B-Q4_K_M.gguf",
				}
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of"))
		})

		It("rejects zero members", func() {
			err := apply("src-none", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Model.Source.Image = nil
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of"))
		})
	})

	Describe("the canary step ladder", func() {
		It("accepts stepWeights alone", func() {
			// The regression case. A kubebuilder default on stepWeight made this
			// combination permanently invalid, because the API server set a
			// field the user never wrote and the has() rule then saw both.
			Expect(apply("ladder-weights", helpers.WithCanary())).To(Succeed())
		})

		It("accepts stepWeight alone", func() {
			Expect(apply("ladder-step", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.StepWeights = nil
					c.StepWeight = ptr.To(int32(25))
					c.MaxWeight = ptr.To(int32(75))
				}))).To(Succeed())
		})

		It("rejects both", func() {
			err := apply("ladder-both", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.StepWeight = ptr.To(int32(25))
				}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at most one of stepWeight and stepWeights"))
		})

		It("rejects a weight outside 1..100", func() {
			err := apply("ladder-range", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.StepWeights = []int32{20, 140}
				}))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("the canary strategy", func() {
		It("rejects type Canary with no canary block", func() {
			// Otherwise the controller would have to invent a rollout policy,
			// and the user would get a progressive delivery they never
			// configured.
			err := apply("canary-missing", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Rollout.Type = inferencev1alpha1.RolloutCanary
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.rollout.canary is required"))
		})

		It("accepts a canary block on a RollingUpdate, which is simply unused", func() {
			// Deliberately allowed: it lets someone stage a canary
			// configuration and switch the strategy on separately, which is a
			// perfectly reasonable GitOps workflow.
			Expect(apply("canary-staged", helpers.WithCanary(),
				func(md *inferencev1alpha1.ModelDeployment) {
					md.Spec.Rollout.Type = inferencev1alpha1.RolloutRollingUpdate
				})).To(Succeed())
		})
	})

	Describe("analysis metrics", func() {
		It("rejects a metric with neither builtin nor query", func() {
			err := apply("metric-neither", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.Analysis.Metrics = []inferencev1alpha1.AnalysisMetric{{
						Name:           "broken",
						ThresholdRange: inferencev1alpha1.ThresholdRange{Max: helpers.QuantityPtr("1")},
					}}
				}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of builtin and query"))
		})

		It("rejects a metric with both", func() {
			builtin := inferencev1alpha1.MetricTTFTP95
			err := apply("metric-both", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.Analysis.Metrics = []inferencev1alpha1.AnalysisMetric{{
						Name:           "ambiguous",
						Builtin:        &builtin,
						Query:          "vector(1)",
						ThresholdRange: inferencev1alpha1.ThresholdRange{Max: helpers.QuantityPtr("1")},
					}}
				}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of builtin and query"))
		})

		It("rejects an unbounded threshold range", func() {
			// A range with neither bound accepts every value, so the check
			// would pass unconditionally — a gate that is decoration.
			builtin := inferencev1alpha1.MetricErrorRate
			err := apply("metric-unbounded", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.Analysis.Metrics = []inferencev1alpha1.AnalysisMetric{{
						Name:    "unbounded",
						Builtin: &builtin,
					}}
				}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("thresholdRange"))
		})
	})

	Describe("canary scale", func() {
		It("rejects both replicas and matchTrafficWeight", func() {
			err := apply("scale-both", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.Scale = &inferencev1alpha1.CanaryScaleSpec{
						Replicas:           ptr.To(int32(2)),
						MatchTrafficWeight: ptr.To(true),
					}
				}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at most one of replicas and matchTrafficWeight"))
		})

		It("accepts a fixed replica count alone", func() {
			Expect(apply("scale-fixed", helpers.WithCanary(
				func(c *inferencev1alpha1.CanarySpec) {
					c.Scale = &inferencev1alpha1.CanaryScaleSpec{Replicas: ptr.To(int32(2))}
				}))).To(Succeed())
		})
	})

	Describe("autoscaling bounds", func() {
		It("rejects maxReplicas below minReplicas", func() {
			err := apply("auto-inverted", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Autoscaling = &inferencev1alpha1.AutoscalingSpec{
					Mode: inferencev1alpha1.AutoscalingBuiltin, MinReplicas: 5, MaxReplicas: 2,
				}
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maxReplicas must be greater than or equal"))
		})

		It("accepts equal bounds, which pins the fleet", func() {
			Expect(apply("auto-pinned", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Autoscaling = &inferencev1alpha1.AutoscalingSpec{
					Mode: inferencev1alpha1.AutoscalingBuiltin, MinReplicas: 3, MaxReplicas: 3,
				}
			})).To(Succeed())
		})

		It("rejects an autoscaling block with no maxReplicas", func() {
			// An autoscaler without a ceiling is a budget incident waiting for a
			// traffic spike. maxReplicas is plainly required rather than
			// CEL-guarded, which is only possible because spec.autoscaling is a
			// nil-able pointer — see ADR-0005.
			err := apply("auto-nomax", func(md *inferencev1alpha1.ModelDeployment) {
				md.Spec.Autoscaling = &inferencev1alpha1.AutoscalingSpec{
					Mode: inferencev1alpha1.AutoscalingBuiltin, MinReplicas: 1,
				}
			})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("defaulting", func() {
		It("fills in the values the controller depends on", func() {
			name := "defaults-" + mdtSuffix()
			md := &inferencev1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: inferencev1alpha1.ModelDeploymentSpec{
					Model: inferencev1alpha1.ModelSpec{
						Name: "m",
						Source: inferencev1alpha1.ModelSourceSpec{
							Image: &inferencev1alpha1.ImageModelSource{Image: "example/model:v1"},
						},
					},
					Engine: inferencev1alpha1.EngineSpec{},
				},
			}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())

			stored := mdtGet(types.NamespacedName{Namespace: namespace, Name: name})

			Expect(stored.Spec.Replicas).NotTo(BeNil())
			Expect(*stored.Spec.Replicas).To(Equal(int32(1)))
			Expect(stored.Spec.Engine.Type).To(Equal(inferencev1alpha1.EngineLlamaCPP))
			Expect(stored.Spec.Serving.Port).To(Equal(int32(8080)))
			Expect(stored.Spec.Model.Source.Image.Path).To(Equal("/weights/model.gguf"))

			By("defaulting the shim ON")
			// The default that matters most: metrics off by accident means every
			// rollout decision is made blind.
			Expect(stored.Spec.Serving.ShimEnabled()).To(BeTrue())
		})
	})
})
