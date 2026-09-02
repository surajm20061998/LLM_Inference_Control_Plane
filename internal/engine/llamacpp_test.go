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

package engine

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/naming"
)

// Fixture values shared by the tests below. Spelling each one once means two
// tests cannot silently disagree about what the fixture contains.
const (
	testModelPath = "/models/model.gguf"
	testModelName = "demo-model"

	apiKeySecretName = "llm-api-key"
	apiKeySecretKey  = "key"

	// Two llama-server flags the builder knows nothing about, used to prove
	// spec.engine.extraArgs reaches the command line untouched.
	flagFlashAttn = "--flash-attn"
	flagNoMmap    = "--no-mmap"

	// Deliberately not in alphabetical order: the builder must preserve the
	// caller's order, and sorted output would be the bug these names catch.
	envNameLast  = "ZZZ_LAST"
	envNameFirst = "AAA_FIRST"
)

// llamaArgs describes an expected llama-server command line by the values that
// differ between cases. The flag spellings and the order they must appear in
// are written out exactly once, in build(), so a renamed or reordered flag
// still fails every case below — without the whole vector being repeated for
// each one.
type llamaArgs struct {
	port      string
	modelPath string // empty omits -m entirely
	ctxSize   string
	parallel  string
	threads   string
	alias     string
	extra     []string
}

// defaultLlamaArgs is the command line testModelDeployment and
// testBuildContext must produce between them. Each case states its difference
// from it.
func defaultLlamaArgs() llamaArgs {
	return llamaArgs{
		port:      "8000",
		modelPath: testModelPath,
		ctxSize:   "4096",
		parallel:  "4",
		threads:   "2",
		alias:     testModelName,
	}
}

func (a llamaArgs) build() []string {
	args := []string{
		"--host", "0.0.0.0",
		"--port", a.port,
	}
	if a.modelPath != "" {
		args = append(args, "-m", a.modelPath)
	}
	args = append(args,
		"-c", a.ctxSize,
		"--parallel", a.parallel,
		"-t", a.threads,
		"--metrics",
		"--alias", a.alias,
	)
	return append(args, a.extra...)
}

func TestLlamaCPPBuildArgs(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*v1alpha1.ModelDeployment)
		bc        func(*BuildContext)
		wantArgs  func(*llamaArgs)
		wantImage string
	}{
		{
			name:      "representative spec",
			wantImage: LlamaCPPDefaultImage,
		},
		{
			name: "engine image override",
			mutate: func(md *v1alpha1.ModelDeployment) {
				md.Spec.Engine.Image = "registry.example.com/stub-server:v1"
			},
			wantImage: "registry.example.com/stub-server:v1",
		},
		{
			name: "context size, concurrency, threads and port all customised",
			mutate: func(md *v1alpha1.ModelDeployment) {
				md.Spec.Engine.ContextSize = int32Ptr(32768)
				md.Spec.Engine.MaxConcurrency = int32Ptr(16)
			},
			bc: func(bc *BuildContext) {
				bc.Threads = 9
				bc.Port = 9001
			},
			wantArgs: func(a *llamaArgs) {
				a.port, a.ctxSize, a.parallel, a.threads = "9001", "32768", "16", "9"
			},
			wantImage: LlamaCPPDefaultImage,
		},
		{
			name: "extra args land last, verbatim and in order",
			mutate: func(md *v1alpha1.ModelDeployment) {
				md.Spec.Engine.ExtraArgs = []string{flagFlashAttn, flagNoMmap, "-c", "8192"}
			},
			wantArgs: func(a *llamaArgs) {
				a.extra = []string{flagFlashAttn, flagNoMmap, "-c", "8192"}
			},
			wantImage: LlamaCPPDefaultImage,
		},
		{
			name: "alias carries the model name",
			mutate: func(md *v1alpha1.ModelDeployment) {
				md.Spec.Model.Name = "qwen3-0.6b"
			},
			wantArgs: func(a *llamaArgs) {
				a.alias = "qwen3-0.6b"
			},
			wantImage: LlamaCPPDefaultImage,
		},
		{
			name: "no -m when the engine fetches its own weights",
			bc: func(bc *BuildContext) {
				bc.ModelPath = ""
			},
			wantArgs: func(a *llamaArgs) {
				a.modelPath = ""
			},
			wantImage: LlamaCPPDefaultImage,
		},
		{
			name: "undefaulted spec falls back to the CRD defaults",
			mutate: func(md *v1alpha1.ModelDeployment) {
				md.Spec.Engine.ContextSize = nil
				md.Spec.Engine.MaxConcurrency = nil
			},
			bc: func(bc *BuildContext) {
				bc.Threads = 0
				bc.Port = 0
			},
			wantArgs: func(a *llamaArgs) {
				a.threads = "1"
			},
			wantImage: LlamaCPPDefaultImage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := testModelDeployment()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			bc := testBuildContext(md)
			if tc.bc != nil {
				tc.bc(&bc)
			}

			want := defaultLlamaArgs()
			if tc.wantArgs != nil {
				tc.wantArgs(&want)
			}
			wantArgs := want.build()

			got, err := mustProfile(t).Build(bc)
			if err != nil {
				t.Fatalf("Build() returned error: %v", err)
			}

			if !reflect.DeepEqual(got.Container.Args, wantArgs) {
				t.Errorf("args mismatch\n got: %q\nwant: %q", got.Container.Args, wantArgs)
			}
			if got.Container.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", got.Container.Image, tc.wantImage)
			}
		})
	}
}

func TestLlamaCPPBuildContainerShape(t *testing.T) {
	md := testModelDeployment()
	md.Spec.Engine.ImagePullPolicy = corev1.PullAlways
	md.Spec.Engine.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}

	got, err := mustProfile(t).Build(testBuildContext(md))
	if err != nil {
		t.Fatalf("Build() returned error: %v", err)
	}
	c := got.Container

	if c.Name != ContainerName {
		t.Errorf("container name = %q, want %q", c.Name, ContainerName)
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("imagePullPolicy = %q, want %q", c.ImagePullPolicy, corev1.PullAlways)
	}
	if got.HealthPath != "/health" {
		t.Errorf("HealthPath = %q, want %q", got.HealthPath, "/health")
	}

	// Ports.
	wantPorts := []corev1.ContainerPort{{
		Name:          naming.PortNameHTTP,
		ContainerPort: naming.EnginePort,
		Protocol:      corev1.ProtocolTCP,
	}}
	if !reflect.DeepEqual(c.Ports, wantPorts) {
		t.Errorf("ports = %#v, want %#v", c.Ports, wantPorts)
	}

	// Volume mount: the model file's DIRECTORY, read-only.
	wantMounts := []corev1.VolumeMount{{
		Name:      ModelVolumeName,
		MountPath: "/models",
		ReadOnly:  true,
	}}
	if !reflect.DeepEqual(c.VolumeMounts, wantMounts) {
		t.Errorf("volumeMounts = %#v, want %#v", c.VolumeMounts, wantMounts)
	}

	// Resources are copied through verbatim.
	if !apiEqual(&c.Resources, &md.Spec.Engine.Resources) {
		t.Errorf("resources = %#v, want %#v", c.Resources, md.Spec.Engine.Resources)
	}

	// Probes stay the controller's business.
	if c.LivenessProbe != nil || c.ReadinessProbe != nil || c.StartupProbe != nil {
		t.Error("Build set a probe; probe construction belongs to the controller")
	}
}

func TestLlamaCPPBuildVolumeMountUsesParentDirectory(t *testing.T) {
	tests := []struct {
		name          string
		modelPath     string
		wantMountPath string
		wantNoMount   bool
	}{
		{name: "default path", modelPath: testModelPath, wantMountPath: "/models"},
		{name: "nested path", modelPath: "/data/weights/q4/model.gguf", wantMountPath: "/data/weights/q4"},
		{name: "root-level file", modelPath: "/model.gguf", wantMountPath: "/"},
		{name: "no model file", modelPath: "", wantNoMount: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bc := testBuildContext(testModelDeployment())
			bc.ModelPath = tc.modelPath

			got, err := mustProfile(t).Build(bc)
			if err != nil {
				t.Fatalf("Build() returned error: %v", err)
			}

			if tc.wantNoMount {
				if got.Container.VolumeMounts != nil {
					t.Fatalf("volumeMounts = %#v, want nil", got.Container.VolumeMounts)
				}
				return
			}

			if len(got.Container.VolumeMounts) != 1 {
				t.Fatalf("got %d volume mounts, want 1", len(got.Container.VolumeMounts))
			}
			m := got.Container.VolumeMounts[0]
			if m.Name != ModelVolumeName {
				t.Errorf("mount name = %q, want %q", m.Name, ModelVolumeName)
			}
			if m.MountPath != tc.wantMountPath {
				t.Errorf("mountPath = %q, want %q", m.MountPath, tc.wantMountPath)
			}
			if !m.ReadOnly {
				t.Error("model volume mount is not read-only")
			}
		})
	}
}

func TestLlamaCPPBuildSecurityContext(t *testing.T) {
	got, err := mustProfile(t).Build(testBuildContext(testModelDeployment()))
	if err != nil {
		t.Fatalf("Build() returned error: %v", err)
	}

	sc := got.Container.SecurityContext
	if sc == nil {
		t.Fatal("SecurityContext is nil; the engine container must run under the restricted profile")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation is not false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem is not true")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot is not true")
	}
	if sc.Capabilities == nil || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("capabilities = %#v, want Drop: [ALL]", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccompProfile = %#v, want RuntimeDefault", sc.SeccompProfile)
	}
}

func TestLlamaCPPBuildEnv(t *testing.T) {
	userEnv := []corev1.EnvVar{
		{Name: envNameLast, Value: "z"},
		{Name: envNameFirst, Value: "a"},
		{Name: "FROM_FIELD", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		}},
	}
	apiKeyRef := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
		Key:                  apiKeySecretKey,
	}

	tests := []struct {
		name    string
		env     []corev1.EnvVar
		keyRef  *corev1.SecretKeySelector
		wantEnv []corev1.EnvVar
	}{
		{
			name:    "no env and no api key yields nil",
			wantEnv: nil,
		},
		{
			name: "user env preserved in the caller's order",
			env:  userEnv,
			wantEnv: []corev1.EnvVar{
				{Name: envNameLast, Value: "z"},
				{Name: envNameFirst, Value: "a"},
				{Name: "FROM_FIELD", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				}},
			},
		},
		{
			name:   "api key appended as a secret reference",
			keyRef: apiKeyRef,
			wantEnv: []corev1.EnvVar{
				{Name: llamaCPPAPIKeyEnvVar, ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
						Key:                  apiKeySecretKey,
					},
				}},
			},
		},
		{
			name:   "api key follows the user's env",
			env:    userEnv[:2],
			keyRef: apiKeyRef,
			wantEnv: []corev1.EnvVar{
				{Name: envNameLast, Value: "z"},
				{Name: envNameFirst, Value: "a"},
				{Name: llamaCPPAPIKeyEnvVar, ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
						Key:                  apiKeySecretKey,
					},
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := testModelDeployment()
			md.Spec.Engine.Env = tc.env
			md.Spec.Engine.APIKeySecretRef = tc.keyRef

			got, err := mustProfile(t).Build(testBuildContext(md))
			if err != nil {
				t.Fatalf("Build() returned error: %v", err)
			}

			if !reflect.DeepEqual(got.Container.Env, tc.wantEnv) {
				t.Errorf("env mismatch\n got: %#v\nwant: %#v", got.Container.Env, tc.wantEnv)
			}

			// The API key must never be visible on the command line.
			for _, a := range got.Container.Args {
				if strings.Contains(a, apiKeySecretName) || strings.Contains(a, "--api-key") {
					t.Errorf("api key leaked into args: %q", got.Container.Args)
				}
			}
		})
	}
}

// TestLlamaCPPBuildDoesNotAliasSpec asserts Build deep-copies everything it
// takes from the spec. The ModelDeployment it is handed comes out of the shared
// informer cache; mutating it through an aliased slice or pointer would corrupt
// every other reader in the process.
func TestLlamaCPPBuildDoesNotAliasSpec(t *testing.T) {
	md := testModelDeployment()
	md.Spec.Engine.Env = []corev1.EnvVar{{Name: "KEEP", Value: "original"}}
	md.Spec.Engine.ExtraArgs = []string{flagFlashAttn}
	md.Spec.Engine.APIKeySecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
		Key:                  apiKeySecretKey,
	}
	md.Spec.Engine.Resources = corev1.ResourceRequirements{Limits: cpu("2")}
	before := md.DeepCopy()

	got, err := mustProfile(t).Build(testBuildContext(md))
	if err != nil {
		t.Fatalf("Build() returned error: %v", err)
	}

	// Mutate every mutable part of the result.
	got.Container.Env[0].Value = "mutated"
	got.Container.Env[1].ValueFrom.SecretKeyRef.Name = "mutated"
	got.Container.Args[len(got.Container.Args)-1] = "--mutated"
	got.Container.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("64")

	if !apiEqual(before, md) {
		t.Errorf("Build aliased the spec; mutating the result changed the input:\n before: %#v\n  after: %#v",
			before.Spec.Engine, md.Spec.Engine)
	}
}

// TestBuildIsDeterministic is the guard described in the package comment. If
// Build ever ranges over a map to produce output, Go's randomised map iteration
// order makes this fail — and in production it would instead make the
// controller and the API server rewrite the same Deployment forever.
func TestBuildIsDeterministic(t *testing.T) {
	md := testModelDeployment()
	md.Spec.Engine.Env = []corev1.EnvVar{
		{Name: "OMP_NUM_THREADS", Value: "2"},
		{Name: "GGML_LOG_LEVEL", Value: "info"},
		{Name: "ALPHA", Value: "1"},
	}
	md.Spec.Engine.ExtraArgs = []string{flagFlashAttn, flagNoMmap}
	md.Spec.Engine.APIKeySecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
		Key:                  apiKeySecretKey,
	}
	md.Spec.Engine.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("4"),
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}

	p := mustProfile(t)
	bc := testBuildContext(md)

	first, err := p.Build(bc)
	if err != nil {
		t.Fatalf("Build() returned error: %v", err)
	}

	for i := 1; i < 100; i++ {
		got, err := p.Build(bc)
		if err != nil {
			t.Fatalf("Build() iteration %d returned error: %v", i, err)
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("Build is not deterministic at iteration %d:\n first: %#v\n   got: %#v", i, first, got)
		}
		if !apiequality.Semantic.DeepEqual(first, got) {
			t.Fatalf("Build is not semantically deterministic at iteration %d", i)
		}
	}
}

func TestLlamaCPPValidate(t *testing.T) {
	tests := []struct {
		name        string
		source      v1alpha1.ModelSourceSpec
		wantErr     bool
		wantContain []string
	}{
		{
			name: "image source accepted",
			source: v1alpha1.ModelSourceSpec{
				Image: &v1alpha1.ImageModelSource{Image: "example.com/models:v1", Path: testModelPath},
			},
		},
		{
			name: "huggingface rejected as unimplemented",
			source: v1alpha1.ModelSourceSpec{
				HuggingFace: &v1alpha1.HuggingFaceModelSource{Repo: "unsloth/Qwen3-0.6B-GGUF"},
			},
			wantErr:     true,
			wantContain: []string{"huggingFace", "not implemented"},
		},
		{
			name: "pvc rejected as unimplemented",
			source: v1alpha1.ModelSourceSpec{
				PersistentVolumeClaim: &v1alpha1.PVCModelSource{ClaimName: "weights", Path: "/w/model.gguf"},
			},
			wantErr:     true,
			wantContain: []string{"persistentVolumeClaim", "not implemented"},
		},
		{
			name:        "nil image source rejected",
			source:      v1alpha1.ModelSourceSpec{},
			wantErr:     true,
			wantContain: []string{"image", "required"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &v1alpha1.ModelDeploymentSpec{
				Model: v1alpha1.ModelSpec{Name: testModelName, Source: tc.source},
			}

			err := mustProfile(t).Validate(spec)
			if tc.wantErr == (err == nil) {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestLlamaCPPValidateNilSpec(t *testing.T) {
	if err := mustProfile(t).Validate(nil); err == nil {
		t.Error("Validate(nil) returned no error")
	}
}

func TestLlamaCPPBuildNilModelDeployment(t *testing.T) {
	if _, err := mustProfile(t).Build(BuildContext{}); err == nil {
		t.Error("Build with a nil ModelDeployment returned no error")
	}
}

// TestLlamaCPPDefaultImageIsPinned guards the reproducibility property the
// const comment explains: a floating tag makes two replicas of the same
// revision potentially different binaries.
func TestLlamaCPPDefaultImageIsPinned(t *testing.T) {
	img := mustProfile(t).DefaultImage()

	tag := img[strings.LastIndex(img, ":")+1:]
	if tag == "server" || tag == "latest" || !strings.Contains(img, ":") {
		t.Errorf("DefaultImage() = %q, which is a floating tag; pin it to a build", img)
	}
	if img != LlamaCPPDefaultImage {
		t.Errorf("DefaultImage() = %q, want %q", img, LlamaCPPDefaultImage)
	}
}

// --- helpers ---------------------------------------------------------------

// testModelDeployment returns a fully defaulted ModelDeployment, as the API
// server would hand one to the controller.
func testModelDeployment() *v1alpha1.ModelDeployment {
	return &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: v1alpha1.ModelDeploymentSpec{
			Replicas: int32Ptr(1),
			Model: v1alpha1.ModelSpec{
				Name: testModelName,
				Source: v1alpha1.ModelSourceSpec{
					Image: &v1alpha1.ImageModelSource{
						Image:      "example.com/models/demo:v1",
						Path:       testModelPath,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
			Engine: v1alpha1.EngineSpec{
				Type:            v1alpha1.EngineLlamaCPP,
				ImagePullPolicy: corev1.PullIfNotPresent,
				ContextSize:     int32Ptr(4096),
				MaxConcurrency:  int32Ptr(4),
			},
			Serving: v1alpha1.ServingSpec{Port: naming.ServicePort},
		},
	}
}

func testBuildContext(md *v1alpha1.ModelDeployment) BuildContext {
	return BuildContext{
		MD:        md,
		Variant:   v1alpha1.VariantPrimary,
		ModelPath: testModelPath,
		Threads:   2,
		Port:      naming.EnginePort,
	}
}

func mustProfile(t *testing.T) Profile {
	t.Helper()
	p, err := Get(v1alpha1.EngineLlamaCPP)
	if err != nil {
		t.Fatalf("Get(%q) returned error: %v", v1alpha1.EngineLlamaCPP, err)
	}
	return p
}

// apiEqual compares Kubernetes API objects with the semantics the API server
// uses, so that e.g. two resource.Quantity values written differently but
// meaning the same thing compare equal.
func apiEqual(a, b any) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}
