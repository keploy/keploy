package replay

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
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
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
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
func TestWaitForAppReady_ProbeAddrGate_DeadAddrProceeds(t *testing.T) {
	// Pick an address nothing is listening on by opening then closing a listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = addr
	cfg.Test.HealthPollTimeout = 1 * time.Second // tiny ceiling for the test

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
		t.Fatal("waitForAppReady should proceed (return true) after the ceiling on a dead probe address")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("should have waited ~the ceiling before proceeding, took only %v", elapsed)
	}
}

// ctx cancellation during the probe wait must unblock immediately and report
// not-ready (false), preserving the "false only on ctx cancel" contract.
func TestWaitForAppReady_ProbeAddrGate_CtxCancel(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = addr
	cfg.Test.HealthPollTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg) {
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
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
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
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
		t.Fatal("waitForAppReady should treat \":<port>\" as localhost and pass for a listening port")
	}
	// The leading dial in pkg.WaitForPort makes an already-ready port return
	// without the 1s ticker floor.
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("ready port should return promptly (leading dial), took %v", elapsed)
	}
}

// TestWaitForAppReady_HealthTimeoutWarnsWithConsequence pins the visibility of
// the health-gate fallback.
//
// When the health probe never sees a 2xx, waitForAppReady deliberately proceeds
// on a fixed delay rather than blocking or failing — that behaviour is not in
// question here. What matters is that the operator can tell: firing a whole
// suite at an app that never reported healthy typically produces every test
// failing with no response at all, which is indistinguishable from a real
// regression unless this line stands out. Reported at Info it drowned in a long
// run; the sibling TCP port gate in the same function already reports the
// identical situation at Warn.
func TestWaitForAppReady_HealthTimeoutWarnsWithConsequence(t *testing.T) {
	// An address nothing listens on, so the health probe can never see a 2xx.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthURL = "http://" + addr + "/healthz"
	cfg.Test.HealthPollTimeout = 300 * time.Millisecond

	core, logs := observer.New(zapcore.DebugLevel)
	if !waitForAppReady(context.Background(), zap.New(core), cfg) {
		t.Fatal("the gate must still proceed after the ceiling; this test is about the log, not the fallback")
	}

	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) == 0 {
		t.Fatalf("health-probe timeout must be reported at Warn — at Info it is invisible in a real run "+
			"and the resulting all-tests-fail looks like a regression; got %v", logs.All())
	}
	var found bool
	for _, w := range warns {
		if strings.Contains(w.Message, "health probe timed out") {
			found = true
			if !strings.Contains(w.Message, "status_code got=0") {
				t.Errorf("the warning must name the consequence so the failure mode is recognisable, got %q", w.Message)
			}
			if !strings.Contains(w.Message, "--health-poll-timeout") {
				t.Errorf("the warning must name the knob to turn, got %q", w.Message)
			}
		}
	}
	if !found {
		t.Errorf("no health-probe-timeout warning found; got %v", warns)
	}
}
