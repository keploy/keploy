//go:build linux

package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// A client's FIN ends what it sends, not always what it reads: a client that
// only half-closed (shutdown(SHUT_WR)) takes what the destination sends after
// its FIN, and the relay says so once (Config.ClientReadAfterFIN), as the first
// such chunk the client's stack took is teed, before it goes on the stream. A
// client that closed its socket refuses what comes after (Config.ClientStopped)
// and is not said to read; nor is a client to which nothing was sent after
// its FIN, closed or half-closed: the destination's FIN alone does not tell
// the two apart.
func TestTheRelaySaysAHalfClosedClientReadAfterItsFIN(t *testing.T) {
	for _, row := range []struct {
		name string
		// close: the client closes its socket, rather than shutting its
		// sending side down.
		close bool
		// sent is what the destination sends after the client's FIN, in
		// writes the client reads one by one where it still reads.
		sent []string
		// want: ClientReadAfterFIN is told; stopped: ClientStopped is.
		want, stopped bool
	}{
		{name: "half-closed, two chunks after its FIN", sent: []string{"first", "second"}, want: true},
		{name: "half-closed, nothing after its FIN"},
		{name: "closed, a chunk after its FIN", close: true, sent: []string{"first"}, stopped: true},
		{name: "closed, nothing after its FIN", close: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			var told, stopped atomic.Int32
			clientEnded := make(chan struct{})
			r := New(Config{Logger: zap.NewNop(),
				ClientReadAfterFIN: func() { told.Add(1) },
				ClientStopped:      func() { stopped.Add(1) },
				PeerEnded: func(dir fakeconn.Direction, _ bool) {
					if dir == fakeconn.FromClient {
						close(clientEnded)
					}
				},
			}, proxyClient, connseq.NewUpstream(proxyServer))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			go func() { _, _ = io.Copy(io.Discard, r.ClientStream()) }()
			// toldAtFirst is what had been told when the first chunk the
			// destination sent after the FIN went on the stream: -1 if none
			// went on it.
			var toldAtFirst atomic.Int32
			toldAtFirst.Store(-1)
			streamDone := make(chan struct{})
			go func() {
				defer close(streamDone)
				for {
					c, err := r.DestStream().ReadChunk()
					if err != nil {
						return
					}
					if bytes.Contains(c.Bytes, []byte("first")) {
						toldAtFirst.CompareAndSwap(-1, told.Load())
					}
				}
			}()

			if _, err := app.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			readFull(t, server, "request")
			if row.close {
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := app.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-clientEnded:
			case <-time.After(10 * time.Second):
				t.Fatal("the relay did not read the client's end within 10s")
			}
			for _, b := range row.sent {
				if _, err := server.Write([]byte(b)); err != nil {
					t.Fatal(err)
				}
				if !row.close {
					readFull(t, app, b)
				}
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			if !row.close {
				_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
				if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
					t.Fatalf("the half-closed client read %d byte(s), %v, after the destination's FIN; want io.EOF", n, err)
				}
				_ = app.Close()
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the relay did not end within 10s of both ends")
			}
			<-streamDone
			if got := told.Load(); row.want && got != 1 || !row.want && got != 0 {
				t.Fatalf("ClientReadAfterFIN told %d time(s), want %v (once, or never)", got, row.want)
			}
			if row.want && toldAtFirst.Load() != 1 {
				t.Fatalf("ClientReadAfterFIN had been told %d time(s) as the first chunk after the FIN went on the stream, want once: a parser that reads that chunk must know", toldAtFirst.Load())
			}
			if got := stopped.Load() != 0; got != row.stopped {
				t.Fatalf("ClientStopped told: %v, want %v", got, row.stopped)
			}
		})
	}
}

// readFull reads len(want) bytes at c and checks they are want.
func readFull(t *testing.T, c net.Conn, want string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != want {
		t.Fatalf("read %q, %v; want %q", buf, err, want)
	}
	_ = c.SetReadDeadline(time.Time{})
}
