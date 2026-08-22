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
	"strconv"
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

// helperListenEnv turns this test binary into a server application: it listens
// on the given port and serves one request. Used by the ingress test.
const helperListenEnv = "KEPLOY_WINSHIM_TEST_HELPER_LISTEN"

// TestHelperServer is the server application under test. Like TestHelperApp it
// is not a test — it runs only when the parent re-executes this binary.
func TestHelperServer(t *testing.T) {
	port := os.Getenv(helperListenEnv)
	if port == "" {
		t.Skip("not the helper server process")
	}
	// The app asks for this port. If Keploy is doing its job, the shim moves the
	// bind somewhere else and Keploy takes this one over.
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		fmt.Println("HELPER-LISTEN-ERROR", err)
		os.Exit(1)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("HELPER-LISTENING-ON %d\n", actual)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello-from-app")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()

	// Stay up long enough for the parent to observe the move and probe us.
	time.Sleep(6 * time.Second)
	_ = srv.Close()
}

// TestShimMovesApplicationListener proves the ingress half of the unprivileged
// backend: the application's server bind is relocated so Keploy can own the port
// it advertises, and the ingress event that drives Keploy's forwarder is
// published only once the socket really listens.
//
// A kernel filter would get this for free by redirecting inbound packets; user
// space has to move the listener instead.
func TestShimMovesApplicationListener(t *testing.T) {
	if os.Getenv(helperEnv) != "" || os.Getenv(helperListenEnv) != "" || os.Getenv(helperDNSEnv) != "" {
		t.Skip("helper process")
	}

	logger := zap.NewNop()
	if testing.Verbose() {
		logger, _ = zap.NewDevelopment()
	}
	clientPID := uint32(os.Getpid())

	hooks := NewHooks(logger, &config.Config{ProxyPort: 16789})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gCtx := errgroup.WithContext(ctx)
	gCtx = context.WithValue(gCtx, models.ErrGroupKey, g)

	// Record mode: moving a listener is record-only, because only the record
	// path stands up the ingress forwarder that gives the advertised port back.
	setupOpts := config.Agent{}
	setupOpts.ClientNSPID = clientPID
	setupOpts.Mode = models.MODE_RECORD
	if err := hooks.Load(gCtx, agent.HookCfg{Pid: clientPID, Mode: models.MODE_RECORD}, setupOpts); err != nil {
		t.Fatalf("Load: %v", err)
	}

	events, err := hooks.WatchBindEvents(gCtx)
	if err != nil {
		t.Fatalf("WatchBindEvents: %v", err)
	}

	// A port the application will ask for and which nothing else holds.
	appPort := freePort(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: fmt.Sprintf(`cmd /C ""%s" -test.run=TestHelperServer -test.v"`, self),
	}
	cmd.Env = append(os.Environ(), helperListenEnv+"="+strconv.Itoa(appPort))
	// The application is still running while this test makes its assertions, so
	// os/exec's copier is writing to this concurrently — it has to be locked.
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := StartInstrumented(logger, cmd, ShimPath(SessionDir(clientPID))); err != nil {
		t.Fatalf("StartInstrumented: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// The ingress event is what tells Keploy to take over the advertised port.
	var ev models.IngressEvent
	select {
	case ev = <-events:
	case <-time.After(20 * time.Second):
		t.Fatalf("no ingress event was published; application output:\n%s", out.String())
	}

	if ev.OrigAppPort != uint16(appPort) {
		t.Fatalf("ingress event advertises port %d, want %d", ev.OrigAppPort, appPort)
	}
	if ev.NewAppPort == 0 || ev.NewAppPort == uint16(appPort) {
		t.Fatalf("the application's listener was not moved (moved port %d)", ev.NewAppPort)
	}

	// And the application really is listening on the moved port, not the one it
	// asked for — otherwise Keploy could not take that one over.
	deadline := time.Now().Add(10 * time.Second)
	var reached bool
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ev.NewAppPort), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			reached = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !reached {
		t.Fatalf("nothing is listening on the moved port %d; application output:\n%s", ev.NewAppPort, out.String())
	}

	// The port the application asked for must now be free for Keploy to bind.
	// This is the whole point of the move: without it Keploy has nowhere to put
	// the forwarder that gives the application its advertised port back.
	probe, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", appPort))
	if err != nil {
		t.Fatalf("keploy cannot take over the advertised port %d, so the move achieved nothing: %v", appPort, err)
	}
	_ = probe.Close()
}

// syncBuffer is an io.Writer safe to read while the process writing to it is
// still running. Used only for test diagnostics.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freePort returns a port that is free right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
