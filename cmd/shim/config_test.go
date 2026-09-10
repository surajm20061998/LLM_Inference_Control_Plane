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

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{modelFlag, "qwen3", testNamespaceFlag, testDeploymentFlag}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	// The defaults must agree with internal/naming, or a shim started by hand
	// for debugging listens somewhere the operator's probes do not look.
	if want := ":8080"; cfg.listen != want {
		t.Errorf("listen = %q, want %q", cfg.listen, want)
	}
	if want := ":9090"; cfg.metricsListen != want {
		t.Errorf("metricsListen = %q, want %q", cfg.metricsListen, want)
	}
	if want := "http://127.0.0.1:8000"; cfg.upstream.String() != want {
		t.Errorf("upstream = %q, want %q", cfg.upstream, want)
	}
	if cfg.variant != string(variantPrimary) {
		t.Errorf("variant = %q, want %q", cfg.variant, variantPrimary)
	}
	_ = naming.ShimPort
}

// Test-local constants. variantPrimary keeps the default assertion readable
// without importing the API package into a binary that has no business
// depending on it; modelFlag is repeated in every valid argument vector because
// the shim refuses to start without it.
const (
	variantPrimary     = "primary"
	modelFlag          = "-model"
	modelArg           = "--model=m"
	testNamespace      = "team-a"
	testNamespaceFlag  = "--namespace=team-a"
	testDeployment     = "chat"
	testDeploymentFlag = "--model-deployment=chat"
)

func TestParseConfigRejectsMissingModel(t *testing.T) {
	// An empty model label merges every ModelDeployment's series in a shared
	// Prometheus and produces a percentile computed across unrelated
	// workloads — a wrong number that looks entirely plausible.
	_, err := parseConfig(nil, io.Discard)
	if err == nil {
		t.Fatal("expected an error when -model is absent")
	}
	if !strings.Contains(err.Error(), "-model is required") {
		t.Fatalf("error = %v, want it to name -model", err)
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"relative upstream":  {modelFlag, "m", "-upstream", "127.0.0.1:8000"},
		"unparseable URL":    {modelFlag, "m", "-upstream", "http://[::1"},
		"relative health":    {modelFlag, "m", "-health-path", "health"},
		"zero concurrency":   {modelFlag, "m", "-max-concurrency", "0"},
		"negative concurren": {modelFlag, "m", "-max-concurrency", "-3"},
	}
	for name, args := range cases {
		args = append(args, testNamespaceFlag, testDeploymentFlag)
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseConfigAcceptsOperatorFlags(t *testing.T) {
	// Exactly the argument vector the controller renders.
	cfg, err := parseConfig([]string{
		"--listen=:8080",
		"--metrics-listen=:9090",
		"--upstream=http://127.0.0.1:8000",
		"--health-path=/health",
		"--model=qwen3-0.6b",
		"--namespace=team-a",
		"--model-deployment=chat",
		"--variant=canary",
		"--max-concurrency=4",
		"--log-level=debug",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.namespace != testNamespace || cfg.modelDeployment != testDeployment ||
		cfg.variant != "canary" || cfg.model != "qwen3-0.6b" || cfg.maxConcurrency != 4 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestParseConfigRequiresResourceIdentity(t *testing.T) {
	for _, args := range [][]string{
		{modelArg},
		{modelArg, testNamespaceFlag},
		{modelArg, testDeploymentFlag},
		{modelArg, "--namespace= ", testDeploymentFlag},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Errorf("accepted incomplete identity: %v", args)
		}
	}
}
