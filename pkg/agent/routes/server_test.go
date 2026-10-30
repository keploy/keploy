package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// grabPort binds an ephemeral port and returns the listener (still holding the
// port), the numeric port, and the ":port" address StartAgentServer would use.
func grabPort(t *testing.T) (net.Listener, int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, port, fmt.Sprintf(":%d", port)
}

// TestListenWithRetry_RecoversWhenPortFrees reproduces the couchbase-node flake:
// a previous session's agent still holds the published host port when this agent
// binds. The bind must not hard-fail — once the departing owner releases the
// port the retry acquires it.
func TestListenWithRetry_RecoversWhenPortFrees(t *testing.T) {
	holder, _, addr := grabPort(t)

	// Release the port shortly, mimicking the previous agent finishing teardown.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = holder.Close()
	}()

	ln, err := listenWithRetry(context.Background(), zap.NewNop(), addr, 5*time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected bind to recover after the port freed, got: %v", err)
	}
	_ = ln.Close()
}

// In docker the agent's HTTP listener is on every interface of its container,
// so it is plain TCP, never Go's default MPTCP.
func TestListenWithRetry_ListensPlainTCP(t *testing.T) {
	ln, err := listenWithRetry(context.Background(), zap.NewNop(), agentBindAddr(0, true), time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	plaintcptest.RequirePlainTCP(t, ln)
}

// TestListenWithRetry_GivesUpAfterBudget ensures the retry is bounded: a port
// that never frees fails after the budget rather than hanging forever.
func TestListenWithRetry_GivesUpAfterBudget(t *testing.T) {
	holder, _, addr := grabPort(t)
	defer func() { _ = holder.Close() }()

	start := time.Now()
	_, err := listenWithRetry(context.Background(), zap.NewNop(), addr, 300*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the port stays occupied past the budget")
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("gave up before the budget elapsed: %v", elapsed)
	}
}

// TestListenWithRetry_HonorsContextCancel ensures a cancelled context unblocks
// the retry loop promptly instead of waiting out the budget.
func TestListenWithRetry_HonorsContextCancel(t *testing.T) {
	holder, _, addr := grabPort(t)
	defer func() { _ = holder.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := listenWithRetry(ctx, zap.NewNop(), addr, 10*time.Second, 50*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("did not honor cancel promptly: %v", elapsed)
	}
}

// TestListenWithRetry_ReturnsImmediatelyOnNonAddrInUse ensures the retry is
// strictly for "address already in use": any other listen error (here, an
// out-of-range port) surfaces at once and is never retried — the retry must not
// mask a genuine misconfiguration.
func TestListenWithRetry_ReturnsImmediatelyOnNonAddrInUse(t *testing.T) {
	start := time.Now()
	_, err := listenWithRetry(context.Background(), zap.NewNop(), "127.0.0.1:99999", 10*time.Second, time.Second)
	if err == nil {
		t.Fatal("expected an error for an invalid port")
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("an out-of-range port must not be classified as address-in-use: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a non-address-in-use error must not be retried: took %v", elapsed)
	}
}

// TestAgentBindAddr_NativeModeIsLoopbackOnly guards the fix for the
// unauthenticated-agent-exposure report: native mode must never bind every
// interface, since /agent/pcap/keylog, /agent/stop, and /agent/storemocks
// have no auth and would otherwise be reachable by any local or
// network-adjacent process.
func TestAgentBindAddr_NativeModeIsLoopbackOnly(t *testing.T) {
	addr := agentBindAddr(12345, false)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("native mode must bind loopback only, got addr %q", addr)
	}
}

// TestAgentBindAddr_DockerModeBindsAllInterfaces documents the accepted
// trade-off in docker mode: the container's internal listener must stay on
// every interface for the docker proxy to forward the published port in.
// Host-network exposure is closed at the publish step instead (see
// GenerateKeployAgentService and agentPortPublish), not here.
//
// Asserted by splitting the address rather than by "is not 127.0.0.1:". A
// negative prefix check passes for every other string, and several of those
// are wrong in ways that matter: "localhost:%d" binds loopback and would break
// docker mode under a test named BindsAllInterfaces, and "" binds every
// interface on a RANDOM port — a silent misbind, which is worse than an error.
// Splitting also avoids failing on "0.0.0.0:%d", which is byte-identical in
// effect to ":%d".
func TestAgentBindAddr_DockerModeBindsAllInterfaces(t *testing.T) {
	addr := agentBindAddr(12345, true)

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("agentBindAddr(12345, true) = %q, which is not a valid listen address: %v", addr, err)
	}
	if port != "12345" {
		t.Errorf("agentBindAddr(12345, true) = %q, want port 12345", addr)
	}
	// The unspecified address, however it is spelled.
	switch host {
	case "", "0.0.0.0", "::":
	default:
		t.Errorf("docker mode must bind every interface inside the container so docker can forward the published port in; got host %q from %q", host, addr)
	}
}

// TestStartAgentServer_ServesAfterTransientPortConflict is the end-to-end proof:
// the agent HTTP server starts while the port is still held, and begins serving
// once it frees — exactly the previous-session-teardown race from CI.
func TestStartAgentServer_ServesAfterTransientPortConflict(t *testing.T) {
	holder, port, _ := grabPort(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Release the port after the server has already started retrying.
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = holder.Close()
	}()

	go StartAgentServer(ctx, zap.NewNop(), port, false, handler)

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return // server recovered and is serving
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent HTTP server never started serving after the port freed (last err: %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestNewAgentHTTPServer_BoundsIdleClientsWithoutCuttingStreams pins the two
// halves of the server's timeout policy. /agent/pcap/keylog and
// /agent/pcap/traffic hold one response open for a whole recording, so a
// ReadTimeout or WriteTimeout would cut them mid-stream; and without
// ReadHeaderTimeout a client that opens a connection and never finishes its
// request holds a goroutine forever on a port any local process can reach.
// Neither shows up in a test of a working flow: the streams are only cut after
// the timeout, and the slow client only costs a goroutine.
func TestNewAgentHTTPServer_BoundsIdleClientsWithoutCuttingStreams(t *testing.T) {
	srv := newAgentHTTPServer(http.NotFoundHandler())

	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout=%v WriteTimeout=%v: either cuts the long-lived pcap and keylog streams mid-session",
			srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Error("no ReadHeaderTimeout: a client that never finishes its request line holds a goroutine indefinitely")
	}
	if srv.IdleTimeout <= 0 {
		t.Error("no IdleTimeout: a keep-alive connection that goes quiet is held indefinitely")
	}
}

// A server that cannot bind for a reason a fresh start would meet again (here,
// a port no TCP address has) fails the agent's start, reported at once: the
// agent lives on without its server, and nothing else would say so. Run in a
// process of its own: the start's report is the process's.
func TestABindThatFailsForGoodFailsTheStart(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_BOOT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestABindThatFailsForGoodFailsTheStart$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "KEPLOY_TEST_BOOT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	path := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reporter writes into the test's temp dir until it has seen ctx end:
	// it is waited for before that dir is removed.
	reported := utils.ReportBootProgress(ctx, zap.NewNop(), path)
	t.Cleanup(func() { cancel(); <-reported })
	StartAgentServer(ctx, zap.NewNop(), 70000, false, http.NotFoundHandler()) // returns: no listener
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if p, err := utils.ParseBootProgress(b); err == nil && p.Failed {
				if p.Ready || !strings.Contains(p.Failure, "127.0.0.1:70000") {
					t.Fatalf("report %+v; want the bind's failure, not ready", p)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("a bind that failed for good did not fail the start")
		}
		time.Sleep(time.Millisecond)
	}
}

// A port still held when the bind's retries run out is no failure of the
// agent's start: a fresh start (the CLI's retry, on ports of its own) would not
// meet it again. The start, taking no step while it waited, is left to the
// CLI to give up on as stalled, which retries. Run in a process of its own:
// the start's report is the process's.
func TestABindToAPortStillHeldDoesNotFailTheStart(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_BOOT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestABindToAPortStillHeldDoesNotFailTheStart$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "KEPLOY_TEST_BOOT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	held, port, _ := grabPort(t)
	defer held.Close()
	budget, interval := bindRetryBudget, bindRetryInterval
	bindRetryBudget, bindRetryInterval = 300*time.Millisecond, 20*time.Millisecond
	defer func() { bindRetryBudget, bindRetryInterval = budget, interval }()
	StartAgentServer(context.Background(), zap.NewNop(), port, false, http.NotFoundHandler()) // returns: the port stays held
	select {
	case <-utils.BootOver():
		t.Fatal("a port still held when the bind's retries ran out failed (or ended) the agent's start")
	default:
	}
}

// Under the proven switch the line the agent's HTTP server logs as it starts
// names the agent process as its answers do (processInstance,
// models.AgentInstanceHeader) and its pid, so the two processes a verdict
// names when the agent was started again ("agent process X, then Y") can be
// matched to the agent's starts in its log. With the switch off the line is
// as it was, its address alone.
func TestTheServersStartLineNamesTheAgentProcessUnderTheSwitch(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run(provenswitch.Env+"="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			// A port held and a context already done: the server logs its
			// start, finds the port taken and gives up at once, failing no
			// start (the context ended it).
			holder, port, _ := grabPort(t)
			defer holder.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			core, logs := observer.New(zap.InfoLevel)
			StartAgentServer(ctx, zap.New(core), port, false, http.NotFoundHandler())

			starts := logs.FilterMessage("Starting Agent's HTTP server").All()
			if len(starts) != 1 {
				t.Fatalf("%d start lines, want 1: %v", len(starts), logs.All())
			}
			got := starts[0].ContextMap()
			want := map[string]any{"addr": agentBindAddr(port, false)}
			if proven == "1" {
				want["agent process"] = processInstance
				want["pid"] = int64(os.Getpid())
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("start line's fields %v, want %v", got, want)
			}
		})
	}
	if processInstance == "" {
		t.Fatal("the agent process has no instance")
	}
}
