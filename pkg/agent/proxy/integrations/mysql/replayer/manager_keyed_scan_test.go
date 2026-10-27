package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The tests in this file cover a command matched while no test window is
// active: under exact serving, whose store reports none, and in keploy test
// before a set's first test. A COM_QUERY or COM_STMT_EXECUTE then reads, of
// the session tier, only the mocks preparedIndex files under its statement.

// keyedSetText is the text query of mock i of keyedSet: a key read of one of
// 500 rows.
func keyedSetText(i int) string {
	return fmt.Sprintf("SELECT id, name, payload FROM small WHERE id = %d", i%500+1)
}

// keyedSet is a recording shaped like a key-value workload, n mocks: text
// queries, and EXECUTEs of diffQueryA and diffQueryB (live statements 7 and
// 8 of diffSide) that kept their query, diffQueryB's in lower case as some
// clients send it. lifetime is how replay classified the data mocks.
func keyedSet(n int, lifetime models.Lifetime) []*models.Mock {
	pool := make([]*models.Mock, 0, n)
	typ := "mocks"
	if lifetime == models.LifetimeSession {
		typ = "config"
	}
	for i := 0; i < n; i++ {
		ts := diffBase.Add(time.Duration(i) * time.Millisecond)
		var req mysql.Request
		switch i % 4 {
		case 0, 1:
			req = queryReq(keyedSetText(i))
		case 2:
			req = executeReq(7, fmt.Sprintf("k%d", i%500))
			req.PacketBundle.SetStoredQuery(diffQueryA)
		default:
			req = executeReq(8, fmt.Sprintf("k%d", i%500))
			req.PacketBundle.SetStoredQuery(strings.ToLower(diffQueryB))
		}
		pool = append(pool, diffMock(fmt.Sprintf("m-%d", i), "c1", []mysql.Request{req}, ts, lifetime, typ))
	}
	return pool
}

// stageNoWindow stages pool whole in the session tier, in recorded order,
// with no test window active.
func (s *diffSide) stageNoWindow() {
	copies := make([]*models.Mock, 0, len(s.pool))
	for _, m := range s.pool {
		if !s.consumed[m.Name] {
			copies = append(copies, m.DeepCopy())
		}
	}
	s.mm.SetMocksWithWindow(nil, copies, time.Time{}, time.Time{})
	s.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
}

// TestNoWindowSetsServeWhatTheFullScanServes replays sets of 100, 1,000 and
// 10,000 mocks with no test window, each command matched by the keyed pass on
// one side and by the full scan on the other, and requires the two to serve,
// consume and report the same at every command: recorded reads, reads of a
// row the set never recorded, and EXECUTEs whose parameters drifted.
func TestNoWindowSetsServeWhatTheFullScanServes(t *testing.T) {
	sizes := []int{100, 1000, 10000}
	if testing.Short() || raceEnabled {
		sizes = sizes[:2]
	}
	for _, n := range sizes {
		for _, strict := range []bool{false, true} {
			pool := keyedSet(n, models.LifetimePerTest)
			ref, fast := newDiffSide(pool), newDiffSide(pool)
			ref.stageNoWindow()
			fast.stageNoWindow()
			var eng *schemanoise.Engine
			if strict {
				eng = replayer.StrictEngineForTest()
			}
			step := n / 100
			for i := 0; i < n; i += step {
				var reqs []mysql.Request
				switch i % 4 {
				case 0, 1:
					reqs = []mysql.Request{queryReq(keyedSetText(i)), queryReq(keyedSetText(i) + " LIMIT 1")}
				case 2:
					reqs = []mysql.Request{executeReq(7, fmt.Sprintf("k%d", i%500)), executeReq(7, "never-recorded")}
				default:
					reqs = []mysql.Request{executeReq(8, fmt.Sprintf("k%d", i%500)), executeReq(8, "never-recorded")}
				}
				for _, req := range reqs {
					want := ref.match(context.Background(), req, eng, true)
					got := fast.match(context.Background(), req, eng, false)
					if want != got {
						t.Fatalf("set of %d strict=%v command for mock %d:\n full scan:  %s\n keyed pass: %s", n, strict, i, want, got)
					}
				}
			}
			ref.mm.Close()
			fast.mm.Close()
		}
	}
}

// TestExactServingServesWhatTheFullScanServes is the keyed pass under exact
// serving, as keploy mock replay serves a test: the set staged, each test of
// eight recordings opened in turn with those as its own, and its commands
// matched by the keyed pass on one side and by the full scan on the other.
func TestExactServingServesWhatTheFullScanServes(t *testing.T) {
	for _, strict := range []bool{false, true} {
		pool := keyedSet(1000, models.LifetimePerTest)
		ref, fast := newDiffSide(pool), newDiffSide(pool)
		ref.stageNoWindow()
		fast.stageNoWindow()
		var eng *schemanoise.Engine
		if strict {
			eng = replayer.StrictEngineForTest()
		}
		for test := 0; test*8 < len(pool); test += 7 {
			batch := pool[test*8 : test*8+8]
			for _, s := range []*diffSide{ref, fast} {
				owned := make([]*models.Mock, len(batch))
				for i, m := range batch {
					owned[i] = m.DeepCopy()
				}
				s.mm.ServeExactTest(fmt.Sprintf("test-%d", test), owned)
			}
			for k := range batch {
				i := test*8 + k
				var req mysql.Request
				switch i % 4 {
				case 0, 1:
					req = queryReq(keyedSetText(i))
				case 2:
					req = executeReq(7, fmt.Sprintf("k%d", i%500))
				default:
					req = executeReq(8, fmt.Sprintf("k%d", i%500))
				}
				want := ref.match(context.Background(), req, eng, true)
				got := fast.match(context.Background(), req, eng, false)
				if want != got {
					t.Fatalf("strict=%v test %d command for mock %d:\n full scan:  %s\n keyed pass: %s", strict, test, i, want, got)
				}
				if test == 0 && k == 0 && !strings.Contains(want, "ok=true") {
					t.Fatalf("the test's own recording was not served: %s", want)
				}
			}
		}
		ref.mm.Close()
		fast.mm.Close()
	}
}

// TestNoWindowCommandReadsOnlyItsStatement bounds the candidates a command
// visits with no test window active: a COM_QUERY the first mock of its
// statement, a COM_STMT_EXECUTE at most the mocks of its own statement (fewer
// where the scan stops at the command's own recording). The full scan visited
// the whole set for each EXECUTE, and for a COM_QUERY every mock recorded
// before its statement's first.
//
// The EXECUTE is settled by its statement's candidates whether they hold a
// FIFO one (the statement recorded with other parameters) or only definitive
// ones (every recording of the statement made with the live parameters, as
// when all its calls bind the same values, or one recording is left): either
// outranks every candidate of another statement.
func TestNoWindowCommandReadsOnlyItsStatement(t *testing.T) {
	const n = 1000
	pool := keyedSet(n, models.LifetimePerTest)
	// sameParams is pool with every EXECUTE of diffQueryB bound to "k1".
	sameParams := append([]*models.Mock(nil), pool...)
	ofB := 0
	for i := range pool {
		if i%4 != 3 {
			continue
		}
		ofB++
		req := executeReq(8, "k1")
		req.PacketBundle.SetStoredQuery(strings.ToLower(diffQueryB))
		sameParams[i] = diffMock(pool[i].Name, "c1", []mysql.Request{req}, diffBase.Add(time.Duration(i)*time.Millisecond), models.LifetimePerTest, "mocks")
	}
	for _, tc := range []struct {
		name string
		pool []*models.Mock
		req  mysql.Request
		max  int
	}{
		// The last text query whose statement the set records once.
		{"COM_QUERY", pool, queryReq(keyedSetText(n - 3)), 1},
		{"COM_STMT_EXECUTE", pool, executeReq(8, fmt.Sprintf("k%d", (n-1)%500)), ofB},
		{"COM_STMT_EXECUTE, every candidate of its parameters", sameParams, executeReq(8, "k1"), ofB},
		// Parameters no recording bound (a value the app made at replay):
		// only FIFO candidates, and they settle it too.
		{"COM_STMT_EXECUTE, parameters no recording binds", pool, executeReq(8, "no-such-key"), ofB},
	} {
		s := newDiffSide(tc.pool)
		s.stageNoWindow()
		resp, ok, examined, err := replayer.MatchCommandExaminedForTest(context.Background(), tc.req, s.mm, s.dctx, nil, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("%s: ok=%v err=%v", tc.name, ok, err)
		}
		if examined < 1 || examined > tc.max {
			t.Errorf("%s: visited %d candidates of %d, want 1 to %d", tc.name, examined, n, tc.max)
		}
		s.mm.Close()
	}
}

// TestNoWindowCommandAllocationsDoNotGrowWithOtherStatements: a command's
// allocations with no test window active do not grow with the mocks of other
// statements the set holds. Comparing each of them allocated.
func TestNoWindowCommandAllocationsDoNotGrowWithOtherStatements(t *testing.T) {
	allocs := func(others int, req mysql.Request) float64 {
		// 20 reusable mocks of the live statement, then others of others.
		own := keyedSet(80, models.LifetimeSession)
		pool := append([]*models.Mock(nil), own...)
		for i, m := range keyedSet(others, models.LifetimeSession) {
			if i%4 == 3 {
				continue // diffQueryB's
			}
			m.Name = "other-" + m.Name
			if r := &m.Spec.MySQLRequests[0]; r.Header.Type == "COM_QUERY" {
				r.Message = &mysql.QueryPacket{Query: keyedSetText(i) + " AND 1=1"}
			}
			pool = append(pool, m)
		}
		s := newDiffSide(pool)
		s.stageNoWindow()
		defer s.mm.Close()
		return testing.AllocsPerRun(20, func() {
			if _, ok, _, err := replayer.MatchCommandExaminedForTest(context.Background(), req, s.mm, s.dctx, nil, false); !ok || err != nil {
				t.Fatalf("not served: ok=%v err=%v", ok, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		req  mysql.Request
	}{
		{"COM_QUERY", queryReq(keyedSetText(77))},
		{"COM_STMT_EXECUTE", executeReq(8, "k3")},
	} {
		small, large := allocs(200, tc.req), allocs(2000, tc.req)
		if large > small+3 {
			t.Errorf("%s: %v allocations per command beside 150 mocks of other statements, %v beside 1,500", tc.name, small, large)
		}
	}
}

// TestExactServedCommandReadsNoTierOfAOneCommandCandidate counts the reads of
// the config tag (a map lookup on a mock's metadata) by commands served under
// exact serving while the open test owns a whole set, as keploy mock replay
// serves one go test whose subtests made the recording. A COM_QUERY and an
// EXECUTE of each recorded query read the tag only of the one owned mock that
// is not a single COM_* command, so the count is the same for 100 owned mocks
// as for 1,000. Main read it for every owned mock on every command, twice for
// an EXECUTE (its index keys, then the scan's filter): 18% of the agent's CPU
// at 10,000 mocks.
func TestExactServedCommandReadsNoTierOfAOneCommandCandidate(t *testing.T) {
	readsFor := func(n int) []int {
		// First, a bundle of two requests tagged config, which every command's
		// filter must classify (and skip): the count is not zero by a hook
		// never called.
		pool := append([]*models.Mock{diffMock("bundle", "c1", []mysql.Request{queryReq("SET NAMES utf8mb4"), queryReq("SELECT @@version")},
			diffBase.Add(-time.Millisecond), models.LifetimePerTest, "config")}, keyedSet(n, models.LifetimePerTest)...)
		s := newDiffSide(pool)
		defer s.mm.Close()
		s.stageNoWindow()
		owned := make([]*models.Mock, len(pool))
		for i, m := range pool {
			owned[i] = m.DeepCopy()
		}
		s.mm.ServeExactTest("whole-set", owned)
		var reads []int
		for _, req := range []mysql.Request{
			queryReq(keyedSetText(n - 3)),
			executeReq(7, fmt.Sprintf("k%d", (n-2)%500)),
			executeReq(8, fmt.Sprintf("k%d", (n-1)%500)),
		} {
			var resp *mysql.Response
			var ok bool
			var err error
			reads = append(reads, replayer.ConfigTagReadsForTest(func() {
				resp, ok, _, err = replayer.MatchCommandExaminedForTest(context.Background(), req, s.mm, s.dctx, nil, false)
			}))
			if err != nil || !ok || resp == nil {
				t.Fatalf("set of %d: %s not served: ok=%v err=%v", n, req.Header.Type, ok, err)
			}
		}
		return reads
	}
	small, large := readsFor(100), readsFor(1000)
	for i, name := range []string{"COM_QUERY", "EXECUTE of the upper-case query", "EXECUTE of the lower-case query"} {
		if small[i] == 0 {
			t.Errorf("%s: no config tag read, though the owned set holds a bundle the filter must classify", name)
		}
		if large[i] != small[i] {
			t.Errorf("%s: %d config tag reads with 1,000 owned mocks, %d with 100: the reads grow with the mocks the test owns", name, large[i], small[i])
		}
	}
}

// ownWholeSet opens one test owning every mock of s's pool, as keploy mock
// replay serves a go test whose run recorded the whole set.
func (s *diffSide) ownWholeSet() {
	owned := make([]*models.Mock, len(s.pool))
	for i, m := range s.pool {
		owned[i] = m.DeepCopy()
	}
	s.mm.ServeExactTest("whole-set", owned)
}

// TestExactServingWholeSetServesWhatTheFullScanServes: one test owns a set of
// 1,000 mocks, and each command is matched by the keyed pass on one side and
// by the full scan on the other: recorded reads, EXECUTEs whose parameters no
// recording bound (each settled by the test's own FIFO candidate) and reads
// of rows the set never recorded. Both serve, consume and report the same.
func TestExactServingWholeSetServesWhatTheFullScanServes(t *testing.T) {
	// Each kind of command 13 times under -race (see raceEnabled), 50 without.
	commands := 400
	if raceEnabled {
		commands = 104
	}
	for _, strict := range []bool{false, true} {
		pool := keyedSet(1000, models.LifetimePerTest)
		ref, fast := newDiffSide(pool), newDiffSide(pool)
		var eng *schemanoise.Engine
		if strict {
			eng = replayer.StrictEngineForTest()
		}
		for _, s := range []*diffSide{ref, fast} {
			s.stageNoWindow()
			s.ownWholeSet()
		}
		for i := 0; i < commands; i++ {
			var req mysql.Request
			switch i % 8 {
			case 0:
				req = queryReq(keyedSetText(i))
			case 1:
				req = queryReq("SELECT id, name, payload FROM small WHERE id = 9999")
			case 2, 4:
				req = executeReq(7, fmt.Sprintf("k%d", i%500))
			case 3:
				req = executeReq(8, fmt.Sprintf("k%d", i%500))
			default:
				req = executeReq(uint32(7+i%2), fmt.Sprintf("app-made-%d", i))
			}
			want := ref.match(context.Background(), req, eng, true)
			got := fast.match(context.Background(), req, eng, false)
			if want != got {
				t.Fatalf("strict=%v command %d:\n full scan:  %s\n keyed pass: %s", strict, i, want, got)
			}
			if i == 5 && !strict && !strings.Contains(want, "ok=true") {
				t.Fatalf("the FIFO candidate was not served: %s", want)
			}
		}
		ref.mm.Close()
		fast.mm.Close()
	}
}

// TestExactServingOwnedSetCommandCosts bounds what a command costs under exact
// serving while one test owns a whole set of 1,000 or 10,000 mocks:
//
//   - an EXECUTE visits at most the candidates the full scan visits;
//   - an EXECUTE whose parameters no recording bound is settled by the test's
//     own FIFO candidate: it visits the test's mocks and its statement's,
//     and the full scan does not run on top;
//   - after the test's first command, a command computes no mock's
//     preparedIndex keys: the view's index is built once. Each EXECUTE
//     computed the keys of every mock the test had not consumed, twice.
func TestExactServingOwnedSetCommandCosts(t *testing.T) {
	sizes := []int{1000, 10000}
	if testing.Short() || raceEnabled {
		sizes = sizes[:1]
	}
	for _, n := range sizes {
		ofB := n / 4 // the set's EXECUTEs of diffQueryB
		for _, tc := range []struct {
			name string
			req  mysql.Request
			max  int // visits at most, besides the full scan's
		}{
			{"EXECUTE, parameters no recording binds", executeReq(8, "no-such-key"), n + ofB},
			{"EXECUTE, its own recording at pool index 3", executeReq(8, "k3"), 4},
			{"EXECUTE, its own recording at pool index 499", executeReq(8, "k499"), 500},
		} {
			var visits [2]int
			var served [2]string
			for k, full := range []bool{false, true} {
				s := newDiffSide(keyedSet(n, models.LifetimePerTest))
				s.stageNoWindow()
				s.ownWholeSet()
				resp, ok, examined, err := replayer.MatchCommandExaminedForTest(context.Background(), tc.req, s.mm, s.dctx, nil, full)
				if err != nil || !ok || resp == nil {
					t.Fatalf("n=%d %s (full scan %v): ok=%v err=%v", n, tc.name, full, ok, err)
				}
				visits[k], served[k] = examined, s.describe(resp, ok, "", err)
				s.mm.Close()
			}
			if served[0] != served[1] {
				t.Fatalf("n=%d %s: keyed pass served %s, full scan %s", n, tc.name, served[0], served[1])
			}
			if visits[0] > visits[1] {
				t.Errorf("n=%d %s: keyed pass visited %d candidates, more than the full scan's %d", n, tc.name, visits[0], visits[1])
			}
			if visits[0] > tc.max {
				t.Errorf("n=%d %s: keyed pass visited %d candidates, want at most %d", n, tc.name, visits[0], tc.max)
			}
		}

		s := newDiffSide(keyedSet(n, models.LifetimePerTest))
		s.stageNoWindow()
		s.ownWholeSet()
		match := func(req mysql.Request) {
			if _, ok, _, err := replayer.MatchCommandExaminedForTest(context.Background(), req, s.mm, s.dctx, nil, false); err != nil || !ok {
				t.Fatalf("n=%d: ok=%v err=%v", n, ok, err)
			}
		}
		match(executeReq(8, "k1")) // the test's first command builds the indexes
		computed := replayer.CountPreparedKeysForTest(func() {
			for i := 0; i < 6; i++ {
				match(executeReq(8, "no-such-key"))
				match(executeReq(7, fmt.Sprintf("k%d", 2+4*i)))
				match(queryReq(keyedSetText(4 * i)))
			}
		})
		if computed != 0 {
			t.Errorf("n=%d: 18 commands after the test's first computed %d mocks' keys, want 0", n, computed)
		}
		s.mm.Close()
	}
}

// TestNoWindowExecuteServesWhatTheFullScanServesAcrossTheStatementIndex: two
// cases the keyed pass of a COM_STMT_EXECUTE settles from outside the mocks
// filed under the live query, each served on one side by the keyed pass and
// on the other by the full scan, which must serve the same recording.
//
//   - An EXECUTE recorded without its query (an older recorder's; its query
//     is its PREPARE's in the pool) is filed under no query. With one in the
//     session tier, before an EXECUTE of the same query recorded with it, the
//     full scan serves the first of them, as recorded: the unnamed one. The
//     keyed pass must leave such a command to the full scan.
//   - Under exact serving between tests, the pool holds the live query's
//     recording with other parameters (a FIFO candidate) before its recording
//     with the live parameters (a definitive one). Under exact serving the
//     definitive one ranks first, so the keyed pass must read past the FIFO
//     candidate.
func TestNoWindowExecuteServesWhatTheFullScanServesAcrossTheStatementIndex(t *testing.T) {
	at := func(ms int) time.Time { return diffBase.Add(time.Duration(ms) * time.Millisecond) }
	named := func(name, param string, ms int) *models.Mock {
		req := executeReq(7, param)
		req.PacketBundle.SetStoredQuery(diffQueryA)
		return diffMock(name, "c1", []mysql.Request{req}, at(ms), models.LifetimeSession, "mocks")
	}
	for _, c := range []struct {
		name  string
		pool  []*models.Mock
		exact bool
		live  mysql.Request
		want  string
	}{{
		name: "an EXECUTE recorded without its query, first in the session tier",
		pool: []*models.Mock{
			diffPrepareMock("prepare", "c1", diffQueryA, 7, at(0)),
			diffMock("unnamed", "c1", []mysql.Request{executeReq(7, "k1")}, at(1), models.LifetimeSession, "mocks"),
			named("named", "k2", 2),
		},
		live: executeReq(7, "app-made"),
		want: "served unnamed ",
	}, {
		name:  "under exact serving between tests, a FIFO candidate before the definitive one",
		pool:  []*models.Mock{named("fifo", "k2", 1), named("definitive", "k1", 2)},
		exact: true,
		live:  executeReq(7, "k1"),
		want:  "served definitive ",
	}} {
		ref, fast := newDiffSide(c.pool), newDiffSide(c.pool)
		for _, s := range []*diffSide{ref, fast} {
			s.stageNoWindow()
			if c.exact {
				names := make([]string, 0, len(c.pool))
				for _, m := range c.pool {
					names = append(names, m.Name)
				}
				s.mm.ServeExactPool("-", names)
			}
		}
		want := ref.match(context.Background(), c.live, nil, true)
		got := fast.match(context.Background(), c.live, nil, false)
		if !strings.HasPrefix(want, c.want) {
			t.Fatalf("%s: the full scan %s, want %q", c.name, want, c.want)
		}
		if got != want {
			t.Fatalf("%s:\n full scan:  %s\n keyed pass: %s", c.name, want, got)
		}
		ref.mm.Close()
		fast.mm.Close()
	}
}
