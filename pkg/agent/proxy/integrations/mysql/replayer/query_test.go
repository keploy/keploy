package replayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// misses is the proxy's mismatch sink as a test sees it: what the replayer
// reported through models.ReportMockMismatch, in order.
type misses struct {
	mu   sync.Mutex
	errs []error
}

func (m *misses) report(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs = append(m.errs, err)
}

func (m *misses) all() []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]error(nil), m.errs...)
}

// replayReporting is replay with the proxy's mismatch reporter installed, as
// the proxy installs it for every connection of a test run.
func replayReporting(t *testing.T, db integrations.MockMemDb) (mysqlClient, <-chan error, *misses) {
	t.Helper()
	client, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
	sink := &misses{}
	ctx := models.WithMockMismatchReporter(context.Background(), sink.report)
	ret := make(chan error, 1)
	go func() {
		ret <- Replay(ctx, zap.NewNop(), agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	return mysqlClient{t: t, c: client}, ret, sink
}

// versionMock is a session mock answering SELECT @@version with one row.
func versionMock() *models.Mock {
	rs := &mysql.TextResultSet{
		ColumnCount: 1,
		Columns: []*mysql.ColumnDefinition41{{Header: mysql.Header{PayloadLength: 22, SequenceID: 2}, Catalog: "def", Name: "v", OrgName: "v",
			FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255, Type: byte(mysql.FieldTypeVarString), Filler: []byte{0, 0}}},
		EOFAfterColumns: []byte{5, 0, 0, 3, 0xfe, 0, 0, 2, 0},
		Rows:            []*mysql.TextRow{{Header: mysql.Header{SequenceID: 4}, Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeVarString, Name: "v", Value: "8.0.36"}}}},
		FinalResponse:   &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, 5, 0xfe, 0, 0, 2, 0}},
	}
	q := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "version", Kind: models.MySQL}
	q.TestModeInfo.Lifetime = models.LifetimeSession
	q.Spec.Metadata = map[string]string{"type": "config"}
	q.Spec.MySQLRequests = []mysql.Request{comQueryReq("SELECT @@version")}
	q.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "TextResultSet"}, Message: rs}}}
	return q
}

// loggedIn is a client that has logged in to a replay serving db with the
// proxy's reporter installed.
func loggedIn(t *testing.T, db integrations.MockMemDb) (mysqlClient, <-chan error, *misses) {
	t.Helper()
	client, done, sink := replayReporting(t, db)
	// A wrong answer can leave both ends of the pipe writing, each waiting
	// for the other to read: the deadline turns that into a failure.
	if err := client.c.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	return client, done, sink
}

// requireOneMiss fails the test unless the sink holds exactly one miss, of
// summary.
func requireOneMiss(t *testing.T, sink *misses, summary string) {
	t.Helper()
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the replay reported %d misses (%v); want the one miss of %s", len(got), got, summary)
	}
	var r reportCarrier
	if !errors.Is(got[0], models.ErrNoMockMatched) || !errors.As(got[0], &r) || r.MismatchReport() == nil {
		t.Fatalf("the replay reported %v; want a miss with its report", got[0])
	}
	if s := r.MismatchReport().ActualSummary; s != summary {
		t.Fatalf("the miss reported is of %q; want %q", s, summary)
	}
}

// A statement with no matching mock is answered as a server answers a
// statement it cannot run, with an ERR packet, and the connection serves the
// client's next statement. The replay closed the connection instead: inside a
// transaction the app may not reconnect, so the rest of its request failed
// (keploy/api-server#2514). The miss is reported all the same, before the
// answer, so its test still has it.
func TestACommandMissIsAnsweredWithAnErrorAndTheConnectionServesOn(t *testing.T) {
	client, done, sink := loggedIn(t, &fakeMockDb{session: []*models.Mock{nativeHandshake(), versionMock()}})

	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1 AS drifted"...))
	var hdr [4]byte
	if _, err := io.ReadFull(client.c, hdr[:]); err != nil {
		t.Fatalf("the client waited for the answer to its unmatched query and got %v; want an error packet on an open connection", err)
	}
	if hdr[3] != 1 {
		t.Fatalf("the error packet has sequence id %d; want 1, one past the query's", hdr[3])
	}
	payload := make([]byte, int(binary.LittleEndian.Uint32(append(hdr[:3:3], 0))))
	if _, err := io.ReadFull(client.c, payload); err != nil {
		t.Fatal(err)
	}
	errPkt, err := phase.DecodeERR(context.Background(), payload, mysql.CLIENT_PROTOCOL_41)
	if err != nil || errPkt.Header != mysql.ERR {
		t.Fatalf("the client read % x (%v); want an ERR packet", payload, err)
	}
	if errPkt.ErrorCode != missErrCode || errPkt.SQLState != missErrState || errPkt.ErrorMessage != missErrMessage {
		t.Fatalf("the error is %d (%s) %q; want %d (%s) %q", errPkt.ErrorCode, errPkt.SQLState, errPkt.ErrorMessage, missErrCode, missErrState, missErrMessage)
	}
	requireOneMiss(t, sink, "COM_QUERY SELECT 1 AS drifted")

	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT @@version"...))
	if got := client.readResultSet(); !bytes.Contains(got, []byte("8.0.36")) {
		t.Fatalf("the client read % x for its next query; want the recorded row", got)
	}
	if n := len(sink.all()); n != 1 {
		t.Fatalf("the replay reported %d misses; want only the first query's", n)
	}

	_ = client.c.Close()
	if err := <-done; errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned %v once the client left; its miss was reported, not returned", err)
	}
}

// A command the client expects no answer to (COM_STMT_CLOSE here) that has no
// matching mock is reported and left unanswered: an error packet would be
// read as the answer to the client's next command. The next query gets its
// own answer.
func TestACommandMissTheClientExpectsNoAnswerToIsLeftUnanswered(t *testing.T) {
	client, done, sink := loggedIn(t, &fakeMockDb{session: []*models.Mock{nativeHandshake(), versionMock()}})

	client.send(0, []byte{mysql.COM_STMT_CLOSE, 7, 0, 0, 0})
	requireEventually(t, func() bool { return len(sink.all()) == 1 }, "the replay to report the miss of COM_STMT_CLOSE")
	requireOneMiss(t, sink, "COM_STMT_CLOSE")

	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT @@version"...))
	if got := client.readResultSet(); !bytes.Contains(got, []byte("8.0.36")) {
		t.Fatalf("the client read % x for the query after COM_STMT_CLOSE; want the recorded row first", got)
	}

	_ = client.c.Close()
	<-done
}

// With no reporter to take the miss (a caller that installed none), the miss
// is returned and the connection ends unanswered, so no miss goes unreported.
func TestACommandMissWithNoReporterEndsTheConnection(t *testing.T) {
	client, done := replay(t, &fakeMockDb{session: []*models.Mock{nativeHandshake()}})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1"...))
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %q, %v; want its connection closed unanswered", got, err)
	}
	if err := <-done; !errors.Is(err, models.ErrNoMockMatched) || models.IsMismatchReported(err) {
		t.Fatalf("Replay returned %v; want the unreported miss", err)
	}
}

// A miss whose error answer cannot be written (the client is gone) ends the
// connection, and is returned marked reported: it was queued already, and the
// proxy must not queue it again.
func TestACommandMissWhoseAnswerCannotBeWrittenIsNotReportedTwice(t *testing.T) {
	client, done, sink := loggedIn(t, &fakeMockDb{session: []*models.Mock{nativeHandshake()}})
	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1"...))
	requireEventually(t, func() bool { return len(sink.all()) == 1 }, "the replay to report the miss")
	// The replay is writing its answer to a client that reads nothing; on a
	// pipe that write waits, and fails once the client closes.
	_ = client.c.Close()
	err := <-done
	if !models.IsMismatchReported(err) || !errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned %v; want the miss, marked reported", err)
	}
	requireOneMiss(t, sink, "COM_QUERY SELECT 1")
}

// requireEventually waits for cond, failing the test with what after 20s.
func requireEventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A COM_STMT_CLOSE that misses still forgets its statement: the connection
// serves on after the miss, and the closed statement's decode state would
// otherwise stay for as long as the connection does.
func TestAClosedStatementIsForgottenWhenItsCloseMisses(t *testing.T) {
	client, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_TEST,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7}},
		StmtIDToQuery:      map[uint32]string{7: "SELECT ?"},
		NextStmtID:         8,
	}
	decodeCtx.ServerGreetings.Store(agentEnd, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
	decodeCtx.LastOp.Store(agentEnd, wire.RESET)
	sink := &misses{}
	ctx := models.WithMockMismatchReporter(context.Background(), sink.report)
	done := make(chan error, 1)
	go func() {
		done <- simulateCommandPhase(ctx, zap.NewNop(), agentEnd, testDBAddr, &fakeMockDb{}, decodeCtx, models.OutgoingOptions{})
	}()

	c := mysqlClient{t: t, c: client}
	c.send(0, []byte{mysql.COM_STMT_CLOSE, 7, 0, 0, 0})
	requireEventually(t, func() bool { return len(sink.all()) == 1 }, "the replay to report the miss of COM_STMT_CLOSE")
	// The miss is reported after the statement is forgotten, under the
	// sink's lock: reading the maps now is ordered after their update.
	if _, ok := decodeCtx.PreparedStatements[7]; ok {
		t.Fatal("statement 7 is still prepared after its COM_STMT_CLOSE missed")
	}
	if _, ok := decodeCtx.StmtIDToQuery[7]; ok {
		t.Fatal("statement 7's query is still kept after its COM_STMT_CLOSE missed")
	}
	_ = client.Close()
	<-done
}
