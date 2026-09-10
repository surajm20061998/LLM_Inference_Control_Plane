package engine

import (
	"fmt"
	"strings"

	v1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// The aliases and environment names are checked against the pinned engine:
// https://github.com/ggml-org/llama.cpp/blob/b10731/common/arg.cpp
// Recheck this contract whenever LlamaCPPDefaultImage changes. This is a list
// of settings the controller owns, not a general allowlist of tuning options.
const (
	settingModelSource        = ".spec.model.source"
	settingModelSourceAndPath = ".spec.model.source and .spec.model.path"
	settingHFRepo             = ".spec.model.source.huggingFace.repo"
	settingHFFile             = ".spec.model.source.huggingFace.file"
	settingHFToken            = ".spec.model.source.huggingFace.tokenSecretRef"
	settingServingHost        = "the controller-managed serving host"
	settingServingPort        = ".spec.serving.port (the external Service port)"
	settingModelName          = ".spec.model.name"
	settingMaxConcurrency     = ".spec.engine.maxConcurrency"
	settingThreads            = ".spec.engine.threads"
	settingContextSize        = ".spec.engine.contextSize"
	settingMetricsEndpoint    = "the controller-managed metrics endpoint"
	settingAPIKey             = ".spec.engine.apiKeySecretRef"
)

var llamaCPPReservedFlags = map[string]string{
	"-m": settingModelSourceAndPath, "--model": settingModelSourceAndPath,
	"-mu": settingModelSource, "--model-url": settingModelSource,
	"-dr": settingModelSource, "--docker-repo": settingModelSource,
	"-hf": settingHFRepo, "-hfr": settingHFRepo, "--hf-repo": settingHFRepo,
	"-hff": settingHFFile, "--hf-file": settingHFFile,
	"-hft": settingHFToken, "--hf-token": settingHFToken,
	llamaCPPHostFlag: settingServingHost, llamaCPPPortFlag: settingServingPort,
	"-a": settingModelName, "--alias": settingModelName,
	"-np": settingMaxConcurrency, llamaCPPParallelFlag: settingMaxConcurrency,
	"-t": settingThreads, "--threads": settingThreads,
	"-tb": settingThreads, "--threads-batch": settingThreads,
	"-c": settingContextSize, "--ctx-size": settingContextSize,
	"--metrics": settingMetricsEndpoint,
	"--api-key": settingAPIKey, "--api-key-file": settingAPIKey,
	// Router mode and presets can replace the declared single model and its
	// runtime settings outside the controller's rendering contract.
	"--models-dir": settingModelSource, "--models-preset": settingModelSource,
}

var llamaCPPReservedEnv = map[string]string{
	"LLAMA_ARG_MODEL":            settingModelSourceAndPath,
	"LLAMA_ARG_MODEL_URL":        settingModelSource,
	"LLAMA_ARG_DOCKER_REPO":      settingModelSource,
	"LLAMA_ARG_HF_REPO":          settingHFRepo,
	"LLAMA_ARG_HF_FILE":          settingHFFile,
	"HF_TOKEN":                   settingHFToken,
	"LLAMA_ARG_HOST":             settingServingHost,
	"LLAMA_ARG_PORT":             settingServingPort,
	"LLAMA_ARG_ALIAS":            settingModelName,
	"LLAMA_ARG_N_PARALLEL":       settingMaxConcurrency,
	"LLAMA_ARG_THREADS":          settingThreads,
	"LLAMA_ARG_CTX_SIZE":         settingContextSize,
	"LLAMA_ARG_ENDPOINT_METRICS": settingMetricsEndpoint,
	"LLAMA_CACHE":                "the controller-managed model cache",
	llamaCPPAPIKeyEnvVar:         settingAPIKey,
	"LLAMA_ARG_API_KEY_FILE":     settingAPIKey,
	// Also reserve the previous operator spelling: it must not become a
	// credential escape hatch with user-supplied older engine images.
	"LLAMA_ARG_API_KEY":       settingAPIKey,
	"LLAMA_ARG_MODELS_DIR":    settingModelSource,
	"LLAMA_ARG_MODELS_PRESET": settingModelSource,
}

func validateLlamaCPPReservedSettings(spec *v1alpha1.ModelDeploymentSpec) error {
	for _, arg := range spec.Engine.ExtraArgs {
		// Kubernetes expands $(VAR) in container arguments after reconciliation.
		// A dynamic argument could become a reserved flag that is invisible to
		// this validation, so tuning arguments must be literal.
		if strings.Contains(arg, "$(") {
			return fmt.Errorf(
				"llamacpp: .spec.engine.extraArgs must use literal arguments; " +
					"environment expansion can override controller-owned settings",
			)
		}
		flag, _, _ := strings.Cut(arg, "=")
		// Treat underscore spellings consistently with their hyphenated
		// equivalents; only the setting name is included in errors, never its
		// value (which might be a credential).
		flag = strings.ReplaceAll(flag, "_", "-")
		if field, reserved := llamaCPPReservedFlags[flag]; reserved {
			return fmt.Errorf("llamacpp: .spec.engine.extraArgs setting %q is controller-owned; use %s", flag, field)
		}
	}
	for _, env := range spec.Engine.Env {
		if field, reserved := llamaCPPReservedEnv[env.Name]; reserved {
			return fmt.Errorf("llamacpp: .spec.engine.env setting %q is controller-owned; use %s", env.Name, field)
		}
	}
	return nil
}
