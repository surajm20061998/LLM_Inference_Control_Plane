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

// Command shim is llmcp-shim: a reverse proxy injected as a native sidecar
// beside every inference engine this operator runs.
//
// # Why it exists
//
// It is not a convenience layer, it is the reason metric-driven rollouts are
// possible at all. llama.cpp's /metrics exposes GAUGES only — no histograms, no
// request duration, no time-to-first-token, no HTTP status-code counter. p95
// latency and success rate are therefore not merely awkward to compute from the
// engine's own output, they are impossible, and a canary controller built on it
// would have nothing to gate a promotion on.
//
// The second reason is engine independence. `llmcp_inference_ttft_seconds`
// means the same thing whether the engine behind it is llama.cpp, vLLM or the
// deterministic test stub, because one binary measures it in one place, one
// way. Every dashboard, alert and rollout gate therefore survives an engine
// swap unchanged — which is what makes the engine abstraction real rather than
// aspirational.
//
// # The three things that must be right
//
// Each of them fails SILENTLY, producing a plausible number rather than an
// error, which is why each is called out where it lives in the code:
//
//  1. httputil.ReverseProxy{FlushInterval: -1} and Transport.DisableCompression.
//     Without both, the stream is buffered and every TTFT equals the total
//     duration. See newProxy and newTransport.
//  2. TTFT is taken at the first SSE frame carrying generated text — not the
//     first byte, not the first frame. See streamObserver.
//  3. TTFT is never recorded for a non-streaming response, because there the
//     first token and the last arrive together. See metrics.observe.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// readHeaderTimeout bounds slow-header clients.
//
// There is deliberately no WriteTimeout and no ReadTimeout: a streamed
// completion is long-lived by design, and either deadline would truncate one
// mid-generation — producing a client error and a distorted latency sample from
// a request that was working perfectly.
const readHeaderTimeout = 15 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		// flag.ErrHelp means -h; that is a successful invocation, not a failure.
		if errors.Is(err, flagErrHelp) {
			return
		}
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.logLevel)}))

	if err := run(context.Background(), cfg, logger); err != nil {
		logger.Error("shim exited with error", "error", err)
		os.Exit(1)
	}
}

// flagErrHelp is flag.ErrHelp, aliased so main does not import flag.
var flagErrHelp = errHelp()

// run starts both listeners and blocks until one fails or a signal arrives.
func run(parent context.Context, cfg config, logger *slog.Logger) error {
	m := newMetrics(cfg)
	p := newProxy(cfg, m, logger, time.Now)

	proxySrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           p.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle(naming.ShimMetricsPath, promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// Report collection problems as a 500 rather than serving a partial
		// exposition. A truncated scrape looks to Prometheus like the series
		// simply stopped existing, and analysis reading it would see "no data"
		// where it should see "collection is broken".
		ErrorHandling: promhttp.HTTPErrorOnError,
	}))
	metricsSrv := &http.Server{
		Addr:              cfg.metricsListen,
		Handler:           metricsMux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting llmcp-shim", cfg.logValue()...)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serve(proxySrv, "proxy") })
	g.Go(func() error { return serve(metricsSrv, "metrics") })

	g.Go(func() error {
		<-gctx.Done()
		logger.Info("draining", "grace", cfg.shutdownGrace.String())

		// A generous grace period, and the reason is specific to this workload:
		// an in-flight completion can legitimately still be generating tokens
		// when SIGTERM arrives, and killing it produces a client-visible error
		// plus a truncated latency sample. As a native sidecar the shim is
		// terminated AFTER the engine container, so by the time this runs the
		// engine is already going away and the drain is bounded in practice.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), cfg.shutdownGrace)
		defer cancel()

		// Metrics first: nothing is scraping a terminating pod for long, and
		// keeping the proxy up marginally longer is the friendlier order.
		_ = metricsSrv.Shutdown(shutdownCtx)
		return proxySrv.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("llmcp-shim stopped")
	return nil
}

// serve runs one HTTP server, naming it in any error it reports.
func serve(srv *http.Server, name string) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener on %s: %w", name, srv.Addr, err)
	}
	return nil
}

// parseLevel maps a log level name onto a slog level, defaulting to info.
func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
