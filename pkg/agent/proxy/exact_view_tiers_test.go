package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// tierSet stages, as keploy mock replay stages a whole recording, the open
// test's four recordings and n others (every other one per-test, the rest
// session-lifetime, every third one MySQL, the rest HTTP), five
// protocol-phase config mocks, a pool of three and a worker scope of two.
func tierSet(t *testing.T, n int) (mm *MockManager, owned []*models.Mock, pool, workers []string) {
	t.Helper()
	at := time.Duration(0)
	next := func(name string, lifetime models.Lifetime) *models.Mock {
		at += time.Millisecond
		return exactMock(name, at, lifetime)
	}
	var perTest, session []*models.Mock
	for i := 0; i < 4; i++ {
		mk := next(fmt.Sprintf("own-%d", i), models.LifetimePerTest)
		owned = append(owned, mk)
		perTest = append(perTest, mk)
	}
	for i := 0; i < n; i++ {
		lifetime := models.LifetimeSession
		if i%2 == 0 {
			lifetime = models.LifetimePerTest
		}
		mk := next(fmt.Sprintf("rec-%05d", i), lifetime)
		if i%3 == 0 {
			mk.Kind = models.MySQL
		}
		if lifetime == models.LifetimePerTest {
			perTest = append(perTest, mk)
		} else {
			session = append(session, mk)
		}
	}
	for i := 0; i < 5; i++ {
		at += time.Millisecond
		session = append(session, configMock(fmt.Sprintf("cfg-%d", i), at))
	}
	for _, name := range []string{"p0", "p1", "p2", "w0", "w1"} {
		at += time.Millisecond
		session = append(session, httpCall(name, at, "/"+name))
	}
	return stagedManager(t, perTest, session), owned, []string{"p0", "p1", "p2"}, []string{"w0", "w1"}
}

// tierReads is every unkeyed read of the staged tiers a parser makes, by name,
// in a fixed order.
func tierReads(mm *MockManager) ([]string, map[string]func() ([]*models.Mock, error)) {
	byKind := func(read func(models.Kind) ([]*models.Mock, error), kind models.Kind) func() ([]*models.Mock, error) {
		return func() ([]*models.Mock, error) { return read(kind) }
	}
	reads := map[string]func() ([]*models.Mock, error){
		"unfiltered":               mm.GetUnFilteredMocks,
		"session-scoped":           mm.GetSessionScopedMocks,
		"startup":                  mm.GetStartupMocks,
		"session":                  mm.GetSessionMocks,
		"filtered":                 mm.GetFilteredMocks,
		"filtered-in-window":       mm.GetFilteredMocksInWindow,
		"per-test-in-window":       mm.GetPerTestMocksInWindow,
		"unfiltered-by-kind http":  byKind(mm.GetUnFilteredMocksByKind, models.HTTP),
		"unfiltered-by-kind mysql": byKind(mm.GetUnFilteredMocksByKind, models.MySQL),
		"startup-by-kind http":     byKind(mm.GetStartupMocksByKind, models.HTTP),
		"startup-by-kind mysql":    byKind(mm.GetStartupMocksByKind, models.MySQL),
		"filtered-by-kind http":    byKind(mm.GetFilteredMocksByKind, models.HTTP),
		"filtered-by-kind mysql":   byKind(mm.GetFilteredMocksByKind, models.MySQL),
		"round": func() ([]*models.Mock, error) {
			s, ok := mm.RoundSnapshot(integrations.RoundQuery{})
			if !ok {
				return nil, nil
			}
			var out []*models.Mock
			for _, part := range [][]*models.Mock{s.PerTest, s.Startup, s.SessionScoped, s.Session} {
				out = append(append(out, part...), nil)
			}
			return out, nil
		},
	}
	order := make([]string, 0, len(reads))
	for name := range reads {
		order = append(order, name)
	}
	slices.Sort(order)
	return order, reads
}

// walkedRead is read as it was made before the view's reads were handed what
// it shows of each tier (shownOf): over every value of the tier.
func walkedRead(t *testing.T, read func() ([]*models.Mock, error)) []*models.Mock {
	t.Helper()
	tierReadsWalk.Store(true)
	defer tierReadsWalk.Store(false)
	ms, err := read()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// mainRead is the read named r (tierReads) as it was made before the view's
// reads were handed what it shows of each tier: each tier's values, read
// whole, narrowed by the view. The reads that compose others (session, the
// round) are left to walkedRead. nil, false for those, and with no view.
func mainRead(mm *MockManager, r string) ([]*models.Mock, bool) {
	v := mm.exactView()
	if v == nil {
		return nil, false
	}
	mm.treesMu.RLock()
	filtered, unfiltered, startup := mm.filtered, mm.unfiltered, mm.startup
	fk := map[models.Kind]*TreeDb{models.HTTP: mm.filteredByKind[models.HTTP], models.MySQL: mm.filteredByKind[models.MySQL]}
	uk := map[models.Kind]*TreeDb{models.HTTP: mm.unfilteredByKind[models.HTTP], models.MySQL: mm.unfilteredByKind[models.MySQL]}
	mm.treesMu.RUnlock()
	perTest := func(kind models.Kind, staged []*models.Mock) []*models.Mock {
		var teardown []*models.Mock
		if v.afterTests(mm.exact.fired.Load()) {
			teardown = mm.teardown(v.poolKey, mm.treeValues(startup, kind))
		}
		return v.perTest(kind, staged, teardown)
	}
	read, k, _ := strings.Cut(r, " ")
	kind := map[string]models.Kind{"http": models.HTTP, "mysql": models.MySQL}[k]
	switch {
	case r == "unfiltered" || r == "session-scoped":
		return v.reusable("", mm.treeValues(unfiltered, ""), mm.treeValues(startup, "")), true
	case r == "startup":
		if startup == nil {
			return nil, true
		}
		return v.reusable("", mm.treeValues(startup, ""), mm.treeValues(unfiltered, "")), true
	case r == "filtered" || r == "filtered-in-window" || r == "per-test-in-window":
		return perTest("", mm.treeValues(filtered, "")), true
	case kind != "" && read == "unfiltered-by-kind":
		return v.reusable(kind, mm.treeValues(uk[kind], ""), mm.treeValues(startup, kind)), true
	case kind != "" && read == "startup-by-kind":
		if startup == nil {
			return nil, true
		}
		return v.reusable(kind, mm.treeValues(startup, kind), mm.treeValues(unfiltered, kind)), true
	case kind != "" && read == "filtered-by-kind":
		return perTest(kind, mm.treeValues(fk[kind], "")), true
	}
	return nil, false
}

// TestATierReadUnderExactServingCostsWhatTheViewShows: under exact serving an
// unkeyed read of a tier (GetUnFilteredMocks, GetStartupMocks,
// GetSessionMocks, GetFilteredMocks, their by-kind forms, the round's
// snapshot) was handed every value of the tier and narrowed it to what the
// view shows: at keploy mock replay's 10k recordings, every test's
// recordings, on every call. Counted: once the tree's split is built, a read
// hands the narrowing (viewNarrowed) and examines (keyedExamined) the same
// number of mocks over a set of 300 and one of 3,000, in a pool view and in a
// test's view, and gives what the walk of every value gives, mock for mock.
func TestATierReadUnderExactServingCostsWhatTheViewShows(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type cost struct{ narrowed, examined uint64 }
	measure := func(n int) map[string]cost {
		mm, owned, pool, workers := tierSet(t, n)
		order, reads := tierReads(mm)
		out := map[string]cost{}
		views := []struct {
			name    string
			install func()
		}{
			{"pool view", func() { mm.ServeExactPool("k", pool) }},
			{"test view", func() {
				mm.ServeExactWorker(workers)
				mm.ServeExactTest("T", owned)
			}},
		}
		for _, view := range views {
			view.install()
			for _, r := range order {
				want := walkedRead(t, reads[r])
				if got := freshMocks(t, reads[r]); !sameMocks(got, want) {
					t.Fatalf("n=%d, %s: the %s read gives %v; the walk of every value gives %v", n, view.name, r, verifyNames(got), verifyNames(want))
				}
				const times = 10
				narrowed, examined := viewNarrowed.Load(), keyedExamined.Load()
				for i := 0; i < times; i++ {
					freshMocks(t, reads[r])
				}
				c := cost{(viewNarrowed.Load() - narrowed) / times, (keyedExamined.Load() - examined) / times}
				if c.narrowed > 64 || c.examined > 64 {
					t.Fatalf("n=%d, %s: a %s read handed the narrowing %d mocks and examined %d; want at most 64 each (what the view shows)", n, view.name, r, c.narrowed, c.examined)
				}
				out[view.name+" "+r] = c
			}
		}
		return out
	}
	small, large := measure(300), measure(3000)
	for name, c := range small {
		if large[name] != c {
			t.Fatalf("%s: a read over 300 recordings handed %d mocks and examined %d, over 3,000 %d and %d; want the same", name, c.narrowed, c.examined, large[name].narrowed, large[name].examined)
		}
	}
}

func freshMocks(t *testing.T, read func() ([]*models.Mock, error)) []*models.Mock {
	t.Helper()
	ms, err := read()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// TestATierReadIsTheWalkOfEveryValueUnderRandomChanges: over random stagings,
// test opens and closes, pool views, worker scopes, and consumes and updates
// through each door, every unkeyed read of the tiers gives, after each step,
// what it gives handed every value of the tier (walkedRead): the same mocks,
// by identity, in the same order. The verify mode (on in this package) checks
// each read's split under the tree's lock as well.
func TestATierReadIsTheWalkOfEveryValueUnderRandomChanges(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	if !exactViewVerify.Load() {
		t.Fatal("precondition: the rows run with the verify mode on")
	}
	rng := rand.New(rand.NewSource(20261027))
	const tests, perTest = 4, 4
	var staged []*models.Mock
	byTest := make([][]*models.Mock, tests)
	at := time.Duration(0)
	for k := 0; k < tests; k++ {
		for j := 0; j < perTest; j++ {
			at += time.Millisecond
			mk := exactMock(fmt.Sprintf("t%d-%d", k, j), at, models.LifetimePerTest)
			if j == perTest-1 {
				mk.Kind = models.MySQL
			}
			byTest[k] = append(byTest[k], mk)
			staged = append(staged, mk)
		}
	}
	var session []*models.Mock
	for i := 0; i < 3; i++ {
		session = append(session, configMock(fmt.Sprintf("cfg-%d", i), time.Second+time.Duration(i)*time.Millisecond))
	}
	pool := []string{"p0", "p1", "p2", "p3"}
	for i, p := range pool {
		session = append(session, httpCall(p, 2*time.Second+time.Duration(i)*time.Millisecond, "/"+p))
	}
	workers := []string{"w0", "w1"}
	for i, w := range workers {
		session = append(session, httpCall(w, 3*time.Second+time.Duration(i)*time.Millisecond, "/"+w))
	}
	// A teardown recording: per-test, after every other recording.
	session = append(session, exactMock("teardown", 4*time.Second, models.LifetimePerTest))
	mm := stagedManager(t, staged, session)
	mm.ServeExactPool("k", nil)
	order, reads := tierReads(mm)
	subset := func(names []string) []string {
		var out []string
		for _, n := range names {
			if rng.Intn(2) == 0 {
				out = append(out, n)
			}
		}
		return out
	}
	const steps = 2000
	consumes, mained := 0, 0
	for step := 0; step < steps; step++ {
		what := ""
		switch r := rng.Intn(20); {
		case r == 0:
			what = "restage"
			mm.SetMocksWithWindow(staged, session, models.BaseTime, time.Now())
		case r <= 3:
			k := rng.Intn(tests)
			what = fmt.Sprintf("open t%d", k)
			mm.ServeExactWorker(subset(workers))
			mm.ServeExactTest(fmt.Sprintf("T%d", k), byTest[k])
		case r <= 5:
			what = "pool view"
			var ahead []*models.Mock
			if rng.Intn(2) == 0 {
				ahead = byTest[rng.Intn(tests)][:1+rng.Intn(perTest)]
			}
			mm.ServeExactPool("k", subset(append(slices.Clone(pool), "teardown")), ahead...)
		default:
			var shown []*models.Mock
			for _, r := range order {
				for _, mk := range freshMocks(t, reads[r]) {
					if mk != nil {
						shown = append(shown, mk)
					}
				}
			}
			if len(shown) == 0 {
				what = "nothing to consume"
				break
			}
			mk := shown[rng.Intn(len(shown))]
			var ok bool
			switch rng.Intn(4) {
			case 0:
				what = "delete-filtered " + mk.Name
				ok = mm.DeleteFilteredMock(*mk)
			case 1:
				what = "delete-unfiltered " + mk.Name
				ok = mm.DeleteUnFilteredMock(*mk)
			case 2:
				what = "delete-startup " + mk.Name
				ok = mm.DeleteStartupMock(*mk)
			default:
				what = "re-stamp " + mk.Name
				re := mk.ShallowCopy()
				re.TestModeInfo.SortOrder++
				ok = mm.UpdateUnFilteredMock(mk, re)
			}
			if ok {
				consumes++
			}
		}
		for _, r := range order {
			want := walkedRead(t, reads[r])
			got := freshMocks(t, reads[r])
			if !sameMocks(got, want) {
				t.Fatalf("step %d (%s): the %s read gives %v; the walk of every value gives %v", step, what, r, verifyNames(got), verifyNames(want))
			}
			if was, ok := mainRead(mm, r); ok {
				mained++
				if !sameMocks(got, was) {
					t.Fatalf("step %d (%s): the %s read gives %v; each tier read whole and narrowed by the view gives %v", step, what, r, verifyNames(got), verifyNames(was))
				}
			}
		}
	}
	if consumes < steps/4 {
		t.Fatalf("only %d of %d steps consumed: the walk does not exercise consumes", consumes, steps)
	}
	if mained != steps*12 {
		t.Fatalf("%d of %d reads were compared with the reads before the split; want the twelve whose form it knows on every step", mained, steps*len(order))
	}
	t.Logf("%d steps, %d consumes, %d reads compared with the reads before the split", steps, consumes, mained)
}

// TestATierReadSkipsAnEntryRemovedAtTheSameGeneration: a removal from a tree
// does not move its generation (storedLocked), so the split a view's tier read
// uses still files the removed entry; the read checks each entry is still in
// the tree. A protocol-phase mock and a worker-scope recording removed after
// the first read are in no later read, at the same generation.
func TestATierReadSkipsAnEntryRemovedAtTheSameGeneration(t *testing.T) {
	db, ms := shownTree(40)
	v := &exactView{test: "T", worker: nameSet("m005")}
	if got := v.shownOf(db, ""); len(got) != 5 {
		t.Fatalf("precondition: the view shows %d of the tree; want its 4 protocol-phase mocks and its worker-scope one", len(got))
	}
	gen := db.gen
	phase, worker := ms[9], ms[4]
	if !phase.IsProtocolPhase() || worker.Name != "m005" {
		t.Fatalf("precondition: %s is not protocol-phase or %s is not the worker scope's", phase.Name, worker.Name)
	}
	if !db.delete(phase.TestModeInfo) || !db.delete(worker.TestModeInfo) {
		t.Fatal("precondition: the removals found nothing")
	}
	if db.gen != gen {
		t.Fatal("precondition: a removal moved the tree's generation")
	}
	for _, kind := range []models.Kind{"", models.MySQL} {
		got := v.shownOf(db, kind)
		if slices.Contains(got, phase) || slices.Contains(got, worker) || len(got) != 3 {
			t.Fatalf("after two removals the view's read of kind %q gives %v; want the 3 protocol-phase mocks left", kind, verifyNames(got))
		}
	}
}

// TestRacingSpendsOfOneStagedMockAnswerOnce: a staged mock a test's view shows
// and a parser evicts through a delete door is spent in the view, and the
// first spend wins: of 16 consumes racing for one protocol-phase mock,
// exactly one is served, in each of 200 views. The round lock serialises the
// consumes, so the later ones find the mock spent (shows); the spend also
// refuses a name already spent, under the view's lock, should two consumes
// reach it together.
func TestRacingSpendsOfOneStagedMockAnswerOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	owned := []*models.Mock{exactMock("own", time.Millisecond, models.LifetimePerTest)}
	cfg := configMock("cfg", time.Second)
	mm := stagedManager(t, owned, []*models.Mock{cfg})
	const racers, views = 16, 200
	for round := 0; round < views; round++ {
		mm.ServeExactTest("T", owned)
		start := make(chan struct{})
		won := make(chan bool, racers)
		for i := 0; i < racers; i++ {
			go func() {
				<-start
				won <- mm.DeleteUnFilteredMock(*cfg)
			}()
		}
		close(start)
		n := 0
		for i := 0; i < racers; i++ {
			if <-won {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("view %d: %d of %d racing consumes of one protocol-phase mock were served; want 1", round, n, racers)
		}
	}
}
