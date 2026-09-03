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
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// Defaults. Each mirrors a constant in internal/naming so that a shim started
// by hand for debugging lands on the same ports the operator would have given
// it, and a mismatch between the two cannot be introduced by editing only one.
const (
	defaultShutdownGrace = 20 * time.Second

	// defaultUpstreamTimeout bounds a health check against the engine, not a
	// completion. Completions are deliberately NOT timed out by the shim: a
	// long generation is normal, and a proxy that cut one off would both break
	// the client and record a fabricated error against the variant serving it.
	defaultUpstreamTimeout = 3 * time.Second
)

// config is the shim's fully resolved runtime configuration.
type config struct {
	// listen is the address the proxied OpenAI surface is served on.
	listen string

	// metricsListen is the address /metrics is served on. Separate from listen
	// so that scrape traffic and client traffic cannot reach each other's
	// endpoints; see naming.ShimMetricsPort.
	metricsListen string

	// upstream is the engine's base URL, normally http://127.0.0.1:8000.
	upstream *url.URL

	// healthPath is the engine's readiness endpoint, proxied at
	// naming.ShimHealthPath. It is engine-specific (llama.cpp uses /health) and
	// therefore configured rather than assumed.
	healthPath string

	// model and variant label every metric this shim emits.
	model   string
	variant string

	// maxConcurrency is how many requests the engine can work on at once. It is
	// the engine's --parallel, passed in so that queue depth can be derived:
	// in-flight requests beyond this number are, by definition, waiting.
	maxConcurrency int

	logLevel string

	shutdownGrace time.Duration
}

// parseConfig resolves configuration from command line arguments.
//
// Configuration is by FLAG rather than by environment variable, deliberately.
// The operator renders these flags into the pod template, so `kubectl describe
// pod` shows exactly what the shim was told — including which upstream it is
// proxying and which variant it will label its metrics with. When a canary's
// metrics look wrong, that is the first thing worth checking, and having to
// exec into a container to read its environment makes the check slower than the
// question deserves.
func parseConfig(args []string, errOut io.Writer) (config, error) {
	fs := flag.NewFlagSet("llmcp-shim", flag.ContinueOnError)
	fs.SetOutput(errOut)

	var (
		listen = fs.String("listen", fmt.Sprintf(":%d", naming.ShimPort),
			"address to serve proxied inference traffic on")
		metricsListen = fs.String("metrics-listen", fmt.Sprintf(":%d", naming.ShimMetricsPort),
			"address to serve Prometheus metrics on")
		upstream = fs.String("upstream", fmt.Sprintf("http://127.0.0.1:%d", naming.EnginePort),
			"base URL of the inference engine")
		healthPath = fs.String("health-path", "/health",
			"engine readiness path, proxied at "+naming.ShimHealthPath)
		model = fs.String("model", "",
			"value of the `model` metric label (spec.model.name)")
		variant = fs.String("variant", "primary",
			"value of the `variant` metric label: primary or canary")
		maxConcurrency = fs.Int("max-concurrency", 1,
			"requests the engine serves concurrently; in-flight beyond this counts as queued")
		logLevel = fs.String("log-level", "info",
			"debug, info, warn or error")
		grace = fs.Duration("shutdown-grace", defaultShutdownGrace,
			"how long to let in-flight streams finish on SIGTERM")
	)

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	u, err := url.Parse(*upstream)
	if err != nil {
		return config{}, fmt.Errorf("parsing -upstream: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return config{}, fmt.Errorf("-upstream must be an absolute URL with a scheme and host, got %q", *upstream)
	}

	if *model == "" {
		// Not defaulted. An empty `model` label would silently merge every
		// ModelDeployment's series in a shared Prometheus, and the resulting
		// p95 would be computed across unrelated workloads — a wrong number
		// that looks entirely plausible. Refusing to start is the loud failure.
		return config{}, errors.New("-model is required: it is the `model` label on every emitted metric")
	}

	if !strings.HasPrefix(*healthPath, "/") {
		return config{}, fmt.Errorf("-health-path must start with %q, got %q", "/", *healthPath)
	}

	if *maxConcurrency < 1 {
		return config{}, fmt.Errorf("-max-concurrency must be at least 1, got %d", *maxConcurrency)
	}

	return config{
		listen:         *listen,
		metricsListen:  *metricsListen,
		upstream:       u,
		healthPath:     *healthPath,
		model:          *model,
		variant:        *variant,
		maxConcurrency: *maxConcurrency,
		logLevel:       *logLevel,
		shutdownGrace:  *grace,
	}, nil
}

// logValue renders the configuration as structured log fields.
func (c config) logValue() []any {
	return []any{
		"listen", c.listen,
		"metricsListen", c.metricsListen,
		"upstream", c.upstream.String(),
		"healthPath", c.healthPath,
		"model", c.model,
		"variant", c.variant,
		"maxConcurrency", c.maxConcurrency,
	}
}
