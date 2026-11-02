package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// backgroundStaging is a set of three recordings of one exchange: the pool's
// (made at start-up), a worker's (made in test-0's window) and test-0's own.
// Under the background rules the pool and the worker are the set's
// background, and the run has reached the worker's place only when reached
// is set (ServeExactWorker). Under today's rules the worker scope is the
// reached worker only. No view is installed yet.
type backgroundStaging struct {
	mm                *MockManager
	pool, worker, own *models.Mock
}

func stageBackground(t *testing.T, bgRules, reached bool) backgroundStaging {
	t.Helper()
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	worker := ownRepeatExchange("mock-worker-ping", "PING", "WRKR", start.Add(50*time.Millisecond))
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{worker, own}, []*models.Mock{pool}, start, start.Add(900*time.Millisecond))
	if bgRules {
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{pool.Name}, []string{worker.Name})
	}
	if reached {
		mm.ServeExactWorker([]string{worker.Name})
	}
	s := backgroundStaging{mm: mm, pool: roundMock(mm, pool.Name), worker: roundMock(mm, worker.Name), own: roundMock(mm, own.Name)}
	if s.pool == nil || s.worker == nil || s.own == nil {
		t.Fatal("precondition: a recording was not staged")
	}
	return s
}

func (s backgroundStaging) betweenTests(t *testing.T) {
	t.Helper()
	s.mm.ServeExactPool(models.PoolKeyNone, []string{s.pool.Name})
	if !s.mm.ExactServing() {
		t.Fatal("precondition: the pool view is not served exactly")
	}
}

func (s backgroundStaging) openTest(t *testing.T) {
	t.Helper()
	s.mm.ServeExactTest("test-0", []*models.Mock{s.own})
	if !s.mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}
}

// The store's answer whether a recording is one of the background's
// (MockManager.InBackground) and a snapshot's of the same view
// (RoundSnapshot.InBackground) are one answer, in each view of a run: the
// HTTP learn step reads the store's on a live read (learnRules) and the
// snapshot's on a round read (learnScopeIn), and Postgres, Mongo and the
// enterprise parsers read the store's. Under the background rules the
// set's pool and worker scope are the background, before, in and after a
// test, whether or not the run has reached the worker's place. Under
// today's rules only the worker scope the run reached is, and only in a
// test: between tests it answers the calls the workers make. A test's own
// recording is never the background's.
//
// It pins two checks no other row pins: the snapshot's term for the
// worker scope of a set under the background rules (between tests, and in
// a test before the run reached its place, the term before it reads
// nothing), and the store's check that a test is open before it counts the
// reached worker scope under today's rules.
func TestTheStoreAndItsSnapshotTellTheBackgroundAlike(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type want struct{ pool, worker bool }
	cases := []struct {
		name             string
		bgRules, reached bool
		between, inTest  want
	}{
		{"background rules, worker not reached", true, false, want{true, true}, want{true, true}},
		{"background rules, worker reached", true, true, want{true, true}, want{true, true}},
		{"today's rules, worker reached", false, true, want{false, false}, want{false, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := stageBackground(t, c.bgRules, c.reached)
			check := func(phase string, w want) {
				t.Helper()
				snap, ok := s.mm.RoundSnapshot(integrations.RoundQuery{})
				if !ok {
					t.Fatalf("%s: precondition: the store gives no round snapshot", phase)
				}
				for _, r := range []struct {
					mk   *models.Mock
					want bool
				}{{s.pool, w.pool}, {s.worker, w.worker}, {s.own, false}} {
					if got := s.mm.InBackground(r.mk.Name); got != r.want {
						t.Errorf("%s: the store says %s is the background's: %v; want %v", phase, r.mk.Name, got, r.want)
					}
					if got := snap.InBackground(r.mk.Name); got != r.want {
						t.Errorf("%s: the snapshot (test %q) says %s is the background's: %v; want %v", phase, snap.Test, r.mk.Name, got, r.want)
					}
				}
			}
			s.betweenTests(t)
			check("before test-0", c.between)
			s.openTest(t)
			check("in test-0", c.inTest)
			s.betweenTests(t)
			check("after test-0", c.between)
		})
	}
}

// heldWithDecision holds a call to the store's round as it is now and gives
// it the decision that it is one of the open test's own calls, asked at once.
func heldWithDecision(t *testing.T, mm *MockManager) integrations.MockMemDb {
	t.Helper()
	held, _ := mm.HoldToRound(mm.MarkCallRound())
	hider, ok := held.(integrations.BackgroundHider)
	if !ok {
		t.Fatal("precondition: the held store hides no background")
	}
	held = hider.HideBackgroundWhen(func() bool { return true })
	hider.HidesBackground()
	return held
}

// hiddenBy reports which of mks the held store reports spent, by each of
// its two spent reads: Spent (integrations.SpentReader, HTTP's strict index
// and WithoutSpent) and MockSpent (integrations.StagingGenerations, HTTP's
// learn index).
func hiddenBy(t *testing.T, held integrations.MockMemDb, mks ...*models.Mock) (spent, mockSpent map[string]bool) {
	t.Helper()
	sr, ok := held.(integrations.SpentReader)
	if !ok {
		t.Fatal("precondition: the held store has no Spent read")
	}
	sg, ok := held.(integrations.StagingGenerations)
	if !ok {
		t.Fatal("precondition: the held store has no MockSpent read")
	}
	spent, mockSpent = map[string]bool{}, map[string]bool{}
	for _, mk := range mks {
		spent[mk.Name], mockSpent[mk.Name] = sr.Spent(mk), sg.MockSpent(mk.Name)
	}
	return spent, mockSpent
}

// A call decided one of the open test's own is hidden the background by
// each of the held store's two spent reads on its own, and never its own
// test's recording: Spent, which HTTP's strict index and every WithoutSpent
// read skip, and MockSpent, which HTTP's learn index skips. A parser that
// builds its index of the staging from either read alone (enterprise's,
// built on this store) never offers the call a background recording its
// consume would refuse again at every re-match. Neither the pool's nor the
// worker's recording is spent in the manager: the hide is the call's alone.
func TestEachSpentReadOfAHeldCallHidesTheBackgroundAlone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	s := stageBackground(t, true, true)
	s.betweenTests(t)
	s.openTest(t)
	held := heldWithDecision(t, s.mm)
	spent, mockSpent := hiddenBy(t, held, s.pool, s.worker, s.own)
	for _, mk := range []*models.Mock{s.pool, s.worker} {
		if !spent[mk.Name] {
			t.Errorf("the held call's Spent shows it the background's %s: a strict index built from it offers a recording the consume refuses", mk.Name)
		}
		if !mockSpent[mk.Name] {
			t.Errorf("the held call's MockSpent shows it the background's %s: a learn index built from it offers a recording the consume refuses", mk.Name)
		}
		if s.mm.MockSpent(mk.Name) {
			t.Errorf("precondition: the background's %s is spent in the manager", mk.Name)
		}
	}
	if spent[s.own.Name] || mockSpent[s.own.Name] {
		t.Errorf("the held call hides its own test's %s (Spent %v, MockSpent %v)", s.own.Name, spent[s.own.Name], mockSpent[s.own.Name])
	}
}

// A held call's decision that it is one of the open test's own hides the
// background only while a test is open in the view in force: there is no
// test whose recordings the call could be one more than. A call held
// between tests, given a decision anyway, and a call held in a test whose
// reads come after the test closed, both read the pool's and the worker's
// recordings as unspent: between tests the background answers the calls.
func TestAHeldCallHidesTheBackgroundOnlyWhileATestIsOpen(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, shape := range []string{"held between tests", "held in a test, read after it"} {
		t.Run(shape, func(t *testing.T) {
			s := stageBackground(t, true, true)
			s.betweenTests(t)
			var held integrations.MockMemDb
			if shape == "held between tests" {
				held = heldWithDecision(t, s.mm)
			} else {
				s.openTest(t)
				held = heldWithDecision(t, s.mm)
				if spent, _ := hiddenBy(t, held, s.worker); !spent[s.worker.Name] {
					t.Fatal("precondition: in the test the held call does not hide the worker's recording")
				}
				s.betweenTests(t)
			}
			spent, mockSpent := hiddenBy(t, held, s.pool, s.worker)
			for _, mk := range []*models.Mock{s.pool, s.worker} {
				if spent[mk.Name] || mockSpent[mk.Name] {
					t.Errorf("between tests the held call hides the background's %s (Spent %v, MockSpent %v)", mk.Name, spent[mk.Name], mockSpent[mk.Name])
				}
			}
		})
	}
}

// Under the proven switch, on a set served by the background rules, an
// exchange the open test recorded only as another protocol's recording (an
// Aerospike frame keeps its bytes in GenericRequests, as Redis's and Kafka's
// do) is no exchange of the test's own: the generic own-call check reads only
// the test's GENERIC recordings (OwnIndex's kind), so the pool's recording of
// the exchange answers it in the test. Read across kinds, the test's other
// recording of the same bytes made the call one more than the test recorded,
// and the exchange missed though the test never recorded it.
func TestAnExchangeTheTestRecordedOnlyAsAnotherProtocolIsNotOneOfItsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	own := ownRepeatExchange("mock-own-aerospike-ping", "PING", "AERO", start.Add(100*time.Millisecond))
	own.Kind = models.Aerospike
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool}, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{pool.Name}, nil)
	mm.ServeExactPool(models.PoolKeyNone, []string{pool.Name})
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() || !integrations.S12Background(mm) {
		t.Fatal("precondition: test-0 is not served exactly, by the background rules")
	}
	if got, miss := genericCall(t, mm, "PING"); got != "POOL" {
		t.Fatalf("test-0's PING, an exchange it recorded only as Aerospike's, was answered %q (miss %+v); want the pool's POOL", got, miss)
	}
}
