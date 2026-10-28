package replayer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// recordedWindowsDb is a fakeMockDb that also knows the recorded window of
// every test of the set (integrations.TestWindows), as a MockManager does
// once keploy test seeds them.
type recordedWindowsDb struct {
	*fakeMockDb
	windows [][2]time.Time
}

func (d recordedWindowsDb) InTestWindow(t time.Time) bool {
	return !t.Before(d.winStart) && !t.After(d.winEnd)
}

func (d recordedWindowsDb) index(t time.Time) int {
	for i, w := range d.windows {
		if !t.Before(w[0]) && !t.After(w[1]) {
			return i
		}
	}
	return -1
}

func (d recordedWindowsDb) InRecordedTestWindow(t time.Time) (inside, known bool) {
	if len(d.windows) == 0 || t.IsZero() {
		return false, false
	}
	return d.index(t) >= 0, true
}

func (d recordedWindowsDb) SameRecordedTestWindow(a, b time.Time) (same, known bool) {
	if len(d.windows) == 0 || a.IsZero() || b.IsZero() {
		return false, false
	}
	i := d.index(a)
	return i >= 0 && i == d.index(b), true
}

// withTestWindows is orderTests' store with the recorded windows of its two
// tests sent, as keploy test sends them: test A's recordings are then another
// test's (the defExecMockTest tier), not ones made outside every test.
func withTestWindows(fake *fakeMockDb) recordedWindowsDb {
	testA, testB := stmtT0.Add(10*time.Second), stmtT0.Add(20*time.Second)
	return recordedWindowsDb{fakeMockDb: fake, windows: [][2]time.Time{
		{testA.Add(-time.Second), testA.Add(time.Second)},
		{testB.Add(-time.Second), testB.Add(time.Second)},
	}}
}

// matchOrderOn is matchLiveOrder on any store.
func matchOrderOn(t *testing.T, db integrations.MockMemDb, params ...string) (*mysql.Response, bool, *mockMiss) {
	t.Helper()
	req := liveExecute(params...)
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, "", nil, nil)
	if err != nil {
		t.Fatalf("matchCommand: %v", err)
	}
	return resp, ok, miss
}

// With an EXECUTE in the window that lost its statement, an exact match on
// the query and every parameter recorded in another test's window (the
// replay sent the recorded windows) is still served, as one recorded outside
// every test is: it is the answer the matcher gives a reusable read.
func TestMatchCommand_AnExactMatchInAnotherTestsRecordedWindowIsServedBesideALostStatement(t *testing.T) {
	fake, execA, _ := orderTests("")
	resp, ok, miss := matchOrderOn(t, withTestWindows(fake), "k1", "2026-10-14 12:00:10")
	if !ok || resp.Payload != "A: 201" {
		t.Fatalf("served %v (ok %v, miss %+v), want %s, the exact match on the query and every parameter", resp, ok, miss, execA.Name)
	}
}

// A call in test B's window whose own EXECUTE lost its statement, with an
// exact match on the query and every parameter recorded by test A, beside a
// recording of the query with other parameters (the FIFO candidate). The
// exact match ranks above the FIFO candidate, and the lost-statement check
// serves an exact match, so the call is answered with test A's recording of
// its request, as it is with no FIFO candidate
// (TestMatchCommand_AnExactMatchIsServedBesideALostStatement). Ranked the
// other way, the FIFO candidate was chosen, the lost-statement check refused
// it, and the call was a miss naming B's EXECUTE.
//
// The matcher takes test A's recording on one of two branches: with no
// recorded windows sent it is a recording made outside every test
// (defExecMock); with them sent, as keploy test sends them, it is another
// test's (defExecMockTest). Each has its own rank and its own exemption in
// the lost-statement check, so the row holds the answer on both.
func TestMatchCommand_AnExactMatchBesideALostStatementOutranksAFIFOCandidate(t *testing.T) {
	for _, windows := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows=%v", windows), func(t *testing.T) {
			fake, execA, _ := orderTests("")
			fifo := executedMock("mock-execute-c", 1, stmtT0.Add(10*time.Second+2*time.Millisecond), "C: 201", "", "k2", "2026-10-14 12:00:11")
			fake.session = append(fake.session, fifo)
			var db integrations.MockMemDb = fake
			if windows {
				db = withTestWindows(fake)
			}
			resp, ok, miss := matchOrderOn(t, db, "k1", "2026-10-14 12:00:10")
			if !ok || resp.Payload != "A: 201" {
				t.Fatalf("served %v (ok %v, miss %+v), want %s, the exact match on the query and every parameter, above %s, of other parameters", resp, ok, miss, execA.Name, fifo.Name)
			}
		})
	}
}
