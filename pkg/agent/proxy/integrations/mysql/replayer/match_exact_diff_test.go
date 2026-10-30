package replayer_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// setStatements and setTexts are what a recorded set runs: statements 1 and 2
// prepared on every connection, and three text queries.
var (
	setStatements = map[uint32]string{1: poolQuery, 2: diffQueryB}
	setTexts      = []string{
		"SELECT COUNT(*) FROM small",
		"SELECT id FROM small ORDER BY id LIMIT 3",
		"SELECT name FROM small WHERE id = 7",
	}
)

// setCommand is one recorded command of a set: what the connection sent,
// and the recording that answered it.
type setCommand struct {
	conn string
	req  mysql.Request
	// key says what the request was (statement and parameter, or the
	// text), the same for two recordings of one request.
	key  string
	mock *models.Mock
}

// setTestCalls is how many commands each test of a recorded set made.
const setTestCalls = 10

// recordedSet is a recording of n commands on conns connections, made by
// tests of setTestCalls commands each, test i's inside its window
// (diffWindow(i)). Each connection prepares statements 1 and 2 first; then
// each command is, at random, an EXECUTE of either with a parameter from a
// range of n/8 (so many requests are recorded several times, by one test or
// by several, on one connection or on several) or one of the text queries.
// Every command is its test's own (per-test) and answers with its own name.
func recordedSet(n, conns int, seed int64) (preps []*models.Mock, tests [][]setCommand) {
	r := rand.New(rand.NewSource(seed))
	for c := 1; c <= conns; c++ {
		conn := fmt.Sprintf("c%d", c)
		for id := uint32(1); id <= 2; id++ {
			at := diffBase.Add(-time.Millisecond + time.Duration(2*c+int(id))*time.Microsecond)
			preps = append(preps, diffPrepareMock(fmt.Sprintf("prep-%s-%d", conn, id), conn, setStatements[id], id, at))
		}
	}
	span := n/8 + 1
	for i := 0; i < n; i++ {
		t := i / setTestCalls
		if t == len(tests) {
			tests = append(tests, nil)
		}
		conn := fmt.Sprintf("c%d", 1+r.Intn(conns))
		var req mysql.Request
		var key string
		switch k := r.Intn(5); {
		case k < 4:
			stmt := uint32(1 + k%2)
			p := fmt.Sprint(1 + r.Intn(span))
			req, key = executeReq(stmt, p), fmt.Sprintf("exec-%d-%s", stmt, p)
		default:
			q := setTexts[r.Intn(len(setTexts))]
			req, key = queryReq(q), "query-"+q
		}
		start, _ := diffWindow(t)
		at := start.Add(time.Duration(1+i%setTestCalls) * 300 * time.Microsecond)
		mk := diffMock(fmt.Sprintf("m%05d-%s-%s", i, conn, key), conn, []mysql.Request{req}, at, models.LifetimePerTest, "mocks")
		tests[t] = append(tests[t], setCommand{conn: conn, req: req, key: key, mock: mk})
	}
	return preps, tests
}

func setMocks(tests [][]setCommand) []*models.Mock {
	var out []*models.Mock
	for _, cmds := range tests {
		for _, c := range cmds {
			out = append(out, c.mock)
		}
	}
	return out
}

// outsideStartCalls and outsideBetweenCalls are how many commands the app of
// a recorded set made at start-up, and after each test of outsideTests
// before the next.
const outsideStartCalls, outsideBetweenCalls = 8, 2

// outsideTests are the tests of a set after which its app made commands
// before the next test began: every tenth test a replay runs. Each such
// command reads, under exact serving, every recording the store holds (the
// pool view's read filters the whole staged set), so there are 10 of them in
// the 100 set and 28 in the larger sets, which keeps the replayer package
// under -race well inside go test's ten minutes.
func outsideTests(tests int) []int {
	replayed := replayedTests(tests)
	var out []int
	for i := 0; i < len(replayed); i += 10 {
		out = append(out, replayed[i])
	}
	return out
}

// outsideCalls are the commands a recorded set's app made outside every
// test: at start-up, before the first test's window, and after test i's
// window, before the next one's (after[i]).
type outsideCalls struct {
	startup []setCommand
	after   map[int][]setCommand
}

// recordedOutside is a recording, beside recordedSet's tests, of the
// commands its app made on conns connections outside every test:
// outsideStartCalls at start-up and outsideBetweenCalls after each test of
// between (outsideTests). Each is, at random, an EXECUTE of statement 1 or 2 with a
// parameter from a range of 3 (so the recordings outside every test hold
// each statement with several parameters, and some requests several times)
// or one of the text queries. They are per-test recordings, as recorded,
// that no test owns: exact serving shows them in its pool view, while no
// test is open.
func recordedOutside(conns int, between []int, seed int64) *outsideCalls {
	r := rand.New(rand.NewSource(seed))
	cmd := func(name string, at time.Time) setCommand {
		conn := fmt.Sprintf("c%d", 1+r.Intn(conns))
		var req mysql.Request
		var key string
		if k := r.Intn(5); k < 4 {
			stmt := uint32(1 + k%2)
			p := fmt.Sprint(1 + r.Intn(3))
			req, key = executeReq(stmt, p), fmt.Sprintf("exec-%d-%s", stmt, p)
		} else {
			q := setTexts[r.Intn(len(setTexts))]
			req, key = queryReq(q), "query-"+q
		}
		mk := diffMock(fmt.Sprintf("%s-%s-%s", name, conn, key), conn, []mysql.Request{req}, at, models.LifetimePerTest, "mocks")
		return setCommand{conn: conn, req: req, key: key, mock: mk}
	}
	o := &outsideCalls{after: map[int][]setCommand{}}
	for i := 0; i < outsideStartCalls; i++ {
		// After the PREPAREs, before the first window.
		o.startup = append(o.startup, cmd(fmt.Sprintf("o-start-%02d", i), diffBase.Add(-time.Millisecond+time.Duration(100+50*i)*time.Microsecond)))
	}
	for _, ti := range between {
		_, end := diffWindow(ti)
		for i := 0; i < outsideBetweenCalls; i++ {
			o.after[ti] = append(o.after[ti], cmd(fmt.Sprintf("o-after-%05d-%d", ti, i), end.Add(time.Duration(1+i)*time.Millisecond)))
		}
	}
	return o
}

// commands are o's commands, in recorded order.
func (o *outsideCalls) commands() []setCommand {
	out := append([]setCommand{}, o.startup...)
	tests := make([]int, 0, len(o.after))
	for ti := range o.after {
		tests = append(tests, ti)
	}
	sort.Ints(tests)
	for _, ti := range tests {
		out = append(out, o.after[ti]...)
	}
	return out
}

// names are the names of o's recordings: the pool view exact serving shows
// while no test is open.
func (o *outsideCalls) names() []string {
	var out []string
	for _, c := range o.commands() {
		out = append(out, c.mock.Name)
	}
	return out
}

// setConn is connection conn of a replay of a recorded set, its statements
// prepared as recorded.
func setConn(conn string) (context.Context, *wire.DecodeContext) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, conn)
	stmts := map[uint32]string{}
	for id, sql := range setStatements {
		stmts[id] = sql
	}
	return ctx, &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      stmts,
		NextStmtID:         100,
	}
}

// setReplay is one replay of a recorded set against one store: as keploy
// test serves it (each test's window holds its recording, staged as the agent
// stages it, diffSide) or under exact serving (each test's mapping is its
// recording).
type setReplay struct {
	exact bool
	eng   *schemanoise.Engine
	side  *diffSide
	mm    *proxy.MockManager
	dctxs map[string]*wire.DecodeContext
	ctxs  map[string]context.Context
}

func newSetReplay(t *testing.T, preps []*models.Mock, tests [][]setCommand, outside *outsideCalls, exact, strict bool) *setReplay {
	pool := append([]*models.Mock{}, preps...)
	for _, c := range outside.startup {
		pool = append(pool, c.mock)
	}
	for i, cmds := range tests {
		for _, c := range append(append([]setCommand{}, cmds...), outside.after[i]...) {
			pool = append(pool, c.mock)
		}
	}
	s := &setReplay{exact: exact, dctxs: map[string]*wire.DecodeContext{}, ctxs: map[string]context.Context{}}
	if exact {
		s.mm = proxy.NewMockManager(nil, nil, zap.NewNop())
		s.mm.SetMocksWithWindow(nil, pool, models.BaseTime, time.Now().Add(time.Hour))
	} else {
		s.side = newDiffSide(pool)
		s.mm = s.side.mm
	}
	t.Cleanup(s.mm.Close)
	if strict {
		s.eng = replayer.StrictEngineForTest()
	}
	for c := 1; c <= 8; c++ {
		conn := fmt.Sprintf("c%d", c)
		s.ctxs[conn], s.dctxs[conn] = setConn(conn)
	}
	return s
}

// open opens test i of tests.
func (s *setReplay) open(tests [][]setCommand, i int) {
	if s.exact {
		var owned []*models.Mock
		for _, c := range tests[i] {
			owned = append(owned, c.mock)
		}
		s.mm.ServeExactTest(fmt.Sprintf("T%d", i), owned)
		return
	}
	s.side.stage(diffWindow(i))
}

// openPool serves, under exact serving, the pool view: no test open, the
// recordings made outside every test shown.
func (s *setReplay) openPool(outside *outsideCalls) {
	s.mm.ServeExactPool("pool", outside.names())
}

func (s *setReplay) run(c setCommand) (*mysql.Response, error) {
	resp, ok, miss, err := replayer.MatchWithEngineForTest(s.ctxs[c.conn], zap.NewNop(), c.req, s.mm, s.dctxs[c.conn], s.eng)
	if err == nil && !ok {
		err = fmt.Errorf("no match: %s", miss)
	}
	return resp, err
}

// setOrders are the orders a test's commands are replayed in: as recorded,
// and reversed, so each command finds, recorded before its own, recordings
// of the statement with other parameters, and of the same request.
var setOrders = []string{"recorded", "reversed"}

// setRuns are the replays of a set: in each order, with and without strict
// enforcement.
//
// The replays run side by side (t.Parallel), each against stores of its own.
// Every command of a replay under exact serving reads the whole staged set
// (the view filters it), so one at a time the 10k replays alone took most of
// go test's ten minutes under -race.
var setRuns = []struct {
	order  string
	strict bool
}{{"recorded", false}, {"reversed", false}, {"recorded", true}, {"reversed", true}}

func ordered(cmds []setCommand, order string) []setCommand {
	if order == "recorded" {
		return cmds
	}
	out := make([]setCommand, len(cmds))
	for i, c := range cmds {
		out[len(cmds)-1-i] = c
	}
	return out
}

// replayedTests are the tests of a set a replay runs: every one of a set of
// up to 100 tests, and of a larger set 100 spread across it (the pool each
// command reads is the whole set either way).
func replayedTests(tests int) []int {
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

// TestExactServingAnswersARecordedSetAsKeployTestDoes replays recorded sets of
// 100, 1k and 10k commands (10k only without -short and -race, see
// raceEnabled) twice, as keploy test serves them (the path this
// change leaves as it was) and under exact serving, and requires both,
// command by command, to answer with the same recording, byte for byte; that
// recording to be one of the command's own request; and no recording to
// answer twice. Under exact serving the EXECUTE ranking read the window,
// which exact serving never opens: an EXECUTE whose statement its test
// recorded earlier with other parameters was answered with those parameters'
// rows.
//
// The set's app also made commands outside every test (recordedOutside: at
// start-up and between tests), which exact serving answers from its pool
// view, where no recording is a test's own: each is answered with a
// recording of its own request, none twice, and the recordings the tests
// made answer them never. There the first recording of the statement with
// other parameters outranked the one recorded with the command's own. keploy
// test, which answers them with its own ranking, runs the tests' commands
// only, with those recordings staged.
//
// The keploy test answers' digest is logged, so runs of this test on two
// builds show the switch-off answers byte-identical between them.
func TestExactServingAnswersARecordedSetAsKeployTestDoes(t *testing.T) {
	sizes := []int{100, 1000, 10000}
	if testing.Short() || raceEnabled {
		sizes = sizes[:2]
	}
	for _, n := range sizes {
		for _, run := range setRuns {
			t.Run(fmt.Sprintf("n=%d/%s/strict=%v", n, run.order, run.strict), func(t *testing.T) {
				// Each run builds its own stores and shares nothing with the
				// others, so they run side by side (see setRuns).
				t.Parallel()
				order := run.order
				preps, tests := recordedSet(n, 4, int64(n))
				outside := recordedOutside(4, outsideTests(len(tests)), int64(n))
				byName := map[string]setCommand{}
				for _, c := range append(setMocksCommands(tests), outside.commands()...) {
					byName[c.mock.Name] = c
				}
				ref := newSetReplay(t, preps, tests, outside, false, run.strict)
				exact := newSetReplay(t, preps, tests, outside, true, run.strict)
				digest := sha256.New()
				served := map[string]int{}
				commands, wrong := 0, 0
				outsideRun, outsideWrong := 0, 0
				runOutside := func(where string, cmds []setCommand) {
					exact.openPool(outside)
					for i, c := range ordered(cmds, order) {
						outsideRun++
						got, err := exact.run(c)
						if err != nil {
							t.Fatalf("%s command %d (%s on %s): exact serving: %v", where, i, c.key, c.conn, err)
						}
						if own := byName[got.Payload]; own.key != c.key || !strings.HasPrefix(got.Payload, "o-") {
							if outsideWrong++; outsideWrong <= 5 {
								t.Errorf("%s command %d (%s on %s) was answered with %s, the recording of %s; want one made outside every test of its own request", where, i, c.key, c.conn, got.Payload, own.key)
							}
							continue
						}
						if served[got.Payload]++; served[got.Payload] > 1 {
							t.Fatalf("recording %s answered a second command", got.Payload)
						}
					}
				}
				runOutside("start-up", outside.startup)
				for _, ti := range replayedTests(len(tests)) {
					ref.open(tests, ti)
					exact.open(tests, ti)
					for i, c := range ordered(tests[ti], order) {
						commands++
						want, werr := ref.run(c)
						got, gerr := exact.run(c)
						if werr != nil {
							t.Fatalf("test %d command %d (%s on %s): keploy test: %v", ti, i, c.key, c.conn, werr)
						}
						b, _ := json.Marshal(want)
						digest.Write(b)
						if gerr != nil {
							t.Fatalf("test %d command %d (%s on %s): exact serving: %v (keploy test answered %s)", ti, i, c.key, c.conn, gerr, want.Payload)
						}
						if !reflect.DeepEqual(want, got) {
							if wrong++; wrong <= 5 {
								t.Errorf("test %d command %d (%s on %s): exact serving answered %s; keploy test %s", ti, i, c.key, c.conn, got.Payload, want.Payload)
							}
							continue
						}
						if own := byName[got.Payload]; own.key != c.key {
							t.Fatalf("test %d command %d (%s on %s) was answered with %s, the recording of %s", ti, i, c.key, c.conn, got.Payload, own.key)
						}
						if served[got.Payload]++; served[got.Payload] > 1 {
							t.Fatalf("recording %s answered a second command", got.Payload)
						}
					}
					runOutside(fmt.Sprintf("after test %d,", ti), outside.after[ti])
				}
				t.Logf("n=%d/%s/strict=%v: %d commands in tests, %d outside; keploy test answers sha256 %x", n, order, run.strict, commands, outsideRun, digest.Sum(nil))
				if wrong > 0 {
					t.Errorf("%d of %d commands answered differently under exact serving", wrong, commands)
				}
				if outsideWrong > 0 {
					t.Errorf("%d of %d commands outside every test answered with the recording of another request", outsideWrong, outsideRun)
				}
			})
		}
	}
}

func setMocksCommands(tests [][]setCommand) []setCommand {
	var out []setCommand
	for _, cmds := range tests {
		out = append(out, cmds...)
	}
	return out
}

// TestExactServingAnswersARecordedSetRunEightAtOnce replays recorded sets of
// 100, 1k and 10k commands (10k only without -short and -race, see
// raceEnabled) under exact serving on eight connections: in each
// test, each connection's commands in recorded order, the eight connections'
// next commands at once (each reads the pool before any consumes), and so
// the commands made outside every test (recordedOutside), from the pool view.
// Every command is answered with a recording of its own request, one made
// outside every test for a command made there, and no recording answers
// twice. Which of two recordings of one request answers which command is the
// order of the consumes, not compared.
func TestExactServingAnswersARecordedSetRunEightAtOnce(t *testing.T) {
	const conns = 8
	sizes := []int{100, 1000, 10000}
	if testing.Short() || raceEnabled {
		sizes = sizes[:2]
	}
	for _, n := range sizes {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("n=%d/strict=%v", n, strict), func(t *testing.T) {
				t.Parallel()
				eightAtOnce(t, n, conns, strict)
			})
		}
	}
}

func eightAtOnce(t *testing.T, n, conns int, strict bool) {
	preps, tests := recordedSet(n, conns, int64(n))
	outside := recordedOutside(conns, outsideTests(len(tests)), int64(n))
	byName := map[string]setCommand{}
	for _, c := range append(setMocksCommands(tests), outside.commands()...) {
		byName[c.mock.Name] = c
	}
	s := newSetReplay(t, preps, tests, outside, true, strict)
	served := map[string]int{}
	commands, outsideRun, outsideWrong := 0, 0, 0
	// runRounds runs cmds, each connection's in recorded order, the
	// connections' next commands at once.
	runRounds := func(where string, cmds []setCommand, isOutside bool) {
		perConn := map[string][]setCommand{}
		for _, c := range cmds {
			perConn[c.conn] = append(perConn[c.conn], c)
		}
		for round := 0; ; round++ {
			var batch []setCommand
			for c := 1; c <= conns; c++ {
				if q := perConn[fmt.Sprintf("c%d", c)]; round < len(q) {
					batch = append(batch, q[round])
				}
			}
			if len(batch) == 0 {
				break
			}
			for i, a := range runSetAtOnce(s, batch) {
				c := batch[i]
				if a.err != nil || !a.ok {
					t.Fatalf("%s round %d, %s on %s: ok=%v err=%v miss=%s", where, round, c.key, c.conn, a.ok, a.err, a.miss)
				}
				own := byName[a.payload]
				if isOutside {
					outsideRun++
					if own.key != c.key || !strings.HasPrefix(a.payload, "o-") {
						if outsideWrong++; outsideWrong <= 5 {
							t.Errorf("%s round %d, %s on %s was answered with %s, the recording of %s; want one made outside every test of its own request", where, round, c.key, c.conn, a.payload, own.key)
						}
						continue
					}
				} else {
					commands++
					if own.key != c.key {
						t.Fatalf("%s round %d, %s on %s was answered with %s, the recording of %s", where, round, c.key, c.conn, a.payload, own.key)
					}
				}
				if served[a.payload]++; served[a.payload] > 1 {
					t.Fatalf("recording %s answered a second command", a.payload)
				}
			}
		}
	}
	s.openPool(outside)
	runRounds("start-up", outside.startup, true)
	for _, ti := range replayedTests(len(tests)) {
		s.open(tests, ti)
		runRounds(fmt.Sprintf("test %d", ti), tests[ti], false)
		s.openPool(outside)
		runRounds(fmt.Sprintf("after test %d,", ti), outside.after[ti], true)
	}
	t.Logf("%d commands in tests, %d outside", commands, outsideRun)
	if outsideWrong > 0 {
		t.Errorf("%d of %d commands outside every test answered with the recording of another request", outsideWrong, outsideRun)
	}
}

// runSetAtOnce runs each of batch's commands on its connection, all at once:
// each reads the pool before any consumes.
func runSetAtOnce(s *setReplay, batch []setCommand) []poolAnswer {
	return atOnce(s.mm, len(batch), func(i int, db integrations.MockMemDb) (*mysql.Response, bool, string, error) {
		c := batch[i]
		return replayer.MatchWithEngineForTest(s.ctxs[c.conn], zap.NewNop(), c.req, db, s.dctxs[c.conn], s.eng)
	})
}
