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
// index stands down and the tree is walked.
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
			if r.Intn(25) == 0 {
				mk.Spec.ReqTimestampMock = time.Time{}
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
			// The window that holds the first test.
			want := windowOfSessionMocks(t, mm, at(0), at(10))
			if got, _ := mm.GetSessionMocksInWindow(at(0), at(10)); !sameMocks(got, want) {
				t.Fatalf("seed %d, %s, first window:\n got  %s\n want %s", seed, stage, describe(got), describe(want))
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
		// it, so lookups fall back to a walk and stay exact.
		old := current[0]
		moved := *old
		moved.Spec.ReqTimestampMock = at(1234)
		mm.UpdateUnFilteredMock(old, &moved)
		check("after a request-time move")
		if got, _ := mm.GetSessionMocksInWindow(at(1234), at(1234)); !containsMock(got, &moved) {
			t.Fatalf("seed %d: the mock moved to 1234ms is not in its new window: %s", seed, describe(got))
		}

		// The next staging rebuilds the index.
		mm.SetMocksWithWindow(perTest, session, at(10), at(20))
		check("restaged")
		mm.Close()
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
