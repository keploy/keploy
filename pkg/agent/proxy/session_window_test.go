package proxy

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// windowOfSessionMocks is the definition GetSessionMocksInWindow must meet:
// GetSessionMocks, filtered to the mocks recorded in [start, end] or undated.
func windowOfSessionMocks(t *testing.T, mm *MockManager, start, end time.Time) []*models.Mock {
	t.Helper()
	all, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	var out []*models.Mock
	for _, mk := range all {
		if recordedIn(mk, start, end) {
			out = append(out, mk)
		}
	}
	return out
}

func sameMocks(a, b []*models.Mock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func describe(list []*models.Mock) string {
	out := make([]string, len(list))
	for i, mk := range list {
		out[i] = fmt.Sprintf("%s@%d", mk.Name, mk.TestModeInfo.SortOrder)
	}
	return fmt.Sprint(out)
}

// TestGetSessionMocksInWindow_IsTheWindowOfGetSessionMocks checks the window
// index against its definition through everything a replay does to the session
// tier between two stagings: the startup tier in front of it, a mock listed
// twice, undated mocks, a matched mock re-stamped to the back of the tree,
// deletions, and an update that moves a mock's request time, after which the
// index is built again. Some request times are beyond what Unix nanoseconds
// hold, which the index checks one by one.
func TestGetSessionMocksInWindow_IsTheWindowOfGetSessionMocks(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	for seed := int64(1); seed <= 8; seed++ {
		r := rand.New(rand.NewSource(seed))
		mm := NewMockManager(nil, nil, zap.NewNop())

		// Startup tier: per-test mocks recorded before the first window.
		var perTest []*models.Mock
		for i := 0; i < 5; i++ {
			perTest = append(perTest, newMockForTest(fmt.Sprintf("s%d-startup%d", seed, i), at(-100+i), models.LifetimePerTest))
		}
		var session []*models.Mock
		for i := 0; i < 300; i++ {
			mk := newMockForTest(fmt.Sprintf("s%d-m%d", seed, i), at(r.Intn(2000)), models.LifetimeSession)
			switch r.Intn(25) {
			case 0:
				mk.Spec.ReqTimestampMock = time.Time{}
			case 1:
				mk.Spec.ReqTimestampMock = time.Date(1500+r.Intn(2)*900, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			session = append(session, mk)
		}
		session = append(session, session[7]) // the same mock listed twice
		// Recorded order with some disorder, as a re-filtered pool can have.
		r.Shuffle(len(session)/10, func(i, j int) { session[i], session[j] = session[j], session[i] })

		mm.SetMocksWithWindow(perTest, session, at(0), at(10))

		check := func(stage string) {
			t.Helper()
			for w := 0; w < 40; w++ {
				s := at(r.Intn(2100) - 50)
				e := s.Add(time.Duration(r.Intn(60)) * time.Millisecond)
				want := windowOfSessionMocks(t, mm, s, e)
				got, err := mm.GetSessionMocksInWindow(s, e)
				if err != nil {
					t.Fatalf("GetSessionMocksInWindow: %v", err)
				}
				if !sameMocks(got, want) {
					t.Fatalf("seed %d, %s, window [%v, %v]:\n got  %s\n want %s", seed, stage, s.Sub(base), e.Sub(base), describe(got), describe(want))
				}
			}
			// The window that holds the first test, and windows reaching
			// past what Unix nanoseconds hold.
			for _, w := range [][2]time.Time{
				{at(0), at(10)},
				{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), at(1000)},
				{at(1000), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
				{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
				{time.Date(1400, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
			} {
				want := windowOfSessionMocks(t, mm, w[0], w[1])
				if got, _ := mm.GetSessionMocksInWindow(w[0], w[1]); !sameMocks(got, want) {
					t.Fatalf("seed %d, %s, window [%v, %v]:\n got  %s\n want %s", seed, stage, w[0], w[1], describe(got), describe(want))
				}
			}
		}
		check("staged")

		// What a matcher does to a session mock it serves (mysql updateMock):
		// a copy re-stamped with the next sort order, so it moves to the back.
		current, _ := mm.GetSessionScopedMocks()
		for i := 0; i < 60; i++ {
			old := current[r.Intn(len(current))]
			upd := *old
			upd.TestModeInfo.SortOrder = pkg.GetNextSortNum()
			mm.UpdateUnFilteredMock(old, &upd)
			current, _ = mm.GetSessionScopedMocks()
		}
		check("after re-stamps")

		for i := 0; i < 20; i++ {
			mm.DeleteUnFilteredMock(*current[r.Intn(len(current))])
			current, _ = mm.GetSessionScopedMocks()
		}
		check("after deletes")

		// An update that moves a mock's request time: the index cannot follow
		// it, so it is dropped and the next lookup builds it again.
		old := current[0]
		moved := *old
		moved.Spec.ReqTimestampMock = at(1234)
		mm.UpdateUnFilteredMock(old, &moved)
		check("after a request-time move")
		if got, _ := mm.GetSessionMocksInWindow(at(1234), at(1234)); !containsMock(got, &moved) {
			t.Fatalf("seed %d: the mock moved to 1234ms is not in its new window: %s", seed, describe(got))
		}

		// The next staging brings a new tree, indexed on its first lookup.
		mm.SetMocksWithWindow(perTest, session, at(10), at(20))
		check("restaged")
		mm.Close()
	}
}

// unixNs holds a time exactly when Unix nanoseconds can: from minNsTime to
// maxNsTime inclusive. A time one nanosecond outside would wrap, and be filed
// and looked up at the far end of the index.
func TestUnixNsHoldsExactlyTheNanosecondRange(t *testing.T) {
	for _, c := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"the earliest", minNsTime, true},
		{"a nanosecond before the earliest", minNsTime.Add(-time.Nanosecond), false},
		{"the latest", maxNsTime, true},
		{"a nanosecond after the latest", maxNsTime.Add(time.Nanosecond), false},
		{"the epoch", time.Unix(0, 0), true},
	} {
		ns, ok := unixNs(c.at)
		if ok != c.ok {
			t.Errorf("%s (%s): ok=%v, want %v", c.name, c.at.UTC().Format(time.RFC3339Nano), ok, c.ok)
			continue
		}
		if ok && !time.Unix(0, ns).Equal(c.at) {
			t.Errorf("%s: %d nanoseconds do not round-trip to %s", c.name, ns, c.at.UTC().Format(time.RFC3339Nano))
		}
	}

	// Mocks at each bound and a nanosecond outside it, looked up through
	// windows that start or end exactly there.
	var session []*models.Mock
	for i, at := range []time.Time{
		minNsTime.Add(-time.Nanosecond), minNsTime, minNsTime.Add(time.Nanosecond),
		maxNsTime.Add(-time.Nanosecond), maxNsTime, maxNsTime.Add(time.Nanosecond),
	} {
		session = append(session, newMockForTest(fmt.Sprintf("b%d", i), at, models.LifetimeSession))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, session, minNsTime, maxNsTime)
	for _, w := range [][2]time.Time{
		{minNsTime.Add(-time.Nanosecond), minNsTime.Add(-time.Nanosecond)},
		{minNsTime.Add(-time.Nanosecond), minNsTime},
		{minNsTime, minNsTime},
		{minNsTime, minNsTime.Add(time.Nanosecond)},
		{maxNsTime.Add(-time.Nanosecond), maxNsTime},
		{maxNsTime, maxNsTime},
		{maxNsTime, maxNsTime.Add(time.Nanosecond)},
		{maxNsTime.Add(time.Nanosecond), maxNsTime.Add(time.Nanosecond)},
	} {
		want := windowOfSessionMocks(t, mm, w[0], w[1])
		if got, _ := mm.GetSessionMocksInWindow(w[0], w[1]); !sameMocks(got, want) {
			t.Errorf("window %s - %s:\n got  %s\n want %s", w[0].UTC().Format(time.RFC3339Nano), w[1].UTC().Format(time.RFC3339Nano), describe(got), describe(want))
		}
	}
}

// The window index is built by the first windowed lookup on a staged tree, not
// by the staging: a replay that never looks a window up (no MySQL traffic)
// paid for an index on every staging (see BenchmarkSessionWindowStaging).
// A change the index cannot follow drops it, and the next lookup builds it
// again.
func TestTheWindowIndexIsBuiltByTheFirstLookup(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var session []*models.Mock
	for i := 0; i < 100; i++ {
		session = append(session, newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	indexed := func() bool { return sessionIndexed(mm) }

	mm.SetMocksWithWindow(nil, session, base, base.Add(10*time.Millisecond))
	if indexed() {
		t.Fatal("staging built the window index; the first lookup should")
	}
	if got, _ := mm.GetSessionMocksInWindow(base, base.Add(9*time.Millisecond)); len(got) != 10 {
		t.Fatalf("lookup returned %d mocks, want 10", len(got))
	}
	if !indexed() {
		t.Fatal("the first lookup did not keep the index it built")
	}

	old := session[3]
	moved := *old
	moved.Spec.ReqTimestampMock = base.Add(50 * time.Millisecond)
	if !mm.UpdateUnFilteredMock(old, &moved) {
		t.Fatal("UpdateUnFilteredMock failed")
	}
	if indexed() {
		t.Fatal("an update that moved a request time left the index in place")
	}
	got, _ := mm.GetSessionMocksInWindow(base.Add(50*time.Millisecond), base.Add(50*time.Millisecond))
	if len(got) != 2 || !containsMock(got, &moved) {
		t.Fatalf("window at 50ms after the move: %s, want m50 and the moved m3", describe(got))
	}
	if !indexed() {
		t.Fatal("the lookup after the move did not build the index again")
	}
}

func containsMock(list []*models.Mock, mk *models.Mock) bool {
	for _, m := range list {
		if m == mk {
			return true
		}
	}
	return false
}

// BenchmarkSessionWindow compares the session-tier read a matcher made per
// command (the whole snapshot, then a filter) with the window index, over a
// pool the size of a 7,000-test MySQL recording.
func BenchmarkSessionWindow(b *testing.B) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	const mocks = 40000
	session := make([]*models.Mock, mocks)
	for i := range session {
		session[i] = newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
	start, end := base.Add(35000*time.Millisecond), base.Add(35004*time.Millisecond)

	b.Run("snapshot+filter", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			all, _ := mm.GetSessionMocks()
			kept := 0
			for _, mk := range all {
				if recordedIn(mk, start, end) {
					kept++
				}
			}
			if kept != 5 {
				b.Fatalf("kept %d", kept)
			}
		}
	})
	b.Run("window-index", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			got, _ := mm.GetSessionMocksInWindow(start, end)
			if len(got) != 5 {
				b.Fatalf("got %d", len(got))
			}
		}
	})
}

// BenchmarkSessionWindowStaging measures what a staging and its first windowed
// lookup cost over a pool the size of a 7,000-test MySQL recording in lax mode:
// the reusable pool followed by the promoted per-test mocks, two runs ordered
// by request time, staged from fresh copies as the agent stages them.
func BenchmarkSessionWindowStaging(b *testing.B) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	const mocks = 38000
	var pool []*models.Mock
	for i := 0; i < mocks/10; i++ {
		pool = append(pool, newMockForTest(fmt.Sprintf("c%d", i), base.Add(time.Duration(i*10)*time.Millisecond), models.LifetimeSession))
	}
	for i := 0; i < mocks-mocks/10; i++ {
		pool = append(pool, newMockForTest(fmt.Sprintf("p%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
	}
	start, end := base.Add(time.Second), base.Add(time.Second+4*time.Millisecond)
	fresh := func() []*models.Mock {
		out := make([]*models.Mock, len(pool))
		for i, mk := range pool {
			out[i] = mk.DeepCopy()
		}
		return out
	}
	for _, lookup := range []bool{false, true} {
		name := "stage"
		if lookup {
			name = "stage+first-lookup"
		}
		b.Run(name, func(b *testing.B) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			for n := 0; n < b.N; n++ {
				b.StopTimer()
				staged := fresh()
				b.StartTimer()
				mm.SetMocksWithWindow(nil, staged, start, end)
				if lookup {
					if got, _ := mm.GetSessionMocksInWindow(start, end); len(got) != 6 {
						b.Fatalf("got %d", len(got))
					}
				}
			}
		})
	}
}
