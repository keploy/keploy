//go:build windows && amd64

package winshim

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// helperDNSEnv makes this test binary resolve and call a host that does not
// exist, which is what a replay against a retired dependency looks like.
const helperDNSEnv = "KEPLOY_WINSHIM_TEST_HELPER_DNS"

// unresolvableHost is in the reserved .invalid TLD (RFC 2606), so it can never
// resolve anywhere, on any machine, ever.
const unresolvableHost = "retired-dependency.invalid"

// TestHelperResolver is the application under test for the DNS case. It is not a
// test: it runs only when the parent re-executes this binary.
func TestHelperResolver(t *testing.T) {
	if os.Getenv(helperDNSEnv) == "" {
		t.Skip("not the resolver helper process")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get("http://" + unresolvableHost + "/")
	if err != nil {
		fmt.Println("HELPER-DNS-ERROR", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	fmt.Printf("HELPER-DNS-OK %s %s\n", resp.Status, strings.TrimSpace(string(body)))
}

// TestShimResolvesUnresolvableHostOnReplay proves the DNS half of the backend.
//
// On replay a recorded dependency may no longer exist. Without help the
// application fails inside the resolver — before it ever reaches connect() — so
// the mock that would have answered is never consulted and the replay fails for
// a reason that has nothing to do with the code under test. The shim asks the
// agent for a synthetic address instead, and the connection to THAT address is
// what the proxy answers.
//
// This exercises the whole chain: the resolver hook, the DNS control verb, the
// synthetic-address allocator, the redirect of a loopback destination (synthetic
// addresses are in 127.0.0.0/8, so a shim that skipped loopback would break
// exactly this), and the proxy serving the response.
func TestShimResolvesUnresolvableHostOnReplay(t *testing.T) {
	if os.Getenv(helperEnv) != "" || os.Getenv(helperListenEnv) != "" || os.Getenv(helperDNSEnv) != "" {
		t.Skip("helper process")
	}

	// Sanity: the host really must not resolve, or this test proves nothing.
	if _, err := net.LookupHost(unresolvableHost); err == nil {
		t.Skipf("%s unexpectedly resolves on this machine", unresolvableHost)
	}

	logger := zap.NewNop()
	if testing.Verbose() {
		logger, _ = zap.NewDevelopment()
	}
	clientPID := uint32(os.Getpid())

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the proxy: %v", err)
	}
	defer func() { _ = proxyLn.Close() }()
	proxyPort := uint32(proxyLn.Addr().(*net.TCPAddr).Port)

	hooks := NewHooks(logger, &config.Config{ProxyPort: proxyPort})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gCtx := errgroup.WithContext(ctx)
	gCtx = context.WithValue(gCtx, models.ErrGroupKey, g)

	// Replay mode: substituting an address for a name that does not resolve is
	// replay-only. During record a resolution failure is a real failure.
	setupOpts := config.Agent{}
	setupOpts.ClientNSPID = clientPID
	setupOpts.Mode = models.MODE_TEST
	if err := hooks.Load(gCtx, agent.HookCfg{Pid: clientPID, Mode: models.MODE_TEST}, setupOpts); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Stand in for Keploy's proxy answering from a mock: whatever destination the
	// application thought it was reaching, reply with a canned response.
	served := make(chan *agent.NetworkAddress, 4)
	go serveMockProxy(proxyLn, hooks, served)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: fmt.Sprintf(`cmd /C ""%s" -test.run=TestHelperResolver -test.v"`, self),
	}
	cmd.Env = append(os.Environ(), helperDNSEnv+"=1")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := StartInstrumented(logger, cmd, ShimPath(SessionDir(clientPID))); err != nil {
		t.Fatalf("StartInstrumented: %v", err)
	}
	waitErr := cmd.Wait()
	t.Logf("application output:\n%s", out.String())
	if waitErr != nil {
		t.Fatalf("the application under test failed: %v", waitErr)
	}
	if !strings.Contains(out.String(), "HELPER-DNS-OK") {
		t.Fatalf("the application could not reach a host that does not resolve; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "served-from-mock") {
		t.Fatalf("the application did not get the proxy's response; output:\n%s", out.String())
	}

	// The destination the proxy recovered must be the synthetic loopback address
	// the agent handed out — proof the substitution, not a real lookup, is what
	// carried the request.
	select {
	case addr := <-served:
		if addr.Version != 4 {
			t.Fatalf("recovered a non-IPv4 destination: %+v", addr)
		}
		ip := ipString(addr)
		if !strings.HasPrefix(ip, "127.") {
			t.Fatalf("recovered destination %s is not a synthetic loopback address", ip)
		}
		if addr.Port != 80 {
			t.Fatalf("recovered port %d, want the port the application dialled (80)", addr.Port)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no connection reached the proxy")
	}
}

// serveMockProxy answers every accepted connection with a canned HTTP response,
// the way a served mock would, and reports the destination it recovered.
func serveMockProxy(ln net.Listener, hooks *Hooks, served chan<- *agent.NetworkAddress) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer func() { _ = conn.Close() }()
			srcPort := uint16(conn.RemoteAddr().(*net.TCPAddr).Port)
			addr, err := hooks.Get(context.Background(), srcPort)
			if err != nil {
				return
			}
			select {
			case served <- addr:
			default:
			}
			// Read the request line so the client's write completes.
			_, _ = bufio.NewReader(conn).ReadString('\n')
			const body = "served-from-mock"
			_, _ = io.WriteString(conn, fmt.Sprintf(
				"HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body))
		}(conn)
	}
}
