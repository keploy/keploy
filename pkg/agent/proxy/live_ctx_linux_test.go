//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test's call to the app it started is handed to the live handler with the
// proxy's life, not the connection's: the handler builds the test case of a
// connection's last exchange after the connection has ended (the HTTP/1
// ingress builds each off its forwarding path), and the cancel of the
// connection's context as handleConnection returned dropped it whenever it
// came first: half the calls of a client that closes each connection after
// one exchange were recorded as nothing, and nothing counted them. The
// connection's values reach the handler all the same, a context derived from
// the handler's does not take the connection's cancel for its own, and the
// proxy's end still ends them.
func TestTheLiveHandlersContextOutlivesItsConnectionWithItsValues(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	port := pingServer(t)
	dest := ipv4Dest(t, fmt.Sprintf("127.0.0.1:%d", port))
	// The listener is this process's, the run's own; its caller is this
	// process's parent, which it descends from: a test's call to the app it
	// started.
	dest.KernelPid = uint32(os.Getppid())
	dest.AgentNsPid = uint32(os.Getppid())
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	cfg.Agent.MockMode = true
	p := New(zap.NewNop(), fakeDestInfo{dest: dest}, cfg)
	p.appPID = uint32(os.Getpid())
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
	handed := make(chan context.Context, 1)
	p.live = func(ctx context.Context, conn net.Conn, upstream string, _ uint16) {
		handed <- ctx
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		util.RelayRawPassthrough(conn, up)
	}

	life, end := context.WithCancelCause(context.Background())
	defer end(nil)
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(life, src) }()
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(app, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("the live call was not relayed to the app: %q, %v", buf, err)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
	ctx := awaitEvent(t, handed, "the live handler being handed the call")

	if err := ctx.Err(); err != nil {
		t.Fatalf("the live handler's context ended with its connection (%v): a test case built after the connection's end is dropped", err)
	}
	if got := ctx.Value(models.ClientConnectionIDKey); got == nil {
		t.Fatal("the live handler's context lacks the connection's values (its client connection id)")
	}
	derived, stop := context.WithCancel(ctx)
	defer stop()
	if err := derived.Err(); err != nil {
		t.Fatalf("a context derived from the live handler's took the connection's cancel for its own (%v)", err)
	}

	end(errProxyStopped) // the proxy stops
	if ctx.Err() == nil {
		t.Fatal("the proxy's end did not end the live handler's context")
	}
	awaitEvent(t, derived.Done(), "the proxy's end reaching a context derived from the live handler's")
	for name, c := range map[string]context.Context{"the live handler's context": ctx, "a context derived from it": derived} {
		if got := context.Cause(c); got != errProxyStopped {
			t.Fatalf("%s ended for %v, want the proxy's own cause (%v), not the connection's cancel", name, got, errProxyStopped)
		}
	}
}

var errProxyStopped = errors.New("the proxy stopped")
