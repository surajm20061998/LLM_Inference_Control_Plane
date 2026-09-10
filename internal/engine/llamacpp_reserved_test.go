package engine

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestLlamaCPPReservedSettings(t *testing.T) {
	// Explicit fixtures keep this contract independent of the implementation's
	// map. Both Validate and Build must protect against historical/direct input.
	flags := []string{"-m", "--model", "--model-url", "-mu", "-dr", "--docker-repo",
		"-hf", "-hfr", "--hf-repo", "-hff", "--hf-file", "-hft", "--hf-token",
		llamaCPPHostFlag, llamaCPPPortFlag, "-a", "--alias", "-np", llamaCPPParallelFlag, "-t", "--threads",
		"-tb", "--threads-batch", "-c", "--ctx-size", "--metrics", "--api-key", "--api-key-file",
		"--models-dir", "--models-preset"}
	for _, flag := range flags {
		forms := [][]string{{flag, "private-value"}, {flag + "=private-value"}}
		if strings.HasPrefix(flag, "--") && strings.Contains(flag[2:], "-") {
			forms = append(forms, []string{"--" + strings.ReplaceAll(flag[2:], "-", "_") + "=private-value"})
		}
		for _, args := range forms {
			t.Run(args[0], func(t *testing.T) {
				md := testModelDeployment()
				md.Spec.Engine.ExtraArgs = args
				if err := mustProfile(t).Validate(&md.Spec); err == nil || strings.Contains(err.Error(), "private-value") {
					t.Fatalf("Validate must reject setting without exposing value: %v", err)
				}
				if _, err := mustProfile(t).Build(testBuildContext(md)); err == nil {
					t.Fatal("Build accepted a reserved setting")
				}
			})
		}
	}
	envNames := []string{
		"LLAMA_ARG_MODEL", "LLAMA_ARG_MODEL_URL", "LLAMA_ARG_DOCKER_REPO",
		"LLAMA_ARG_HF_REPO", "LLAMA_ARG_HF_FILE", "HF_TOKEN", "LLAMA_ARG_HOST",
		"LLAMA_ARG_PORT", "LLAMA_ARG_ALIAS", "LLAMA_ARG_N_PARALLEL",
		"LLAMA_ARG_THREADS", "LLAMA_ARG_CTX_SIZE", "LLAMA_ARG_ENDPOINT_METRICS",
		"LLAMA_CACHE", llamaCPPAPIKeyEnvVar, "LLAMA_ARG_API_KEY",
		"LLAMA_ARG_API_KEY_FILE", "LLAMA_ARG_MODELS_DIR", "LLAMA_ARG_MODELS_PRESET",
	}
	for _, name := range envNames {
		t.Run(name, func(t *testing.T) {
			md := testModelDeployment()
			md.Spec.Engine.Env = []corev1.EnvVar{{Name: name, Value: "private-value"}}
			if err := mustProfile(t).Validate(&md.Spec); err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("Validate must reject environment override without exposing value: %v", err)
			}
			if _, err := mustProfile(t).Build(testBuildContext(md)); err == nil {
				t.Fatal("Build accepted a reserved environment variable")
			}
		})
	}
}

func TestLlamaCPPAPIKeyUsesPinnedEngineEnvironment(t *testing.T) {
	// Keep the upstream spelling explicit: comparing only values produced from
	// the shared constant would let a coordinated but incorrect rename pass.
	if llamaCPPAPIKeyEnvVar != "LLAMA_API_KEY" {
		t.Fatalf("API key environment = %q, want LLAMA_API_KEY", llamaCPPAPIKeyEnvVar)
	}
	md := testModelDeployment()
	md.Spec.Engine.APIKeySecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "engine-key"},
		Key:                  "key",
	}
	got, err := mustProfile(t).Build(testBuildContext(md))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range got.Container.Env {
		if env.Name == llamaCPPAPIKeyEnvVar && env.Value == "" && env.ValueFrom != nil &&
			env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == "engine-key" {
			return
		}
	}
	t.Fatal("missing LLAMA_API_KEY Secret reference required by pinned llama.cpp")
}

func TestLlamaCPPRejectsDynamicArguments(t *testing.T) {
	md := testModelDeployment()
	md.Spec.Engine.Env = []corev1.EnvVar{{Name: "OPTION", Value: llamaCPPParallelFlag}}
	md.Spec.Engine.ExtraArgs = []string{"$(OPTION)", "32"}
	if err := mustProfile(t).Validate(&md.Spec); err == nil {
		t.Fatal("dynamic argument can bypass authoritative concurrency")
	}
	if _, err := mustProfile(t).Build(testBuildContext(md)); err == nil {
		t.Fatal("Build accepted a dynamic argument")
	}
}
