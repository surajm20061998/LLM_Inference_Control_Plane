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

// Command fakeengine is a deterministic, OpenAI-compatible stub inference
// server. It is the default engine for CI and for every end-to-end test in
// this repository.
//
// A real LLM cannot be used for those tests: its latency varies by orders of
// magnitude between runs, so canary and rollout assertions written against it
// would be flaky by construction. fakeengine instead produces exactly the
// latency, token count and failure pattern it is configured to produce, with
// no randomness anywhere, so a test can assert "the canary rolled back after
// exactly two failed checks" and mean it.
//
// The HTTP surface deliberately mimics llama.cpp's llama-server (including its
// 503 "Loading model" health response and its tolerance of llama-server
// command line flags) so that the operator's real llamacpp engine profile is
// exercised unchanged against this binary.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	// readHeaderTimeout bounds slow-header clients. Note there is deliberately
	// no WriteTimeout: streaming responses are long-lived by design and a write
	// deadline would truncate them mid-stream.
	readHeaderTimeout = 10 * time.Second

	// shutdownTimeout is short on purpose. Kubernetes gives a terminating pod a
	// grace period, and an engine that lingers slows every rollout test down.
	shutdownTimeout = 5 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := newConfig(os.Getenv, os.Args[1:], logger)
	if err := run(cfg, logger); err != nil {
		logger.Error("fakeengine exited with error", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until it fails or a termination signal
// arrives, then drains connections within shutdownTimeout.
func run(cfg config, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:              cfg.addr(),
		Handler:           newServer(cfg, logger).handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting fakeengine", cfg.logValue()...)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down fakeengine")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
