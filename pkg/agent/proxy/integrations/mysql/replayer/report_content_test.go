package replayer

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// queryMiss is the report of a COM_QUERY no recorded mock answers, replayed
// with the handshake and data, the data mocks the session records.
func queryMiss(t *testing.T, query string, data ...*models.Mock) *models.MockMismatchReport {
	t.Helper()
	client, done := replay(t, &fakeMockDb{session: append([]*models.Mock{nativeHandshake()}, data...)})
	client.t = t
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_QUERY}, query...))
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %q, %v after its query; want its connection closed unanswered", got, err)
	}
	var r reportCarrier
	if err := <-done; !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want the query's miss with its report", err)
	}
	return r.MismatchReport()
}

// A query's miss names its statement with its literals written "?" by
// default, in the summary and in the diff: a literal is a value the
// application sent.
func TestAQueryMissNamesItsStatementWithoutLiteralsByDefault(t *testing.T) {
	const query = "SELECT * FROM users WHERE email = 'kp-mysql-secret' AND id = 4242"
	rep := queryMiss(t, query)
	for _, f := range []string{rep.ActualSummary, rep.Diff, rep.NextSteps} {
		if strings.Contains(f, "kp-mysql-secret") || strings.Contains(f, "4242") {
			t.Errorf("the default report carries a literal: summary %q, diff %q", rep.ActualSummary, rep.Diff)
		}
	}
	if want := "COM_QUERY SELECT * FROM users WHERE email = ? AND id = ?"; rep.ActualSummary != want {
		t.Errorf("summary = %q, want %q", rep.ActualSummary, want)
	}
	if !strings.Contains(rep.Diff, "actual: SELECT * FROM users WHERE email = ? AND id = ?") || !rep.ContentWithheld {
		t.Errorf("diff = %q, withheld %v", rep.Diff, rep.ContentWithheld)
	}

	t.Cleanup(logcontent.Set(true))
	rep = queryMiss(t, query)
	if rep.ActualSummary != "COM_QUERY "+query || !strings.Contains(rep.Diff, "actual: "+query) || rep.ContentWithheld {
		t.Errorf("with the switch on: summary %q, diff %q", rep.ActualSummary, rep.Diff)
	}
}

// A query's miss names the closest recorded statement as it names the live
// one: with its literals written "?" by default, as a recorded literal is a
// value the application sent when it was recorded.
func TestAQueryMissNamesItsClosestStatementWithoutLiteralsByDefault(t *testing.T) {
	const recorded = "SELECT token FROM sessions WHERE owner = 'kp-recorded-secret' AND tries = 77"
	const query = "SELECT * FROM users WHERE email = 'kp-mysql-secret' AND id = 4242"
	closest := readbackMock("q-sessions", recorded, "row-1", zeroTime())
	rep := queryMiss(t, query, closest)
	if rep.ClosestMock != "q-sessions" {
		t.Fatalf("the miss names closest mock %q, want q-sessions: %+v", rep.ClosestMock, *rep)
	}
	for _, f := range []string{rep.ActualSummary, rep.Diff, rep.NextSteps} {
		if strings.Contains(f, "kp-recorded-secret") || strings.Contains(f, "77") {
			t.Errorf("the default report carries a recorded literal: summary %q, diff %q, next steps %q", rep.ActualSummary, rep.Diff, rep.NextSteps)
		}
	}
	if !strings.Contains(rep.Diff, "closest: SELECT token FROM sessions WHERE owner = ? AND tries = ?") || !rep.ContentWithheld {
		t.Errorf("diff = %q, withheld %v; want the closest statement named without its literals", rep.Diff, rep.ContentWithheld)
	}

	t.Cleanup(logcontent.Set(true))
	rep = queryMiss(t, query, readbackMock("q-sessions", recorded, "row-1", zeroTime()))
	if !strings.Contains(rep.Diff, "closest: "+recorded) || rep.ContentWithheld {
		t.Errorf("with the switch on: diff %q, withheld %v", rep.Diff, rep.ContentWithheld)
	}
}

// A statement longer than the report keeps is shaped whole before it is cut:
// a cut inside a literal would leave a literal never closed, of which nothing
// after its start could be kept, so the summary and the diff still name the
// statement's every clause, and no part of the literal.
func TestALongQueryMissIsShapedBeforeItIsCut(t *testing.T) {
	query := "SELECT * FROM users WHERE note = '" + strings.Repeat("kp-long-", 25) + "' AND id = 4242"
	rep := queryMiss(t, query)
	if want := "COM_QUERY SELECT * FROM users WHERE note = ? AND id = ?"; rep.ActualSummary != want {
		t.Errorf("summary = %q, want %q", rep.ActualSummary, want)
	}
	if !strings.Contains(rep.Diff, "actual: SELECT * FROM users WHERE note = ? AND id = ?") || strings.Contains(rep.Diff, "kp-long-") {
		t.Errorf("diff = %q", rep.Diff)
	}
}

// loginMiss is the report of a client that logs in as user, against a
// recorded handshake of recordedUser: no recorded HandshakeResponse41
// matches, and the miss's diff names the closest one's mismatch.
func loginMiss(t *testing.T, recordedUser, user string) *models.MockMismatchReport {
	t.Helper()
	m := nativeHandshake()
	m.Spec.MySQLRequests[0].PacketBundle.Message = clientHandshake(recordedUser, mysql.Native)
	client, done := replay(t, &fakeMockDb{session: []*models.Mock{m}})
	client.t = t
	client.greetAndAnswer(user, mysql.Native)
	var r reportCarrier
	if err := <-done; !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want the handshake's miss with its report", err)
	}
	return r.MismatchReport()
}

// A login as another user than the recording's misses at its
// HandshakeResponse41, and by default its report names the field that
// differs and neither user: a username is the application's login, as its
// database is. Through the agent's door too. With the switch on the diff
// names both, as it did.
func TestAHandshakeMissNamesNoUserByDefault(t *testing.T) {
	rep := loginMiss(t, "kp-rec-user", "kp-live-user")
	door := models.UnmatchedCall{Protocol: rep.Protocol, ActualSummary: rep.ActualSummary, Diff: rep.Diff, NextSteps: rep.NextSteps, ContentWithheld: rep.ContentWithheld}.ReportContent()
	for _, text := range []string{rep.ActualSummary + "\n" + rep.Diff + "\n" + rep.NextSteps, door.ActualSummary + "\n" + door.Diff + "\n" + door.NextSteps} {
		for _, v := range []string{"kp-rec-user", "kp-live-user"} {
			if strings.Contains(text, v) {
				t.Errorf("the default report names the user %q:\n%s", v, text)
			}
		}
	}
	if want := "username mismatch for handshake response, expected: (withheld), actual: (withheld)"; !strings.Contains(rep.Diff, want) || !rep.ContentWithheld {
		t.Errorf("the default diff %q does not name the username as what differs (%s)", rep.Diff, want)
	}
	t.Cleanup(logcontent.Set(true))
	rep = loginMiss(t, "kp-rec-user", "kp-live-user")
	if want := "username mismatch for handshake response, expected: kp-rec-user, actual: kp-live-user"; !strings.Contains(rep.Diff, want) || rep.ContentWithheld {
		t.Errorf("with the switch on the diff is %q, want it to name both users (%s)", rep.Diff, want)
	}
}

// The HandshakeResponse41 comparison's words are the miss's diff: by default
// a username or database that differs is named, and its values are not. With
// the switch on they are, word for word as before. A protocol option (the
// packet size, the character set, the plugin) is no value of the
// application's and is named with its values either way.
func TestAHandshakeMismatchNamesTheFieldAndNoLoginValue(t *testing.T) {
	bundle := func(user, db string) mysql.PacketBundle {
		p := clientHandshake(user, mysql.Native)
		p.Database = db
		return mysql.PacketBundle{Header: &mysql.PacketInfo{Type: mysql.HandshakeResponse41}, Message: p}
	}
	for _, tc := range []struct {
		name               string
		rec, live          mysql.PacketBundle
		withheld, verbatim string
	}{
		{"username", bundle("kp-rec-user", "db"), bundle("kp-live-user", "db"),
			"username mismatch for handshake response, expected: (withheld), actual: (withheld)",
			"username mismatch for handshake response, expected: kp-rec-user, actual: kp-live-user"},
		{"database", bundle("app", "kp-rec-db"), bundle("app", "kp-live-db"),
			"database mismatch for handshake response, expected: (withheld), actual: (withheld)",
			"database mismatch for handshake response, expected: kp-rec-db, actual: kp-live-db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				restore := logcontent.Set(on)
				err := matchHanshakeResponse41(context.Background(), zap.NewNop(), tc.rec, tc.live)
				restore()
				want := tc.withheld
				if on {
					want = tc.verbatim
				}
				if err == nil || err.Error() != want {
					t.Errorf("content %v: the comparison says %v, want %q", on, err, want)
				}
			}
		})
	}
	rec, live := bundle("app", "db"), bundle("app", "db")
	live.Message.(*mysql.HandshakeResponse41Packet).CharacterSet = 0x2d
	if err := matchHanshakeResponse41(context.Background(), zap.NewNop(), rec, live); err == nil ||
		err.Error() != "character set mismatch for handshake response, expected: 33, actual: 45" {
		t.Errorf("a protocol option's mismatch says %v", err)
	}
}
