//go:build chaos_broken_parser

// Chaos build — this file is compiled when the test is invoked as:
//
//	go run -tags chaos_broken_parser ./tests/e2e/chaos-broken-parser/harness/
//
// It stands up an in-process keploy proxy with a synthetic Postgres
// parser that panics on its first chunk read. The supervisor in
// pkg/agent/proxy/supervisor/ is expected to recover the panic and
// return FallthroughToPassthrough so recordViaSupervisor KEEPS the
// relay running for raw byte forwarding until the peer closes.
// (recordViaSupervisor intentionally does NOT call globalPassThrough
// on the fallback path — cancelling the relay first would introduce
// a stall during the handoff. See pkg/agent/proxy/proxy_v2.go:
// "the relay keeps forwarding client↔dest bytes end-to-end".)
// The application (psql-client sidecar) continues to see a working
// database connection because the relay's forwarder goroutines
// remain active after the parser exits.
//
// -----------------------------------------------------------------
// STATUS ON THIS BRANCH (feat/proxy-v2-foundation):
//
// The V2 packages (pkg/agent/proxy/fakeconn, directive, supervisor,
// relay, proxy_v2.go, integrations.IntegrationsV2) ARE landed on
// this branch — the chaos stub was written in an isolated worktree
// based on an older snapshot and missed them. What remains for a
// focused follow-up is the in-process proxy instantiation and the
// broken parser type definition. Concretely:
//
//  1. Define a local parser type that implements
//     integrations.IntegrationsV2 with:
//     - MatchType sniffing the Postgres startup packet (length-
//     prefix + protocol version 0x00 0x03 0x00 0x00 at bytes 4..8).
//     - IsV2() returning true so the dispatcher runs it under the
//     supervisor.
//     - RecordOutgoing that does `_, _ = sess.V2.ClientStream.ReadChunk()`
//     then panics.
//
//  2. integrations.Register it under a synthetic IntegrationType
//     (e.g. "chaos_pg") with a priority strictly greater than
//     integrations.POSTGRES_V2 so p.integrationsPriority lists it
//     first, overriding the real Postgres parser for the test.
//
//  3. Instantiate a proxy on an ephemeral port using proxy.New(...)
//     plus a stub MockManager. Retarget the compose-driven queries
//     at that port instead of the postgres service directly, and
//     watch the harness' logSink for:
//
//     "parser supervisor triggered passthrough fallback"
//
//     which recordViaSupervisor emits on FallthroughToPassthrough.
//
// The compose stack, query driver, logSink, and pass/fail evaluator
// are already live and pass when run in-process. Wiring this stub
// closes the loop end-to-end for invariants I1/I2/I3.
// -----------------------------------------------------------------
package main

import (
	"context"
	"log"
)

// errChaosNotYetWired is declared alongside the stub in
// broken_parser_stub.go so main.go can reference it unconditionally
// (no build-tag dance at the call site). The tagged path here still
// returns it until the in-process V2 proxy wiring lands — the
// harness treats that case as "skip the supervisor-fallback
// assertion" rather than a failure.

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
