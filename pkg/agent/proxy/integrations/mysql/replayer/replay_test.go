package replayer

import (
	"errors"
	"io"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

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
