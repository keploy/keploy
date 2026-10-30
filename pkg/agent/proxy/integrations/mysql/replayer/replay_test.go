package replayer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// exactDb is fakeMockDb serving exactly (integrations.ExactServing): a
// recording answers a call only with the call's own values.
type exactDb struct{ *fakeMockDb }

func (exactDb) ExactServing() bool { return true }

// The line a command's miss closes its connection on names what to do next
// as the miss report says it (mismatch.LogNextStep): a recording turned away
// for another inline literal advises marking the literal as noise, written as
// the user marks it ({"body.query.literals.0": []}, a position keploy
// counts, kept whole), and no stretch of either side's literal appears in the
// log in any encoding. Shaped as a key, the position was written {} in the
// advice to write it as listed.
func TestACommandMissClosesItsConnectionWithTheAdviceAsTheUserWritesIt(t *testing.T) {
	const recTok, liveTok = "sk_rec_Mm3Pq8Zt", "sk_live_Q7wZr9Kf"
	tokenQuery := func(tok string) string { return "SELECT id FROM sessions WHERE token = '" + tok + "'" }
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	db := exactDb{&fakeMockDb{session: []*models.Mock{nativeHandshake(), readbackMock("q1", tokenQuery(recTok), "row=1", zeroTime())}}}
	client, done := replayLogged(t, db, logger)
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_QUERY}, tokenQuery(liveTok)...))
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %q, %v after its query; want its connection closed unanswered", got, err)
	}
	if err := <-done; !errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned %v; want the query's miss", err)
	}
	const line = "Connection closing due to no matching mock found."
	lines := logs.FilterMessage(line).All()
	if len(lines) == 0 {
		t.Fatalf("the line %q was not logged; got %v", line, logs.All())
	}
	if next := fmt.Sprint(lines[0].ContextMap()["next_step"]); !strings.Contains(next, `{"body.query.literals.0": []}`) {
		t.Errorf("next_step does not advise the literal's noise key as the user writes it: %s", next)
	}
	log := out.String()
	if !strings.Contains(log, line) {
		t.Fatalf("the rendered log is not the one checked: %s", log)
	}
	for what, content := range map[string]string{"the recorded literal": recTok, "the live literal": liveTok} {
		if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}

// markingDb is fakeMockDb marking every command's round, of a staging view
// when staging (integrations.StagedFinder), and holding it to that round
// (integrations.RoundHolder): the replay never moves under it.
type markingDb struct {
	*fakeMockDb
	staging bool
}

func (m markingDb) MarkCallRound() integrations.CallRound {
	return integrations.NewCallRound(1, nil).InStaging(m.staging)
}

func (markingDb) FindStagedNotOwned(models.Kind, integrations.CallRound, func(*models.Mock) bool) (string, bool) {
	return "", false
}

func (m markingDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return m, func() bool { return false }
}

// restagingDb is markingDb whose hold finds the staging moved under every
// command: the replay staged a test set again after the command's mark.
type restagingDb struct{ markingDb }

func (m restagingDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return m, func() bool { return true }
}

// A command the replay staged a test set again under misses saying so, and
// is never logged as a command the set never recorded (REPLAY-ORPHAN): its
// matcher, held to its first read, read nothing of the set because the set
// moved, not because the recording lacks the command. A command of a set
// that recorded no data mock for it still is.
func TestACommandStagedAgainUnderIsNoOrphanOfTheRecording(t *testing.T) {
	const orphan = "REPLAY-ORPHAN"
	const note = "staged a test set again while it decided this command"
	for _, tc := range []struct {
		name     string
		db       func(*fakeMockDb) integrations.MockMemDb
		restaged bool
	}{
		{"staged again under the command", func(f *fakeMockDb) integrations.MockMemDb { return restagingDb{markingDb{f, false}} }, true},
		{"a set that recorded no data mock for it", func(f *fakeMockDb) integrations.MockMemDb { return markingDb{f, false} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.ErrorLevel)
			client, done := replayLogged(t, tc.db(&fakeMockDb{session: []*models.Mock{nativeHandshake()}}), zap.New(core))
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
			if said := strings.Contains(r.MismatchReport().NextSteps, note); said != tc.restaged {
				t.Fatalf("the miss says the set was staged again under it: %v, want %v (%q)", said, tc.restaged, r.MismatchReport().NextSteps)
			}
			if logged := logs.FilterMessageSnippet(orphan).Len() > 0; logged == tc.restaged {
				t.Fatalf("the command's %s line was logged: %v, want %v", orphan, logged, !tc.restaged)
			}
		})
	}
}

// A command-phase miss carries where its command first read the round
// (integrations.StampMark): the proxy tells the miss it was made while the
// set's mocks were being staged from that mark, where the command was
// decided, never from the view serving as the miss reaches it. A store that
// marks no round leaves it unmarked.
func TestACommandMissCarriesWhereItsCommandWasMarked(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   func(*fakeMockDb) integrations.MockMemDb
		want models.RoundMark
	}{
		{"marked in a staging", func(f *fakeMockDb) integrations.MockMemDb { return markingDb{f, true} }, models.RoundMarkedStaging},
		{"marked in a staged set", func(f *fakeMockDb) integrations.MockMemDb { return markingDb{f, false} }, models.RoundMarkedSettled},
		{"a store that marks no round", func(f *fakeMockDb) integrations.MockMemDb { return f }, models.RoundUnmarked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, done := replay(t, tc.db(&fakeMockDb{session: []*models.Mock{nativeHandshake()}}))
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
			if got := r.MismatchReport().Mark; got != tc.want {
				t.Fatalf("the command's miss carries mark %d; want %d", got, tc.want)
			}
		})
	}
}

// testMarkingDb is markingDb whose mark is of a test's view: the connection's
// first read found a test open.
type testMarkingDb struct{ markingDb }

func (m testMarkingDb) MarkCallRound() integrations.CallRound {
	return m.markingDb.MarkCallRound().InView(true, false)
}

// heldEmptyDb is markingDb whose hold reads nothing: the replay staged the
// next set after the connection's mark, and the held store finds no
// recording of the set the connection was marked in, while the store itself
// (the next set's) holds a handshake that would answer it.
type heldEmptyDb struct{ markingDb }

func (m heldEmptyDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return &fakeMockDb{}, func() bool { return true }
}

// A miss of the connection phase carries where its connection first read the
// round (integrations.StampMark), as a command's does: the proxy tells the
// miss it was made while the set's mocks were being staged, and in which
// test, from that mark, never from the view serving as the miss reaches it.
// So does a connection with no handshake to greet it with, and one whose
// HandshakeResponse41 the recording does not hold. A store that marks no
// round leaves it unmarked.
func TestAConnectionMissCarriesWhereItsConnectionWasMarked(t *testing.T) {
	misses := map[string]struct {
		session []*models.Mock
		client  func(c mysqlClient)
	}{
		"no handshake to greet it with": {nil, func(mysqlClient) {}},
		"a handshake response the recording does not hold": {[]*models.Mock{nativeHandshake()}, func(c mysqlClient) {
			c.greetAndAnswer("someone-else", mysql.Native)
		}},
	}
	for _, tc := range []struct {
		name   string
		db     func(*fakeMockDb) integrations.MockMemDb
		want   models.RoundMark
		inTest bool
	}{
		{"marked in a staging", func(f *fakeMockDb) integrations.MockMemDb { return markingDb{f, true} }, models.RoundMarkedStaging, false},
		{"marked between tests in a staged set", func(f *fakeMockDb) integrations.MockMemDb { return markingDb{f, false} }, models.RoundMarkedSettled, false},
		{"marked in a test", func(f *fakeMockDb) integrations.MockMemDb { return testMarkingDb{markingDb{f, false}} }, models.RoundMarkedSettled, true},
		{"a store that marks no round", func(f *fakeMockDb) integrations.MockMemDb { return f }, models.RoundUnmarked, false},
	} {
		for miss, m := range misses {
			t.Run(tc.name+": "+miss, func(t *testing.T) {
				client, done := replay(t, tc.db(&fakeMockDb{session: m.session}))
				m.client(client)
				if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
					t.Fatalf("the client read %q, %v; want its connection closed unanswered", got, err)
				}
				err := <-done
				var r reportCarrier
				if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
					t.Fatalf("Replay returned %v; want the connection's miss with its report", err)
				}
				if got := r.MismatchReport(); got.Mark != tc.want || got.MarkedInTest != tc.inTest {
					t.Fatalf("WRONG MARK: the connection's miss carries mark %d (in a test: %v); want %d (%v)", got.Mark, got.MarkedInTest, tc.want, tc.inTest)
				}
			})
		}
	}
}

// A connection the replay staged the next set under, after its mark, is
// never answered with that set's handshake: its connection phase reads the
// store held to the mark (integrations.HoldToRound), finds nothing of the set
// it was marked in, and misses saying the set was staged again, never that
// the recording holds no handshake (no "no mysql mocks found" error line). A
// connection whose handshake response the recording does not hold, with the
// staging moved under it, says so too.
func TestAConnectionStagedAgainUnderIsNeverAnsweredByTheNextSet(t *testing.T) {
	const note = "staged a test set again while it answered this connection's handshake"
	for _, tc := range []struct {
		name   string
		db     func(*fakeMockDb) integrations.MockMemDb
		client func(c mysqlClient)
	}{
		{"the next set holds the handshake", func(f *fakeMockDb) integrations.MockMemDb { return heldEmptyDb{markingDb{f, false}} }, func(mysqlClient) {}},
		{"staged again under its handshake response", func(f *fakeMockDb) integrations.MockMemDb { return restagingDb{markingDb{f, false}} }, func(c mysqlClient) {
			c.greetAndAnswer("someone-else", mysql.Native)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.ErrorLevel)
			client, done := replayLogged(t, tc.db(&fakeMockDb{session: []*models.Mock{nativeHandshake()}}), zap.New(core))
			tc.client(client)
			// One header read, not a read to the end: a connection greeted
			// from the next set's handshake waits for its answer.
			var hdr [4]byte
			if n, err := io.ReadFull(client.c, hdr[:]); n != 0 || err == nil {
				t.Fatalf("WRONG SET: the client read %x, %v; want its connection closed unanswered, never greeted from the next set", hdr[:n], err)
			}
			err := <-done
			var r reportCarrier
			if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
				t.Fatalf("Replay returned %v; want the connection's miss with its report", err)
			}
			if got := r.MismatchReport(); !strings.Contains(got.NextSteps, note) || got.Mark != models.RoundMarkedSettled {
				t.Fatalf("WRONG REASON: the miss says %q (mark %d); want the set staged again under it, marked where it was", got.NextSteps, got.Mark)
			}
			if n := logs.FilterMessageSnippet("no mysql mocks found").Len(); n != 0 {
				t.Fatalf("the connection logged %d \"no mysql mocks found\" errors: the recording holds its handshake", n)
			}
		})
	}
}

// heldCountingDb is markingDb counting the consumes made through it (outer)
// and through the store its hold returns (held).
type heldCountingDb struct {
	markingDb
	outer, held *int
}

func (m heldCountingDb) UpdateUnFilteredMock(*models.Mock, *models.Mock) bool {
	*m.outer++
	return true
}

func (m heldCountingDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return heldCounter{m.fakeMockDb, m.held}, func() bool { return false }
}

// heldCounter is the store heldCountingDb's hold returns.
type heldCounter struct {
	*fakeMockDb
	n *int
}

func (h heldCounter) UpdateUnFilteredMock(*models.Mock, *models.Mock) bool {
	*h.n++
	return true
}

// A connection's login is consumed through the store held to the
// connection's mark (integrations.HoldToRound), never through the parser's
// store: once the replay staged a test set again, the held store refuses the
// consume, and a handshake of the set the connection was marked in is never
// spent in the next one.
func TestAConnectionsLoginIsConsumedThroughTheStoreHeldToItsMark(t *testing.T) {
	var outer, held int
	client, done := replay(t, heldCountingDb{markingDb{&fakeMockDb{session: []*models.Mock{nativeHandshake()}}, false}, &outer, &held})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	_ = client.c.Close()
	<-done
	if outer != 0 || held != 1 {
		t.Fatalf("WRONG STORE: the login was consumed %d times through the parser's store and %d through the held one; want 0 and 1", outer, held)
	}
}
