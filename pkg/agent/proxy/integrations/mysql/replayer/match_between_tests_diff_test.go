package replayer_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// btStatements are the statements every connection of a between-tests set
// prepares; a set whose between-tests calls are polls prepares
// btPollStatement too.
var btStatements = map[uint32]string{7: diffQueryA, 8: diffQueryB}

// btPollStatement is a status poll no test runs, with btPollQuery.
const (
	btPollStatement uint32 = 11
	btPollQuery            = "SELECT status FROM health WHERE probe = ?"
)

// btSetStatements are the statements a set's connections prepare.
func btSetStatements(polls bool) map[uint32]string {
	out := map[uint32]string{}
	for id, sql := range btStatements {
		out[id] = sql
	}
	if polls {
		out[btPollStatement] = btPollQuery
	}
	return out
}

// btCall is one recorded call of a between-tests set and the recording that
// answered it. key names the request (statement and parameter), the same for
// two recordings of one request.
type btCall struct {
	conn string
	req  mysql.Request
	key  string
	mock *models.Mock
}

// btTestCalls is how many calls each test of a between-tests set made.
const btTestCalls = 10

// btRecordedSet is a recording of n calls on conns connections: start-up
// calls before the first test, tests of btTestCalls calls each inside their
// windows (diffWindow), and, after about half of the tests, one to three
// calls made between that test and the next. Each call is an EXECUTE of
// statement 7 or 8 with a parameter from a range of n/8, so most requests are
// recorded several times, by tests and between them. Every recording is
// per-test, as keploy record files it, and answers with its own name.
// between[i] are the calls made after test i. With polls, every call made
// between two tests, and the last start-up call, is instead a poll
// (btPollStatement) with one of three parameters, so most poll requests are
// recorded in many gaps.
func btRecordedSet(n, conns int, seed int64, polls bool) (preps []*models.Mock, startup []btCall, tests, between [][]btCall) {
	r := rand.New(rand.NewSource(seed))
	for c := 1; c <= conns; c++ {
		conn := fmt.Sprintf("c%d", c)
		for id, sql := range btSetStatements(polls) {
			at := diffBase.Add(-time.Millisecond + time.Duration(2*c+int(id))*time.Microsecond)
			preps = append(preps, diffPrepareMock(fmt.Sprintf("prep-%s-%d", conn, id), conn, sql, id, at))
		}
	}
	span := n/8 + 1
	seq := 0
	call := func(at time.Time, poll bool) btCall {
		conn := fmt.Sprintf("c%d", 1+r.Intn(conns))
		var stmt uint32
		var p string
		if poll {
			stmt, p = btPollStatement, fmt.Sprint(1+r.Intn(3))
		} else {
			stmt, p = uint32(7+r.Intn(2)), fmt.Sprint(1+r.Intn(span))
		}
		key := fmt.Sprintf("exec-%d-%s", stmt, p)
		req := executeReq(stmt, p)
		seq++
		mk := diffMock(fmt.Sprintf("m%05d-%s-%s", seq, conn, key), conn, []mysql.Request{req}, at, models.LifetimePerTest, "mocks")
		return btCall{conn: conn, req: req, key: key, mock: mk}
	}
	for i := 0; i < 3; i++ {
		startup = append(startup, call(diffBase.Add(time.Duration(i-5)*100*time.Microsecond), polls && i == 2))
	}
	for made := len(startup); made < n; {
		ti := len(tests)
		start, _ := diffWindow(ti)
		var calls, gap []btCall
		for i := 0; i < btTestCalls && made < n; i++ {
			calls = append(calls, call(start.Add(time.Duration(1+i)*300*time.Microsecond), false))
			made++
		}
		// The gap after test ti is (start+4ms, start+10ms).
		for i, k := 0, r.Intn(4); i < k && r.Intn(2) == 0 && made < n; i++ {
			gap = append(gap, call(start.Add(5*time.Millisecond+time.Duration(i)*time.Millisecond), polls))
			made++
		}
		tests, between = append(tests, calls), append(between, gap)
	}
	return preps, startup, tests, between
}

// btReplayedTests are the tests of a set a replay runs: every one of a set of
// up to 100 tests, and of a larger set 100 spread across it (the pool every
// call reads is the whole set either way).
func btReplayedTests(tests int) []int {
	step := 1
	if tests > 100 {
		step = tests / 100
	}
	var out []int
	for i := 0; i < tests; i += step {
		out = append(out, i)
	}
	return out
}

// btStaging is how a replay stages a recorded set's recordings.
type btStaging string

const (
	// btLax is keploy test with every recording staged at once
	// (KEPLOY_STRICT_MOCK_WINDOW=0, test.strictMockWindow: false): each
	// test's window as the agent stages it (diffSide.stage).
	btLax btStaging = "lax"
	// btStrictWindow is keploy test's default strict mock window: before the
	// first test the whole set, as keploy test's first staging
	// (BaseTime..now) stages it, and for each test its window's recordings
	// beside the start-up ones (btStageStrictWindow).
	btStrictWindow btStaging = "strictwindow"
	// btMockReplay is keploy mock replay: the whole set staged once
	// (BaseTime..now) and no recorded windows.
	btMockReplay btStaging = "mockreplay"
)

// btTiming is when keploy test sends a call the app made between two tests.
type btTiming string

const (
	// btNextWindow: once the next test began, inside its window. keploy
	// test sends a test as soon as the previous one is answered, so this
	// is the usual timing. A call made after the set's last test, which no
	// test follows, is sent with that test's window still open.
	btNextWindow btTiming = "next"
	// btStillOpen: before the replay sent the next test, with the window
	// of the test the call followed still open.
	btStillOpen btTiming = "open"
)

// btReplays are the replays of each recorded set: keploy test with every
// recording staged and with the strict mock window, each in both timings,
// and keploy mock replay, which opens no window.
var btReplays = []struct {
	staging btStaging
	timing  btTiming
}{
	{btLax, btNextWindow}, {btLax, btStillOpen},
	{btStrictWindow, btNextWindow}, {btStrictWindow, btStillOpen},
	{btMockReplay, btStillOpen},
}

// btStageStrictWindow stages the window [start, end] of a test as keploy
// test's strict mock window does: the per-test recordings inside the window
// and those made at start-up (before the set's first test), less those
// consumed. A recording made between two tests is in no test's window and
// after the start-up band, so it is not staged.
func btStageStrictWindow(s *diffSide, start, end time.Time) {
	for _, st := range s.mm.GetConsumedMocks() {
		if st.Usage == models.Deleted {
			s.consumed[st.Name] = true
		}
	}
	var perTest, reusable []*models.Mock
	for _, m := range s.pool {
		c := m.DeepCopy()
		if c.TestModeInfo.Lifetime != models.LifetimePerTest || c.Spec.Metadata["type"] == "config" {
			reusable = append(reusable, c)
			continue
		}
		if s.consumed[c.Name] {
			continue
		}
		at := c.Spec.ReqTimestampMock
		if at.Before(diffBase) || (!at.Before(start) && !at.After(end)) {
			perTest = append(perTest, c)
		}
	}
	for _, part := range [][]*models.Mock{perTest, reusable} {
		sort.SliceStable(part, func(i, j int) bool { return part[i].Spec.ReqTimestampMock.Before(part[j].Spec.ReqTimestampMock) })
	}
	s.mm.SetMocksWithWindow(perTest, reusable, start, end)
}

// TestBetweenTestsCallsOfARecordedSetAreAnsweredWithTheirOwnRequestsRecording
// replays recorded sets of 100, 1k and 10k calls with the strict noise engine
// off and on (replayer.StrictEngineForTest), in the replays of btReplays.
// keploy test's first staging (BaseTime..now) stages the whole set and opens
// no window (start-up); it opens each test's window as it stages the test and
// leaves it open until it stages the next. The row requires every call to be
// answered with a recording of its own request (statement and parameter), no
// recording made in a test to answer twice, and each start-up and
// between-tests call of keploy test to be answered with its own recording,
// the one of its gap: the session pool keeps a recording made outside every
// test after it answers, so a second call never takes it again.
//
// Two exceptions are counted, not held. A between-tests call under the strict
// mock window: its own recording is never staged there. When, sent in the next
// test's window, it takes one of that test's recordings, the test's calls that
// follow in the window are not held either: one of them lost its recording
// (main answers them the same). And the stated limit
// of the still-open timing: a between-tests call sent with the window of the
// test it followed still open takes the recording of its request made in the
// gap before that window when there is one.
//
// In the next-window timing the between-tests calls are polls of a statement
// no test runs: a call of a statement the next test recorded with other
// parameters takes that test's recording there (a stated limit), which this
// row does not measure. The still-open timing's sets make the between-tests
// calls with the tests' statements.
//
// The first recording of the statement with other parameters (the FIFO
// fallback) outranked a recording with the call's parameters that was not
// the open test's own: a start-up or between-tests call, or every call of a
// mock replay, was answered with another call's rows, and could consume a
// later test's recording, whose own call then took another. And of two
// recordings of a request made outside every test, the first answered every
// call of it.
//
// With KEPLOY_MYSQL_BETWEEN_ANSWERS=<dir> every call's answer is written to
// <dir>/n<n>-engine<bool>-<replay>.txt, one line a call: "<kind> <call>
// <key> <answer> <answer's key>", kind startup, test or between, answer MISS
// for a call nothing answered. A comparison of the files of two builds shows
// which calls changed answer.
//
// The replays run side by side (t.Parallel), each against stores of its own.
// A mock replay opens no window, so each of its calls reads every recording
// of the set: under -race its 10k replays alone ran for 970 s, summed over
// the parallel subtests (box .142). Under -race the row replays the 100 sets
// and the 1k sets but their mock replays; the plain run replays all.
func TestBetweenTestsCallsOfARecordedSetAreAnsweredWithTheirOwnRequestsRecording(t *testing.T) {
	for _, n := range []int{100, 1000, 10000} {
		for _, engine := range []bool{false, true} {
			for _, rp := range btReplays {
				name := string(rp.staging)
				if rp.staging != btMockReplay {
					name += "-" + string(rp.timing)
				}
				t.Run(fmt.Sprintf("n=%d/engine=%v/%s", n, engine, name), func(t *testing.T) {
					if raceEnabled && (n == 10000 || (n == 1000 && rp.staging == btMockReplay)) {
						t.Skip("left to the run without -race")
					}
					t.Parallel()
					btReplaySet(t, n, engine, rp.staging, rp.timing, name)
				})
			}
		}
	}
}

func btReplaySet(t *testing.T, n int, engine bool, staging btStaging, timing btTiming, name string) {
	polls := timing == btNextWindow
	preps, startup, tests, between := btRecordedSet(n, 4, int64(n), polls)
	keys := map[string]string{}
	pool := append([]*models.Mock{}, preps...)
	for _, part := range [][][]btCall{{startup}, tests, between} {
		for _, calls := range part {
			for _, c := range calls {
				keys[c.mock.Name] = c.key
				pool = append(pool, c.mock)
			}
		}
	}
	side := newDiffSide(pool)
	defer side.mm.Close()
	if staging != btMockReplay {
		seedWindows(side.mm, len(tests))
	}
	inTest := map[string]bool{}
	for _, calls := range tests {
		for _, c := range calls {
			inTest[c.mock.Name] = true
		}
	}
	// gapOf is the gap a recording made outside every test was made in: -1
	// for start-up, i for the gap after test i.
	gapOf := map[string]int{}
	for _, c := range startup {
		gapOf[c.mock.Name] = -1
	}
	for i, calls := range between {
		for _, c := range calls {
			gapOf[c.mock.Name] = i
		}
	}
	var eng *mocknoise.Engine
	if engine {
		eng = replayer.StrictEngineForTest()
	}
	ctxs, dctxs := map[string]context.Context{}, map[string]*wire.DecodeContext{}
	for c := 1; c <= 4; c++ {
		conn := fmt.Sprintf("c%d", c)
		ctxs[conn] = context.WithValue(context.Background(), models.ClientConnectionIDKey, conn)
		dctxs[conn] = &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: btSetStatements(polls), NextStmtID: 100}
	}

	var out strings.Builder
	digest := sha256.New()
	served := map[string]int{}
	calls, wrong := 0, map[string]int{}
	open := -1 // the test whose window is open, -1 for none
	// strictTaken: under the strict mock window, a between-tests call sent
	// in the open window took a recording of that window's test (its own
	// is not staged), whose own call then takes another.
	strictTaken := false
	run := func(kind string, c btCall) {
		calls++
		resp, ok, miss, err := replayer.MatchCommandForTest(ctxs[c.conn], zap.NewNop(), c.req, side.mm, dctxs[c.conn], eng, false)
		if err != nil {
			t.Fatalf("%s call %d (%s on %s): err=%v", kind, calls, c.key, c.conn, err)
		}
		held := staging != btStrictWindow || kind == "startup" || (kind == "test" && !strictTaken)
		answer := "MISS"
		if ok {
			answer = resp.Payload
		}
		fmt.Fprintf(&out, "%s %d %s %s %s\n", kind, calls, c.key, answer, keys[answer])
		digest.Write([]byte(answer + "\n"))
		if !held && kind == "between" && inTest[answer] {
			strictTaken = true
		}
		if keys[answer] != c.key {
			if !held {
				wrong["unheld "+kind]++
			} else if wrong[kind]++; wrong[kind] <= 3 {
				t.Errorf("%s call %d (%s on %s) was answered with %s, the recording of %s (miss: %v)", kind, calls, c.key, c.conn, answer, keys[answer], miss)
			}
		}
		if !ok {
			return
		}
		// A start-up or between-tests call of keploy test, its request
		// answered, is answered with its own recording, the one of its gap:
		// another recording of its request may hold other rows. The stated
		// limit: sent with the window of the test it followed still open,
		// it takes the recording of its request made in the gap before that
		// window.
		limit := false
		if held && kind != "test" && staging != btMockReplay && keys[answer] == c.key && answer != c.mock.Name {
			if g, outside := gapOf[answer]; kind == "between" && outside && open == gapOf[c.mock.Name] && g == open-1 {
				limit = true
				wrong["limit"]++
			} else if wrong["other gap"]++; wrong["other gap"] <= 3 {
				t.Errorf("%s call %d (%s on %s) was answered with %s, not its own recording %s", kind, calls, c.key, c.conn, answer, c.mock.Name)
			}
		}
		if served[answer]++; served[answer] > 1 {
			switch {
			case !inTest[answer] && !held:
				wrong["unheld reused"]++
			case !inTest[answer] && limit:
				wrong["limit reused"]++
			case !inTest[answer]:
				// keploy test keeps a recording made outside every test
				// in the session pool, where an answer does not consume
				// it (lax mode): a later call of the same request, made in
				// another gap, took an earlier gap's.
				if wrong["reused"]++; wrong["reused"] <= 3 {
					t.Errorf("%s call %d (%s on %s): outside-test recording %s answered a second call", kind, calls, c.key, c.conn, answer)
				}
			default:
				if wrong["twice"]++; wrong["twice"] <= 3 {
					t.Errorf("%s call %d (%s on %s): test recording %s answered a second call", kind, calls, c.key, c.conn, answer)
				}
			}
		}
	}
	stage := func(ti int) {
		switch staging {
		case btStrictWindow:
			s, e := diffWindow(ti)
			btStageStrictWindow(side, s, e)
		case btLax:
			side.stage(diffWindow(ti))
		default:
			return // a mock replay opens no window
		}
		open, strictTaken = ti, false
	}

	// keploy test's first staging, strict window or not, and keploy mock
	// replay's only one: every per-test recording is placed in [BaseTime,
	// now] (pkg/util.go, filterByTimeStampInFlightAware), and the store
	// opens no window.
	side.stage(models.BaseTime, time.Now())
	if staging != btMockReplay {
		side.mm.SetCurrentTestWindow(time.Time{}, time.Time{}) // start-up: no test begun
	}
	for _, c := range startup {
		run("startup", c)
	}
	for _, ti := range btReplayedTests(len(tests)) {
		if open != ti {
			stage(ti)
		}
		for _, c := range tests[ti] {
			run("test", c)
		}
		// Test ti ended; its window stays open until the next is staged.
		if timing == btNextWindow && ti+1 < len(tests) {
			stage(ti + 1)
		}
		for _, c := range between[ti] {
			run("between", c)
		}
	}
	t.Logf("%d calls; answers sha256 %x; answered with another request's recording: %d start-up, %d in a test, %d between tests (and, under the strict mock window, not held: %d between tests, %d in a test after a between-tests call took one of its recordings); test recordings answering twice: %d; outside-test recordings answering again: %d (and %d under the strict mock window, %d by the stated limit, not held); start-up and between-tests calls answered with another recording of their request: %d (and %d by the stated limit of the still-open timing, not held)",
		calls, digest.Sum(nil), wrong["startup"], wrong["test"], wrong["between"], wrong["unheld between"], wrong["unheld test"], wrong["twice"], wrong["reused"], wrong["unheld reused"], wrong["limit reused"], wrong["other gap"], wrong["limit"])
	if dir := os.Getenv("KEPLOY_MYSQL_BETWEEN_ANSWERS"); dir != "" {
		if err := os.WriteFile(fmt.Sprintf("%s/n%d-engine%v-%s.txt", dir, n, engine, name), []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
