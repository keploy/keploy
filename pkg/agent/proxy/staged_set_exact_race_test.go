package proxy

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// exactRaceSet is a manager under the proven switch whose two sets, a and b,
// the lock-free exact reads read through the view serve installs: stage
// stages a set's trees, each set's per-test recordings being one the view's
// pool or worker scope names (X-pt), one it does not (X-else) and a
// protocol-phase one (X-hs), its session tier a pool recording (X-pool). The
// views name both sets' recordings, so a view reads alike over either set,
// and only the set read tells the two apart. A set's staging starts the set
// (models.BaseTime): its per-test recordings are filed in the startup tier;
// windowed, it is a staging in the set, of a test's window, which files them
// in the per-test tier and moves no staging epoch. Set b alone has a
// recording of a kind set a has none of (b-kind, models.GENERIC), which the
// views name too: a read of that kind over set a reads no tree of the live
// trees, where set b's is already in place as set b is staged.
func exactRaceSet(t *testing.T, windowed bool, serve func(mm *MockManager)) (mm *MockManager, stage func(set string)) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm = NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	start, end := models.BaseTime, time.Now().Add(time.Hour)
	if windowed {
		start, end = exactT0, exactT0.Add(10*time.Second)
	}
	stage = func(set string) {
		hs := exactMock(set+"-hs", 3*time.Second, models.LifetimePerTest)
		hs.Spec.Metadata = map[string]string{"type": "config"}
		perTest := []*models.Mock{exactMock(set+"-pt", 2*time.Second, models.LifetimePerTest), exactMock(set+"-else", 4*time.Second, models.LifetimePerTest), hs}
		if set == "b" {
			kind := exactMock("b-kind", 2*time.Second, models.LifetimePerTest)
			kind.Kind = models.GENERIC
			perTest = append(perTest, kind)
		}
		mm.SetMocksWithWindow(
			perTest,
			[]*models.Mock{exactMock(set+"-pool", time.Second, models.LifetimeSession)},
			start, end)
		serve(mm)
	}
	return mm, stage
}

// raceNames is ms by name, sorted, or the error.
func raceNames(ms []*models.Mock, err error) string {
	if err != nil {
		return "error " + err.Error()
	}
	n := exactNames(ms)
	sort.Strings(n)
	return strings.Join(n, ",")
}

// exactReads is what each exact read that takes no staging lock gives in the
// view in force: the per-test getters (GetFilteredMocks,
// GetFilteredMocksByKind, of HTTP and of the kind only set b has,
// RangePerTestMocksInWindow), the background readers (Background,
// KindBackground) and SharedMocks.
func exactReads(mm *MockManager) string {
	var ranged []*models.Mock
	err := mm.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
		ranged = append(ranged, mk)
		return true
	})
	unused, spent := mm.KindBackground(models.HTTP)
	pt, se, ok := mm.SharedMocks()
	return fmt.Sprintf("per-test %s; by kind %s; of b's kind %s; ranged %s; background %s; by kind %s spent %s; shared %v: %s / %s",
		raceNames(mm.GetFilteredMocks()), raceNames(mm.GetFilteredMocksByKind(models.HTTP)), raceNames(mm.GetFilteredMocksByKind(models.GENERIC)), raceNames(ranged, err),
		raceNames(mm.Background(models.HTTP), nil), raceNames(unused, nil), raceNames(spent, nil), ok, raceNames(pt, nil), raceNames(se, nil))
}

// missNow is a miss's diagnosis (FindStagedNotOwned) for a call whose first
// read is now, of a recording made elsewhere: the per-test tier's that no
// view names (X-else).
func missNow(mm *MockManager) string {
	name, restaged := mm.FindStagedNotOwned(models.HTTP, mm.MarkCallRound(), func(mk *models.Mock) bool {
		return strings.HasSuffix(mk.Name, "-else")
	})
	return fmt.Sprintf("named %q, restaged %v", name, restaged)
}

// raceViews are the views a lock-free exact read reads the staged set
// through: a pool view and a test's under the background rules, a test's
// that shows the worker scope it reached (its per-test reads take that of
// the startup tier), the empty pool view the agent installs before it hands a
// set's sources and stages it (agent's exact staging), in force until the
// set's own pool view, and a pool view and a test's over a staging in the set
// (windowed), whose per-test reads read the per-test tier.
var raceViews = []struct {
	name     string
	windowed bool
	serve    func(mm *MockManager)
}{
	{"pool", false, racePoolView},
	{"test", false, raceTestView},
	{"worker", false, func(mm *MockManager) {
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"a-pool", "b-pool"}, []string{"a-pt", "b-pt", "b-kind"})
		mm.ServeExactWorker([]string{"a-pt", "b-pt", "b-kind"})
		mm.ServeExactTest("T", nil)
	}},
	{"staging", false, func(mm *MockManager) {
		mm.StageExactBackground(models.RecordedWithS12Routing)
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}},
	{"pool, staged in a window", true, racePoolView},
	{"test, staged in a window", true, raceTestView},
}

var raceBackground = []string{"a-pt", "a-pool", "b-pt", "b-kind", "b-pool"}

func racePoolView(mm *MockManager) {
	mm.ServeExactBackground(models.RecordedWithS12Routing, raceBackground, nil)
	mm.ServeExactPool(models.PoolKeyNone, raceBackground)
}

func raceTestView(mm *MockManager) {
	mm.ServeExactBackground(models.RecordedWithS12Routing, raceBackground, nil)
	mm.ServeExactTest("T", nil)
}

// Each exact read that takes no staging lock races the next set's staging,
// read from another goroutine as the staging publishes set b: the per-test
// getters, the background readers, SharedMocks and a miss's diagnosis. Each
// loads the set staged once (stagedSet) and reads its trees from it, so it
// reads set a whole until set b is published, and set b whole once it is,
// though every tree in place is already set b's. A miss's diagnosis is a
// call's, whose staging is the one its first read saw: a call whose first
// read saw set b's staging begin is told restaged (it names nothing) until
// set b is published, never named one of set a's recordings as set b's. A
// staging in the set moves no epoch: a miss read before it publishes names
// set a's recording, from set a's trees.
//
// The view is not part of the set: the agent installs a set's pool view,
// which names the set's recordings, only once the set is staged, and every
// test's open and close installs another while the set stays staged. A read
// between the set's publish and its pool view reads the new set through the
// empty pool view the agent installed before it handed the set's sources,
// which shows no recording of either set to these reads (the "staging"
// view); a miss there names the new set's.
func TestALockFreeExactReadRacingTheNextSetsStagingReadsOneSet(t *testing.T) {
	for _, view := range raceViews {
		t.Run(view.name, func(t *testing.T) {
			// What each set reads alone, each on a manager of its own: a
			// miss names a recording once in a staging epoch, and a staging
			// in the set moves none.
			setOf, missOf := map[string]string{}, map[string]string{}
			for _, set := range []string{"a", "b"} {
				mm, stage := exactRaceSet(t, view.windowed, view.serve)
				stage(set)
				setOf[set], missOf[set] = exactReads(mm), missNow(mm)
			}
			if view.name == "staging" {
				// What a read between a set's publish and its pool view sees.
				if want := "per-test ; by kind ; of b's kind ; ranged ; background ; by kind  spent ; shared false:  / "; setOf["a"] != want || setOf["b"] != want {
					t.Errorf("the staging's empty pool view read %s over set a, %s over set b; want %s: no recording of either set", setOf["a"], setOf["b"], want)
				}
			} else if setOf["a"] == setOf["b"] {
				t.Fatalf("sets a and b read alike (%s): the row tells neither", setOf["a"])
			}
			if missOf["a"] == missOf["b"] {
				t.Fatalf("the misses of sets a and b read alike (%s): the row tells neither", missOf["a"])
			}
			if view.windowed && !strings.Contains(setOf["b"], "of b's kind b-kind;") {
				t.Fatalf("set b's per-test tier read none of its kind set a has none of (%s): the row tells no read of that kind", setOf["b"])
			}
			mm, stage := exactRaceSet(t, view.windowed, view.serve)
			stage("a")
			got, miss := map[bool]string{}, map[bool]string{}
			mm.exact.onPublish = func(published bool) {
				done := make(chan [2]string, 1)
				go func() { done <- [2]string{exactReads(mm), missNow(mm)} }()
				select {
				case r := <-done:
					got[published], miss[published] = r[0], r[1]
				case <-time.After(5 * time.Second):
					got[published] = "no answer: the read waited for the staging"
				}
			}
			stage("b")
			mm.exact.onPublish = nil
			for _, c := range []struct {
				published bool
				set       string
			}{{false, "a"}, {true, "b"}} {
				if got[c.published] != setOf[c.set] {
					t.Errorf("a read as set b's staging had published its set: %v: %s; want set %s's: %s", c.published, got[c.published], c.set, setOf[c.set])
				}
			}
			want := `named "", restaged true`
			if view.windowed {
				want = missOf["a"]
			}
			if miss[false] != want {
				t.Errorf("a miss of a call of set b's staging read before set b was published: %s; want %s", miss[false], want)
			}
			if miss[true] != missOf["b"] {
				t.Errorf("a miss of a call of set b's staging read once set b was published: %s; want %s", miss[true], missOf["b"])
			}
		})
	}
}

// A spent background read (SpentBackground) racing a staging in the set
// reads one staging's recordings against the staging epoch's spends, which a
// staging in the set keeps (it moves no epoch): set a's test spent a's pool
// recording, so a read before set b's trees are published lists it, and one
// after lists none of set b's, which nothing spent. A set's staging starts a
// new epoch first, and no recording of either set is spent in it before its
// publish (bumpStagingEpoch).
func TestASpentBackgroundReadRacingAStagingInTheSetReadsOneStaging(t *testing.T) {
	mm, stage := exactRaceSet(t, true, raceTestView)
	stage("a")
	if got := pollFirst(t, mm, "a-pool"); got != "a-pool" {
		t.Fatalf("precondition: set a's pool recording did not answer in the test: %s", got)
	}
	if got := raceNames(mm.SpentBackground(models.HTTP), nil); got != "a-pool" {
		t.Fatalf("precondition: set a's spent background is %q; want a-pool", got)
	}
	got := map[bool]string{}
	mm.exact.onPublish = func(published bool) {
		done := make(chan string, 1)
		go func() { done <- raceNames(mm.SpentBackground(models.HTTP), nil) }()
		select {
		case got[published] = <-done:
		case <-time.After(5 * time.Second):
			got[published] = "no answer: the read waited for the staging"
		}
	}
	// The staging alone: the view stays set a's test's.
	mm.SetMocksWithWindow(nil, []*models.Mock{exactMock("b-pool", time.Second, models.LifetimeSession)}, exactT0, exactT0.Add(10*time.Second))
	mm.exact.onPublish = nil
	if got[false] != "a-pool" || got[true] != "" {
		t.Errorf("the spent background read before set b's trees were published: %q, after: %q; want a-pool, then none", got[false], got[true])
	}
}
