//go:build linux

package http

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// A recordOne endpoint keeps one representative call (ptRecorder). A
// representative the session's manager drops as it is handed over is not
// one: its mark is given back, and the endpoint's next call is recorded. Held,
// the mark would skip every later call as a duplicate of a mock no recording
// holds, and the endpoint would replay with none. Each shape reaches the mark
// by its own path: a whole answer and an answer cut short after its head
// (recordV2, the latter through buildFaultAfterHead), and no answer
// (recordFault).

const recordOneOK = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"

// recordOneShapes are the calls, each on its own connection: what the
// upstream sends, and whether it then closes the connection.
var recordOneShapes = []struct {
	name   string
	answer string
	closes bool
}{
	{"a whole answer", recordOneOK, false},
	{"an answer cut short after its head", "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n0123456789", true},
	{"no answer", "", true},
}

// relayedTo is newRelayedConn with its destination accepted on l, so the
// calls of every connection reach one endpoint: the port is in its key.
func relayedTo(t *testing.T, l net.Listener, mgr *syncMock.SyncMockManager) *relayedConn {
	t.Helper()
	app, src := loopback(t)
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		got <- c
	}()
	dst, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dest := <-got
	if dest == nil {
		t.Fatal("the endpoint accepted no connection")
	}
	t.Cleanup(func() { _ = dst.Close() })
	core, logs := observer.New(zapcore.DebugLevel)
	return &relayedConn{app: app, dest: dest, src: src, dst: dst, mocks: make(chan *models.Mock, 8), spans: &spanLog{},
		mgr: mgr, core: core, logs: logs, done: make(chan error, 1)}
}

// recordOneCall records one call of shape on a connection of its own to l,
// by h, its session routed to mgr as the proxy routes it, and returns once
// recordV2 has returned.
func recordOneCall(t *testing.T, h *HTTP, l net.Listener, mgr *syncMock.SyncMockManager, answer string, closes bool) {
	t.Helper()
	c := relayedTo(t, l, mgr)
	c.startRelay(t)
	c.sess.RouteMocksViaSyncMock = true
	c.sess.Opts = models.OutgoingOptions{PassThroughPorts: []models.PassThroughRule{
		{Port: uint32(l.Addr().(*net.TCPAddr).Port), Mode: models.PassThroughRecordOne},
	}}
	go func() { c.done <- h.recordV2(context.Background(), c.sess) }()
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	if answer != "" {
		write(t, c.dest, c.dst, answer)
	}
	if closes {
		_ = c.dest.Close()
		if got, err := appRead(t, c.app); got != answer || err != io.EOF {
			t.Fatalf("the application read %q, %v; want %q, then EOF", got, err, answer)
		}
	} else {
		relayed(t, c.app, answer)
	}
	if _, err := c.finish(t); err != nil {
		t.Fatalf("recordV2: %v", err)
	}
}

// recordOneOut sets mgr's output to a new channel, which it returns: closed
// at once (closed), as at a recording's end, so the manager drops what it is
// handed, or open.
func recordOneOut(mgr *syncMock.SyncMockManager, closed bool) chan *models.Mock {
	out := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(out)
	if closed {
		mgr.CloseOutChan()
	}
	return out
}

// recordedTo is how many HTTP mocks out holds within a short wait for the first.
func recordedTo(out chan *models.Mock) int {
	n := 0
	timeout := time.After(time.Second)
	for {
		select {
		case m, ok := <-out:
			if !ok {
				return n
			}
			if m != nil && m.Kind == models.HTTP {
				n++
				timeout = time.After(100 * time.Millisecond)
			}
		case <-timeout:
			return n
		}
	}
}

func TestRecordV2_ARecordOneRepresentativeTheManagerDropsIsRecordedAgain(t *testing.T) {
	for _, shape := range recordOneShapes {
		t.Run(shape.name, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			h := &HTTP{Logger: zaptest.NewLogger(t), ptRecorder: &ptRecorder{logger: zaptest.NewLogger(t)}}
			mgr := syncMock.New(nil)

			// The control: a representative taken holds the endpoint, and
			// its next call is not recorded.
			out := recordOneOut(mgr, false)
			recordOneCall(t, h, l, mgr, shape.answer, shape.closes)
			if n := recordedTo(out); n != 1 {
				t.Fatalf("setup: the first call recorded %d mocks, want 1", n)
			}
			recordOneCall(t, h, l, mgr, shape.answer, shape.closes)
			if n := recordedTo(out); n != 0 {
				t.Fatalf("setup: a call after the representative was taken recorded %d mocks, want 0 (its duplicate)", n)
			}

			// A new endpoint (a fresh parser), whose first representative
			// the manager drops: the output has closed.
			h = &HTTP{Logger: zaptest.NewLogger(t), ptRecorder: &ptRecorder{logger: zaptest.NewLogger(t)}}
			recordOneOut(mgr, true)
			before := mgr.DropCount()
			recordOneCall(t, h, l, mgr, shape.answer, shape.closes)
			if got := mgr.DropCount(); got != before+1 {
				t.Fatalf("setup: the manager dropped %d mocks of the first call, want 1", got-before)
			}
			out = recordOneOut(mgr, false)
			recordOneCall(t, h, l, mgr, shape.answer, shape.closes)
			if n := recordedTo(out); n != 1 {
				t.Fatalf("the call after a representative the manager dropped recorded %d mocks, want 1: the dropped one kept its mark, and the endpoint replays with no mock", n)
			}
		})
	}
}

// The legacy path (parseFinalHTTP) gives a dropped representative's mark back
// as recordV2 does.
func TestParseFinalHTTP_ARecordOneRepresentativeTheManagerDropsIsRecordedAgain(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t), ptRecorder: &ptRecorder{logger: zaptest.NewLogger(t)}}
	opts := models.OutgoingOptions{PassThroughPorts: []models.PassThroughRule{{Port: 80, Mode: models.PassThroughRecordOne}}}
	mgr := syncMock.New(nil)
	ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "conn-1"), mgr)
	call := func() {
		t.Helper()
		at := time.Now()
		if err := h.parseFinalHTTP(ctx, &FinalHTTP{Req: canonicalRequest, Resp: canonicalResponse, ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}, 80, nil, opts, nil); err != nil {
			t.Fatalf("parseFinalHTTP: %v", err)
		}
	}
	recordOneOut(mgr, true)
	call()
	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("setup: the manager dropped %d mocks of the first call, want 1", got)
	}
	out := recordOneOut(mgr, false)
	call()
	if n := recordedTo(out); n != 1 {
		t.Fatalf("the call after a representative the manager dropped recorded %d mocks, want 1: the dropped one kept its mark", n)
	}
	call()
	if n := recordedTo(out); n != 0 {
		t.Fatalf("a call after the representative was taken recorded %d mocks, want 0 (its duplicate)", n)
	}
}
