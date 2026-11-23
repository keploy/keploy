package replayer_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

type ownerTestCase struct {
	name       string
	start, end time.Duration
	zeroEnd    bool // true: recorded with no response timestamp (End is zero in schedule; replayed with +1s grace)
	calls      []connCall
}

// replayPoolWithWindows replays tests in recorded Start order against pool,
// seeding MockManager with the exact recorded test windows (as keploy test does).
func replayPoolWithWindows(t *testing.T, pool []*models.Mock, tests []ownerTestCase, fullScan bool) [][]string {
	t.Helper()
	side := newDiffSide(t, pool)
	t.Cleanup(side.mm.Close)

	ws := make([]models.TestWindow, 0, len(tests))
	for _, tc := range tests {
		end := diffBase.Add(tc.end)
		if tc.zeroEnd {
			end = time.Time{}
		}
		ws = append(ws, models.TestWindow{
			TestCase: tc.name,
			Start:    diffBase.Add(tc.start),
			End:      end,
		})
	}
	side.mm.SeedRecordedWindows(ws)

	ctxs, dctxs := map[string]context.Context{}, map[string]*wire.DecodeContext{}
	got := make([][]string, 0, len(tests))
	for i, tc := range tests {
		winStart := diffBase.Add(tc.start)
		winEnd := diffBase.Add(tc.end)
		if tc.zeroEnd {
			winEnd = winStart.Add(time.Second)
		}
		side.stage(winStart, winEnd)
		answers := make([]string, 0, len(tc.calls))
		for _, c := range tc.calls {
			if dctxs[c.live] == nil {
				ctxs[c.live], dctxs[c.live] = connLive(c.live)
			}
			var resp *mysql.Response
			var ok bool
			var err error
			if fullScan {
				resp, ok, _, err = replayer.MatchBoundForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), side.mm, dctxs[c.live], true)
			} else {
				resp, ok, _, err = replayer.MatchWithEngineForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), side.mm, dctxs[c.live], nil)
			}
			if err != nil {
				t.Fatalf("test %d (%s), %q on %s (fullScan=%v): %v", i, tc.name, c.sql, c.live, fullScan, err)
			}
			if !ok || resp == nil {
				answers = append(answers, "miss")
				continue
			}
			answers = append(answers, resp.Payload)
		}
		got = append(got, answers)
	}
	return got
}

func checkWithWindows(t *testing.T, recs []connRec, tests []ownerTestCase) {
	t.Helper()
	pool, _ := connPool(recs)
	checkPoolWithWindows(t, pool, tests)
}

// checkPoolWithWindows is checkWithWindows over pool (connPool).
func checkPoolWithWindows(t *testing.T, pool []*models.Mock, tests []ownerTestCase) {
	t.Helper()
	for _, fullScan := range []bool{false, true} {
		t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
			got := replayPoolWithWindows(t, pool, tests, fullScan)
			for i, tc := range tests {
				for j, c := range tc.calls {
					if c.want == "*" {
						continue
					}
					if got[i][j] != c.want {
						t.Errorf("test %d (%s) call %d (%q): got %q, want %q", i, tc.name, j, c.sql, got[i][j], c.want)
					}
				}
			}
		})
	}
}

// Nested outer [0,30ms] and inner [10ms,20ms] tests with DIFFERENT answers to
// the same unmapped query, replayed in their recorded Start order (outer first,
// inner second):
//   - outer's call at 12ms (inside inner's span) and inner's call at 15ms:
//     outer gets @12ms ("outer-5"), inner gets @15ms ("inner-6"), both green.
//   - inner's call at 15ms and outer's call at 25ms (outside inner's span):
//     outer skips @15ms (owned by inner) and takes @25ms ("outer-5"), inner
//     takes @15ms ("inner-6"), both green (for COM_QUERY, COM_STMT_EXECUTE
//     definitive, and COM_STMT_EXECUTE FIFO).
//   - outer's call at 5ms (before inner) and inner's call at 15ms:
//     outer gets @5ms ("outer-5"), inner gets @15ms ("inner-6"), both green.
func TestWindowOwnershipNestedOuterInnerDifferentAnswers(t *testing.T) {
	const sql = "SELECT COUNT(*) FROM orders"

	t.Run("outer call at 12ms inner call at 15ms", func(t *testing.T) {
		recs := []connRec{
			{"outer-5/on", "c1", sql, 12 * time.Millisecond, 0},
			{"inner-6/on", "c2", sql, 15 * time.Millisecond, 1},
		}
		tests := []ownerTestCase{
			{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", sql, "outer-5/on"}}},
			{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "inner-6/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})

	t.Run("inner call at 15ms outer call at 25ms COM_QUERY", func(t *testing.T) {
		recs := []connRec{
			{"inner-6/on", "c2", sql, 15 * time.Millisecond, 1},
			{"outer-5/on", "c1", sql, 25 * time.Millisecond, 0},
		}
		tests := []ownerTestCase{
			{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", sql, "outer-5/on"}}},
			{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "inner-6/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})

	t.Run("inner call at 15ms outer call at 25ms COM_STMT_EXECUTE", func(t *testing.T) {
		recs := []connRec{
			{"inner-exec/on", "c2", "exec:p1", 15 * time.Millisecond, 1},
			{"outer-exec/on", "c1", "exec:p1", 25 * time.Millisecond, 0},
		}
		tests := []ownerTestCase{
			{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", "exec:p1", "outer-exec/on"}}},
			{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", "exec:p1", "inner-exec/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})

	t.Run("inner call at 15ms outer call at 25ms COM_STMT_EXECUTE FIFO", func(t *testing.T) {
		recs := []connRec{
			{"inner-fifo/on", "c2", "exec:rec-in", 15 * time.Millisecond, 1},
			{"outer-fifo/on", "c1", "exec:rec-out", 25 * time.Millisecond, 0},
		}
		tests := []ownerTestCase{
			{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", "exec:live-out", "outer-fifo/on"}}},
			{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", "exec:live-in", "inner-fifo/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})

	t.Run("outer call at 5ms inner call at 15ms", func(t *testing.T) {
		recs := []connRec{
			{"outer-5/on", "c1", sql, 5 * time.Millisecond, 0},
			{"inner-6/on", "c2", sql, 15 * time.Millisecond, 1},
		}
		tests := []ownerTestCase{
			{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", sql, "outer-5/on"}}},
			{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "inner-6/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})

	t.Run("partly overlapping non-nested prefers closer start", func(t *testing.T) {
		// A [0,20ms] and B [10,50ms] partly overlap. A's own query at 5ms is
		// owned by A (rank 0), B's query at 15ms is owned by B (closest start:
		// 15-10 < 15-0, rank 2 for A and rank 0 for B). A takes 5ms, B takes 15ms.
		recs := []connRec{
			{"a-5/on", "c1", sql, 5 * time.Millisecond, 0},
			{"b-15/on", "c2", sql, 15 * time.Millisecond, 1},
		}
		tests := []ownerTestCase{
			{name: "a", start: 0, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "a-5/on"}}},
			{name: "b", start: 10 * time.Millisecond, end: 50 * time.Millisecond, calls: []connCall{{"L1", sql, "b-15/on"}}},
		}
		checkWithWindows(t, recs, tests)
	})
}

// Concurrent burst combining 3-level nesting, a right-overlapping peer, and
// distinct responses per test: verifies in recorded Start order that EACH
// specific test receives its OWN exact recorded answer (not just slot counts).
func TestWindowOwnershipBurstChecksPerTestAnswers(t *testing.T) {
	const sql = "SELECT stock FROM products WHERE id = 'p-hot'"
	recs := []connRec{
		// Innermost [20,30ms] queries at 25ms.
		{"ans-innermost/on", "c3", sql, 25 * time.Millisecond, 2},
		// Middle [10,50ms] queries at 40ms (outside innermost, inside middle).
		{"ans-mid/on", "c2", sql, 40 * time.Millisecond, 1},
		// Sibling inner [55,70ms] queries at 60ms.
		{"ans-sib/on", "c4", sql, 60 * time.Millisecond, 3},
		// Outer [0,100ms] queries at 72ms (outside nested windows and before right peer [75,130ms]).
		{"ans-outer/on", "c1", sql, 72 * time.Millisecond, 0},
		// Right peer [75,130ms] queries at 115ms.
		{"ans-peer/on", "c5", sql, 115 * time.Millisecond, 4},
		// Trailing non-overlapping [150,170ms] queries at 160ms.
		{"ans-tail/on", "c6", sql, 160 * time.Millisecond, 5},
	}
	tests := []ownerTestCase{
		{name: "outer", start: 0, end: 100 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-outer/on"}}},
		{name: "mid", start: 10 * time.Millisecond, end: 50 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-mid/on"}}},
		{name: "innermost", start: 20 * time.Millisecond, end: 30 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-innermost/on"}}},
		{name: "sib", start: 55 * time.Millisecond, end: 70 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-sib/on"}}},
		{name: "peer", start: 75 * time.Millisecond, end: 130 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-peer/on"}}},
		{name: "tail", start: 150 * time.Millisecond, end: 170 * time.Millisecond, calls: []connCall{{"L1", sql, "ans-tail/on"}}},
	}
	checkWithWindows(t, recs, tests)
}

// A mock stamped just outside every recorded window (e.g. 30us past its test's
// End) belongs to the window whose nearer edge is closest within
// models.MockWindowEdgeSkew, beats a stale earlier-test mock, and never steals
// a mock that is closer to an adjacent test or outside the skew bound.
func TestWindowOwnershipBoundedEdgeSkew(t *testing.T) {
	const sql = "SELECT id FROM orders WHERE id = 'o-edge'"
	recs := []connRec{
		// Earlier unrelated test [0,5ms].
		{"stale-t0/on", "c0", sql, 2 * time.Millisecond, 0},
		// Test 1 [10ms,20ms]: its mock was stamped 30us past End (outside every window).
		{"edge-t1/on", "c1", sql, 20*time.Millisecond + 30*time.Microsecond, 1},
		// Mock 80us past t1's End (20us before t2's Start at 20.1ms): nearer to t2!
		{"edge-t2/on", "c2", sql, 20*time.Millisecond + 80*time.Microsecond, 2},
	}
	tests := []ownerTestCase{
		{name: "t0", start: 0, end: 5 * time.Millisecond, calls: []connCall{{"L1", sql, "stale-t0/on"}}},
		{name: "t1", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "edge-t1/on"}}},
		{name: "t2", start: 20*time.Millisecond + 100*time.Microsecond, end: 30 * time.Millisecond, calls: []connCall{{"L1", sql, "edge-t2/on"}}},
	}
	// The recordings state their lifetime, as the MySQL recorder's do
	// (recordedLifetimes): the edge rule takes only a plain per-test mock.
	// edge-t1 is answered before t2 begins. Answered after (connPool's
	// recordings are answered 500 us after they are sent) it was in flight into
	// t2, which the in-flight rule gives t2, as on main, and the edge rule
	// leaves it (TestWindowOwnershipLeavesACallInFlightToItsTest).
	pool, _ := connPool(recs)
	recordedLifetimes(pool)
	for _, mk := range pool {
		if mk.Name == "edge-t1/on" {
			mk.Spec.ResTimestampMock = diffBase.Add(20*time.Millisecond + 50*time.Microsecond)
		}
	}
	checkPoolWithWindows(t, pool, tests)

	// A mock the edge rule gives t1 ranks behind t1's own recording, placed
	// in t1's window: the contained recording answers t1's call.
	t.Run("contained mock beats edge-skewed mock", func(t *testing.T) {
		recs := []connRec{
			{"edge-before/on", "c1", sql, 10*time.Millisecond - 30*time.Microsecond, 0},
			{"inside/on", "c2", sql, 15 * time.Millisecond, 0},
		}
		// edge-before lies in the gap after t0 (so it is no startup
		// traffic), 30 us before t1, answered before t1 began (so it is not
		// in flight into t1), and states its lifetime as the recorder does:
		// the edge rule gives it to t1, behind t1's own recording, which is
		// later in recorded order and must still answer first.
		tests := []ownerTestCase{
			{name: "t0", start: 0, end: 5 * time.Millisecond},
			{name: "t1", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", sql, "inside/on"}}},
		}
		pool, _ := connPool(recs)
		recordedLifetimes(pool)
		for _, mk := range pool {
			if mk.Name == "edge-before/on" {
				mk.Spec.ResTimestampMock = diffBase.Add(10*time.Millisecond - 20*time.Microsecond)
			}
		}
		checkPoolWithWindows(t, pool, tests)
		// Staged strictly, both recordings are in t1's per-test tier, the
		// edge one first.
		for _, fullScan := range []bool{false, true} {
			answers, _, _, _ := replayReclaim(t, pool, sql, []reclaimTest{{"t0", 0, 5 * time.Millisecond, 0}, {"t1", 10 * time.Millisecond, 20 * time.Millisecond, 1}}, fullScan)
			if !slices.Equal(answers["t1"], []string{"inside/on"}) {
				t.Errorf("strict staging, fullScan=%v: t1's call answered %v; want [inside/on]", fullScan, answers["t1"])
			}
		}
	})
}

// A test recorded with no response timestamp (zero End) keeps its recorded
// Start place without panicking, runs before later tests, and does not steal a
// later test's recording when its replay window falls back to Start+1s.
func TestWindowOwnershipZeroResponseTimestampKeepsPlace(t *testing.T) {
	const createSQL = "INSERT INTO orders (id) VALUES ('ord-42')"
	const readSQL = "SELECT id FROM orders WHERE id = 'ord-42'"
	recs := []connRec{
		// X starts at 0 with no response timestamp; its INSERT is at 0ms.
		{"x-create/on", "c1", createSQL, 0, 0},
		// Y runs [500ms, 510ms] and reads the order X created; Y also shares
		// a common SELECT with X.
		{"y-read/on", "c2", readSQL, 505 * time.Millisecond, 1},
		{"x-common/on", "c1", "SELECT COUNT(*) FROM orders", 0, 0},
		{"y-common/on", "c2", "SELECT COUNT(*) FROM orders", 506 * time.Millisecond, 1},
	}
	tests := []ownerTestCase{
		{
			name:    "x-no-resp",
			start:   0,
			zeroEnd: true,
			calls: []connCall{
				{"L1", createSQL, "x-create/on"},
				{"L1", "SELECT COUNT(*) FROM orders", "x-common/on"},
			},
		},
		{
			name:  "y-after",
			start: 500 * time.Millisecond,
			end:   510 * time.Millisecond,
			calls: []connCall{
				{"L1", readSQL, "y-read/on"},
				{"L1", "SELECT COUNT(*) FROM orders", "y-common/on"},
			},
		},
	}
	checkWithWindows(t, recs, tests)
}

// Drifted COM_QUERY score fallbacks inside an outer window prefer a recording
// owned by the outer window (rank 0) over an earlier higher-scoring recording
// owned by a strictly nested inner window (rank 3).
func TestWindowOwnershipRanksDriftedQueryByOwnerRank(t *testing.T) {
	const (
		insInner = "INSERT INTO orders (id, total_cents) VALUES ('ord-inn1', 100)"
		insOuter = "INSERT INTO orders (id, total_cents) VALUES ('ord-outer-longer', 12345)"
		// insLive has the exact same length as insInner (+1 PayloadLength score).
		insLive = "INSERT INTO orders (id, total_cents) VALUES ('ord-live', 999)"
	)
	recs := []connRec{
		{"inner-ins/on", "c2", insInner, 15 * time.Millisecond, 1},
		{"outer-ins/on", "c1", insOuter, 25 * time.Millisecond, 0},
	}
	tests := []ownerTestCase{
		{name: "outer", start: 0, end: 30 * time.Millisecond, calls: []connCall{{"L1", insLive, "outer-ins/on"}}},
		{name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L1", insLive, "inner-ins/on"}}},
	}
	checkWithWindows(t, recs, tests)
}

// Online augmenting-path reclaim when unrelated concurrent windows blanket an
// outer window so point ownership cannot distinguish the outer and inner
// identical mocks:
//   - Outer [0,100ms] runs first, Inner [10,20ms] runs second, Unrelated [80,90ms]
//     blankets outer's own mock at 85ms. Both outer and inner succeed with exact
//     1:1 consumption, and an extra call in inner still misses (no false green).
//   - A 2-hop augmenting path A [0,100ms] -> B [10,60ms] -> C [45,55ms] passes
//     all three tests in recorded Start order.
//   - Identical SQL with DIFFERENT recorded answers is NEVER reclaimed.
func TestWindowReclaimNestedIdenticalAndMultiHopAugmentingPath(t *testing.T) {
	const sql = "SELECT sku FROM order_items WHERE order_id = 'ord-shared'"

	t.Run("nested identical blanketed by unrelated window reclaims 1-to-1 and misses extra call", func(t *testing.T) {
		for _, fullScan := range []bool{false, true} {
			t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
				recs := []connRec{
					{"m-in/on", "c1", sql, 15 * time.Millisecond, 0},
					{"m-out/on", "c2", sql, 85 * time.Millisecond, 1},
				}
				pool, _ := connPool(recs)
				// Give both data mocks identical Payload so they are truly interchangeable.
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest && len(mk.Spec.MySQLResponses) > 0 {
						mk.Spec.MySQLResponses[0].Payload = "same-items"
					}
				}

				side := newDiffSide(t, pool)
				t.Cleanup(side.mm.Close)
				side.mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "outer", Start: diffBase, End: diffBase.Add(100 * time.Millisecond)},
					{TestCase: "inner", Start: diffBase.Add(10 * time.Millisecond), End: diffBase.Add(20 * time.Millisecond)},
					{TestCase: "unrelated", Start: diffBase.Add(80 * time.Millisecond), End: diffBase.Add(90 * time.Millisecond)},
				})

				ctx, dctx := connLive("L1")
				call := func() string {
					var resp *mysql.Response
					var ok bool
					var err error
					if fullScan {
						resp, ok, _, err = replayer.MatchBoundForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, true)
					} else {
						resp, ok, _, err = replayer.MatchWithEngineForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, nil)
					}
					if err != nil {
						t.Fatalf("match error: %v", err)
					}
					if !ok || resp == nil {
						return "miss"
					}
					return resp.Payload
				}

				var perTest []*models.Mock
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest {
						perTest = append(perTest, mk.ShallowCopy())
					}
				}
				side.mm.SetMocksWithWindow(perTest, nil, diffBase, diffBase.Add(100*time.Millisecond))
				if got := call(); got != "same-items" {
					t.Fatalf("outer got %q, want same-items", got)
				}
				_ = side.mm.GetConsumedMocks()

				var rem []*models.Mock
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest && mk.Name != "m-in/on" {
						rem = append(rem, mk.ShallowCopy())
					}
				}
				side.mm.SetMocksWithWindow(rem, nil, diffBase.Add(10*time.Millisecond), diffBase.Add(20*time.Millisecond))
				if got := call(); got != "same-items" {
					t.Fatalf("inner first call got %q, want same-items", got)
				}
				// Consumed drain for inner must include its in-window mock (m-in) so mappings.yaml attributes it.
				drained := side.mm.GetConsumedMocks()
				sawInWindow := false
				for _, st := range drained {
					if st.Name == "m-in/on" {
						sawInWindow = true
					}
				}
				if !sawInWindow {
					t.Fatalf("inner consumed drain %v did not include in-window reclaimed mock m-in/on", drained)
				}
				// A second call in inner must miss: both mocks are now consumed.
				if got := call(); got != "miss" {
					t.Fatalf("inner extra call got %q, want miss (no double serving)", got)
				}
			})
		}
	})

	t.Run("2-hop augmenting path A->B->C succeeds in Start order", func(t *testing.T) {
		for _, fullScan := range []bool{false, true} {
			t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
				recs := []connRec{
					{"m1/on", "c1", sql, 15 * time.Millisecond, 0},
					{"m3/on", "c2", sql, 50 * time.Millisecond, 1},
					{"m2/on", "c3", sql, 85 * time.Millisecond, 2},
				}
				pool, _ := connPool(recs)
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest && len(mk.Spec.MySQLResponses) > 0 {
						mk.Spec.MySQLResponses[0].Payload = "same-items"
					}
				}
				side := newDiffSide(t, pool)
				t.Cleanup(side.mm.Close)
				side.mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "A", Start: diffBase, End: diffBase.Add(100 * time.Millisecond)},
					{TestCase: "B", Start: diffBase.Add(10 * time.Millisecond), End: diffBase.Add(60 * time.Millisecond)},
					{TestCase: "u1", Start: diffBase.Add(12 * time.Millisecond), End: diffBase.Add(18 * time.Millisecond)},
					{TestCase: "C", Start: diffBase.Add(45 * time.Millisecond), End: diffBase.Add(55 * time.Millisecond)},
					{TestCase: "u2", Start: diffBase.Add(80 * time.Millisecond), End: diffBase.Add(90 * time.Millisecond)},
				})

				ctx, dctx := connLive("L1")
				// Stage strictly per-test (no out-of-window session fallback) so C
				// can only succeed via the 2-hop augmenting path C->m3->m1->m2.
				consumed := map[string]bool{}
				stageStrict := func(s, e time.Duration) {
					for _, st := range side.mm.GetConsumedMocks() {
						if st.Usage == models.Deleted {
							consumed[st.Name] = true
						}
					}
					var perTest []*models.Mock
					for _, mk := range pool {
						if mk.TestModeInfo.Lifetime == models.LifetimePerTest && !consumed[mk.Name] {
							perTest = append(perTest, mk.ShallowCopy())
						}
					}
					side.mm.SetMocksWithWindow(perTest, nil, diffBase.Add(s), diffBase.Add(e))
				}
				runOne := func(name string, s, e time.Duration) {
					stageStrict(s, e)
					var resp *mysql.Response
					var ok bool
					var err error
					if fullScan {
						resp, ok, _, err = replayer.MatchBoundForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, true)
					} else {
						resp, ok, _, err = replayer.MatchWithEngineForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, nil)
					}
					if err != nil || !ok || resp == nil || resp.Payload != "same-items" {
						t.Fatalf("%s: got (ok=%v, resp=%v, err=%v), want same-items", name, ok, resp, err)
					}
				}
				runOne("A", 0, 100*time.Millisecond)
				runOne("B", 10*time.Millisecond, 60*time.Millisecond)
				runOne("C", 45*time.Millisecond, 55*time.Millisecond)
			})
		}
	})

	t.Run("different answers are never reclaimed", func(t *testing.T) {
		for _, fullScan := range []bool{false, true} {
			t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
				recs := []connRec{
					{"ans-a/on", "c1", sql, 15 * time.Millisecond, 0},
					{"ans-b/on", "c2", sql, 85 * time.Millisecond, 1},
				}
				pool, _ := connPool(recs)
				side := newDiffSide(t, pool)
				t.Cleanup(side.mm.Close)
				side.mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "outer", Start: diffBase, End: diffBase.Add(100 * time.Millisecond)},
					{TestCase: "inner", Start: diffBase.Add(10 * time.Millisecond), End: diffBase.Add(20 * time.Millisecond)},
					{TestCase: "u2", Start: diffBase.Add(80 * time.Millisecond), End: diffBase.Add(90 * time.Millisecond)},
				})
				ctx, dctx := connLive("L1")
				call := func() string {
					var resp *mysql.Response
					var ok bool
					var err error
					if fullScan {
						resp, ok, _, err = replayer.MatchBoundForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, true)
					} else {
						resp, ok, _, err = replayer.MatchWithEngineForTest(ctx, zap.NewNop(), connReq(sql), side.mm, dctx, nil)
					}
					if err != nil {
						t.Fatalf("match error: %v", err)
					}
					if !ok || resp == nil {
						return "miss"
					}
					return resp.Payload
				}
				// Stage strictly per-test (no out-of-window session fallback):
				// outer takes ans-a/on; inner must NOT reclaim ans-a/on using ans-b/on
				// because their recorded responses differ.
				var perTest []*models.Mock
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest {
						perTest = append(perTest, mk.ShallowCopy())
					}
				}
				side.mm.SetMocksWithWindow(perTest, nil, diffBase, diffBase.Add(100*time.Millisecond))
				if got := call(); got != "ans-a/on" {
					t.Fatalf("outer got %q, want ans-a/on", got)
				}
				var rem []*models.Mock
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest && mk.Name != "ans-a/on" {
						rem = append(rem, mk.ShallowCopy())
					}
				}
				side.mm.SetMocksWithWindow(rem, nil, diffBase.Add(10*time.Millisecond), diffBase.Add(20*time.Millisecond))
				if got := call(); got != "miss" {
					t.Fatalf("inner got %q, want miss (different answers must never be reclaimed)", got)
				}
			})
		}
	})
}

// Unbound transactional command (the first query after START TRANSACTION) uses
// its connection's transaction span [txStart, txEnd] to identify the smallest
// containing window even when short unrelated windows cover both endpoints.
func TestWindowOwnershipTransactionSpanPicksOwnConnection(t *testing.T) {
	const (
		beginSQL = "START TRANSACTION"
		custSQL  = "SELECT id FROM customers WHERE id = 'cust-hot'"
		insOut   = "INSERT INTO orders (id) VALUES ('ord-out')"
		insIn    = "INSERT INTO orders (id) VALUES ('ord-in')"
		commit   = "COMMIT"
	)
	recs := []connRec{
		// Inner transaction on c-in completely inside [10,20ms].
		{"in-begin/tx", "c-in", beginSQL, 11 * time.Millisecond, 0},
		{"in-cust/tx", "c-in", custSQL, 12 * time.Millisecond, 1},
		{"in-ins/tx", "c-in", insIn, 15 * time.Millisecond, 2},
		{"in-commit/on", "c-in", commit, 18 * time.Millisecond, 3},
		// Outer transaction on c-out spans [13ms, 86ms] across unrelated windows u1/u2.
		{"out-begin/tx", "c-out", beginSQL, 13 * time.Millisecond, 4},
		{"out-cust/tx", "c-out", custSQL, 14 * time.Millisecond, 5},
		{"out-ins/tx", "c-out", insOut, 84 * time.Millisecond, 6},
		{"out-commit/on", "c-out", commit, 86 * time.Millisecond, 7},
	}
	tests := []ownerTestCase{
		{
			name: "outer", start: 0, end: 100 * time.Millisecond,
			calls: []connCall{
				{"L1", beginSQL, "*"},
				{"L1", custSQL, "out-cust/tx"},
				{"L1", "INSERT INTO orders (id) VALUES ('live-out')", "out-ins/tx"},
				{"L1", commit, "*"},
			},
		},
		{
			name: "inner", start: 10 * time.Millisecond, end: 20 * time.Millisecond,
			calls: []connCall{
				{"L1", beginSQL, "*"},
				{"L1", custSQL, "in-cust/tx"},
				{"L1", "INSERT INTO orders (id) VALUES ('live-in')", "in-ins/tx"},
				{"L1", commit, "*"},
			},
		},
		{name: "u2", start: 80 * time.Millisecond, end: 90 * time.Millisecond},
	}
	checkWithWindows(t, recs, tests)
}

// A drifted COM_QUERY runs the in-window pass first (which does not settle) and
// retries over the full pool. Scoping per-pass score/waiting state inside the
// retry loop guarantees pass 2 starts clean: an out-of-window fallback is not
// blocked by stale pass-1 state, and an in-window owner-rank 0 mock still wins.
func TestWindowOwnershipScoreResetOnFullScanRetry(t *testing.T) {
	const (
		recIn  = "INSERT INTO orders (id) VALUES ('in-1')"
		recOut = "INSERT INTO orders (id) VALUES ('out-1')"
		live   = "INSERT INTO orders (id) VALUES ('live-1')"
	)
	recs := []connRec{
		// Earlier out-of-window reusable session-like recording at -5ms (before test 0).
		{"out-of-win/on", "c0", recOut, -5 * time.Millisecond, -1},
		// In-window recording owned by test 0 at 15ms.
		{"in-win/on", "c1", recIn, 15 * time.Millisecond, 0},
	}
	tests := []ownerTestCase{
		// First call: in-window pass sees in-win/on, does not settle, retries full
		// scan which sees out-of-win/on (not owned) BEFORE in-win/on (owned) —
		// in-win/on must win.
		// Second call: in-win/on is gone; pass 1 sees no DML match, retries full
		// scan, and out-of-win/on must answer cleanly without stale pass-1 state.
		{
			name: "t0", start: 10 * time.Millisecond, end: 20 * time.Millisecond,
			calls: []connCall{
				{"L1", live, "in-win/on"},
				{"L1", live, "out-of-win/on"},
			},
		},
	}
	checkWithWindows(t, recs, tests)
}

// Inside an overlapping window where both candidates have rank 2 (blanketed by
// unrelated short windows), betterWaiting must still prefer:
//   - a transaction-status-matching (/tx) mock over an earlier non-tx (/on) mock
//     even when both are unbound and have identical owner rank (2);
//   - the bound connection's mock over an earlier mock on another connection
//     when both match transaction status and have identical owner rank (2).
func TestWindowOwnershipWaitingPrefersBoundAndTxStatus(t *testing.T) {
	const (
		beginSQL = "START TRANSACTION"
		custSQL  = "SELECT id FROM customers WHERE id = 'cust-wait'"
		bindSQL  = "SELECT id FROM accounts WHERE id = 'acct-bind'"
		prodSQL  = "SELECT id FROM products WHERE id = 'prod-wait'"
		commit   = "COMMIT"
	)
	recs := []connRec{
		// Tx 1 on c-tx1 is entirely inside u1 [10,20ms], so its tx-span owner is u1
		// (rank 2 for outer), matching non-tx-cust/on's rank 2. Both are unbound on
		// L1's first tx query, so ONLY tx-status agreement separates tx-cust/tx from
		// the earlier non-tx-cust/on.
		{"tx1-begin/tx", "c-tx1", beginSQL, 11 * time.Millisecond, 0},
		{"non-tx-cust/on", "c-nontx", custSQL, 12 * time.Millisecond, 1},
		{"tx-cust/tx", "c-tx1", custSQL, 14 * time.Millisecond, 0},
		{"tx1-commit/on", "c-tx1", commit, 18 * time.Millisecond, 0},
		// Tx 2 on c-bound binds L2 via bindSQL, then both c-other and c-bound have
		// /tx prod-wait mocks inside u2 [30,40ms] (both rank 2, both tx-status ok),
		// so ONLY bound-connection preference separates bound-prod/tx from other-prod/tx.
		{"tx2-begin/tx", "c-bound", beginSQL, 22 * time.Millisecond, 0},
		{"tx2-bind/tx", "c-bound", bindSQL, 24 * time.Millisecond, 0},
		{"other-prod/tx", "c-other", prodSQL, 32 * time.Millisecond, 2},
		{"bound-prod/tx", "c-bound", prodSQL, 35 * time.Millisecond, 0},
		{"tx2-commit/on", "c-bound", commit, 38 * time.Millisecond, 0},
	}
	tests := []ownerTestCase{
		{
			name: "outer", start: 0, end: 50 * time.Millisecond,
			calls: []connCall{
				{"L1", beginSQL, "*"},
				{"L1", custSQL, "tx-cust/tx"},
				{"L1", commit, "*"},
				{"L2", beginSQL, "*"},
				{"L2", bindSQL, "tx2-bind/tx"},
				{"L2", prodSQL, "bound-prod/tx"},
				{"L2", commit, "*"},
			},
		},
		{name: "u1", start: 10 * time.Millisecond, end: 20 * time.Millisecond, calls: []connCall{{"L3", custSQL, "non-tx-cust/on"}}},
		{name: "u2", start: 30 * time.Millisecond, end: 40 * time.Millisecond, calls: []connCall{{"L4", prodSQL, "other-prod/tx"}}},
	}
	checkWithWindows(t, recs, tests)
}

// reclaimTest is one recorded test of a reclaim row: its window (after
// diffBase) and how many times its replay sends the row's query.
type reclaimTest struct {
	name       string
	start, end time.Duration
	calls      int
}

// drainEntry is what a test's consumed drain says of one recording: its name,
// and the earlier test its call is credited to (MockState.CreditTo) in place
// of which recording (CreditReplaces), both empty for the test's own consume.
type drainEntry struct{ name, creditTo, replaces string }

func sortedDrain(d []drainEntry) []drainEntry {
	out := slices.Clone(d)
	slices.SortFunc(out, func(a, b drainEntry) int { return strings.Compare(a.name, b.name) })
	return out
}

// recordedLifetimes states each per-test recording's lifetime in pool, as the
// MySQL recorder states every mock's (models.MetadataLifetimeKey). connPool's
// recordings state none, and an unstated MySQL data mock derives to the
// session tier outside strict mode (models' lax kind fallback), which neither
// the in-flight rule nor the edge rule takes.
func recordedLifetimes(pool []*models.Mock) []*models.Mock {
	for _, mk := range pool {
		if mk.TestModeInfo.Lifetime == models.LifetimePerTest {
			mk.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
		}
	}
	return pool
}

// identicalAnswers is recs as recorded mocks (connPool, recordedLifetimes)
// whose per-test ones all carry the one answer "same-items": recordings of
// one call that timestamps alone cannot tell apart.
func identicalAnswers(recs []connRec) []*models.Mock {
	pool, _ := connPool(recs)
	recordedLifetimes(pool)
	for _, mk := range pool {
		if mk.TestModeInfo.Lifetime == models.LifetimePerTest && len(mk.Spec.MySQLResponses) > 0 {
			mk.Spec.MySQLResponses[0].Payload = "same-items"
		}
	}
	return pool
}

func reclaimWindows(tests []reclaimTest) []models.TestWindow {
	ws := make([]models.TestWindow, 0, len(tests))
	for _, tc := range tests {
		ws = append(ws, models.TestWindow{TestCase: tc.name, Start: diffBase.Add(tc.start), End: diffBase.Add(tc.end)})
	}
	return ws
}

// reclaimCalls sends tc's calls on the live connection L1 and returns their
// answers ("miss" for none).
func reclaimCalls(t *testing.T, db integrations.MockMemDb, sql string, tc reclaimTest, fullScan bool) []string {
	t.Helper()
	ctx, dctx := connLive("L1")
	var answers []string
	for i := 0; i < tc.calls; i++ {
		var resp *mysql.Response
		var ok bool
		var err error
		if fullScan {
			resp, ok, _, err = replayer.MatchBoundForTest(ctx, zap.NewNop(), connReq(sql), db, dctx, true)
		} else {
			resp, ok, _, err = replayer.MatchWithEngineForTest(ctx, zap.NewNop(), connReq(sql), db, dctx, nil)
		}
		if err != nil {
			t.Fatalf("%s call %d: %v", tc.name, i, err)
		}
		if !ok || resp == nil {
			answers = append(answers, "miss")
			continue
		}
		answers = append(answers, resp.Payload)
	}
	return answers
}

// tallyStore is a real MockManager that keeps what the matcher tallies of its
// ownership decisions (integrations.OwnershipTally) instead of counting it.
type tallyStore struct {
	*proxy.MockManager
	events []integrations.OwnershipEvent
}

func (s *tallyStore) TallyOwnership(e integrations.OwnershipEvent) { s.events = append(s.events, e) }

// replayReclaim is a keploy test of a set with no mappings.yaml on a real
// MockManager seeded with the recorded windows: each test's window staged
// strictly from the per-test recordings no drain has consumed yet (the
// agent's window load and its filter of what is consumed), in recorded
// order. It returns each test's answers, consumed drain and tallied
// ownership decisions, and the manager's served counts.
func replayReclaim(t *testing.T, pool []*models.Mock, sql string, tests []reclaimTest, fullScan bool) (map[string][]string, map[string][]drainEntry, map[string]uint64, map[string][]integrations.OwnershipEvent) {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	db := &tallyStore{MockManager: mm}
	mm.SeedRecordedWindows(reclaimWindows(tests))
	consumed := map[string]bool{}
	answers, drains, events := map[string][]string{}, map[string][]drainEntry{}, map[string][]integrations.OwnershipEvent{}
	for _, tc := range tests {
		var perTest []*models.Mock
		for _, mk := range pool {
			if mk.TestModeInfo.Lifetime == models.LifetimePerTest && !consumed[mk.Name] {
				perTest = append(perTest, mk.ShallowCopy())
			}
		}
		mm.SetMocksWithWindow(perTest, nil, diffBase.Add(tc.start), diffBase.Add(tc.end))
		db.events = nil
		answers[tc.name] = reclaimCalls(t, db, sql, tc, fullScan)
		events[tc.name] = db.events
		for _, st := range mm.GetConsumedMocks() {
			drains[tc.name] = append(drains[tc.name], drainEntry{st.Name, st.CreditTo, st.CreditReplaces})
			if st.Usage == models.Deleted {
				consumed[st.Name] = true
			}
		}
	}
	return answers, drains, mm.ServedCounts(), events
}

// replayMapped is the next keploy test of the set, from the mappings.yaml the
// first wrote (mapping, each test's recordings): on a fresh MockManager, each
// test's window staged with only the recordings its mapping names (the agent's
// load by name, KEPLOY_PROVEN unset), or, exact, each test served exactly
// those under exact serving (what that ownership gives there; keploy test
// itself does not read a mapping keploy test wrote under the proven switch).
// It returns each test's answers and drain, and the served counts.
func replayMapped(t *testing.T, pool []*models.Mock, sql string, tests []reclaimTest, mapping map[string][]string, exact, fullScan bool) (map[string][]string, map[string][]string, map[string]uint64) {
	t.Helper()
	byName := map[string]*models.Mock{}
	for _, mk := range pool {
		byName[mk.Name] = mk
	}
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(reclaimWindows(tests))
	if exact {
		mm.SetMocksWithWindow(nil, pool, models.BaseTime, time.Now().Add(time.Hour))
	}
	answers, drains := map[string][]string{}, map[string][]string{}
	for _, tc := range tests {
		var mapped []*models.Mock
		for _, n := range mapping[tc.name] {
			mapped = append(mapped, byName[n].ShallowCopy())
		}
		if exact {
			mm.ServeExactTest(tc.name, mapped)
		} else {
			mm.SetMocksWithWindow(mapped, nil, diffBase.Add(tc.start), diffBase.Add(tc.end))
		}
		answers[tc.name] = reclaimCalls(t, mm, sql, tc, fullScan)
		for _, st := range mm.GetConsumedMocks() {
			if byName[st.Name] != nil && byName[st.Name].TestModeInfo.Lifetime == models.LifetimePerTest {
				drains[tc.name] = append(drains[tc.name], st.Name)
			}
		}
	}
	return answers, drains, mm.ServedCounts()
}

// A reclaim credits each recording to one call and each call to one
// recording, on the real MockManager: the inner test is credited the
// recording made in its window, which the outer test's identical call took,
// and the outer test's call is credited the outer window's unused identical
// copy in its place (MockState.CreditTo / CreditReplaces in the inner test's
// drain), each served once. So the mappings.yaml the replay writes from the
// drains lists outer -> m-out/on and inner -> m-in/on, and the next replay, which
// serves each test what its mapping names (staged by window; and exact serving
// of the same ownership), answers every call, each recording once. Before, the inner
// test's drain held m-out as its own and m-in again: three serves for two
// calls, m-in listed for both tests and m-out, recorded in another test's
// window, for the inner test, whose next replay then missed.
func TestWindowReclaimCreditsEachRecordingOnce(t *testing.T) {
	const sql = "SELECT sku FROM order_items WHERE order_id = 'ord-shared'"
	cases := []struct {
		name        string
		recs        []connRec
		tests       []reclaimTest
		wantDrains  map[string][]drainEntry
		wantMapping map[string][]string
	}{
		{
			// Outer [0,100ms] runs first; the unrelated window [80,90ms] holds
			// outer's own copy at 85ms, so outer owns neither recording and takes
			// the earlier one, recorded in inner's window [10,20ms].
			name: "one hop",
			recs: []connRec{
				{"m-in/on", "c1", sql, 15 * time.Millisecond, 0},
				{"m-out/on", "c2", sql, 85 * time.Millisecond, 1},
			},
			tests: []reclaimTest{
				{"outer", 0, 100 * time.Millisecond, 1},
				{"inner", 10 * time.Millisecond, 20 * time.Millisecond, 1},
				{"unrelated", 80 * time.Millisecond, 90 * time.Millisecond, 0},
			},
			wantDrains: map[string][]drainEntry{
				"outer": {{"m-in/on", "", ""}},
				"inner": {{"m-in/on", "", ""}, {"m-out/on", "outer", "m-in/on"}},
			},
			wantMapping: map[string][]string{"outer": {"m-out/on"}, "inner": {"m-in/on"}},
		},
		{
			// A [0,100ms] takes m1 (in u1's window), B [10,60ms] takes m3 (in C's),
			// and C [45,55ms] reclaims m3 along B -> m1 -> A -> m2.
			name: "two hops",
			recs: []connRec{
				{"m1/on", "c1", sql, 15 * time.Millisecond, 0},
				{"m3/on", "c2", sql, 50 * time.Millisecond, 1},
				{"m2/on", "c3", sql, 85 * time.Millisecond, 2},
			},
			tests: []reclaimTest{
				{"A", 0, 100 * time.Millisecond, 1},
				{"B", 10 * time.Millisecond, 60 * time.Millisecond, 1},
				{"u1", 12 * time.Millisecond, 18 * time.Millisecond, 0},
				{"C", 45 * time.Millisecond, 55 * time.Millisecond, 1},
				{"u2", 80 * time.Millisecond, 90 * time.Millisecond, 0},
			},
			wantDrains: map[string][]drainEntry{
				"A": {{"m1/on", "", ""}},
				"B": {{"m3/on", "", ""}},
				"C": {{"m1/on", "B", "m3/on"}, {"m2/on", "A", "m1/on"}, {"m3/on", "", ""}},
			},
			wantMapping: map[string][]string{"A": {"m2/on"}, "B": {"m1/on"}, "C": {"m3/on"}},
		},
		{
			// Outer takes m-in with two spares, m-out and m-x (m-x is late's,
			// [92,98ms]); inner reclaims m-in for m-out, and outer's credit,
			// moved to m-out with m-x its spare still, is reclaimed again by the
			// unrelated test, whose window holds m-out: outer ends credited m-x.
			name: "a second reclaim through a moved credit",
			recs: []connRec{
				{"m-in/on", "c1", sql, 15 * time.Millisecond, 0},
				{"m-out/on", "c2", sql, 85 * time.Millisecond, 1},
				{"m-x/on", "c3", sql, 95 * time.Millisecond, 2},
			},
			tests: []reclaimTest{
				{"outer", 0, 100 * time.Millisecond, 1},
				{"inner", 10 * time.Millisecond, 20 * time.Millisecond, 1},
				{"unrelated", 80 * time.Millisecond, 90 * time.Millisecond, 1},
				{"late", 92 * time.Millisecond, 98 * time.Millisecond, 0},
			},
			wantDrains: map[string][]drainEntry{
				"outer":     {{"m-in/on", "", ""}},
				"inner":     {{"m-in/on", "", ""}, {"m-out/on", "outer", "m-in/on"}},
				"unrelated": {{"m-out/on", "", ""}, {"m-x/on", "outer", "m-out/on"}},
			},
			wantMapping: map[string][]string{"outer": {"m-x/on"}, "inner": {"m-in/on"}, "unrelated": {"m-out/on"}},
		},
		{
			// H [0,100ms] takes u0 (its spares v0, a1, a2), G [5,60ms] takes v0
			// (spare a1); X [10,20ms], whose window holds u0 and v0, reclaims
			// u0 along H -> a1, then v0 along G -> a1 -> H -> a2, through the
			// credit its first reclaim gave H in the same drain: H's entry,
			// listing u0, moves to a2, G's to a1.
			name: "two reclaims in one drain",
			recs: []connRec{
				{"u0/on", "c1", sql, 12 * time.Millisecond, 0},
				{"v0/on", "c2", sql, 18 * time.Millisecond, 1},
				{"a1/on", "c3", sql, 50 * time.Millisecond, 2},
				{"a2/on", "c4", sql, 85 * time.Millisecond, 3},
			},
			tests: []reclaimTest{
				{"H", 0, 100 * time.Millisecond, 1},
				{"G", 5 * time.Millisecond, 60 * time.Millisecond, 1},
				{"X", 10 * time.Millisecond, 20 * time.Millisecond, 2},
				{"W", 45 * time.Millisecond, 55 * time.Millisecond, 0},
				{"U", 80 * time.Millisecond, 90 * time.Millisecond, 0},
			},
			wantDrains: map[string][]drainEntry{
				"H": {{"u0/on", "", ""}},
				"G": {{"v0/on", "", ""}},
				"X": {{"a1/on", "G", "v0/on"}, {"u0/on", "", ""}, {"a2/on", "H", "u0/on"}, {"v0/on", "", ""}},
			},
			wantMapping: map[string][]string{"H": {"a2/on"}, "G": {"a1/on"}, "X": {"u0/on", "v0/on"}},
		},
	}
	for _, c := range cases {
		for _, fullScan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fullScan=%v", c.name, fullScan), func(t *testing.T) {
				pool := identicalAnswers(c.recs)
				answers, drains, served, _ := replayReclaim(t, pool, sql, c.tests, fullScan)
				for _, tc := range c.tests {
					for i, a := range answers[tc.name] {
						if a != "same-items" {
							t.Errorf("first replay: %s call %d answered %q, want same-items", tc.name, i, a)
						}
					}
					if got, want := sortedDrain(drains[tc.name]), sortedDrain(c.wantDrains[tc.name]); !slices.Equal(got, want) {
						t.Errorf("first replay: %s's drain %v, want %v", tc.name, got, want)
					}
				}
				for _, r := range c.recs {
					if served[r.name] != 1 {
						t.Errorf("first replay: %s served %d times, want once (served counts %v)", r.name, served[r.name], served)
					}
				}
				for _, exact := range []bool{false, true} {
					answers, drains, served := replayMapped(t, pool, sql, c.tests, c.wantMapping, exact, fullScan)
					for _, tc := range c.tests {
						for i, a := range answers[tc.name] {
							if a != "same-items" {
								t.Errorf("replay from the mapping (exact=%v): %s call %d answered %q, want same-items", exact, tc.name, i, a)
							}
						}
						if got := drains[tc.name]; !slices.Equal(got, c.wantMapping[tc.name]) {
							t.Errorf("replay from the mapping (exact=%v): %s consumed %v, want its mapping %v", exact, tc.name, got, c.wantMapping[tc.name])
						}
					}
					for _, r := range c.recs {
						if served[r.name] != 1 {
							t.Errorf("replay from the mapping (exact=%v): %s served %d times, want once (served counts %v)", exact, r.name, served[r.name], served)
						}
					}
				}
			})
		}
	}
}

// refusingStore is a real MockManager whose window reclaim refuses the path
// of the consumed recording named refuse, as the store refuses one the open
// test holds a step of, or one gone since (CommitWindowReclaim's false), and
// keeps the recordings it was asked to reclaim, in order.
type refusingStore struct {
	*proxy.MockManager
	refuse string
	asked  []string
}

func (s *refusingStore) CommitWindowReclaim(reclaimed *models.Mock, winStart, winEnd time.Time) (*models.Mock, bool) {
	s.asked = append(s.asked, reclaimed.Name)
	if reclaimed.Name == s.refuse {
		return nil, false
	}
	return s.MockManager.CommitWindowReclaim(reclaimed, winStart, winEnd)
}

// When the store refuses the best reclaim candidate's path, the matcher
// tries the next candidate in rank order instead of missing. Outer [0,100ms]
// takes both recordings made in inner's window [10,20ms] (r1, r2), with the
// copies two unrelated windows hold as spares; inner's call, r1's path
// refused, reclaims r2. Before, the matcher tried r1 alone and inner missed.
func TestWindowReclaimTriesTheNextCandidateWhenAPathIsRefused(t *testing.T) {
	const sql = "SELECT sku FROM order_items WHERE order_id = 'ord-shared'"
	pool := identicalAnswers([]connRec{
		{"r1/on", "c1", sql, 12 * time.Millisecond, 0},
		{"r2/on", "c2", sql, 15 * time.Millisecond, 1},
		{"f2/on", "c3", sql, 65 * time.Millisecond, 2},
		{"f1/on", "c4", sql, 85 * time.Millisecond, 3},
	})
	tests := []reclaimTest{
		{"outer", 0, 100 * time.Millisecond, 2},
		{"inner", 10 * time.Millisecond, 20 * time.Millisecond, 1},
		{"v", 60 * time.Millisecond, 70 * time.Millisecond, 0},
		{"u", 80 * time.Millisecond, 90 * time.Millisecond, 0},
	}
	for _, fullScan := range []bool{false, true} {
		t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			db := &refusingStore{MockManager: mm, refuse: "r1/on"}
			mm.SeedRecordedWindows(reclaimWindows(tests))
			consumed := map[string]bool{}
			answers := map[string][]string{}
			for _, tc := range tests {
				var perTest []*models.Mock
				for _, mk := range pool {
					if mk.TestModeInfo.Lifetime == models.LifetimePerTest && !consumed[mk.Name] {
						perTest = append(perTest, mk.ShallowCopy())
					}
				}
				mm.SetMocksWithWindow(perTest, nil, diffBase.Add(tc.start), diffBase.Add(tc.end))
				answers[tc.name] = reclaimCalls(t, db, sql, tc, fullScan)
				for _, st := range mm.GetConsumedMocks() {
					if st.Usage == models.Deleted {
						consumed[st.Name] = true
					}
				}
			}
			if want := []string{"same-items", "same-items"}; !slices.Equal(answers["outer"], want) {
				t.Errorf("outer's calls answered %v; want %v", answers["outer"], want)
			}
			if want := []string{"same-items"}; !slices.Equal(answers["inner"], want) {
				t.Errorf("inner's call answered %v; want %v (r1's path refused, r2 reclaimed)", answers["inner"], want)
			}
			if want := []string{"r1/on", "r2/on"}; !slices.Equal(db.asked, want) {
				t.Errorf("the matcher asked to reclaim %v; want %v", db.asked, want)
			}
		})
	}
}

// A call sent in the gap 30 µs after test A ended and answered once test B
// began is B's by the in-flight rule, as on main: the edge rule, though the
// call lies nearest A's end, does not take it, so it is no other window's and
// answers B's first call at once, in recorded order, and B's own later
// recording its second. Before, it ranked as A's (by the edge rule) and
// waited, so B's later recording answered B's first call and the in-flight one
// its second; and A's staging could serve it too.
func TestWindowOwnershipLeavesACallInFlightToItsTest(t *testing.T) {
	const sql = "SELECT stock FROM products WHERE id = 'p-1'"
	pool, _ := connPool([]connRec{
		{"in-flight/on", "c1", sql, 10*time.Millisecond + 30*time.Microsecond, 1},
		{"own/on", "c2", sql, 15 * time.Millisecond, 1},
	})
	recordedLifetimes(pool)
	for _, mk := range pool {
		if mk.Name == "in-flight/on" {
			mk.Spec.ResTimestampMock = diffBase.Add(11 * time.Millisecond)
		}
	}
	tests := []reclaimTest{{"A", 0, 10 * time.Millisecond, 0}, {"B", 10500 * time.Microsecond, 20 * time.Millisecond, 2}}
	for _, fullScan := range []bool{false, true} {
		t.Run(fmt.Sprintf("fullScan=%v", fullScan), func(t *testing.T) {
			answers, _, _, _ := replayReclaim(t, pool, sql, tests, fullScan)
			if want := []string{"in-flight/on", "own/on"}; !slices.Equal(answers["B"], want) {
				t.Errorf("B's calls answered %v; want %v", answers["B"], want)
			}
		})
	}
	t.Run("A's staging does not hold it", func(t *testing.T) {
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SeedRecordedWindows(reclaimWindows(tests))
		var perTest []*models.Mock
		for _, mk := range pool {
			if mk.TestModeInfo.Lifetime == models.LifetimePerTest {
				perTest = append(perTest, mk.ShallowCopy())
			}
		}
		mm.SetMocksWithWindow(perTest, nil, diffBase, diffBase.Add(10*time.Millisecond))
		staged, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		if len(staged) != 0 {
			t.Errorf("A's staging holds %d per-test mocks (%s...); want none: the in-flight call is B's", len(staged), staged[0].Name)
		}
	})
}

// Each ownership decision the matcher makes reaches the store's tally (the
// set's summary), from the call that made it: an answer the edge rule gave the
// open window (20 µs past its end, answered before the next window began),
// which waited for one of the window's own first; a
// first exact match another window owns, kept from answering at once, and the
// open window's own recording answering instead; one kept from answering that
// then answered, nothing better in the window; a drifted query answered by the
// open window's own recording over a higher-scoring one a nested window owns.
// A call the first exact match answers at once is tallied nothing.
func TestWindowOwnershipTalliesEachDecision(t *testing.T) {
	ev := func(e ...integrations.OwnershipEvent) []integrations.OwnershipEvent { return e }
	const sql = "SELECT COUNT(*) FROM orders"
	const (
		insInner = "INSERT INTO orders (id, total_cents) VALUES ('ord-inn1', 100)"
		insOuter = "INSERT INTO orders (id, total_cents) VALUES ('ord-outer-longer', 12345)"
		insLive  = "INSERT INTO orders (id, total_cents) VALUES ('ord-live', 999)"
	)
	for _, c := range []struct {
		name  string
		sql   string
		recs  []connRec
		tests []reclaimTest
		want  map[string][]integrations.OwnershipEvent
	}{
		{
			name: "edge rule", sql: sql,
			recs:  []connRec{{"edge-a/on", "c1", sql, 10*time.Millisecond + 20*time.Microsecond, 0}},
			tests: []reclaimTest{{"A", 0, 10 * time.Millisecond, 1}, {"B", 11 * time.Millisecond, 20 * time.Millisecond, 0}},
			// The edge rule's recording waits for one of the window's own,
			// then answers: deferred too.
			want: map[string][]integrations.OwnershipEvent{"A": ev(integrations.OwnershipEdgeAnswered, integrations.OwnershipDeferred)},
		},
		{
			name: "deferred, answered by the window's own", sql: sql,
			recs:  []connRec{{"inner/on", "c1", sql, 15 * time.Millisecond, 1}, {"outer/on", "c2", sql, 25 * time.Millisecond, 0}},
			tests: []reclaimTest{{"outer", 0, 30 * time.Millisecond, 1}, {"inner", 10 * time.Millisecond, 20 * time.Millisecond, 1}},
			want: map[string][]integrations.OwnershipEvent{
				"outer": ev(integrations.OwnershipDeferred, integrations.OwnershipPassedOver),
				"inner": nil,
			},
		},
		{
			name: "deferred, then answered", sql: sql,
			recs:  []connRec{{"inner/on", "c1", sql, 15 * time.Millisecond, 1}},
			tests: []reclaimTest{{"outer", 0, 30 * time.Millisecond, 1}, {"inner", 10 * time.Millisecond, 20 * time.Millisecond, 0}},
			want:  map[string][]integrations.OwnershipEvent{"outer": ev(integrations.OwnershipDeferred)},
		},
		{
			name: "drifted query ranked over score", sql: insLive,
			recs:  []connRec{{"inner-ins/on", "c2", insInner, 15 * time.Millisecond, 1}, {"outer-ins/on", "c1", insOuter, 25 * time.Millisecond, 0}},
			tests: []reclaimTest{{"outer", 0, 30 * time.Millisecond, 1}, {"inner", 10 * time.Millisecond, 20 * time.Millisecond, 1}},
			want: map[string][]integrations.OwnershipEvent{
				"outer": ev(integrations.OwnershipDriftRanked),
				"inner": nil,
			},
		},
	} {
		for _, fullScan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fullScan=%v", c.name, fullScan), func(t *testing.T) {
				pool, _ := connPool(c.recs)
				_, _, _, got := replayReclaim(t, recordedLifetimes(pool), c.sql, c.tests, fullScan)
				for test, want := range c.want {
					if !slices.Equal(got[test], want) {
						t.Errorf("%s's calls tallied %v; want %v", test, got[test], want)
					}
				}
			})
		}
	}
}
