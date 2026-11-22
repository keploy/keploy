//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A request a parser read and holds while it reads on (one that reads ahead,
// as Generic does) is in flight in a replay, though its handler is blocked
// reading an empty socket: before holds were kept, that connection was
// settled, and a test's end judged the test without the call (a slip into the
// next test, or past the last one). A record never waits on it. Released, it
// is settled; a second release changes nothing.
func TestARequestTheParserHoldsIsInFlightWhileItReadsOn(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	read := make(chan []byte, 2)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := proxy.Read(buf)
			if err != nil {
				return
			}
			read <- append([]byte(nil), buf[:n]...)
		}
	}()
	if _, err := io.WriteString(app, "PUBLISH a"); err != nil {
		t.Fatal(err)
	}
	<-read
	// The parser holds what it read, and reads on.
	release := integrations.HoldRequest(integrations.WithRequestHolder(context.Background(), proxy))
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a held request on a connection blocked reading: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a held request: pending %d in a record, want 0", n)
	}
	before := tr.Progress()
	release()
	if tr.Progress() == before {
		t.Fatal("a release moved no progress")
	}
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("an answered request: pending %d, want 0", n)
	}
	release()
	if h := proxy.held.Load(); h != 0 {
		t.Fatalf("a second release unheld another request: held %d", h)
	}
}

// Without the proven switch's tracker the parser's holds mark nothing.
func TestAHoldWithoutATrackedConnectionMarksNothing(t *testing.T) {
	integrations.HoldRequest(context.Background())()
	integrations.HoldRequest(integrations.WithRequestHolder(context.Background(), nil))()
}

// A read that waits out its deadline with nothing (a parser polling an idle
// connection) wakes a settle wait but moves no progress, so an idle
// connection cannot keep the watchdog from seeing a wait that is stuck. A
// read that returns bytes moves it.
func TestAReadThatWaitsOutItsDeadlineIsNoProgress(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	buf := make([]byte, 64)
	before := tr.Progress()
	for i := 0; i < 3; i++ {
		if err := proxy.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := proxy.Read(buf); !isTimeout(err) {
			t.Fatalf("want a deadline's timeout, got %v", err)
		}
	}
	if tr.Progress() != before {
		t.Fatalf("idle reads moved progress: %d -> %d", before, tr.Progress())
	}
	if err := proxy.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(app, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Read(buf); err != nil {
		t.Fatal(err)
	}
	if tr.Progress() == before {
		t.Fatal("a read that returned bytes moved no progress")
	}
}

// The kept connection is still its socket to what reads the kernel's state
// of it: whether the application sent anything (util.ReceivedNothing, the
// handshake hold's pre-dial watch) and the TCP connection a refused mock.real
// dial resets (tcpConnOf).
func TestAKeptConnectionIsStillItsSocket(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	if !util.ReceivedNothing(proxy) {
		t.Fatal("the application sent nothing, but the kept connection says otherwise")
	}
	if _, err := io.WriteString(app, "x"); err != nil {
		t.Fatal(err)
	}
	waitUnread(t, proxy)
	if util.ReceivedNothing(proxy) {
		t.Fatal("the application sent a byte, but the kept connection says nothing came")
	}
	if tc, ok := tcpConnOf(proxy); !ok || tc == nil {
		t.Fatal("no TCP connection under the kept connection")
	}
	var plain net.Conn = &net.UnixConn{}
	if _, ok := tcpConnOf(plain); ok {
		t.Fatal("a unix connection is no TCP connection")
	}
}

// stallConn is a connection whose second read waits for release whatever
// deadline is set, then times out: the window in which the generic parser,
// reading on with a deadline before it matches, is blocked reading while it
// holds the request it read, held open for as long as the test needs. Every
// other read is the connection's own (with no deadline: SetReadDeadline is
// ignored), so the parser's next read waits for the connection to close.
type stallConn struct {
	net.Conn
	reads int
	// stalled is closed once the second read waits; release lets it time
	// out.
	stalled, release chan struct{}
}

func (c *stallConn) Read(b []byte) (int, error) {
	c.reads++
	if c.reads == 2 {
		close(c.stalled)
		<-c.release
		return 0, timeoutErr{}
	}
	return c.Conn.Read(b)
}

func (c *stallConn) SetReadDeadline(time.Time) error { return nil }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// genericStore answers PING with PONG from one recorded exchange.
type genericStore struct{ integrations.MockMemDb }

func (genericStore) GetSessionMocks() ([]*models.Mock, error) {
	return []*models.Mock{{Name: "mock-1", Kind: models.GENERIC, TestModeInfo: models.TestModeInfo{IsFiltered: true}, Spec: models.MockSpec{
		GenericRequests:  []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PING"}}}},
		GenericResponses: []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PONG"}}}},
	}}}, nil
}

func (genericStore) UpdateUnFilteredMock(_, _ *models.Mock) bool { return true }

// The generic parser's request is in flight, to a test's end, from the moment
// it is read until it is answered, though the parser reads on, blocked on an
// empty socket, before it matches (the hole keploy #4806's end count had: a
// test's end judged the test while its call was held, and the call slipped
// into the next test, or past the last one, a false green). The parser is
// handed the connection as handleConnection hands it.
func TestAGenericRequestReadAheadIsInFlightUntilAnswered(t *testing.T) {
	tr := &inflight{}
	app, accepted := acceptedPair(t, tr)
	stall := &stallConn{Conn: accepted.Conn, stalled: make(chan struct{}), release: make(chan struct{})}
	proxy := &trackedConn{Conn: stall, t: tr}
	tr.mu.Lock()
	delete(tr.conns, accepted)
	tr.conns[proxy] = struct{}{}
	tr.mu.Unlock()
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	ctx = integrations.WithRequestHolder(ctx, proxy)
	done := make(chan error, 1)
	go func() {
		done <- generic.New(zap.NewNop()).MockOutgoing(ctx, proxy, nil, genericStore{}, models.OutgoingOptions{})
	}()
	if _, err := io.WriteString(app, "PING"); err != nil {
		t.Fatal(err)
	}
	// The parser read PING and reads on: blocked reading, PING held.
	<-stall.stalled
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("the parser holds PING, blocked reading on: pending %d, want 1 (in flight until answered)", n)
	}
	close(stall.release)
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
		t.Fatalf("answer %q, %v", got, err)
	}
	waitSettled(t, tr, func() bool { return proxy.held.Load() == 0 })
	_ = app.Close()
	_ = proxy.Close()
	<-done
}

// readAheadParser is a parser that reads ahead of its answers; holds says
// whether it marks what it holds.
type readAheadParser struct{ holds bool }

func (r readAheadParser) HoldsRequests() bool { return r.holds }

// A connection served by a parser that reads ahead and does not mark what it
// holds is unknown, in a replay, until its handler is done with it: its
// protocol is named. One that marks them, one that answers before it reads
// again, and a record are not.
func TestAConnectionWhoseParserDoesNotMarkItsHoldsIsUnknown(t *testing.T) {
	tr := &inflight{}
	_, grpcConn := acceptedPair(t, tr)
	_, mongoConn := acceptedPair(t, tr)
	_, genericConn := acceptedPair(t, tr)
	_, mysqlConn := acceptedPair(t, tr)
	_, http2Conn := acceptedPair(t, tr)
	grpcConn.servedBy(integrations.GRPC, struct{}{})
	mongoConn.servedBy(integrations.MONGO_V2, readAheadParser{holds: false})
	genericConn.servedBy(integrations.GENERIC, generic.New(zap.NewNop()))
	mysqlConn.servedBy(integrations.MYSQL, struct{}{})
	http2Conn.servedBy(integrations.HTTP2, readAheadParser{holds: true})
	if got := tr.Unknown(true); strings.Join(got, ",") != "grpc,mongo_v2" {
		t.Fatalf("unknown %v, want [grpc mongo_v2]", got)
	}
	if got := tr.Unknown(false); len(got) != 0 {
		t.Fatalf("a record waits on no answer: unknown %v", got)
	}
	tr.forget(grpcConn)
	tr.forget(mongoConn)
	if got := tr.Unknown(true); len(got) != 0 {
		t.Fatalf("connections their handlers are done with: unknown %v", got)
	}
}
