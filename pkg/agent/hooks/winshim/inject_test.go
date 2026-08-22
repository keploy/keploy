//go:build windows && amd64

package winshim

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// helperEnv turns this test binary into the "application under test". The
// standard Go helper-process pattern: re-executing ourselves means the test
// needs no second binary shipped alongside it, which matters because these
// tests are cross-compiled with `go test -c` and run on machines without a Go
// toolchain.
const helperEnv = "KEPLOY_WINSHIM_TEST_HELPER_URL"

// TestHelperApp is the application under test. It is not a test: it runs only
// when the parent re-executes this binary with helperEnv set, and it makes
// exactly one outgoing HTTP request — through Go's netpoller, so it exercises
// the ConnectEx path that a plain connect() hook would miss.
func TestHelperApp(t *testing.T) {
	url := os.Getenv(helperEnv)
	if url == "" {
		t.Skip("not the helper process")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(url)
	if err != nil {
		fmt.Println("HELPER-ERROR", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	fmt.Printf("HELPER-OK %s %s\n", resp.Status, strings.TrimSpace(string(body)))
}

// TestShimInterceptsOutgoingConnections is the end-to-end proof of the
// unprivileged backend: it stages the real shim, injects it into a real process
// with StartInstrumented, and checks that an outgoing connection made by that
// process arrives at Keploy's proxy with its original destination recoverable
// from the source port — the DestInfo.Get contract the whole design rests on.
//
// It deliberately drives the production code (StageShim, Hooks, the control
// server, StartInstrumented) rather than a reimplementation, and it launches
// through `cmd /C` exactly as utils.ExecuteCommand does, so the shim's
// propagation from that shell into the real application is under test too.
func TestShimInterceptsOutgoingConnections(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Skip("helper process")
	}

	// The shim never redirects loopback — that is Keploy's own control plane —
	// so the upstream has to be reachable on a real address.
	host := nonLoopbackIPv4(t)

	// A stand-in for the application's real dependency.
	upstream, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	defer func() { _ = upstream.Close() }()

	var upstreamHits int32
	var hitMu sync.Mutex
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hitMu.Lock()
			upstreamHits++
			hitMu.Unlock()
			_, _ = io.WriteString(w, "hello-from-upstream")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(upstream) }()
	defer func() { _ = srv.Close() }()

	// Stand up the hooks exactly as the agent does.
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

	setupOpts := config.Agent{}
	setupOpts.ClientNSPID = clientPID
	setupOpts.Mode = models.MODE_TEST
	if err := hooks.Load(gCtx, agent.HookCfg{Pid: clientPID, Mode: models.MODE_TEST}, setupOpts); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A minimal stand-in for Keploy's proxy: recover the destination from the
	// source port and forward. This is the contract the real proxy relies on.
	resolved := make(chan string, 4)
	go serveProxy(t, proxyLn, hooks, resolved)

	// Launch the "application" through cmd /C, as utils.ExecuteCommand does.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	target := fmt.Sprintf("http://%s/", upstream.Addr().String())

	// os/exec would escape the inner quotes as \" , which cmd.exe does not
	// understand. SysProcAttr.CmdLine hands Windows the command line verbatim —
	// the standard escape hatch for a `cmd /C` whose argument is itself quoted.
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: fmt.Sprintf(`cmd /C ""%s" -test.run=TestHelperApp -test.v"`, self),
	}
	cmd.Env = append(os.Environ(), helperEnv+"="+target)
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
	if !strings.Contains(out.String(), "HELPER-OK") {
		t.Fatalf("the application did not complete its request; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hello-from-upstream") {
		t.Fatalf("the application did not receive the upstream's body; output:\n%s", out.String())
	}

	// The shim must have announced itself, or nothing above proves interception.
	if !hooks.Armed() {
		t.Fatal("the shim never announced itself; the application ran uninstrumented")
	}

	// And the connection must have gone through the proxy with the original
	// destination recovered from its source port.
	select {
	case got := <-resolved:
		if got != upstream.Addr().String() {
			t.Fatalf("the proxy recovered %q, want %q", got, upstream.Addr().String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no connection reached the proxy; the application's traffic was not redirected")
	}

	hitMu.Lock()
	hits := upstreamHits
	hitMu.Unlock()
	if hits != 1 {
		t.Fatalf("the upstream saw %d requests, want exactly 1", hits)
	}
}

// serveProxy is a minimal stand-in for Keploy's proxy: for each accepted
// connection it recovers the original destination from the source port via
// DestInfo.Get and forwards the bytes there.
func serveProxy(t *testing.T, ln net.Listener, hooks *Hooks, resolved chan<- string) {
	t.Helper()
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
			dest := net.JoinHostPort(ipString(addr), fmt.Sprint(addr.Port))
			select {
			case resolved <- dest:
			default:
			}
			up, err := net.DialTimeout("tcp", dest, 5*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = up.Close() }()
			go func() { _, _ = io.Copy(up, conn) }()
			_, _ = io.Copy(conn, up)
		}(conn)
	}
}

// ipString decodes an agent.NetworkAddress the way the real proxy does.
func ipString(a *agent.NetworkAddress) string {
	if a.Version == 4 {
		return net.IPv4(byte(a.IPv4Addr>>24), byte(a.IPv4Addr>>16), byte(a.IPv4Addr>>8), byte(a.IPv4Addr)).String()
	}
	ip := make(net.IP, net.IPv6len)
	for i, word := range a.IPv6Addr {
		ip[i*4] = byte(word >> 24)
		ip[i*4+1] = byte(word >> 16)
		ip[i*4+2] = byte(word >> 8)
		ip[i*4+3] = byte(word)
	}
	return ip.String()
}

// nonLoopbackIPv4 finds an address the shim will actually redirect. Loopback is
// deliberately left alone by the shim, so a test upstream on 127.0.0.1 would
// prove nothing.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Skip("no non-loopback IPv4 address on this machine; the shim only redirects non-loopback traffic")
	return ""
}
