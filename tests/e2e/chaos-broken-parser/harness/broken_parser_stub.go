//go:build !chaos_broken_parser

// Default build — the harness links a no-op implementation of the
// broken-parser hook. Without the `chaos_broken_parser` build tag the
// harness exercises only the docker-compose orchestration + query
// driver + log-assertion scaffolding; no in-process keploy proxy is
// started. See broken_parser.go (gated with //go:build
// chaos_broken_parser) and README.md for what the tagged build does.
package main

import (
	"context"
	"errors"
	"log"
)

// errChaosNotYetWired is declared in both the default and tagged
// builds so main.go can reference it without a build-tag dance.
// On the default build it is never returned (the stub returns nil);
// on the tagged build the stub broken_parser.go returns it until the
// in-process V2 proxy wiring lands.
var errChaosNotYetWired = errors.New(
	"chaos broken-parser in-process proxy wiring not yet implemented — " +
		"see broken_parser.go header for the TODO checklist",
)

// startBrokenParserProxyIfEnabled is the no-op path. See
// broken_parser.go for the chaos-tagged path that registers the
// panicking Postgres parser and starts an in-process keploy proxy.
func startBrokenParserProxyIfEnabled(_ context.Context, _ *Config, _ *logSink) error {
	log.Printf("broken-parser: skipped (build tag `chaos_broken_parser` not set)")
	return nil
}
