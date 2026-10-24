package replayer

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// clocklessConn is a connection on which a deadline still to come is not set
// (none is, instead): a read waits for data, the close, or a deadline already
// passed, never for the clock. Its reads in progress are counted, and, once
// armed, each read that starts is told on reading, when nothing waits there
// already.
type clocklessConn struct {
	net.Conn
	inFlight atomic.Int32
	armed    atomic.Bool
	reading  chan struct{}
}

// passedOrNone is t once it has passed, and no deadline before.
func passedOrNone(t time.Time) time.Time {
	if t.IsZero() || t.After(time.Now()) {
		return time.Time{}
	}
	return t
}

func (c *clocklessConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(passedOrNone(t))
}

func (c *clocklessConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(passedOrNone(t))
}

func (c *clocklessConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(passedOrNone(t))
}

func (c *clocklessConn) Read(p []byte) (int, error) {
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	if c.armed.Load() {
		select {
		case c.reading <- struct{}{}:
		default:
		}
	}
	return c.Conn.Read(p)
}

// A MySQL replay whose context ends while it waits for the client's next
// command returns only once the goroutine that serves the client has ended.
// Replay returned the context's error at once and left that goroutine
// reading, matching and logging after it had returned. The client sends
// nothing after its login, nothing closes it, and no read ends on the clock
// (clocklessConn: the command phase polls with a read deadline), so only the
// context's end can end the read.
func TestReplayReturnsOnlyOnceItsReaderHasEnded(t *testing.T) {
	client, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
	conn := &clocklessConn{Conn: agentEnd, reading: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- Replay(ctx, zap.NewNop(), conn, &models.ConditionalDstCfg{Addr: testDBAddr}, &fakeMockDb{session: []*models.Mock{nativeHandshake()}}, models.OutgoingOptions{})
	}()
	c := mysqlClient{t: t, c: client}
	c.greetAndAnswer("app", mysql.Native)
	// On a pipe the login was read whole once its write returned: a read
	// that starts from here on waits for what the client sends next.
	conn.armed.Store(true)
	if ok := c.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its login; want the recorded OK", ok)
	}
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.reading: // waiting for the next command
	case <-time.After(time.Minute):
		t.Fatal("Replay did not read on for the next command")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Replay = %v, want the context's error", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Replay did not return once its context was cancelled")
	}
	if n := conn.inFlight.Load(); n != 0 {
		t.Fatalf("Replay returned with %d read(s) of the client still in progress: the goroutine it started outlives it", n)
	}
}

// A client that opens a MySQL connection waits to be greeted. With no MySQL
// session mock to greet it with, Replay closes the connection unanswered, and
// that is a miss. Before, Replay returned nil and the proxy closed the
// connection with nothing reported: a test whose application was refused its
// database passed.
func TestAConnectionWithNoHandshakeMockIsAMiss(t *testing.T) {
	dataOnly := nativeHandshake()
	dataOnly.Spec.Metadata = map[string]string{"type": "mocks"}
	dataOnly.TestModeInfo.Lifetime = models.LifetimePerTest

	for _, tc := range []struct {
		name    string
		session []*models.Mock
		diff    string
	}{
		{"no MySQL session mock at all", nil, "no MySQL session mock"},
		{"MySQL session mocks, none of them a handshake", []*models.Mock{dataOnly}, "none of the 1 MySQL mocks in the session pool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, done := replay(t, &fakeMockDb{session: tc.session})
			client.t = t
			if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
				t.Fatalf("the client read %q, %v; want its connection closed unanswered", got, err)
			}
			requireConnectionMiss(t, <-done, "connection phase: new connection", models.MatchPhaseNoMocks, tc.diff)
		})
	}
}

// A miss of the command phase names the upstream the client dialled, as one of
// the connection phase does: Replay names it on any miss it returns. The query
// below has no data mock to match, so the connection is closed unanswered.
func TestACommandMissNamesItsDestination(t *testing.T) {
	client, done := replay(t, &fakeMockDb{session: []*models.Mock{nativeHandshake()}})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1"...))
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %q, %v after its query; want its connection closed unanswered", got, err)
	}
	err := <-done
	var r reportCarrier
	if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want the query's miss with its report", err)
	}
	if rep := r.MismatchReport(); rep.Destination != testDBAddr || rep.ActualSummary != "COM_QUERY SELECT 1" {
		t.Fatalf("report %+v; want the miss of COM_QUERY SELECT 1 at %s", *rep, testDBAddr)
	}
}
