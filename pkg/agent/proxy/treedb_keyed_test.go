package proxy

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keyedTestIndex files a mock under the keys its Noise lists, so a test can
// give each mock any keys it likes, duplicates included.
var keyedTestIndex = &integrations.MockIndex{Keys: func(m *models.Mock) []string { return m.Noise }}

// walkKeyed is the definition rangeKeyed must meet: the tree's mocks in tree
// order, those keyedTestIndex files under key.
func walkKeyed(db *TreeDb, key string) []*models.Mock {
	var out []*models.Mock
	db.rangeValues(func(v interface{}) bool {
		mk := v.(*models.Mock)
		for _, k := range mk.Noise {
			if k == key {
				out = append(out, mk)
				break
			}
		}
		return true
	})
	return out
}

func rangedKeyed(db *TreeDb, key string) []*models.Mock {
	var out []*models.Mock
	db.rangeKeyed(keyedTestIndex, key, func(mk *models.Mock) bool {
		out = append(out, mk)
		return true
	})
	return out
}

// A key index must give what a walk of the tree gives, through everything a
// replay does to a tree between two stagings: re-stamps that move a mock to the
// back (the index follows them), updates it cannot follow (a move forward, a
// mock stored again under its own key, a change of keys), deletes and inserts.
func TestKeyIndexIsTheTreeFilteredByKey(t *testing.T) {
	keys := []string{"a", "b", "c"}
	for seed := int64(1); seed <= 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		db := NewTreeDb(customComparator)
		var live []*models.Mock
		sort := int64(0)
		newMock := func() *models.Mock {
			sort++
			mk := &models.Mock{Name: fmt.Sprintf("m%d", sort), Kind: models.MySQL}
			for _, k := range keys {
				if r.Intn(3) == 0 {
					mk.Noise = append(mk.Noise, k)
				}
			}
			if r.Intn(8) == 0 && len(mk.Noise) > 0 {
				mk.Noise = append(mk.Noise, mk.Noise[0]) // a key listed twice
			}
			mk.TestModeInfo = models.TestModeInfo{ID: int(sort), SortOrder: sort}
			return mk
		}
		for i := 0; i < 60; i++ {
			mk := newMock()
			db.insert(mk.TestModeInfo, mk)
			live = append(live, mk)
		}
		check := func(step string) {
			t.Helper()
			for _, k := range keys {
				want, got := walkKeyed(db, k), rangedKeyed(db, k)
				if fmt.Sprint(keyedNames(want)) != fmt.Sprint(keyedNames(got)) {
					t.Fatalf("seed %d, %s, key %q:\n got  %v\n want %v", seed, step, k, keyedNames(got), keyedNames(want))
				}
			}
		}
		check("built")
		for op := 0; op < 200; op++ {
			if len(live) == 0 {
				break
			}
			i := r.Intn(len(live))
			old := live[i]
			switch r.Intn(7) {
			case 0, 1, 2: // a matcher's re-stamp: a copy at the back
				sort++
				upd := *old
				upd.TestModeInfo.SortOrder = sort
				db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old)
				live[i] = &upd
			case 3: // moved forward
				upd := *old
				upd.TestModeInfo.SortOrder = -int64(op)
				db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old)
				live[i] = &upd
			case 4: // stored again under its own key, with other keys
				upd := *old
				upd.Noise = []string{keys[r.Intn(len(keys))]}
				db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old)
				live[i] = &upd
			case 5:
				db.deleteMock(old.TestModeInfo, *old)
				live = append(live[:i], live[i+1:]...)
			default:
				mk := newMock()
				db.insert(mk.TestModeInfo, mk)
				live = append(live, mk)
			}
			check(fmt.Sprintf("op %d", op))
		}
	}
}

func keyedNames(ms []*models.Mock) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// RangeSessionMocksWithKey must give GetSessionMocks filtered by the key, in its
// order: the startup tier first, then the session tier, a pointer only once
// when the startup tier holds anything (BaseTime staging puts session mocks in
// both), and duplicates kept when it does not.
func TestRangeSessionMocksWithKeyIsGetSessionMocksFilteredByKey(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	for seed := int64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		var perTest, session []*models.Mock
		for i := 0; i < 80; i++ {
			mk := newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(r.Intn(100))*time.Millisecond), models.LifetimeSession)
			if r.Intn(2) == 0 {
				mk.Noise = []string{"k"}
			}
			if r.Intn(5) == 0 {
				mk.TestModeInfo.Lifetime = models.LifetimePerTest
				perTest = append(perTest, mk)
				continue
			}
			session = append(session, mk)
		}
		session = append(session, session[3]) // listed twice
		mm := NewMockManager(nil, nil, zap.NewNop())
		for _, staging := range []struct {
			name       string
			start, end time.Time
		}{
			{"BaseTime staging", models.BaseTime, time.Now()},
			{"first window", base.Add(50 * time.Millisecond), base.Add(60 * time.Millisecond)},
		} {
			mm.SetMocksWithWindow(perTest, session, staging.start, staging.end)
			all, _ := mm.GetSessionMocks()
			var want []*models.Mock
			for _, mk := range all {
				if len(mk.Noise) > 0 {
					want = append(want, mk)
				}
			}
			var got []*models.Mock
			if err := mm.RangeSessionMocksWithKey(keyedTestIndex, "k", func(mk *models.Mock) bool {
				got = append(got, mk)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("seed %d, %s: got %d mocks, want %d", seed, staging.name, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("seed %d, %s: mock %d is %s, want %s", seed, staging.name, i, got[i].Name, want[i].Name)
				}
			}
			// Stopping early stops the walk.
			n := 0
			_ = mm.RangeSessionMocksWithKey(keyedTestIndex, "k", func(*models.Mock) bool { n++; return n < 3 })
			if len(want) >= 3 && n != 3 {
				t.Fatalf("seed %d, %s: the walk went on after fn returned false (%d calls)", seed, staging.name, n)
			}
		}
		mm.Close()
	}
}

// A walk that a re-stamp interrupts between two batches delivers every mock
// that stayed live and in place. The re-stamp trims the front of the list the
// walk reads, so a walk that resumed at a list position skipped the entries
// that shifted under it.
func TestKeyedWalkSurvivesAReStampMidWalk(t *testing.T) {
	db := NewTreeDb(customComparator)
	var ms []*models.Mock
	for i := 1; i <= 40; i++ {
		mk := &models.Mock{Name: fmt.Sprintf("m%02d", i), Kind: models.MySQL, Noise: []string{"k"}}
		mk.TestModeInfo = models.TestModeInfo{ID: i, SortOrder: int64(i)}
		db.insert(mk.TestModeInfo, mk)
		ms = append(ms, mk)
	}
	var got []string
	calls := 0
	db.rangeKeyed(keyedTestIndex, "k", func(mk *models.Mock) bool {
		calls++
		got = append(got, mk.Name)
		if calls == 1 {
			// Another connection's matcher serves the front mock: a re-stamp.
			old := ms[0]
			upd := *old
			upd.Name = "m01-restamped"
			upd.TestModeInfo.SortOrder = 1000
			if !db.update(old.TestModeInfo, upd.TestModeInfo, &upd, *old) {
				t.Fatal("update failed")
			}
		}
		return true
	})
	want := []string{"m01"}
	for _, mk := range ms[1:] {
		want = append(want, mk.Name)
	}
	want = append(want, "m01-restamped") // the tier as the walk reaches its end
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk delivered\n %v\nwant\n %v", got, want)
	}
}

// A re-stamp moves its mock's entry, wherever it is in the list, so no dead
// entry is left behind for a walk to step over: the lists hold what the tree
// holds, even behind a live entry at the front that no match takes.
func TestKeyedListHoldsOnlyLiveEntries(t *testing.T) {
	db := NewTreeDb(customComparator)
	stuck := &models.Mock{Name: "stuck", Kind: models.MySQL, Noise: []string{"k"}}
	stuck.TestModeInfo = models.TestModeInfo{ID: 1, SortOrder: 1}
	hot := &models.Mock{Name: "hot", Kind: models.MySQL, Noise: []string{"k"}}
	hot.TestModeInfo = models.TestModeInfo{ID: 2, SortOrder: 2}
	db.insert(stuck.TestModeInfo, stuck)
	db.insert(hot.TestModeInfo, hot)
	walk := func() []string {
		var got []string
		db.rangeKeyed(keyedTestIndex, "k", func(mk *models.Mock) bool {
			got = append(got, mk.Name)
			return true
		})
		return got
	}
	walk() // builds the index
	for i := 0; i < 100; i++ {
		upd := *hot
		upd.TestModeInfo.SortOrder = int64(10 + i)
		if !db.update(hot.TestModeInfo, upd.TestModeInfo, &upd, *hot) {
			t.Fatalf("update %d failed", i)
		}
		hot = &upd
		db.mu.RLock()
		n := len(db.keyed[keyedTestIndex].lists["k"])
		db.mu.RUnlock()
		if n != 2 {
			t.Fatalf("after update %d the list holds %d entries for the 2 mocks the tree holds", i, n)
		}
	}
	if got := walk(); fmt.Sprint(got) != "[stuck hot]" {
		t.Fatalf("walk = %v, want [stuck hot]", got)
	}
}

// Two connections take sort numbers and then their re-stamps land in the other
// order: the second lands in front of the first's entry. The index follows it
// rather than drop and rebuild itself over the whole tree.
func TestKeyedIndexFollowsReStampsThatLandOutOfOrder(t *testing.T) {
	db := NewTreeDb(customComparator)
	var ms []*models.Mock
	for i := 1; i <= 5; i++ {
		mk := &models.Mock{Name: fmt.Sprintf("m%d", i), Kind: models.MySQL, Noise: []string{"k"}}
		mk.TestModeInfo = models.TestModeInfo{ID: i, SortOrder: int64(i)}
		db.insert(mk.TestModeInfo, mk)
		ms = append(ms, mk)
	}
	db.rangeKeyed(keyedTestIndex, "k", func(*models.Mock) bool { return true }) // builds the index
	built := db.keyed[keyedTestIndex]
	for _, step := range []struct {
		mk   *models.Mock
		sort int64
	}{{ms[1], 101}, {ms[3], 100}} { // the later sort number lands first
		upd := *step.mk
		upd.TestModeInfo.SortOrder = step.sort
		if !db.update(step.mk.TestModeInfo, upd.TestModeInfo, &upd, *step.mk) {
			t.Fatal("update failed")
		}
	}
	if db.keyed[keyedTestIndex] != built {
		t.Fatal("the index was dropped by a re-stamp that landed in front of another")
	}
	if want, got := keyedNames(walkKeyed(db, "k")), keyedNames(rangedKeyed(db, "k")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
}

// A mock the session tier holds twice is passed to fn twice, as GetSessionMocks
// lists it twice when the startup tier is empty, whichever batches its two
// entries fall in.
func TestKeyedWalkKeepsAMockTheTierHoldsTwice(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	for _, gap := range []int{3, 30} {
		var session []*models.Mock
		for i := 0; i < gap+2; i++ {
			mk := &models.Mock{Name: fmt.Sprintf("m%d", i), Kind: models.MySQL, Noise: []string{"k"}}
			mk.TestModeInfo.Lifetime = models.LifetimeSession
			mk.TestModeInfo.SortOrder = 5 // recorded orders may tie; the ID breaks the tie
			mk.Spec.ReqTimestampMock = base.Add(time.Duration(i) * time.Microsecond)
			session = append(session, mk)
		}
		session = append(session, session[0])
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
		if st, _ := mm.GetStartupMocks(); len(st) != 0 {
			t.Fatalf("precondition: the startup tier holds %d mocks", len(st))
		}
		all, _ := mm.GetSessionMocks()
		n := 0
		if err := mm.RangeSessionMocksWithKey(keyedTestIndex, "k", func(*models.Mock) bool { n++; return true }); err != nil {
			t.Fatal(err)
		}
		if n != len(all) {
			t.Errorf("entries %d apart: GetSessionMocks lists %d mocks, the keyed walk %d", gap, len(all), n)
		}
		mm.Close()
	}
}
