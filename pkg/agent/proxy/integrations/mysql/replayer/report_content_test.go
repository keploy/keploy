package replayer

import (
	"errors"
	"io"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent"
)

// queryMiss is the report of a COM_QUERY no recorded mock answers.
func queryMiss(t *testing.T, query string) *models.MockMismatchReport {
	t.Helper()
	client, done := replay(t, &fakeMockDb{session: []*models.Mock{nativeHandshake()}})
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
