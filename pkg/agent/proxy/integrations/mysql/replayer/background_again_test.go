package replayer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const (
	// sweepQuery is a timer's statement: judging marked its recording.
	sweepQuery = "UPDATE leases SET owner = NULL WHERE expires_at < 1761555000"
	// ownQuery is the test's own statement: not marked.
	ownQuery = "SELECT v FROM kv WHERE id = 1"
	// ownSweep is the test's own statement of the sweep's shape.
	ownSweep = "UPDATE leases SET owner = NULL WHERE expires_at < 1761555111"
)

// againRecording is test T's recording on connection c1: the timer's
// COM_QUERY (bg-sweep) and its EXECUTE of poolQuery with 7 (bg-exec, its
// query kept on it as the recorder keeps it), both marked as the
// application's background work; the test's own COM_QUERY (own-q); and an
// EXECUTE of otherQuery with 5 marked too, whose recording kept no query, as
// an older keploy's did (bg-noquery). And two of the test's own the
// matcher would answer the timer's commands with by kind: an EXECUTE of
// poolQuery with 9 (own-exec-9) and the sweep with another literal
// (own-sweep). The statements' PREPAREs are the connection's.
func againRecording() (preps, owned []*models.Mock) {
	ts := diffBase.Add(time.Millisecond)
	preps = []*models.Mock{diffPrepareMock("prep-pool", "c1", poolQuery, 1, ts), diffPrepareMock("prep-other", "c1", otherQuery, 2, ts)}
	at := func(i int) time.Time { return ts.Add(time.Duration(i) * time.Millisecond) }
	exec := diffMock("bg-exec", "c1", []mysql.Request{executeReq(1, "7")}, at(1), models.LifetimePerTest, "mocks")
	exec.Spec.MySQLRequests[0].SetStoredQuery(poolQuery)
	ownExec := diffMock("own-exec-9", "c1", []mysql.Request{executeReq(1, "9")}, at(5), models.LifetimePerTest, "mocks")
	ownExec.Spec.MySQLRequests[0].SetStoredQuery(poolQuery)
	owned = []*models.Mock{
		diffMock("bg-sweep", "c1", []mysql.Request{queryReq(sweepQuery)}, at(2), models.LifetimePerTest, "mocks"),
		exec,
		diffMock("own-q", "c1", []mysql.Request{queryReq(ownQuery)}, at(3), models.LifetimePerTest, "mocks"),
		diffMock("bg-noquery", "c1", []mysql.Request{executeReq(2, "5")}, at(4), models.LifetimePerTest, "mocks"),
		ownExec,
		diffMock("own-sweep", "c1", []mysql.Request{queryReq(ownSweep)}, at(6), models.LifetimePerTest, "mocks"),
	}
	return preps, owned
}

// stageAgain serves againRecording as the proven switch does (exact: test
// T's mapping is its own recordings, with T's marks) or as keploy test does.
func stageAgain(t *testing.T, exact bool) *proxy.MockManager {
	t.Helper()
	if exact {
		return stageAgainWith(t, nil, nil)
	}
	preps, owned := againRecording()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	start, end := diffWindow(0)
	mm.SetMocksWithWindow(nil, append(append([]*models.Mock{}, preps...), owned...), start, end.Add(time.Hour))
	return mm
}

// stageAgainWith serves againRecording as the proven switch does, test T's
// mapping its own recordings and ownedToo, with T's marks (none of
// ownedToo), and recordings no test owns, the worker scope's, in the view
// beside them.
func stageAgainWith(t *testing.T, ownedToo, worker []*models.Mock) *proxy.MockManager {
	t.Helper()
	preps, owned := againRecording()
	owned = append(owned, ownedToo...)
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	all := append(append(append([]*models.Mock{}, preps...), worker...), owned...)
	mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now().Add(time.Hour))
	if len(worker) > 0 {
		mm.ServeExactWorker(mockNamesOf(worker))
	}
	mm.ServeExactTest("T", owned)
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg-sweep": "a timer's call", "bg-exec": "a timer's call", "bg-noquery": "a timer's call"}})
	return mm
}

// againConn is a context for connection c1 whose reports are collected.
func againConn(reports *[]*models.MockMismatchReport) context.Context {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	ctx = models.WithMockMismatchReporter(ctx, func(err error) {
		var withReport interface {
			MismatchReport() *models.MockMismatchReport
		}
		if errors.As(err, &withReport) {
			*reports = append(*reports, withReport.MismatchReport())
		}
	})
	return ctx
}

// againRun runs commands on connection c1 of a store.
type againRun struct {
	t   *testing.T
	db  integrations.MockMemDb
	ctx context.Context
	rep *[]*models.MockMismatchReport
}

func newAgainRun(t *testing.T, db integrations.MockMemDb) *againRun {
	r := &againRun{t: t, db: db, rep: &[]*models.MockMismatchReport{}}
	r.ctx = againConn(r.rep)
	return r
}

// command runs req on c1, statement 1 being poolQuery and 2 otherQuery,
// and returns the answer's payload ("" for a miss), the reports it made and
// the miss.
func (r *againRun) command(req mysql.Request) (string, []*models.MockMismatchReport, string) {
	r.t.Helper()
	*r.rep = nil
	_, dctx := poolConn("c1")
	dctx.StmtIDToQuery[2] = otherQuery
	dctx.PreparedStatements[1] = &mysql.StmtPrepareOkPacket{StatementID: 1, NumParams: 1}
	dctx.PreparedStatements[2] = &mysql.StmtPrepareOkPacket{StatementID: 2, NumParams: 1}
	resp, ok, miss, err := replayer.MatchForTest(r.ctx, zap.NewNop(), req, r.db, dctx)
	if err != nil {
		r.t.Fatalf("%s: err=%v miss=%s", req.Header.Type, err, miss)
	}
	if !ok {
		return "", *r.rep, miss
	}
	return resp.Payload, *r.rep, ""
}

// answeredAgain is the one report of a command answered again naming want.
func answeredAgain(t *testing.T, reports []*models.MockMismatchReport, want, summary string) {
	t.Helper()
	if len(reports) != 1 {
		t.Fatalf("reports %+v; want one answered again naming %s", reports, want)
	}
	r := reports[0]
	if r.Answer != models.AnswerBackgroundAgain || r.ClosestMock != want || r.CountsAgainstTest() || r.Protocol != "MySQL" || !strings.Contains(r.ActualSummary, summary) {
		t.Fatalf("reported %+v; want answered again naming %s, summary with %q, counting against no test", r, want, summary)
	}
}

// A timer's COM_QUERY and its EXECUTE that judging marked as the
// application's background work answered once in their test's window, and
// the timer runs them again there. Each is answered again with its marked
// recording, unchecked, and reported as such naming it, instead of a miss
// that closes the connection and fails the test. A command only like a
// marked recording (another literal, another parameter, a statement of the
// same shape), the test's own command run once more than it recorded, and
// an EXECUTE whose recording kept no query still miss.
func TestABackgroundCommandRunAgainInItsWindowIsAnsweredAgain(t *testing.T) {
	run := newAgainRun(t, stageAgain(t, true))

	if got, _, _ := run.command(executeReq(2, "5")); got != "bg-noquery" {
		t.Fatalf("bg-noquery first: answered %q", got)
	}

	for _, c := range []struct {
		name    string
		req     mysql.Request
		mock    string
		summary string
	}{
		{"COM_QUERY", queryReq(sweepQuery), "bg-sweep", sweepQuery},
		{"COM_STMT_EXECUTE", executeReq(1, "7"), "bg-exec", poolQuery},
	} {
		if got, reports, _ := run.command(c.req); got != c.mock || len(reports) != 0 {
			t.Fatalf("%s first: answered %q, reports %+v; want %s, unreported", c.name, got, reports, c.mock)
		}
		got, reports, _ := run.command(c.req)
		if got != c.mock {
			t.Fatalf("%s again: answered %q; want %s again", c.name, got, c.mock)
		}
		answeredAgain(t, reports, c.mock, c.summary)
	}
	// Answered again before the matcher's answers by kind, which would have
	// spent the test's own recordings: those still answer their commands.
	for _, c := range []struct {
		req  mysql.Request
		mock string
	}{{executeReq(1, "9"), "own-exec-9"}, {queryReq(ownSweep), "own-sweep"}} {
		if got, reports, _ := run.command(c.req); got != c.mock || len(reports) != 0 {
			t.Fatalf("%s: answered %q, reports %+v; want it, unreported", c.mock, got, reports)
		}
	}

	for name, req := range map[string]mysql.Request{
		"another literal":   queryReq(strings.Replace(sweepQuery, "1761555000", "1761555009", 1)),
		"another parameter": executeReq(1, "8"),
		// bg-exec's parameter, in another statement.
		"another statement": executeReq(2, "7"),
	} {
		if got, reports, _ := run.command(req); got != "" || len(reports) != 0 {
			t.Fatalf("%s: answered %q, reports %+v; want a miss", name, got, reports)
		}
	}

	if got, _, _ := run.command(queryReq(ownQuery)); got != "own-q" {
		t.Fatalf("own-q first: answered %q", got)
	}
	if got, reports, _ := run.command(queryReq(ownQuery)); got != "" || len(reports) != 0 {
		t.Fatalf("the test's own command run again: answered %q, reports %+v; want a miss", got, reports)
	}

	// bg-noquery: which statement it ran is not on the recording (its
	// PREPARE says it; answering again reads the recording alone).
	if got, reports, _ := run.command(executeReq(2, "5")); got != "" || len(reports) != 0 {
		t.Fatalf("an EXECUTE whose recording kept no query, run again: answered %q, reports %+v; want a miss", got, reports)
	}
}

// Without exact serving nothing is answered again: keploy test answers a
// command as it always did.
func TestWithoutExactServingNoCommandIsAnsweredAgain(t *testing.T) {
	run := newAgainRun(t, stageAgain(t, false))
	for i := 0; i < 2; i++ {
		_, reports, _ := run.command(queryReq(sweepQuery))
		for _, r := range reports {
			if r.Answer == models.AnswerBackgroundAgain {
				t.Fatalf("run %d: reported %+v; want no answer again", i+1, r)
			}
		}
	}
}

// restagingStore is a store whose replay staged a test set again under
// every command (HoldToRound's restaged reports true), its held view left
// as it was.
type restagingStore struct{ *proxy.MockManager }

func (s restagingStore) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	db, _ := s.MockManager.HoldToRound(call)
	return db, func() bool { return true }
}

// A command run again while the replay staged a test set again under it is
// answered with no recording, not again with the marked one: it misses
// saying so, as any command the restage caught.
func TestACommandTheReplayRestagedUnderIsNotAnsweredAgain(t *testing.T) {
	mm := stageAgain(t, true)
	first := newAgainRun(t, mm)
	// own-sweep too, which the matcher would answer the sweep with by kind
	// from the view the stub's hold leaves in place.
	for _, q := range []string{sweepQuery, ownSweep} {
		if got, _, _ := first.command(queryReq(q)); got == "" {
			t.Fatalf("%s first: a miss", q)
		}
	}
	run := newAgainRun(t, restagingStore{mm})
	if got, reports, miss := run.command(queryReq(sweepQuery)); got != "" || len(reports) != 0 || !strings.Contains(miss, "restaged:true") {
		t.Fatalf("restaged: answered %q, reports %+v, miss %s; want the restage's miss", got, reports, miss)
	}
}

// A command a recording matches exactly is answered with that recording,
// never again with a marked one it repeats, as before the marks: the
// test's own second recording of the timer's COM_QUERY (own-sweep-copy,
// not marked) answers the sweep run a second time, and stays required; an
// EXECUTE that a recording no test owns (the worker scope's, worker-exec-7)
// matches with its statement and every parameter is answered with that one.
// Answered again with the marked recording instead, the first would leave
// the test's own recording unmade, and judging would read it as the
// application's background work.
func TestACommandARecordingMatchesExactlyIsNeverAnsweredAgain(t *testing.T) {
	ts := diffBase.Add(time.Millisecond)
	copySweep := diffMock("own-sweep-copy", "c1", []mysql.Request{queryReq(sweepQuery)}, ts.Add(7*time.Millisecond), models.LifetimePerTest, "mocks")
	workerExec := diffMock("worker-exec-7", "c1", []mysql.Request{executeReq(1, "7")}, ts.Add(8*time.Millisecond), models.LifetimePerTest, "mocks")
	workerExec.Spec.MySQLRequests[0].SetStoredQuery(poolQuery)
	run := newAgainRun(t, stageAgainWith(t, []*models.Mock{copySweep}, []*models.Mock{workerExec}))
	for _, c := range []struct {
		name          string
		req           mysql.Request
		first, second string
	}{
		{"COM_QUERY", queryReq(sweepQuery), "bg-sweep", "own-sweep-copy"},
		{"COM_STMT_EXECUTE", executeReq(1, "7"), "bg-exec", "worker-exec-7"},
	} {
		if got, reports, _ := run.command(c.req); got != c.first || len(reports) != 0 {
			t.Fatalf("%s first: answered %q, reports %+v; want %s, unreported", c.name, got, reports, c.first)
		}
		if got, reports, _ := run.command(c.req); got != c.second || len(reports) != 0 {
			t.Fatalf("%s again: answered %q, reports %+v; want %s, unreported, never %s again", c.name, got, reports, c.second, c.first)
		}
	}
}

// unheldViewStore is a store whose hold of a command is a view that tells
// it could not hold the command to its round (integrations.UnheldTeller)
// while it still reads the store, the open test's marked recordings that
// answered included: a view over the fail-closed store that forwards more
// than that store offers.
type unheldViewStore struct{ *proxy.MockManager }

func (s unheldViewStore) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return unheldView(s), func() bool { return false }
}

// unheldView is unheldViewStore's hold.
type unheldView struct{ *proxy.MockManager }

func (unheldView) Unheld() string { return integrations.UnheldNote }

// A command a store could not hold to its round is answered nothing again,
// though the view it read still lists the marked recording the command
// repeats: under exact serving a command is answered only from a store
// held to its round, so it misses saying why.
func TestACommandTheStoreCouldNotHoldIsNotAnsweredAgain(t *testing.T) {
	mm := stageAgain(t, true)
	first := newAgainRun(t, mm)
	// own-sweep too, which the matcher would answer the sweep with by kind
	// from the view the stub's hold reads.
	for _, q := range []string{sweepQuery, ownSweep} {
		if got, _, _ := first.command(queryReq(q)); got == "" {
			t.Fatalf("%s first: a miss", q)
		}
	}
	run := newAgainRun(t, unheldViewStore{mm})
	if got, reports, miss := run.command(queryReq(sweepQuery)); got != "" || len(reports) != 0 || !strings.Contains(miss, "restaged:true") {
		t.Fatalf("unheld: answered %q, reports %+v, miss %s; want the unheld store's miss", got, reports, miss)
	}
}
