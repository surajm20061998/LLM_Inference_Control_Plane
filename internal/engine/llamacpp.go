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
	"errors"
	"path"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// LlamaCPPDefaultImage is the image used when EngineSpec.Image is empty.
//
// The tag is pinned to a specific llama.cpp build on purpose. Upstream also
// publishes a floating `:server` tag which is rebuilt many times a day, and a
// pod template referring to it is not reproducible in either direction: two
// replicas created an hour apart can run different engine builds, and a pod
// restarted during an incident can come back as a different binary than the one
// that was serving a minute earlier. Worse for this operator specifically, a
// canary and its primary would no longer differ only by the change under test,
// which is the one assumption the whole rollout comparison rests on.
//
// Bumping this is a deliberate, reviewable commit.
const LlamaCPPDefaultImage = "ghcr.io/ggml-org/llama.cpp:server-b10731"

// LlamaCPPHealthPath is llama-server's readiness endpoint. It returns 503 while
// the model is still loading and 200 once slots are available, which is exactly
// the semantics a readiness probe wants.
const LlamaCPPHealthPath = "/health"

// llamaCPPAPIKeyEnvVar is the environment variable llama-server reads its
// required API key from. Every llama-server CLI flag has a matching
// LLAMA_ARG_* variable; the key is passed this way rather than as an argument
// so the secret never appears in the pod spec, in `kubectl describe`, or in a
// process listing.
const llamaCPPAPIKeyEnvVar = "LLAMA_ARG_API_KEY"

// llamaCPPCacheEnvVar redirects llama.cpp's download cache. It is set only for
// the huggingFace model source; with an image source nothing is downloaded.
const llamaCPPCacheEnvVar = "LLAMA_CACHE"

// llamaCPPHFTokenEnvVar carries a Hugging Face token for gated repositories.
const llamaCPPHFTokenEnvVar = "HF_TOKEN"

// Defaults mirroring the CRD's kubebuilder defaults. The API server normally
// fills these in, but Build must also be correct when handed a spec that never
// went through defaulting — unit tests, and any future dry-run path.
const (
	defaultContextSize    int32 = 4096
	defaultMaxConcurrency int32 = 4
	defaultThreads        int32 = 1
)

// llamaCPP serves GGUF models with llama.cpp's llama-server.
//
// It has no fields: a profile is a pure function collection, and keeping it
// stateless is what makes the single registered instance safe to share across
// concurrent reconciles.
type llamaCPP struct{}

// Compile-time proof that the profile satisfies the interface, so a signature
// drift is a build failure here rather than a nil in the registry.
var _ Profile = llamaCPP{}

func init() {
	Register(llamaCPP{})
}

// Type implements Profile.
func (llamaCPP) Type() v1alpha1.EngineType { return v1alpha1.EngineLlamaCPP }

// DefaultImage implements Profile.
func (llamaCPP) DefaultImage() string { return LlamaCPPDefaultImage }

// Validate implements Profile.
//
// It rejects the model sources this engine cannot serve yet. The union members
// exist in the API from day one so the schema never has to change shape, which
// means the "not built yet" answer has to be given here, at admission-adjacent
// time, rather than by a pod that mysteriously never starts. The wording says
// "not implemented" rather than "unsupported" deliberately: the difference
// between "this will never work" and "this is not built yet" is the difference
// between a user redesigning their setup and a user waiting for a release.
func (llamaCPP) Validate(spec *v1alpha1.ModelDeploymentSpec) error {
	if spec == nil {
		return errors.New("llamacpp: nil spec")
	}

	src := spec.Model.Source

	if src.PersistentVolumeClaim != nil {
		return errors.New("llamacpp: model source .spec.model.source.persistentVolumeClaim is not implemented yet; " +
			"use .spec.model.source.image or .spec.model.source.huggingFace")
	}

	if hf := src.HuggingFace; hf != nil {
		// A Hub repository holds every quantisation of a model side by side —
		// Q4_K_M, Q8_0, BF16 — and llama-server picks one by guessing when it
		// is not told which. That guess is silent, and it changes both the
		// memory footprint and the latency of every pod. Since this operator's
		// whole premise is comparing a canary against its primary on latency,
		// an unpinned quantisation would make the two sides differ by something
		// nobody declared. So the file is required here rather than defaulted.
		if hf.File == "" {
			return errors.New("llamacpp: .spec.model.source.huggingFace.file is required; " +
				"llama.cpp serves a single GGUF file and a repository usually holds several " +
				`quantisations (e.g. "Qwen3-0.6B-Q4_K_M.gguf")`)
		}
		return nil
	}

	if src.Image == nil {
		return errors.New("llamacpp: one of .spec.model.source.image or " +
			".spec.model.source.huggingFace is required")
	}

	return nil
}

// Build implements Profile. See the package comment: the argument order below
// is fixed and must stay fixed, because the resulting container is applied with
// Server-Side Apply and any reordering would read as a change on every
// reconcile.
func (p llamaCPP) Build(bc BuildContext) (BuildResult, error) {
	if bc.MD == nil {
		return BuildResult{}, errors.New("llamacpp: BuildContext.MD is nil")
	}
	spec := &bc.MD.Spec

	port := bc.Port
	if port == 0 {
		port = naming.EnginePort
	}

	image := spec.Engine.Image
	if image == "" {
		image = p.DefaultImage()
	}

	container := corev1.Container{
		Name:            ContainerName,
		Image:           image,
		ImagePullPolicy: spec.Engine.ImagePullPolicy,
		Args:            llamaCPPArgs(spec, bc, port),
		Ports: []corev1.ContainerPort{{
			// Named "engine", not "http". The shim takes the "http" name when
			// it is injected, and container port names must be unique within a
			// pod — so the engine keeps its own name in both configurations
			// rather than swapping between them, which would make the pod
			// template depend on whether a sidecar happens to be present.
			Name:          naming.PortNameEngine,
			ContainerPort: port,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:             llamaCPPEnv(spec, bc.CacheDir),
		VolumeMounts:    llamaCPPVolumeMounts(bc.ModelPath, bc.CacheDir),
		Resources:       *spec.Engine.Resources.DeepCopy(),
		SecurityContext: hardenedSecurityContext(),
	}

	// Probes are deliberately absent: the controller owns their timings, which
	// come from ServingSpec.StartupTimeout and are identical across engines.
	// Only the path is engine-specific, and that is returned here.
	return BuildResult{Container: container, HealthPath: LlamaCPPHealthPath}, nil
}

// llamaCPPArgs builds llama-server's command line.
//
// The slice is appended to in a single fixed sequence — no map is ranged over,
// and no branch reorders an earlier element — so identical input yields an
// identical slice, byte for byte, every time.
func llamaCPPArgs(spec *v1alpha1.ModelDeploymentSpec, bc BuildContext, port int32) []string {
	args := make([]string, 0, 16+len(spec.Engine.ExtraArgs))

	// llama-server binds 127.0.0.1 by default. In a pod that means the port is
	// reachable only from inside the container's own network namespace: kubelet
	// probes fail, the Service has no working endpoint, and the symptom is a
	// pod that logs "server listening" and is never ready. Binding all
	// interfaces is required, and safe here — the container is only reachable
	// through the Service.
	args = append(args, "--host", "0.0.0.0")
	args = append(args, "--port", strconv.Itoa(int(port)))

	// Exactly one of these two branches runs. A pre-staged file is named
	// directly; otherwise the engine is pointed at the Hub and downloads the
	// weights itself into CacheDir (see llamaCPPEnv).
	switch {
	case bc.ModelPath != "":
		args = append(args, "-m", bc.ModelPath)
	case spec.Model.Source.HuggingFace != nil:
		hf := spec.Model.Source.HuggingFace
		args = append(args, "--hf-repo", hf.Repo)
		args = append(args, "--hf-file", hf.File)
	}

	args = append(args, "-c", strconv.Itoa(int(int32OrDefault(spec.Engine.ContextSize, defaultContextSize))))
	args = append(args, "--parallel", strconv.Itoa(int(int32OrDefault(spec.Engine.MaxConcurrency, defaultMaxConcurrency))))

	// Resolved by the controller via ThreadsFor and passed in; never re-derived
	// here. See threads.go for why this flag is not optional.
	args = append(args, "-t", strconv.Itoa(int(atLeastOneThread(orDefaultThreads(bc.Threads)))))

	// Always on. The endpoint costs nothing when nobody scrapes it, and having
	// it unconditionally present means enabling metric-driven canary analysis
	// later does not require restarting every pod in the fleet to turn it on.
	args = append(args, "--metrics")

	// Without an alias, GET /v1/models reports the model's file path. Clients
	// pass spec.model.name in their requests, and that is also the `model`
	// label on every metric, so the served identity is made to match it.
	args = append(args, "--alias", spec.Model.Name)

	// Verbatim, last. Later flags win in llama-server's parser, so appending
	// here is what makes ExtraArgs an escape hatch that can override anything
	// above it rather than a list that silently loses to our defaults. The
	// caller's order is preserved exactly.
	args = append(args, spec.Engine.ExtraArgs...)

	return args
}

// llamaCPPEnv copies the user's environment variables, preserving their order,
// and appends the API key reference.
//
// The copy is deep because EnvVar contains pointers (ValueFrom): a shallow copy
// would alias the caller's spec, and a later mutation of the built container
// would reach back into the ModelDeployment in the informer cache.
func llamaCPPEnv(spec *v1alpha1.ModelDeploymentSpec, cacheDir string) []corev1.EnvVar {
	hf := spec.Model.Source.HuggingFace
	hfToken := hf != nil && hf.TokenSecretRef != nil

	if len(spec.Engine.Env) == 0 && spec.Engine.APIKeySecretRef == nil && cacheDir == "" && !hfToken {
		// nil rather than an empty slice: an empty slice and a nil slice
		// serialize differently, and the API server would normalise one to the
		// other, producing a permanent diff between applied and observed.
		return nil
	}

	env := make([]corev1.EnvVar, 0, len(spec.Engine.Env)+3)
	for i := range spec.Engine.Env {
		env = append(env, *spec.Engine.Env[i].DeepCopy())
	}

	if ref := spec.Engine.APIKeySecretRef; ref != nil {
		env = append(env, corev1.EnvVar{
			Name: llamaCPPAPIKeyEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: ref.DeepCopy(),
			},
		})
	}

	// Redirect the download cache onto the mounted volume. Without this,
	// llama.cpp caches under the user's home directory, which lives on the
	// container's root filesystem — mounted read-only here — so the download
	// would fail with a permission error that reads like a corrupt image.
	if cacheDir != "" {
		env = append(env, corev1.EnvVar{
			Name: llamaCPPCacheEnvVar,
			// The trailing separator is defensive. llama.cpp appends one
			// itself before concatenating the file name onto this value, but
			// the failure mode if a build ever did not is nasty and silent:
			// "/models" would become "/modelsQwen3-0.6B-Q4_K_M.gguf", a write
			// to the read-only root filesystem rather than to the volume, and
			// the error surfaces as a puzzling permission denial at startup.
			Value: ensureTrailingSlash(cacheDir),
		})
	}

	// Passed by environment rather than as --hf-token so the credential never
	// appears in the pod spec, in `kubectl describe`, or in a process listing.
	if hfToken {
		env = append(env, corev1.EnvVar{
			Name: llamaCPPHFTokenEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: hf.TokenSecretRef.DeepCopy(),
			},
		})
	}

	return env
}

// ensureTrailingSlash appends a "/" when one is absent. See the LLAMA_CACHE
// note above.
func ensureTrailingSlash(dir string) string {
	if strings.HasSuffix(dir, "/") {
		return dir
	}
	return dir + "/"
}

// llamaCPPVolumeMounts mounts the model volume when there is a model file.
//
// The mount point is the model file's PARENT directory, not the file itself:
// Kubernetes mounts volumes at directories, and GGUF models split across
// multiple shards live as siblings that llama.cpp opens by scanning that
// directory. Read-only both because weights are never written and because it
// keeps the mount compatible with ReadOnlyRootFilesystem below.
func llamaCPPVolumeMounts(modelPath, cacheDir string) []corev1.VolumeMount {
	switch {
	case modelPath != "":
		return []corev1.VolumeMount{{
			Name:      ModelVolumeName,
			MountPath: path.Dir(modelPath),
			ReadOnly:  true,
		}}
	case cacheDir != "":
		// Writable, necessarily: this is the directory the engine downloads
		// into. It is the ONLY writable path in the container — the root
		// filesystem stays read-only — so a compromised engine can still not
		// modify its own binary.
		return []corev1.VolumeMount{{
			Name:      ModelVolumeName,
			MountPath: cacheDir,
		}}
	default:
		return nil
	}
}

// hardenedSecurityContext returns the restricted-profile security context every
// engine container runs with.
//
// An inference server parses untrusted input (prompts, and model files that may
// come from a public registry) in a large C++ codebase. Dropping every
// capability, refusing privilege escalation, running as non-root and making the
// root filesystem read-only is what makes this pod admissible under the
// restricted Pod Security Standard, and turns a memory-safety bug in the engine
// into a crashed container rather than a foothold on the node.
func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		RunAsNonRoot:             boolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// int32OrDefault dereferences p, falling back to def when p is nil or holds a
// non-positive value.
//
// The CRD defaults these fields and CEL enforces their minimums, so in a real
// cluster p is never nil. Build is nonetheless correct for a hand-built spec
// that never passed through the API server — a unit test, or a future dry-run
// path — because emitting "-c 0" would be a confusing runtime failure rather
// than a validation error.
func int32OrDefault(p *int32, def int32) int32 {
	if p == nil || *p < 1 {
		return def
	}
	return *p
}

// orDefaultThreads guards against a BuildContext built without calling
// ThreadsFor. Profiles must not re-derive the thread count, but they must also
// never emit "-t 0".
func orDefaultThreads(n int32) int32 {
	if n < 1 {
		return defaultThreads
	}
	return n
}

// boolPtr returns a pointer to b. Declared locally rather than pulled from
// k8s.io/utils/ptr to keep this package's dependency set to what go.mod already
// requires directly.
func boolPtr(b bool) *bool { return &b }
