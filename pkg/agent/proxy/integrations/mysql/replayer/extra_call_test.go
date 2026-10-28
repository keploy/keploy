package replayer_test

import (
	"context"
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

// auditInsert is a statement that writes, prepared as statement 3.
const auditInsert = "INSERT INTO audit (ref) VALUES (?)"

// extraRecording is test T's recording on connection c1: a timer's sweep
// run twice (sweep-1, sweep-2), the test's own read (own-q), an EXECUTE of
// auditInsert with 4 (ins-4, its query kept on it as the recorder keeps it)
// and an EXECUTE of otherQuery with 5 whose recording kept no query, as an
// older keploy's did (noquery-5). Nothing is marked.
func extraRecording() (preps, owned []*models.Mock) {
	ts := diffBase.Add(time.Millisecond)
	preps = []*models.Mock{diffPrepareMock("prep-other", "c1", otherQuery, 2, ts), diffPrepareMock("prep-audit", "c1", auditInsert, 3, ts)}
	at := func(i int) time.Time { return ts.Add(time.Duration(i) * time.Millisecond) }
	ins := diffMock("ins-4", "c1", []mysql.Request{executeReq(3, "4")}, at(4), models.LifetimePerTest, "mocks")
	ins.Spec.MySQLRequests[0].SetStoredQuery(auditInsert)
	owned = []*models.Mock{
		diffMock("sweep-1", "c1", []mysql.Request{queryReq(sweepQuery)}, at(1), models.LifetimePerTest, "mocks"),
		diffMock("own-q", "c1", []mysql.Request{queryReq(ownQuery)}, at(2), models.LifetimePerTest, "mocks"),
		diffMock("sweep-2", "c1", []mysql.Request{queryReq(sweepQuery)}, at(3), models.LifetimePerTest, "mocks"),
		ins,
		diffMock("noquery-5", "c1", []mysql.Request{executeReq(2, "5")}, at(5), models.LifetimePerTest, "mocks"),
	}
	return preps, owned
}

// stageExtra serves extraRecording as the proven switch does (exact: test
// T's mapping is its own recordings) or as keploy test does.
func stageExtra(t *testing.T, exact bool) *proxy.MockManager {
	t.Helper()
	preps, owned := extraRecording()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	all := append(append([]*models.Mock{}, preps...), owned...)
	if !exact {
		start, end := diffWindow(0)
		mm.SetMocksWithWindow(nil, all, start, end.Add(time.Hour))
		return mm
	}
	mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactTest("T", owned)
	return mm
}

// extraCommand runs req on connection c1 of db, statement 2 being
// otherQuery and 3 auditInsert, and returns the answer's payload ("" for a
// miss) and the miss's report (nil when answered).
func extraCommand(t *testing.T, db integrations.MockMemDb, req mysql.Request) (string, *models.MockMismatchReport) {
	t.Helper()
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	_, dctx := poolConn("c1")
	dctx.StmtIDToQuery[2], dctx.StmtIDToQuery[3] = otherQuery, auditInsert
	dctx.PreparedStatements[2] = &mysql.StmtPrepareOkPacket{StatementID: 2, NumParams: 1}
	dctx.PreparedStatements[3] = &mysql.StmtPrepareOkPacket{StatementID: 3, NumParams: 1}
	resp, report, err := replayer.MissReportForTest(ctx, zap.NewNop(), req, db, dctx)
	if err != nil {
		t.Fatalf("%s: err=%v", req.Header.Type, err)
	}
	if report != nil {
		return "", report
	}
	return resp.Payload, nil
}

// answerAll answers each of reqs on db with the recording named in want.
func answerAll(t *testing.T, db integrations.MockMemDb, reqs []mysql.Request, want []string) {
	t.Helper()
	for i, req := range reqs {
		if got, report := extraCommand(t, db, req); got != want[i] {
			t.Fatalf("command %d: answered %q (miss %+v); want %s", i+1, got, report, want[i])
		}
	}
}

// A command a test runs once more than it recorded, after each of its
// recordings answered, misses as one call more than the test recorded: the
// miss names the recording it repeats (the one recorded last of its
// copies), how often the test recorded it, and what that recording's
// statement writes ("" for a read), and names no other recording, field or
// noise to declare. A COM_QUERY and a COM_STMT_EXECUTE (its statement read
// from the query its recording kept) both do.
func TestACommandRunOnceMoreThanRecordedNamesTheRecordingItRepeats(t *testing.T) {
	db := stageExtra(t, true)
	// Every recording answers first: one left (noquery-5, whose statement
	// is not on it) would answer an EXECUTE by kind.
	answerAll(t, db,
		[]mysql.Request{queryReq(sweepQuery), queryReq(ownQuery), queryReq(sweepQuery), executeReq(3, "4"), executeReq(2, "5")},
		[]string{"sweep-1", "own-q", "sweep-2", "ins-4", "noquery-5"})

	for _, c := range []struct {
		name     string
		req      mysql.Request
		repeats  string
		recorded int
		write    string
	}{
		{"the timer's sweep a third time", queryReq(sweepQuery), "sweep-2", 2, "MySQL UPDATE"},
		{"the test's own read again", queryReq(ownQuery), "own-q", 1, ""},
		{"the insert's EXECUTE again", executeReq(3, "4"), "ins-4", 1, "MySQL INSERT"},
	} {
		got, r := extraCommand(t, db, c.req)
		if got != "" || r == nil {
			t.Fatalf("%s: answered %q; want a miss", c.name, got)
		}
		if r.MissKind != models.MissExtraCall || r.ClosestMock != c.repeats || r.Recorded != c.recorded || r.Write != c.write {
			t.Fatalf("%s: miss kind %q, closest %q, recorded %d, write %q; want %q, %q, %d, %q",
				c.name, r.MissKind, r.ClosestMock, r.Recorded, r.Write, models.MissExtraCall, c.repeats, c.recorded, c.write)
		}
		if len(r.FieldDiffs) != 0 || r.ClosestMockReq != "" || !strings.Contains(r.Diff, "repeats "+c.repeats) || r.Protocol != "MySQL" {
			t.Fatalf("%s: report %+v; want only the recording it repeats named, and no field", c.name, r)
		}
	}
}

// A command that is only like a recording the test answered (another
// literal), an EXECUTE whose recording kept no query, and any command
// without exact serving, miss as before: no miss kind, no recording
// repeated.
func TestACommandNotRunOnceMoreStaysAMissInTheTest(t *testing.T) {
	db := stageExtra(t, true)
	answerAll(t, db,
		[]mysql.Request{queryReq(sweepQuery), queryReq(sweepQuery), executeReq(3, "4"), executeReq(2, "5")},
		[]string{"sweep-1", "sweep-2", "ins-4", "noquery-5"})
	for name, req := range map[string]mysql.Request{
		"another literal": queryReq(strings.Replace(sweepQuery, "1761555000", "1761555009", 1)),
		// Which statement noquery-5 ran is not on its recording.
		"an EXECUTE whose recording kept no query": executeReq(2, "5"),
	} {
		got, r := extraCommand(t, db, req)
		if got != "" || r == nil || r.MissKind != "" || r.Recorded != 0 || r.Write != "" {
			t.Fatalf("%s: answered %q, report %+v; want a miss with no kind", name, got, r)
		}
	}

	off := stageExtra(t, false)
	for i := 0; i < 3; i++ {
		if _, r := extraCommand(t, off, queryReq(sweepQuery)); r != nil && (r.MissKind != "" || r.Recorded != 0 || r.Write != "") {
			t.Fatalf("without exact serving, sweep %d: report %+v; want no miss kind", i+1, r)
		}
	}
}

// A command the replay staged a test set again under misses saying so, never
// as one call more than the test recorded: its matcher, held to its first
// read, read nothing of the set.
func TestACommandTheReplayRestagedUnderIsNoExtraCall(t *testing.T) {
	mm := stageExtra(t, true)
	answerAll(t, mm, []mysql.Request{queryReq(sweepQuery), queryReq(sweepQuery)}, []string{"sweep-1", "sweep-2"})
	got, r := extraCommand(t, restagingStore{mm}, queryReq(sweepQuery))
	if got != "" || r == nil || r.MissKind != "" || r.Recorded != 0 || !strings.Contains(r.NextSteps, "staged a test set again") {
		t.Fatalf("restaged: answered %q, report %+v; want the restage's miss, no extra call", got, r)
	}
}
