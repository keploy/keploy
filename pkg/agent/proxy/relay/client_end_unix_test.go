//go:build linux || darwin

package relay

import (
	"io"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// Linux and macOS only: Windows' read returns the reset too, and its socket
// keeps nothing that says the FIN came first (TCP_INFO_v0 has no rcv_nxt), so
// a FIN that no look found in before the reset is read as the client's reset
// there (finBeforeReset, client_socket_windows.go).

// The same closed client, with the relay's read of the client's end held
// until the FIN and the client's reset of the answer are both in: Linux's
// read returns the FIN and leaves the reset on the socket; macOS's returns
// the reset, and the relay tells from the socket that the FIN came first. On
// both the relay reads the FIN as the client's end, forwards it, and hands the
// reset on as it ends.
func TestAClosedClientWhoseFINAndResetAreBothInBeforeTheReadHasTheFINRead(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	release := make(chan struct{})
	c := &finInFlightConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: release}
	c.hook = func() {
		_ = app.Close()
		awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
	}
	run := runClientEnd(t, c, proxyServer)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination did not read the request: %v", err)
	}
	if _, err := server.Write([]byte("inflight")); err != nil {
		t.Fatal(err)
	}
	awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the answer")
	close(release)
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, want the forwarded FIN", len(rest), err)
	}
	_ = server.Close()
	run.ended(t, "inflight", true)
}
