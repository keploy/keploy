package replay

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
)

// An app whose ready-probe address is already listening must return immediately
// (the gate adds no latency for a ready app). This is the non-docker analog of
// the docker published-port gate — the path a k8s replay pod's app Service or a
// native app on a fixed host:port takes.
func TestWaitForAppReady_ProbeAddrGate_ReadyAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	cfg := &config.Config{}
	cfg.Test.Delay = 0 // no floor; isolate the probe gate
	cfg.Test.AppReadyProbeAddr = ln.Addr().String()

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady returned false for a listening probe address")
	}
	// pkg.WaitForPort probes once before its 1s ticker, so an already-listening
	// address returns without the first-tick floor — guard that it stays instant.
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("ready probe address should return promptly (leading dial), took %v", elapsed)
	}
}

// An app whose probe address never listens must still return true (proceed
// anyway, matching the historical fixed-delay behavior — the gate never blocks
// forever and never weakens the run) after the bounded ceiling.
func TestWaitForAppReady_ProbeAddrGate_DeadAddrWaitsUntilTheAppExits(t *testing.T) {
	// An address nothing is listening on, held so no other socket takes it.
	addr := testnet.RefusedAddr(t, "tcp")

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = addr
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond // no ceiling on the accept

	// The app's exit cancels its run's context: only that ends the wait.
	ctx, cancel := context.WithTimeout(pkg.EndsWithApp(context.Background()), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady fired at a probe address that never accepted")
	}
	if elapsed := time.Since(start); elapsed < 1400*time.Millisecond {
		t.Fatalf("the wait ended after %v, before the app exited", elapsed)
	}
}

// ctx cancellation during the probe wait must unblock immediately and report
// not-ready (false), preserving the "false only on ctx cancel" contract.
func TestWaitForAppReady_ProbeAddrGate_CtxCancel(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = addr
	cfg.Test.HealthPollTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady should return false when ctx is cancelled mid-probe")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ctx cancel should unblock promptly, took %v", elapsed)
	}
}

// A malformed probe address must NOT fail the run: it is logged and skipped, and
// waitForAppReady falls through to true (never a false-classified user abort).
func TestWaitForAppReady_ProbeAddrGate_InvalidAddrProceeds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = "not-a-host-port" // no colon → SplitHostPort errors

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady should proceed (return true) on a malformed probe address")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("invalid probe address should skip the probe and return promptly, took %v", elapsed)
	}
}

// The ":<port>" shorthand (empty host) must be treated as localhost — parity with
// the docker published-port gate — not rejected as invalid. A listener on that
// port must satisfy the gate.
func TestWaitForAppReady_ProbeAddrGate_EmptyHostShorthand(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = ":" + port // empty host → localhost

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady should treat \":<port>\" as localhost and pass for a listening port")
	}
	// The leading dial in pkg.WaitForPort makes an already-ready port return
	// without the 1s ticker floor.
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("ready port should return promptly (leading dial), took %v", elapsed)
	}
}

// Once test.appReadyProbeAddr accepted, the address gate is done: it does
// not gate again on the address the set's first test dials. The host may
// not reach that address (a port mapped elsewhere), and a second gate there
// would wait until the application exits.
func TestWaitForAppReady_ProbeAddrGate_IsTheOnlyAddressGate(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = ln.Addr().String()
	// The HTTP check after the accept is the user's bound; keep it short.
	cfg.Test.HealthPollTimeout = 300 * time.Millisecond
	host, port, _ := net.SplitHostPort(testnet.RefusedAddr(t, "tcp"))
	probe := httpProbeTarget{scheme: "http", host: host, port: port, ok: true}

	if !readyWithin(t, 20*time.Second, zap.NewNop(), cfg, probe) {
		t.Fatal("waitForAppReady should proceed once the probe address accepted")
	}
}
