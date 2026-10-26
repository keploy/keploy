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
// active: between tests, and under exact serving, whose store reports none. A
// COM_QUERY or COM_STMT_EXECUTE then reads, of the session tier, only the
// mocks preparedIndex files under its statement.

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
	if testing.Short() {
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

// TestNoWindowCommandReadsOnlyItsStatement counts the candidates a command
// visits with no test window active: a COM_QUERY the first mock of its
// statement, a COM_STMT_EXECUTE the mocks of its own statement. The full scan
// visited the whole set for each EXECUTE, and for a COM_QUERY every mock
// recorded before its statement's first.
func TestNoWindowCommandReadsOnlyItsStatement(t *testing.T) {
	const n = 1000
	pool := keyedSet(n, models.LifetimePerTest)
	ofB := 0
	for i := range pool {
		if i%4 == 3 {
			ofB++
		}
	}
	for _, tc := range []struct {
		name string
		req  mysql.Request
		want int
	}{
		// The last text query whose statement the set records once.
		{"COM_QUERY", queryReq(keyedSetText(n - 3)), 1},
		{"COM_STMT_EXECUTE", executeReq(8, fmt.Sprintf("k%d", (n-1)%500)), ofB},
	} {
		s := newDiffSide(pool)
		s.stageNoWindow()
		resp, ok, examined, err := replayer.MatchCommandExaminedForTest(context.Background(), tc.req, s.mm, s.dctx, nil, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("%s: ok=%v err=%v", tc.name, ok, err)
		}
		if examined != tc.want {
			t.Errorf("%s: visited %d candidates of %d, want %d", tc.name, examined, n, tc.want)
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
