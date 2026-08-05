package replay

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// TestWaitForAppReady_DockerCompose_NoGateWithoutTestCase is the pre-fix
// baseline: `-c "docker compose up"` has no `-p`/`--publish` flag for
// dockerPublishedHostPort to parse (port mappings live in the compose file),
// so with no HTTP test case to resolve a probe target from, waitForAppReady
// has nothing to gate on and returns immediately — exactly the gap that let
// docker-compose apps (couchbase, aerospike, memcached in enterprise CI) fire
// their first replayed request with zero readiness check beyond --delay.
func TestWaitForAppReady_DockerCompose_NoGateWithoutTestCase(t *testing.T) {
	cfg := &config.Config{Command: "docker compose up"}
	cfg.Test.Delay = 0

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, nil, "test-set-0") {
		t.Fatal("expected waitForAppReady to proceed (no gate available) for docker-compose with no test case")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected an immediate return with no gate, took %v", elapsed)
	}
}

// TestWaitForAppReady_DockerCompose_WaitsThroughResetsUntilServing is the
// couchbase-node/java regression this fix closes: a docker-compose app whose
// HTTP server starts listening (TCP-accept succeeds) before it has finished
// wiring up its own dependency (e.g. a DB cluster still initializing), so
// early requests get reset mid-exchange. waitForAppReady must now resolve a
// probe target from the recorded test case (the same resolveProbeTarget
// logic already proven for the reset-resend gate) and wait for a full HTTP
// round-trip — not just a completed TCP handshake — before declaring ready.
func TestWaitForAppReady_DockerCompose_WaitsThroughResetsUntilServing(t *testing.T) {
	addr, stop := resettingThenServing(t, 3) // reset the first 3 connections
	defer stop()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	// A plain TCP dial succeeds even mid reset-burst — proves a bare TCP gate
	// would have declared readiness too early (the pre-fix behavior for the
	// docker-run gate, and the total absence of a gate for docker-compose).
	if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
		_ = c.Close()
	}

	cfg := &config.Config{Command: "docker compose up"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 5 * time.Second
	testCases := []*models.TestCase{
		{Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://" + net.JoinHostPort(host, port) + "/health"}},
	}

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, testCases, "test-set-0") {
		t.Fatal("expected waitForAppReady to proceed once the reset burst clears")
	}
	// The first 3 connections are reset before the 4th succeeds — plenty of
	// margin over a same-process TCP round trip, but well under the 5s
	// ceiling, proving it didn't just wait out the full ceiling either.
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("expected readiness shortly after the reset burst clears, took %v", elapsed)
	}
}

// TestWaitForAppReady_DockerCompose_ProceedsAfterCeilingWhenNeverServing
// preserves the existing "never block forever, never weaken an assertion"
// contract: an app that never completes an HTTP round-trip still lets replay
// proceed once HealthPollTimeout elapses, matching the docker-run TCP gate's
// TestWaitForAppReady_DockerPortGate_DeadPortProceeds.
func TestWaitForAppReady_DockerCompose_ProceedsAfterCeilingWhenNeverServing(t *testing.T) {
	addr, stop := resettingThenServing(t, 1<<30) // always reset
	defer stop()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	cfg := &config.Config{Command: "docker compose up"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 700 * time.Millisecond
	testCases := []*models.TestCase{
		{Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://" + net.JoinHostPort(host, port) + "/health"}},
	}

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, testCases, "test-set-0") {
		t.Fatal("expected waitForAppReady to proceed (true) after the ceiling, never block forever")
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("should have waited ~the ceiling before proceeding, took only %v", elapsed)
	}
}

// TestWaitForAppReady_DockerCompose_GRPCOnlySetFallsThrough confirms a test
// set with no probeable HTTP test case (e.g. gRPC-only) is unaffected by this
// change — it falls straight through to the existing behavior (no gate for
// docker-compose, matching TestWaitForAppReady_DockerCompose_NoGateWithoutTestCase)
// rather than erroring or blocking.
func TestWaitForAppReady_DockerCompose_GRPCOnlySetFallsThrough(t *testing.T) {
	cfg := &config.Config{Command: "docker compose up"}
	cfg.Test.Delay = 0
	testCases := []*models.TestCase{{Kind: models.GRPC_EXPORT}}

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, testCases, "test-set-0") {
		t.Fatal("expected waitForAppReady to proceed for a gRPC-only test set")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected an immediate return with no probeable test case, took %v", elapsed)
	}
}
