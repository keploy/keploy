package proxy

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// sharedCache is a parser's index of the shared part of an open test's reads
// (integrations.SharedReads), kept as a parser that adopts it keeps it: built
// again only when SharedGen moves; a lookup is the open test's unanswered
// recordings, then the index less what MockSpent reports (each recording
// once in the session read).
type sharedCache struct {
	kind            models.Kind
	gen             integrations.SharedGen
	built           bool
	perTest, sessio []*models.Mock
	builds          int
}

func (c *sharedCache) lookup(t *testing.T, db integrations.MockMemDb) (perTest, session []string, ok bool) {
	t.Helper()
	p, s, ok := c.lookupMocks(t, db)
	return exactNames(p), exactNames(s), ok
}

func (c *sharedCache) lookupMocks(t *testing.T, db integrations.MockMemDb) (perTest, session []*models.Mock, ok bool) {
	t.Helper()
	sr, ok := db.(integrations.SharedReads)
	if !ok {
		t.Fatal("the store offers no shared reads")
	}
	gen, ok := sr.SharedGeneration(c.kind)
	if !ok {
		return nil, nil, false
	}
	if !c.built || gen != c.gen {
		p, s, ok := sr.SharedMocks()
		if !ok {
			t.Fatal("a store with a shared generation offers no shared reads")
		}
		if again, _ := sr.SharedGeneration(c.kind); again != gen {
			t.Fatalf("the shared generation moved during a single-threaded read: %v then %v", gen, again)
		}
		c.perTest, c.sessio, c.gen, c.built = p, s, gen, true
		c.builds++
	}
	owned, ok := sr.OwnedUnanswered()
	if !ok {
		t.Fatal("a store with a shared generation reads no open test")
	}
	of := func(ms []*models.Mock) []*models.Mock {
		var out []*models.Mock
		for _, mk := range ms {
			if c.kind == "" || mk.Kind == c.kind {
				out = append(out, mk)
			}
		}
		return out
	}
	unspent := func(ms []*models.Mock) []*models.Mock {
		return slices.DeleteFunc(slices.Clone(ms), func(mk *models.Mock) bool { return sr.MockSpent(mk.Name) })
	}
	perTest = of(append(slices.Clone(owned), unspent(c.perTest)...))
	seen := map[*models.Mock]bool{}
	var s []*models.Mock
	for _, mk := range append(slices.Clone(owned), unspent(c.sessio)...) {
		if !seen[mk] {
			seen[mk] = true
			s = append(s, mk)
		}
	}
	return perTest, of(s), true
}

func namesOfKind(t *testing.T, read func() ([]*models.Mock, error), kind models.Kind) []string {
	t.Helper()
	ms, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		ms = slices.DeleteFunc(slices.Clone(ms), func(mk *models.Mock) bool { return mk.Kind != kind })
	}
	return exactNames(ms)
}

// sameValues reports whether a and b hold the same recordings in the same
// order, each the same value: the same pointer, or one equal to it in name,
// kind and spec (a learned copy handed out again).
func sameValues(a, b []*models.Mock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == b[i] {
			continue
		}
		if a[i] == nil || b[i] == nil || a[i].Name != b[i].Name || a[i].Kind != b[i].Kind || !reflect.DeepEqual(a[i].Spec, b[i].Spec) {
			return false
		}
	}
	return true
}

func describeMocks(ms []*models.Mock) string {
	var out []string
	for _, mk := range ms {
		out = append(out, fmt.Sprintf("%s(rev %q)", mk.Name, mk.Spec.Metadata["rev"]))
	}
	return "[" + strings.Join(out, " ") + "]"
}

func ofKind(read func() ([]*models.Mock, error), kind models.Kind) []*models.Mock {
	ms, _ := read()
	if kind != "" {
		ms = slices.DeleteFunc(slices.Clone(ms), func(mk *models.Mock) bool { return mk.Kind != kind })
	}
	return ms
}

// sharedWalk is what a shared-index walk runs under: the background rules
// (S12 routing, the pool shown in a test's session read) and kept learning
// (each recording handed out as its learned copy, the learning set again at
// random test opens).
type sharedWalk struct {
	seed       int64
	background bool
	kept       bool
}

// walkSharedIndex: over random stagings, test opens (with random and with
// unchanged worker scopes), pool views and consumes through each door, an
// index of the shared part of the open test's reads, kept per SharedGen
// (every kind's, HTTP's and DNS's), with the open test's unanswered
// recordings before it and less what MockSpent reports, reads what the
// store's per-test and session reads give, value for value and in the same
// order, after every step while a test is open. An update stores a changed
// copy (its metadata names the update) or the same recording again. A
// staging may leave out some of the pool's and the worker scope's
// recordings. A test's open with the worker scope and learning unchanged
// builds nothing again.
func walkSharedIndex(t *testing.T, w sharedWalk) {
	t.Setenv(provenswitch.Env, "1")
	rng := rand.New(rand.NewSource(w.seed))
	const tests, perTest = 5, 4
	var staged []*models.Mock
	byTest := make([][]*models.Mock, tests)
	at := time.Duration(0)
	for k := 0; k < tests; k++ {
		for j := 0; j < perTest; j++ {
			at += time.Millisecond
			mk := exactMock(fmt.Sprintf("t%d-%d", k, j), at, models.LifetimePerTest)
			if j%2 == 1 {
				mk = httpCall(fmt.Sprintf("t%d-%d", k, j), at, fmt.Sprintf("/t%d", k))
				mk.TestModeInfo.Lifetime = models.LifetimePerTest
			}
			byTest[k] = append(byTest[k], mk)
			staged = append(staged, mk)
		}
	}
	// Protocol-phase recordings no test owns in the per-test tier, which a
	// scope's staging keeps there.
	for i := 0; i < 2; i++ {
		at += time.Millisecond
		mk := exactMock(fmt.Sprintf("dns-%d", i), at, models.LifetimePerTest)
		mk.Kind = models.DNS
		staged = append(staged, mk)
	}
	var session []*models.Mock
	for i := 0; i < 3; i++ {
		session = append(session, configMock(fmt.Sprintf("cfg-%d", i), time.Second+time.Duration(i)*time.Millisecond))
	}
	pool := []string{"p0", "p1", "p2", "p3"}
	for i, p := range pool {
		session = append(session, httpCall(p, 2*time.Second+time.Duration(i)*time.Millisecond, "/"+p))
	}
	workers := []string{"w0", "w1", "w2"}
	for i, wk := range workers {
		session = append(session, httpCall(wk, 3*time.Second+time.Duration(i)*time.Millisecond, "/"+wk))
	}
	mm := stagedManager(t, staged, session)
	// serveBackground serves the background rules over the pool and worker
	// recordings a staging kept, as the replay does once a set is staged.
	serveBackground := func(present []*models.Mock) {
		if !w.background {
			return
		}
		var pl, wk []string
		for _, mk := range present {
			switch {
			case strings.HasPrefix(mk.Name, "p"):
				pl = append(pl, mk.Name)
			case strings.HasPrefix(mk.Name, "w"):
				wk = append(wk, mk.Name)
			}
		}
		mm.ServeExactBackground(models.RecordedWithS12Routing, pl, wk)
	}
	serveBackground(session)
	mm.ServeExactPool("k", nil)
	if w.kept {
		mm.SetLearnEnded(true)
	}
	subset := func(names []string) []string {
		var out []string
		for _, n := range names {
			if rng.Intn(2) == 0 {
				out = append(out, n)
			}
		}
		return out
	}
	learnable := []string{"p0", "p1", "w0", "w1", "cfg-0", "cfg-1", "dns-0"}
	caches := []*sharedCache{{}, {kind: models.HTTP}, {kind: models.DNS}}
	const steps = 3000
	opens, sameOpens, checked, changed, keptSets, poolRead, learnedRead := 0, 0, 0, 0, 0, 0, 0
	for step := 0; step < steps; step++ {
		what := ""
		sameOpen := false
		switch r := rng.Intn(20); {
		case r == 0:
			// A scope's staging keeps the per-test tier as handed, so the
			// shared per-test part holds the protocol-phase recordings
			// there; one from BaseTime files them in the startup tier. A
			// staging may leave out some of the pool's and the worker
			// scope's recordings: a kind's shared part then changes under
			// the same ident, and only that kind's stored generation tells
			// the HTTP cache to build again.
			restaged := slices.DeleteFunc(slices.Clone(session), func(mk *models.Mock) bool {
				return mk.Kind == models.HTTP && rng.Intn(3) == 0
			})
			if w.background {
				mm.StageExactBackground(models.RecordedWithS12Routing)
			}
			if rng.Intn(2) == 0 {
				what = fmt.Sprintf("restage of %d session recordings", len(restaged))
				mm.SetMocksWithWindow(staged, restaged, models.BaseTime, time.Now())
			} else {
				what = fmt.Sprintf("scope restage of %d session recordings", len(restaged))
				mm.SetScopeMocks(staged, restaged, true)
			}
			serveBackground(restaged)
			if w.kept {
				mm.SetLearnEnded(true)
			}
		case r <= 4:
			k := rng.Intn(tests)
			what = fmt.Sprintf("open t%d", k)
			sameOpen = true
			if rng.Intn(3) == 0 {
				mm.ServeExactWorker(subset(workers))
				sameOpen = false
			}
			if w.kept && rng.Intn(3) == 0 {
				var states []models.MockState
				for _, n := range subset(learnable) {
					states = append(states, models.MockState{Name: n, ReqBodyNoise: map[string][]string{fmt.Sprintf("body.k%d", step): {}}})
				}
				mm.SetKeptLearning(learn.NewKeptLearning(states))
				keptSets++
				sameOpen = false
				what += " with new kept learning"
			}
			mm.ServeExactTest(fmt.Sprintf("T%d", k), byTest[k])
			opens++
		case r <= 6:
			what = "pool view"
			var ahead []*models.Mock
			if rng.Intn(2) == 0 {
				ahead = byTest[rng.Intn(tests)][:1+rng.Intn(perTest)]
			}
			mm.ServeExactPool("k", subset(pool), ahead...)
		default:
			var shown []*models.Mock
			for _, read := range []func() ([]*models.Mock, error){mm.GetPerTestMocksInWindow, mm.GetSessionMocks, mm.GetUnFilteredMocks} {
				ms, _ := read()
				shown = append(shown, ms...)
			}
			if len(shown) == 0 {
				what = "nothing to consume"
				break
			}
			mk := shown[rng.Intn(len(shown))]
			switch rng.Intn(4) {
			case 0:
				what = "delete-filtered " + mk.Name
				mm.DeleteFilteredMock(*mk)
			case 1:
				what = "delete-unfiltered " + mk.Name
				mm.DeleteUnFilteredMock(*mk)
			case 2:
				what = "update " + mk.Name + " with itself"
				mm.UpdateUnFilteredMock(mk, mk)
			default:
				// A matcher stores a changed copy (a learned-noise copy,
				// say): every index must read the stored value.
				cp := mk.ShallowCopy()
				md := make(map[string]string, len(mk.Spec.Metadata)+1)
				for k, v := range mk.Spec.Metadata {
					md[k] = v
				}
				changed++
				md["rev"] = fmt.Sprint(changed)
				cp.Spec.Metadata = md
				what = "update " + mk.Name + " with a changed value"
				mm.UpdateUnFilteredMock(mk, cp)
			}
		}
		if sameOpen {
			sameOpens++
		}
		// A parser looks up only when a call comes: one consumes a
		// recording before any lookup in its test now and then (another
		// connection's), so an index is built after a view's spends too.
		if rng.Intn(3) == 0 {
			continue
		}
		for _, c := range caches {
			gotP, gotS, ok := c.lookupMocks(t, mm)
			if !ok {
				continue
			}
			checked++
			for _, mk := range gotS {
				if strings.HasPrefix(mk.Name, "p") {
					poolRead++
				}
				for k := range mk.Spec.ReqBodyNoise {
					if strings.HasPrefix(k, "body.k") {
						learnedRead++
					}
				}
			}
			if want := ofKind(mm.GetPerTestMocksInWindow, c.kind); !sameValues(gotP, want) {
				t.Fatalf("step %d (%s): the shared per-test index of kind %q reads %s; the store reads %s", step, what, c.kind, describeMocks(gotP), describeMocks(want))
			}
			if want := ofKind(mm.GetSessionMocks, c.kind); !sameValues(gotS, want) {
				t.Fatalf("step %d (%s): the shared session index of kind %q reads %s; the store reads %s", step, what, c.kind, describeMocks(gotS), describeMocks(want))
			}
		}
	}
	if checked < steps/2 {
		t.Fatalf("only %d checks over %d steps: the walk opens too few tests", checked, steps)
	}
	if changed == 0 {
		t.Fatal("precondition: no update with a changed value")
	}
	if w.background && poolRead == 0 {
		t.Fatal("precondition: no test's shared session read held the pool under the background rules")
	}
	if w.kept && learnedRead == 0 {
		t.Fatal("precondition: no lookup read a learned copy")
	}
	if !w.kept {
		if b := caches[0].builds; b >= opens {
			t.Fatalf("the shared index was built %d times over %d test opens (%d with the worker scope unchanged): a test's open rebuilds it", b, opens, sameOpens)
		}
	}
	t.Logf("%d steps, %d test opens (%d with the worker scope and learning unchanged), %d checks, %d updates with a changed value, %d learning sets; the pool read %d times, learned copies %d; shared index built %d times (HTTP's %d, DNS's %d)", steps, opens, sameOpens, checked, changed, keptSets, poolRead, learnedRead, caches[0].builds, caches[1].builds, caches[2].builds)
}

var sharedWalkSeeds = []int64{20261027, 1, 2, 3}

// walkSeeds is the seeds walkSharedIndex draws: all four, or the first under
// -race, where a walk starts no goroutine of its own and the four walks of
// each row take several seconds of the race step; the plain run (go test
// ./...) draws all four.
func walkSeeds() []int64 {
	if raceOn {
		return sharedWalkSeeds[:1]
	}
	return sharedWalkSeeds
}

// TestASharedIndexIsTheReadUnderRandomChanges: walkSharedIndex with neither
// the background rules nor kept learning.
func TestASharedIndexIsTheReadUnderRandomChanges(t *testing.T) {
	for _, seed := range walkSeeds() {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { walkSharedIndex(t, sharedWalk{seed: seed}) })
	}
}

// TestASharedIndexIsTheReadUnderTheBackgroundRules: walkSharedIndex under the
// background rules, which show the pool in a test's session read and keep a
// protocol-phase recording a test spent for the next test. A background
// order that drops what the view spent, or an ident that leaves out the
// rules, leaves an index that misses or keeps serving the pool.
func TestASharedIndexIsTheReadUnderTheBackgroundRules(t *testing.T) {
	for _, seed := range walkSeeds() {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { walkSharedIndex(t, sharedWalk{seed: seed, background: true}) })
	}
}

// TestASharedIndexIsTheReadUnderKeptLearning: walkSharedIndex with kept
// learning set again at random test opens (alone and under the background
// rules). The index must read each recording as the learned copy the store
// hands out now, never one the learning before handed out.
func TestASharedIndexIsTheReadUnderKeptLearning(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, seed := range walkSeeds() {
			name := fmt.Sprint(seed)
			if background {
				name += " under the background rules"
			}
			t.Run(name, func(t *testing.T) { walkSharedIndex(t, sharedWalk{seed: seed, background: background, kept: true}) })
		}
	}
}

// TestASharedIndexReadsTheValueAnUpdateStored: a matcher that stores a changed
// copy of a protocol-phase recording a test reads (a learned-noise copy, say)
// moves its kind's stored generation, so an index built before the update
// reads the copy after it, never the value it replaced.
func TestASharedIndexReadsTheValueAnUpdateStored(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	cfg := configMock("cfg", 0)
	mm := stagedManager(t, nil, []*models.Mock{cfg, httpCall("other", time.Second, "/other")})
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T", nil)
	caches := []*sharedCache{{}, {kind: cfg.Kind}}
	for _, c := range caches {
		if _, s, _ := c.lookupMocks(t, mm); !slices.Contains(s, cfg) {
			t.Fatalf("precondition: the shared session index of kind %q reads %s", c.kind, describeMocks(s))
		}
	}
	cp := cfg.ShallowCopy()
	cp.Spec.Metadata = map[string]string{"rev": "1"}
	for k, v := range cfg.Spec.Metadata {
		cp.Spec.Metadata[k] = v
	}
	if !mm.UpdateUnFilteredMock(cfg, cp) {
		t.Fatal("precondition: the update stored nothing")
	}
	if want := ofKind(mm.GetSessionMocks, ""); !slices.Contains(want, cp) {
		t.Fatalf("precondition: the store reads %s after the update", describeMocks(want))
	}
	for _, c := range caches {
		_, got, _ := c.lookupMocks(t, mm)
		if want := ofKind(mm.GetSessionMocks, c.kind); !sameValues(got, want) {
			t.Fatalf("the shared session index of kind %q reads %s after an update stored a changed copy; the store reads %s", c.kind, describeMocks(got), describeMocks(want))
		}
	}
}

// TestATestsOpenDoesNotRebuildTheSharedIndex: with a worker scope of 200
// recordings every test's view shows, 20 tests each open, read and consume
// their own. An index keyed on the staging generation is built again at each
// test's open, from the whole session read (the shared 200 too); one keyed on
// SharedGen is built once, and each test reads only its own recordings
// besides it. Counted: builds and the recordings filed by them.
func TestATestsOpenDoesNotRebuildTheSharedIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const tests, own, shared = 20, 3, 200
	var staged, session []*models.Mock
	byTest := make([][]*models.Mock, tests)
	at := time.Duration(0)
	for k := range byTest {
		for j := 0; j < own; j++ {
			at += time.Millisecond
			mk := httpCall(fmt.Sprintf("t%02d-%d", k, j), at, fmt.Sprintf("/t%d/%d", k, j))
			mk.TestModeInfo.Lifetime = models.LifetimePerTest
			byTest[k] = append(byTest[k], mk)
			staged = append(staged, mk)
		}
	}
	var names []string
	for i := 0; i < shared; i++ {
		name := fmt.Sprintf("w%03d", i)
		session = append(session, httpCall(name, time.Hour+time.Duration(i)*time.Millisecond, "/"+name))
		names = append(names, name)
	}
	mm := stagedManager(t, staged, session)
	mm.ServeExactPool("k", nil)
	mm.ServeExactWorker(names)

	byGen := &genCache{read: mm.GetSessionMocks}
	sc := &sharedCache{kind: models.HTTP}
	genFiled, sharedFiled := 0, 0
	for k := range byTest {
		// Between two tests the pool's view, as a test's close installs it.
		mm.ServeExactPool("k", nil)
		mm.ServeExactTest(fmt.Sprintf("T%d", k), byTest[k])
		for j, mk := range byTest[k] {
			b := byGen.builds
			byGen.lookup(t, mm)
			if byGen.builds != b {
				genFiled += len(byGen.names)
			}
			b = sc.builds
			_, got, ok := sc.lookup(t, mm)
			if !ok {
				t.Fatalf("test %d: no shared generation while the test is open", k)
			}
			if sc.builds != b {
				sharedFiled += len(sc.sessio)
			}
			if want := namesOfKind(t, mm.GetSessionMocks, models.HTTP); !slices.Equal(got, want) {
				t.Fatalf("test %d call %d: the shared index reads %v; the store reads %v", k, j, got, want)
			}
			if !mm.DeleteFilteredMock(*mk) {
				t.Fatalf("test %d: the consume of its own %s was refused", k, mk.Name)
			}
		}
	}
	if sc.builds != 1 || sharedFiled != shared {
		t.Fatalf("%d test opens built the shared index %d times, filing %d recordings; want once, %d (a test's open rebuilds it)", tests, sc.builds, sharedFiled, shared)
	}
	if byGen.builds != tests {
		t.Fatalf("precondition: the staging generation index was built %d times over %d tests; want one per test", byGen.builds, tests)
	}
	t.Logf("%d tests: staging-generation index built %d times filing %d recordings; shared index built %d times filing %d", tests, byGen.builds, genFiled, sc.builds, sharedFiled)
}

// TestASpendInOneTestIsReadAgainInTheNextThroughTheSharedIndex: a parser
// consumes a protocol-phase recording of the per-test tier and one of the
// session tier in test A, before any shared lookup there (another connection's
// handshake, say), so the shared index is first built in A after those spends.
// Test B opens with A's worker scope, so it keeps A's shared generation and
// reuses that index; its spends start afresh, and both recordings answer
// again in B. The index must hold them: it is built with what a view spent
// kept, and a lookup drops what MockSpent reports.
func TestASpendInOneTestIsReadAgainInTheNextThroughTheSharedIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a := httpCall("a0", time.Millisecond, "/a")
	a.TestModeInfo.Lifetime = models.LifetimePerTest
	b := httpCall("b0", 2*time.Millisecond, "/b")
	b.TestModeInfo.Lifetime = models.LifetimePerTest
	// A DNS recording is protocol-phase whatever its lifetime: one recorded
	// in a test's window is staged in the per-test tier.
	hsPerTest := exactMock("hs-per-test", 3*time.Millisecond, models.LifetimePerTest)
	hsPerTest.Kind = models.DNS
	hsSession := configMock("hs-session", time.Second)
	w := httpCall("w0", 2*time.Second, "/w0")
	mm := stagedManager(t, nil, nil)
	// A staging from BaseTime files every per-test recording in the startup
	// tier; a scope's staging keeps the per-test tier as it is handed.
	mm.SetScopeMocks([]*models.Mock{a, b, hsPerTest}, []*models.Mock{hsSession, w}, true)
	mm.ServeExactPool("k", nil)
	mm.ServeExactWorker([]string{"w0"})

	sc := &sharedCache{}
	check := func(when string, wantP, wantS []string) {
		t.Helper()
		gotP, gotS, ok := sc.lookup(t, mm)
		if !ok {
			t.Fatalf("%s: no shared generation while a test is open", when)
		}
		storeP := namesOfKind(t, mm.GetPerTestMocksInWindow, "")
		storeS := namesOfKind(t, mm.GetSessionMocks, "")
		if !slices.Equal(storeP, wantP) || !slices.Equal(storeS, wantS) {
			t.Fatalf("precondition, %s: the store reads per-test %v and session %v; want %v and %v", when, storeP, storeS, wantP, wantS)
		}
		if !slices.Equal(gotP, storeP) {
			t.Fatalf("%s: the shared per-test index reads %v; the store reads %v", when, gotP, storeP)
		}
		if !slices.Equal(gotS, storeS) {
			t.Fatalf("%s: the shared session index reads %v; the store reads %v", when, gotS, storeS)
		}
	}

	mm.ServeExactTest("A", []*models.Mock{a})
	if !mm.DeleteFilteredMock(*hsPerTest) || !mm.DeleteUnFilteredMock(*hsSession) {
		t.Fatal("precondition: test A's consume of a protocol-phase recording was refused")
	}
	if !mm.MockSpent(hsPerTest.Name) || !mm.MockSpent(hsSession.Name) {
		t.Fatal("precondition: the protocol-phase recordings consumed in A are not spent there")
	}
	check("test A after its spends", []string{"a0"}, []string{"a0", "w0"})

	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("B", []*models.Mock{b})
	if mm.MockSpent(hsPerTest.Name) || mm.MockSpent(hsSession.Name) {
		t.Fatal("precondition: a protocol-phase recording spent in A is still spent in B")
	}
	check("test B", []string{"b0", "hs-per-test"}, []string{"b0", "hs-session", "w0"})
	if sc.builds != 1 {
		t.Fatalf("precondition: the shared index was built %d times over two tests with one worker scope; want once (B reuses A's)", sc.builds)
	}
}
