package replayer

import (
	"errors"
	"io"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent"
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
// The summary names the query whole, as a report carries it with
// logcontent's switch on.
func TestACommandMissNamesItsDestination(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
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
