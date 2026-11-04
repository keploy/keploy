package proxy

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// teardownRaceSet is a manager under keploy mock replay's pool names whose
// sets a and b tell apart only by what bounds a test process's teardown, as
// the per-test reads of a pool view after a test read it (perTestFromStartup,
// GetFilteredMocks): each set's startup tier holds a pool recording of the
// per-test lifetime made at 30s (X-pool-pt), which such a read takes once it
// is after every recording of the set outside the pool. Set a's only other
// recording is on disk, at 5s (its times, SetRecordingTimes): its teardown
// is a-pool-pt. Set b's are one in its trees, at 40s (b-late), and one on
// disk, at 50s: its teardown is none. hand hands a set's times; stage stages
// its trees and serves a test, then the pool after it.
func teardownRaceSet(t *testing.T) (mm *MockManager, hand, stage func(set string), read func() string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm = NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	pool := []string{"a-pool-pt", "a-pool", "b-pool-pt", "b-pool"}
	mm.SetPoolNames(pool)
	disk := map[string]*models.Mock{
		"a": exactMock("a-disk", 5*time.Second, models.LifetimePerTest),
		"b": exactMock("b-disk", 50*time.Second, models.LifetimePerTest),
	}
	hand = func(set string) {
		mk := disk[set]
		mm.SetRecordingTimes(func(visit func(string, time.Time, bool, string) bool) error {
			visit(mk.Name, models.PlaceOf(mk), false, "")
			return nil
		})
	}
	stage = func(set string) {
		perTest := []*models.Mock{exactMock(set+"-pool-pt", 30*time.Second, models.LifetimePerTest)}
		if set == "b" {
			perTest = append(perTest, exactMock("b-late", 40*time.Second, models.LifetimePerTest))
		}
		mm.SetMocksWithWindow(perTest, []*models.Mock{exactMock(set+"-pool", time.Second, models.LifetimeSession)}, models.BaseTime, time.Now())
		mm.ServeExactBackground(models.RecordedWithS12Routing, pool, nil)
		mm.ServeExactTest("T", nil)
		mm.ServeExactPool(models.PoolKeyNone, pool)
	}
	read = func() string { return raceNames(mm.GetFilteredMocks()) }
	return mm, hand, stage, read
}

// raceRead is read's answer from another goroutine, as a parser's: a read
// that waited for the staging under way would never answer.
func raceRead(read func() string) string {
	done := make(chan string, 1)
	go func() { done <- read() }()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		return "no answer: the read waited for the staging"
	}
}

// A per-test read of a pool view after a test reads the teardown of one set
// (perTestFromStartup): the set it loaded once, its startup tree, its trees
// and the times published with them, never the times as the agent handed
// them for the next set, nor the trees the next set's staging has put in
// place before it publishes them. Each window is read on a manager of its
// own, with no teardown bound cached: the read reads the set anew there.
func TestAPerTestTeardownReadBetweenAHandingAndAStagingReadsOneSet(t *testing.T) {
	setOf := map[string]string{}
	for _, set := range []string{"a", "b"} {
		_, hand, stage, read := teardownRaceSet(t)
		hand(set)
		stage(set)
		setOf[set] = read()
	}
	if setOf["a"] != "a-pool-pt" || setOf["b"] != "" {
		t.Fatalf("precondition: set a's teardown read %q, set b's %q; want a-pool-pt, then none", setOf["a"], setOf["b"])
	}
	t.Run("handed", func(t *testing.T) {
		mm, hand, stage, read := teardownRaceSet(t)
		hand("a")
		stage("a")
		mm.exact.teardownLast.Store(nil)
		hand("b")
		if got := read(); got != setOf["a"] {
			t.Errorf("a read between set b's handing and its staging: %q; want set a's: %q", got, setOf["a"])
		}
	})
	t.Run("staging", func(t *testing.T) {
		mm, hand, stage, read := teardownRaceSet(t)
		hand("a")
		stage("a")
		mm.exact.teardownLast.Store(nil)
		hand("b")
		got := map[bool]string{}
		mm.exact.onPublish = func(published bool) { got[published] = raceRead(read) }
		stage("b")
		mm.exact.onPublish = nil
		if got[false] != setOf["a"] || got[true] != setOf["b"] {
			t.Errorf("a read as set b's staging published its set: before %q, after %q; want set a's %q, then set b's %q", got[false], got[true], setOf["a"], setOf["b"])
		}
	})
}

// The teardown bound cached (teardownBound) is one publish's: a staging in
// the set (of a test's window) keeps the set's times and moves no generation
// before it publishes its trees, so a read in its publish-to-generation
// window that took the bound cached before would read the trees it put in
// place with the bound of the trees it replaced. Here the staging's per-test
// tier holds a recording outside the pool at 40s: set a's bound is its
// recording on disk at 5s before it, so a call at 35s is a teardown; once the
// staging is published the bound is 40s, and that call is not.
func TestATeardownReadRacingAStagingInTheSetReadsItsBound(t *testing.T) {
	mm, hand, stage, _ := teardownRaceSet(t)
	hand("a")
	stage("a")
	read := func() string {
		after := mm.teardownAfter("", "")
		return fmt.Sprintf("a call at 35s is a teardown: %v", after != nil && after(exactT0.Add(35*time.Second)))
	}
	before := "a call at 35s is a teardown: true"
	if got := read(); got != before {
		t.Fatalf("precondition: set a's bound: %s", got)
	}
	if mm.exact.teardownLast.Load() == nil {
		t.Fatal("precondition: the read cached no teardown bound")
	}
	gen := mm.StagingGeneration()
	got, genAt := map[bool]string{}, map[bool]uint64{}
	mm.exact.onPublish = func(published bool) {
		genAt[published] = mm.StagingGeneration()
		got[published] = raceRead(read)
	}
	mm.SetMocksWithWindow([]*models.Mock{exactMock("w-late", 40*time.Second, models.LifetimePerTest)},
		[]*models.Mock{exactMock("a-pool", time.Second, models.LifetimeSession)}, exactT0.Add(35*time.Second), exactT0.Add(45*time.Second))
	mm.exact.onPublish = nil
	if genAt[true] != gen {
		t.Fatalf("precondition: the staging generation moved from %d to %d before the set was published", gen, genAt[true])
	}
	after := "a call at 35s is a teardown: false"
	if got[false] != before || got[true] != after || read() != after {
		t.Errorf("a teardown read as the staging in the set published its trees: before %q, after %q, once it returned %q; want %q, then %q", got[false], got[true], read(), before, after)
	}
}

// A bare tier setter (SetFilteredMocks, SetUnFilteredMocks: Proxy.SetMocks)
// is a whole staging: it publishes the set with its tree before any
// generation moves, so a reader that read the new generation reads the new
// set.
func TestABareTierSetterPublishesTheSetBeforeTheGenerationsMove(t *testing.T) {
	for _, tier := range []string{"filtered", "unfiltered"} {
		t.Run(tier, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			gen, rev, kind := mm.StagingGeneration(), mm.Revision(), mm.RevisionByKind(models.HTTP)
			type at struct{ gen, rev, kind uint64 }
			seen := map[bool]at{}
			mm.exact.onPublish = func(published bool) {
				seen[published] = at{mm.StagingGeneration(), mm.Revision(), mm.RevisionByKind(models.HTTP)}
			}
			mocks := []*models.Mock{exactMock("m-0", time.Second, models.LifetimeSession)}
			if tier == "filtered" {
				mm.SetFilteredMocks(mocks)
			} else {
				mm.SetUnFilteredMocks(mocks)
			}
			mm.exact.onPublish = nil
			want := at{gen, rev, kind}
			if len(seen) != 2 || seen[false] != want || seen[true] != want {
				t.Errorf("the generations (staging, revision, HTTP's) were %v before the setter, %v and %v as it published its set; want none moved before the publish", want, seen[false], seen[true])
			}
			if mm.StagingGeneration() == gen || mm.Revision() == rev || mm.RevisionByKind(models.HTTP) == kind {
				t.Errorf("the setter left the generations at %d, %d, %d (from %v): they must move once the set is published", mm.StagingGeneration(), mm.Revision(), mm.RevisionByKind(models.HTTP), want)
			}
			tree := mm.stagedNow().filtered
			if tier == "unfiltered" {
				tree = mm.stagedNow().unfiltered
			}
			if names := exactNames(mm.treeValues(tree, "")); len(names) != 1 || names[0] != "m-0" {
				t.Errorf("the set published holds %v in its %s tree; want m-0", names, tier)
			}
		})
	}
}
