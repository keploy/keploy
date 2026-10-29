package generic

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/identity"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMain fixes the replay pod's own name (identity.PodNameEnv) before any
// row reads it: without it the name is the test machine's hostname, which a
// CI container may name localhost or too short to alias, and the identity
// rows would rewrite nothing.
func TestMain(m *testing.M) {
	_ = os.Setenv(identity.PodNameEnv, "keploy-test-live-pod")
	os.Exit(m.Run())
}

// clocklessConn is a connection on which a deadline still to come is not set
// (none is, instead): a read waits for data, the close, or a deadline already
// passed, never for the clock. Its reads in progress are counted, and each
// read that starts is told on reading, when nothing waits there already.
type clocklessConn struct {
	net.Conn
	inFlight atomic.Int32
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
	select {
	case c.reading <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}

// A generic replay whose context ends while the parser reads the client
// returns only once the goroutine that reads it has ended. decodeGeneric
// returned the context's error at once and left that goroutine reading,
// matching and logging after it had returned. Nothing writes to the client or
// closes it here, and no read ends on the clock (clocklessConn), so only the
// context's end can end the read.
func TestTheGenericParserReturnsOnlyOnceItsReaderHasEnded(t *testing.T) {
	_, proxySide := tcpPair(t)
	conn := &clocklessConn{Conn: proxySide, reading: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), conn, nil, bigStore(), models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.reading: // reading on for more of the request
	case <-time.After(time.Minute):
		t.Fatal("the parser did not read the client")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeGeneric = %v, want the context's error", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("decodeGeneric did not return once its context was cancelled")
	}
	if n := conn.inFlight.Load(); n != 0 {
		t.Fatalf("decodeGeneric returned with %d read(s) of the client still in progress: the goroutine it started outlives it", n)
	}
}

// reply is a recorded exchange: req answered resp.
func reply(name, req, resp string) *models.Mock {
	m := exchange(name, req)
	m.TestModeInfo.IsFiltered = true
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: resp}}}}
	return m
}

// A connection kept open across test sets: each buffer the generic parser reads
// is aliased to the recorded pod of the test set armed then (identity.Current),
// not of the set armed when the connection was accepted.
func TestTheGenericParserAliasesEachReadToTheTestSetArmedThen(t *testing.T) {
	live := identity.Live().Name
	set1 := &models.IdentityAliasing{RecordedPods: []models.PodIdentity{{Name: "api-55f5fd9659-set1"}}}
	set2 := &models.IdentityAliasing{RecordedPods: []models.PodIdentity{{Name: "api-55f5fd9659-set2"}}}
	store := pingStore{mocks: []*models.Mock{
		reply("mock-1", "WHO api-55f5fd9659-set1", "ONE"),
		reply("mock-2", "WHO api-55f5fd9659-set2", "TWO"),
	}}
	var armed atomic.Pointer[models.IdentityAliasing]
	armed.Store(set1)
	ctx, cancel := context.WithCancel(identity.WithCurrent(context.Background(), func() (*models.IdentityAliasing, bool) { return armed.Load(), true }))
	defer cancel()
	app, proxySide := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(ctx, zap.NewNop(), []byte("WHO "+live), proxySide, nil, store, models.OutgoingOptions{Identity: set1})
	}()
	got := make([]byte, 3)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "ONE" {
		t.Fatalf("first set's answer %q, %v", got, err)
	}
	armed.Store(set2) // the next test set is armed; the connection stays open
	if _, err := io.WriteString(app, "WHO "+live); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "TWO" {
		t.Fatalf("the second set's read on the kept connection was answered %q, %v, want the second set's", got, err)
	}
	cancel()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the generic parser did not return once its context was cancelled")
	}
}
