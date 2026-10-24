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
