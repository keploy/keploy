package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// shownTree stages n MySQL mocks filed under "k" (keyedTestIndex files a mock
// under its Noise), every tenth one protocol-phase (a recorder-tagged config
// mock), the rest data-plane recordings, in recorded order.
func shownTree(n int) (*TreeDb, []*models.Mock) {
	db := NewTreeDb(customComparator)
	ms := make([]*models.Mock, 0, n)
	for i := 1; i <= n; i++ {
		ms = append(ms, shownMock(db, i, i%10 == 0, "k"))
	}
	return db, ms
}

func shownMock(db *TreeDb, i int, phase bool, keys ...string) *models.Mock {
	mk := &models.Mock{Name: fmt.Sprintf("m%03d", i), Kind: models.MySQL, Noise: keys}
	if phase {
		mk.Spec.Metadata = map[string]string{"type": "config"}
	}
	mk.TestModeInfo = models.TestModeInfo{ID: i, SortOrder: int64(i)}
	db.insert(mk.TestModeInfo, mk)
	return mk
}

func shownWalk(v *exactView, db *TreeDb, key string) []string {
	var out []string
	v.rangeShownKeyed(db, keyedTestIndex, key, func(mk *models.Mock) bool {
		out = append(out, mk.Name)
		return true
	})
	return out
}

// shownOracle is the walk an exact view's keyed read made before the tree
// split its entries: every mock the tree files under key, in tree order,
// kept when the view shows it.
func shownOracle(v *exactView, db *TreeDb, key string) []string {
	var out []string
	db.rangeKeyed(keyedTestIndex, key, func(mk *models.Mock) bool {
		if v.shows(mk) {
			out = append(out, mk.Name)
		}
		return true
	})
	return out
}

func nameSet(set ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(set))
	for _, n := range set {
		out[n] = struct{}{}
	}
	return out
}

// TestAKeyedReadOfATestViewExaminesOnlyWhatItShows: under exact serving a
// keyed read of the staged trees (the MySQL matcher's PREPARE and CLOSE
// read) examines only the entries the view shows, never the open test's own
// recordings filed under the same key. Counted, not timed: on a tree of 300
// mocks under one key, 30 of them protocol-phase and one of the worker scope,
// 40 reads after the first examine 31 entries each and file nothing (main
// examined all 300 on every read), and give what main gave.
func TestAKeyedReadOfATestViewExaminesOnlyWhatItShows(t *testing.T) {
	db, _ := shownTree(300)
	v := &exactView{test: "T", worker: nameSet("m005")}
	want := shownOracle(v, db, "k")
	if len(want) != 31 {
		t.Fatalf("precondition: the view shows %d mocks under k; want 30 protocol-phase and one worker-scope", len(want))
	}
	if got := shownWalk(v, db, "k"); !slices.Equal(got, want) {
		t.Fatalf("the first read gives %v; want %v", got, want)
	}
	examined, builds := keyedExamined.Load(), shownBuilds.Load()
	for i := 0; i < 40; i++ {
		if got := shownWalk(v, db, "k"); !slices.Equal(got, want) {
			t.Fatalf("read %d gives %v; want %v", i, got, want)
		}
	}
	if n := keyedExamined.Load() - examined; n != 40*31 {
		t.Fatalf("40 keyed reads examined %d entries; want %d (31 a read: what the view shows)", n, 40*31)
	}
	if n := shownBuilds.Load() - builds; n != 0 {
		t.Fatalf("40 keyed reads at one generation filed what the view shows %d times; want 0", n)
	}
}

// TestANewViewFilesWhatItShowsByItsNames: a view installed at a tree
// generation another view already split looks up the few names it shows
// beside the protocol-phase mocks (its worker scope) instead of examining
// every other entry under the key: its first read on a tree of 300 under one
// key, 270 of them data-plane, examines its one worker-scope entry to file it
// and the 31 it gives. A view whose names outnumber those entries (a pool of
// 2,000 names) examines the 270 instead.
func TestANewViewFilesWhatItShowsByItsNames(t *testing.T) {
	db, _ := shownTree(300)
	shownWalk(&exactView{test: "T"}, db, "k")
	v := &exactView{test: "T", worker: nameSet("m005")}
	examined := keyedExamined.Load()
	if got := shownWalk(v, db, "k"); len(got) != 31 {
		t.Fatalf("the read gives %d mocks; want 31", len(got))
	}
	if n := keyedExamined.Load() - examined; n != 1+31 {
		t.Fatalf("a new view's first read examined %d entries; want %d (its one name, then what it gives)", n, 1+31)
	}
	pool := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		pool[fmt.Sprintf("p%04d", i)] = struct{}{}
	}
	pool["m001"] = struct{}{}
	v = &exactView{pool: pool}
	examined = keyedExamined.Load()
	if got := shownWalk(v, db, "k"); len(got) != 31 {
		t.Fatalf("the pool view's read gives %d mocks; want 31", len(got))
	}
	if n := keyedExamined.Load() - examined; n != 270+31 {
		t.Fatalf("a pool view's first read examined %d entries; want %d (the 270 data-plane entries, then what it gives)", n, 270+31)
	}
}

// TestAKeyedReadUnderExactServingExaminesNoOwnedRecording: the same through
// the manager's door. One test owns all 300 recordings, filed under "even"
// or "odd"; a keyed read gives the test's unanswered ones from the view and
// examines no entry of the staged trees, where main examined the 150 filed
// under the key on every read.
func TestAKeyedReadUnderExactServingExaminesNoOwnedRecording(t *testing.T) {
	mm, _, ix, _ := keyedSet(t, 300)
	if got := keyedWalk(t, mm, ix, "even"); len(got) != 150 {
		t.Fatalf("precondition: the first read gives %d mocks; want the test's 150 under even", len(got))
	}
	examined := keyedExamined.Load()
	for i := 0; i < 40; i++ {
		if got := keyedWalk(t, mm, ix, "even"); len(got) != 150 {
			t.Fatalf("read %d gives %d mocks; want 150", i, len(got))
		}
	}
	if n := keyedExamined.Load() - examined; n != 0 {
		t.Fatalf("40 keyed reads examined %d staged entries; want 0 (the view shows none of them)", n)
	}
}

// TestAKeyedReadFollowsEveryChangeThatStoresAMock: each change that stores a
// value in a tree moves its generation, so the next read files again what the
// view shows and gives what the tree holds: a re-stamp (found by key, and by
// the ID index), a swap in place, an insert, a clear. A removal moves nothing:
// the walk skips the entry. Each read is the walk before the split.
func TestAKeyedReadFollowsEveryChangeThatStoresAMock(t *testing.T) {
	db, ms := shownTree(60)
	v := &exactView{test: "T"}
	check := func(step string, wantBuilds bool) {
		t.Helper()
		builds := shownBuilds.Load()
		got, want := shownWalk(v, db, "k"), shownOracle(v, db, "k")
		if !slices.Equal(got, want) {
			t.Fatalf("%s: a keyed read gives %v; want %v", step, got, want)
		}
		if n := shownBuilds.Load() - builds; (n != 0) != wantBuilds {
			t.Fatalf("%s: the read filed what the view shows %d times; want rebuilt=%v", step, n, wantBuilds)
		}
	}
	check("first read", true)
	check("same generation", false)

	old := ms[9] // m010, protocol-phase
	upd := *old
	upd.Name = "m010-restamped"
	upd.TestModeInfo.SortOrder = 1000
	if !db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old) {
		t.Fatal("re-stamp failed")
	}
	check("re-stamp found by key", true)

	old = ms[19] // m020, by its ID: the key the caller has is stale
	stale := old.TestModeInfo
	stale.SortOrder = -1
	upd2 := *old
	upd2.Name = "m020-restamped"
	upd2.TestModeInfo.SortOrder = 1001
	if !db.update(stale, upd2.TestModeInfo, &upd2, *old) {
		t.Fatal("re-stamp by ID failed")
	}
	check("re-stamp found by ID", true)

	old = ms[29]
	sw := *old
	sw.Name = "m030-swapped"
	if !db.swapValue(old.TestModeInfo, old, &sw) {
		t.Fatal("swap failed")
	}
	check("swap in place", true)

	shownMock(db, 500, true, "k")
	check("insert", true)

	if !db.removeValue(ms[39].TestModeInfo, ms[39]) {
		t.Fatal("remove failed")
	}
	check("removal", false)

	db.deleteAll()
	check("clear", true)
	if got := shownWalk(v, db, "k"); len(got) != 0 {
		t.Fatalf("a cleared tree gives %v", got)
	}
}

// TestAKeyedReadOfAPoolViewIsTheWalkBeforeTheSplit: a view between tests
// shows its pool slice as well as the protocol-phase mocks, and a test view
// under the background rules the background's pool, async lanes aside;
// whether the view looks its names up or examines the rest, it gives what the
// walk before the split gave.
func TestAKeyedReadOfAPoolViewIsTheWalkBeforeTheSplit(t *testing.T) {
	db, ms := shownTree(60)
	ms[3].Spec.Async = &models.AsyncMeta{}
	many := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		many[fmt.Sprintf("m%03d", i)] = struct{}{}
	}
	for _, v := range []*exactView{
		{pool: nameSet("m001", "m002", "m011")},
		{pool: many},
		{test: "T", bg: &backgroundState{pool: nameSet("m003", "m004", "m012")}},
		{test: "T", bg: &backgroundState{pool: many}, worker: nameSet("m007")},
	} {
		got, want := shownWalk(v, db, "k"), shownOracle(v, db, "k")
		if !slices.Equal(got, want) || len(want) <= 6 {
			t.Fatalf("view test=%q pool=%d bg=%v: a keyed read gives %v; want %v", v.test, len(v.pool), v.bg != nil, got, want)
		}
		if slices.Contains(got, "m004") && v.test != "" {
			t.Fatalf("a test view gives the async background recording m004: %v", got)
		}
	}
}

// TestAShownWalkSurvivesAReStampMidWalk: fn runs without the tree's lock and
// may re-stamp a mock it was given; the walk then reaches the new copy at its
// place, as the walk before the split does.
func TestAShownWalkSurvivesAReStampMidWalk(t *testing.T) {
	db, ms := shownTree(400)
	v := &exactView{test: "T"}
	var got []string
	v.rangeShownKeyed(db, keyedTestIndex, "k", func(mk *models.Mock) bool {
		got = append(got, mk.Name)
		if len(got) == 1 {
			old := ms[9]
			upd := *old
			upd.Name = "m010-restamped"
			upd.TestModeInfo.SortOrder = 5000
			if !db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old) {
				t.Fatal("re-stamp failed")
			}
		}
		return true
	})
	if len(got) != 41 || got[0] != "m010" || got[40] != "m010-restamped" {
		t.Fatalf("the walk gives %v; want m010, the other 39 protocol-phase mocks, then m010-restamped", got)
	}
}

// TestAShownKeyedReadIsTheWalkBeforeTheSplitUnderRandomChanges: random
// re-stamps (forward, back, by ID), swaps, inserts, removals, spends and new
// views, under the verify mode, with a read of a random key between them
// compared to the walk before the split.
func TestAShownKeyedReadIsTheWalkBeforeTheSplitUnderRandomChanges(t *testing.T) {
	if !exactViewVerify.Load() {
		t.Fatal("precondition: the rows run with the verify mode on")
	}
	rng := rand.New(rand.NewSource(11))
	db := NewTreeDb(customComparator)
	keysets := [][]string{{"k"}, {"j"}, {"k", "j"}}
	live := map[string]*models.Mock{}
	next := 1
	add := func() {
		mk := shownMock(db, next, rng.Intn(3) == 0, keysets[rng.Intn(3)]...)
		live[mk.Name] = mk
		next++
	}
	for i := 0; i < 80; i++ {
		add()
	}
	pick := func() *models.Mock {
		ns := make([]string, 0, len(live))
		for n := range live {
			ns = append(ns, n)
		}
		slices.Sort(ns)
		return live[ns[rng.Intn(len(ns))]]
	}
	randNames := func() map[string]struct{} {
		out := map[string]struct{}{}
		for i := rng.Intn(next); i > 0; i-- {
			out[fmt.Sprintf("m%03d", 1+rng.Intn(next))] = struct{}{}
		}
		return out
	}
	newView := func() *exactView {
		v := &exactView{worker: randNames()}
		switch rng.Intn(3) {
		case 0:
			v.pool = randNames()
		case 1:
			v.test = "T"
		default:
			v.test = "T"
			v.bg = &backgroundState{pool: randNames()}
		}
		return v
	}
	v := newView()
	for step := 0; step < 3000; step++ {
		switch op := rng.Intn(20); {
		case op < 8:
			key := []string{"k", "j", "none"}[rng.Intn(3)]
			stopAt := rng.Intn(40)
			var got []string
			v.rangeShownKeyed(db, keyedTestIndex, key, func(mk *models.Mock) bool {
				got = append(got, mk.Name)
				return len(got) != stopAt
			})
			want := shownOracle(v, db, key)
			if stopAt > 0 && len(want) > stopAt {
				want = want[:stopAt]
			}
			if !slices.Equal(got, want) {
				t.Fatalf("step %d: a keyed read under %s gives %v; want %v", step, key, got, want)
			}
		case op < 11:
			old := pick()
			upd := *old
			upd.TestModeInfo.SortOrder = old.TestModeInfo.SortOrder + int64(rng.Intn(200)-100)
			from := old.TestModeInfo
			if rng.Intn(3) == 0 {
				from.SortOrder = -7 // found by its ID
			}
			if db.update(from, upd.TestModeInfo, &upd, *old) {
				live[upd.Name] = &upd
			}
		case op < 13:
			old := pick()
			sw := *old
			if db.swapValue(old.TestModeInfo, old, &sw) {
				live[sw.Name] = &sw
			}
		case op < 15:
			add()
		case op < 16:
			if len(live) > 10 {
				old := pick()
				if db.removeValue(old.TestModeInfo, old) {
					delete(live, old.Name)
				}
			}
		case op < 18:
			mk := pick()
			v.mu.Lock()
			if v.spent == nil {
				v.spent = map[string]bool{}
			}
			v.spent[mk.Name] = true
			v.mu.Unlock()
		default:
			v = newView()
		}
	}
}

// TestTheVerifyModeRefusesAStaleShownEntry: the verify mode is what catches a
// kept split gone stale. A view whose extras hold a recording it does not
// show panics on the read, and leaves the tree unlocked; with the verify mode
// off, the same read gives the stale recording, the false answer the mode
// exists to stop.
func TestTheVerifyModeRefusesAStaleShownEntry(t *testing.T) {
	db, ms := shownTree(30)
	v := &exactView{test: "T"}
	shownWalk(v, db, "k")
	at := shownAt{db, keyedTestIndex, "k"}
	v.shown[at].extra = append(v.shown[at].extra, keyedEntry{ms[0].TestModeInfo, ms[0]})
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "exact view verify") {
				t.Fatalf("a stale shown entry read under the verify mode: recovered %v; want the verify mode's panic", r)
			}
		}()
		shownWalk(v, db, "k")
	}()
	if !db.mu.TryLock() {
		t.Fatal("the verify mode's panic left the tree locked")
	}
	db.mu.Unlock()
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false)
	if got := shownWalk(v, db, "k"); !slices.Contains(got, "m001") {
		t.Fatalf("precondition: the stale read gives %v", got)
	}
}
