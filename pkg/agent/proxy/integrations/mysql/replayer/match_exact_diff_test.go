package replayer_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
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

func newSetReplay(t *testing.T, preps []*models.Mock, tests [][]setCommand, exact, strict bool) *setReplay {
	pool := append(append([]*models.Mock{}, preps...), setMocks(tests)...)
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
// 100, 1k and 10k commands twice, as keploy test serves them (the path this
// change leaves as it was) and under exact serving, and requires both,
// command by command, to answer with the same recording, byte for byte; that
// recording to be one of the command's own request; and no recording to
// answer twice. Under exact serving the EXECUTE ranking read the window,
// which exact serving never opens: an EXECUTE whose statement its test
// recorded earlier with other parameters was answered with those parameters'
// rows.
//
// The keploy test answers' digest is logged, so runs of this test on two
// builds show the switch-off answers byte-identical between them.
func TestExactServingAnswersARecordedSetAsKeployTestDoes(t *testing.T) {
	for _, n := range []int{100, 1000, 10000} {
		for _, run := range setRuns {
			t.Run(fmt.Sprintf("n=%d/%s/strict=%v", n, run.order, run.strict), func(t *testing.T) {
				order := run.order
				preps, tests := recordedSet(n, 4, int64(n))
				byName := map[string]setCommand{}
				for _, c := range setMocksCommands(tests) {
					byName[c.mock.Name] = c
				}
				ref := newSetReplay(t, preps, tests, false, run.strict)
				exact := newSetReplay(t, preps, tests, true, run.strict)
				digest := sha256.New()
				served := map[string]int{}
				commands, wrong := 0, 0
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
				}
				t.Logf("%d commands; keploy test answers sha256 %x", commands, digest.Sum(nil))
				if wrong > 0 {
					t.Fatalf("%d of %d commands answered differently under exact serving", wrong, commands)
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
// 100, 1k and 10k commands under exact serving on eight connections: in each
// test, each connection's commands in recorded order, the eight connections'
// next commands at once (each reads the pool before any consumes). Every
// command is answered with a recording of its own request, and no recording
// answers twice. Which of two recordings of one request answers which
// command is the order of the consumes, not compared.
func TestExactServingAnswersARecordedSetRunEightAtOnce(t *testing.T) {
	const conns = 8
	for _, n := range []int{100, 1000, 10000} {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("n=%d/strict=%v", n, strict), func(t *testing.T) {
				eightAtOnce(t, n, conns, strict)
			})
		}
	}
}

func eightAtOnce(t *testing.T, n, conns int, strict bool) {
	preps, tests := recordedSet(n, conns, int64(n))
	byName := map[string]setCommand{}
	for _, c := range setMocksCommands(tests) {
		byName[c.mock.Name] = c
	}
	s := newSetReplay(t, preps, tests, true, strict)
	served := map[string]int{}
	commands := 0
	for _, ti := range replayedTests(len(tests)) {
		s.open(tests, ti)
		perConn := map[string][]setCommand{}
		for _, c := range tests[ti] {
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
				commands++
				if a.err != nil || !a.ok {
					t.Fatalf("test %d round %d, %s on %s: ok=%v err=%v miss=%s", ti, round, c.key, c.conn, a.ok, a.err, a.miss)
				}
				if own := byName[a.payload]; own.key != c.key {
					t.Fatalf("test %d round %d, %s on %s was answered with %s, the recording of %s", ti, round, c.key, c.conn, a.payload, own.key)
				}
				if served[a.payload]++; served[a.payload] > 1 {
					t.Fatalf("recording %s answered a second command", a.payload)
				}
			}
		}
	}
	t.Logf("%d commands", commands)
}

// runSetAtOnce runs each of batch's commands on its connection, all at once:
// each reads the pool before any consumes.
func runSetAtOnce(s *setReplay, batch []setCommand) []poolAnswer {
	return atOnce(s.mm, len(batch), func(i int, db integrations.MockMemDb) (*mysql.Response, bool, string, error) {
		c := batch[i]
		return replayer.MatchWithEngineForTest(s.ctxs[c.conn], zap.NewNop(), c.req, db, s.dctxs[c.conn], s.eng)
	})
}
