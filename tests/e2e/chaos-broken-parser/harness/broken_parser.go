//go:build chaos_broken_parser

// Chaos build — this file is compiled when the test is invoked as:
//
//	go run -tags chaos_broken_parser ./tests/e2e/chaos-broken-parser/harness/
//
// It stands up an in-process keploy proxy with a synthetic Postgres
// parser that panics on its first chunk read. The supervisor in
// pkg/agent/proxy/supervisor/ is expected to recover the panic and
// flip FallthroughToPassthrough so recordViaSupervisor invokes
// p.globalPassThrough(ctx, srcConn, dstConn). The application
// (psql-client sidecar) continues to see a working database
// connection because the raw byte relay in pkg/agent/proxy/relay/
// forwards the client's queries unchanged to the real Postgres
// container.
//
// -----------------------------------------------------------------
// STATUS ON THIS BRANCH (feat/proxy-v2-foundation):
//
// The supervisor + relay + fakeconn packages, and the
// IntegrationsV2 interface with IsV2() bool — all named in the task
// spec — do not exist on feat/proxy-v2-foundation yet. This file is
// a compile-clean stub that documents the registration pattern the
// follow-up lands. The harness still brings up the compose stack,
// drives queries, and evaluates the pass/fail predicate; it will
// fail the "supervisor-fallback log observed" invariant until the
// real registration below is enabled.
//
// To land the real parser, replace the body of
// startBrokenParserProxyIfEnabled with code that:
//
//  1. Constructs an IntegrationsV2-compatible parser whose:
//     - MatchType sniffs the Postgres startup packet (len-prefix +
//     protocol version 0x00 0x03 0x00 0x00 at bytes 4..8).
//     - IsV2() returns true so the dispatcher routes it through
//     the supervisor.
//     - The V2 chunk-reader implementation panics on the first
//     call (this is the "broken parser").
//
//  2. Registers it under a synthetic IntegrationType (e.g.
//     "chaos_pg") via integrations.Register with a priority strictly
//     greater than integrations.POSTGRES_V2 so
//     p.integrationsPriority lists it first.
//
//  3. Starts a proxy with proxy.New(...) on an ephemeral port,
//     points the compose-driven queries at that port instead of the
//     postgres service directly, and waits for the supervisor to
//     emit the canonical fallback log:
//
//     "parser supervisor triggered passthrough fallback"
//
//     which the harness' logSink scans for.
//
// Once those packages ship, the STATUS block can be removed and the
// body replaced with the real in-process proxy wiring. The
// invariants under test (I1: supervisor is a panic firewall; I2:
// dispatcher falls through to globalPassThrough; I3: app's queries
// keep succeeding) are then all verifiable end-to-end by a single
// `go test -tags chaos_broken_parser` run.
// -----------------------------------------------------------------
package main

import (
	"context"
	"errors"
	"log"
)

// errChaosNotYetWired is returned under the chaos build tag until the
// V2 supervisor/relay/fakeconn packages land. It surfaces as a clear
// "stub hasn't been replaced yet" message instead of a cryptic nil-
// pointer panic when the follow-up forgets to finish the migration.
var errChaosNotYetWired = errors.New(
	"chaos broken-parser wiring not yet implemented on this branch — " +
		"see broken_parser.go header for the TODO checklist",
)

// startBrokenParserProxyIfEnabled registers the panicking Postgres
// parser and starts an in-process keploy proxy on an ephemeral port.
// See the file header for the full TODO checklist.
//
// The _ = sink line below is deliberate: once the real wiring lands,
// the in-process proxy is built with a zap.Logger whose WriteSyncer
// forwards into `sink` so assertions about the
// "parser supervisor triggered passthrough fallback" message work
// against the exact logger the supervisor writes to. Leaving the
// parameter explicitly referenced prevents a "declared and not used"
// compile error while keeping the call-site stable for the follow-up.
func startBrokenParserProxyIfEnabled(_ context.Context, _ *Config, sink *logSink) error {
	_ = sink
	log.Printf("broken-parser: chaos_broken_parser build tag set but V2 wiring is a stub on this branch — see broken_parser.go")
	return errChaosNotYetWired
}
