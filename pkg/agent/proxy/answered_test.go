package proxy

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keyedRecording is a per-test recording of kind, keyed by url, recorded at
// at.
func keyedRecording(name string, kind models.Kind, id int, url string, at time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: url},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{ID: id, SortOrder: int64(id), IsFiltered: true, Lifetime: models.LifetimePerTest},
	}
}

// byURL declares, for the test, a request key of HTTP and Generic recordings:
// their URL.
func byURL(t *testing.T) {
	t.Helper()
	keys := func(m *models.Mock) []string {
		if m.Spec.HTTPReq == nil {
			return nil
		}
		return []string{"url:" + m.Spec.HTTPReq.URL}
	}
	t.Cleanup(models.RegisterRequestKeys(models.HTTP, keys))
	t.Cleanup(models.RegisterRequestKeys(models.GENERIC, keys))
}

// A per-test recording that answered a call in an earlier test is found by a
// later test's lookup under its key, with when the later test began at
// record time; the open test's own recording, one of another kind, and one
// the match refuses are not. A new session or a new test set forgets them.
func TestFindAnsweredEarlierNamesAnEarlierTestsAnsweredRecording(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	earlier := keyedRecording("mock-1", models.HTTP, 1, "/count", t0)
	own := keyedRecording("mock-2", models.HTTP, 2, "/count", t0.Add(10*time.Second))
	generic := keyedRecording("mock-3", models.GENERIC, 3, "/count", t0.Add(-time.Second))
	all := func(*models.Mock) bool { return true }

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	// answer consumes the staged copy of mk, as a parser consumes the copy
	// its read handed it.
	answer := func(mk *models.Mock) bool {
		staged, _ := mm.GetFilteredMocks()
		for _, s := range staged {
			if s.Name == mk.Name {
				return mm.DeleteFilteredMock(*s)
			}
		}
		return false
	}
	stage := func() {
		mm.SetMocksWithWindow([]*models.Mock{earlier, own, generic}, nil, t0.Add(-2*time.Second), t0.Add(time.Second))
	}
	stage()
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatal("found a recording before any answered")
	}
	// The first test's calls: its own recording, and a Generic one.
	if !answer(earlier) || !answer(generic) {
		t.Fatal("the first test's recordings did not answer")
	}
	// The second test opens, and its own recording of the call answers too.
	start := t0.Add(5 * time.Second)
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, start, t0.Add(15*time.Second))
	if !answer(own) {
		t.Fatal("the second test's recording did not answer")
	}

	got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all)
	if !ok || got.Mock.Name != earlier.Name {
		t.Fatalf("found %v (%v), want the earlier test's %s: not the open test's own, nor a Generic one", got.Mock, ok, earlier.Name)
	}
	if !got.TestRecordedStart.Equal(start) || got.AnsweredAt.IsZero() {
		t.Fatalf("the open test began at %v at record, answered at %v; want %v and a time", got.TestRecordedStart, got.AnsweredAt, start)
	}
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/other"}, all); ok {
		t.Fatal("found a recording under a key it is not under")
	}
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, func(*models.Mock) bool { return false }); ok {
		t.Fatal("found a recording the match refused")
	}
	if got, ok := mm.FindAnsweredEarlier(models.GENERIC, []string{"url:/count"}, all); !ok || got.Mock.Name != generic.Name {
		t.Fatalf("a Generic lookup found %v (%v), want %s", got.Mock, ok, generic.Name)
	}

	mm.ResetForReplaySession()
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatalf("a new session still found %s", got.Mock.Name)
	}
	stage()
	if !answer(earlier) {
		t.Fatal("the recording did not answer after the reset")
	}
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, start, t0.Add(15*time.Second))
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok {
		t.Fatal("a recording that answered after the reset was not found")
	}
	mm.dropPreviousSetConsumes()
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatalf("a new test set still found %s", got.Mock.Name)
	}
}

// Of several recordings that answered under a key, the one answered last is
// found; a key keeps its newest answeredPerKey, and the log its newest
// answeredMaxKeys keys.
func TestAnsweredLogKeepsTheNewest(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var l answeredLog
	for i := 0; i < answeredPerKey+4; i++ {
		l.note(keyedRecording(fmt.Sprintf("m%d", i), models.HTTP, i, "/a", t0), t0.Add(time.Duration(i)*time.Second))
	}
	c := l.candidates([]string{"url:/a"})
	if len(c) != answeredPerKey || c[0].mock.Name != fmt.Sprintf("m%d", answeredPerKey+3) || c[len(c)-1].mock.Name != "m4" {
		t.Fatalf("kept %d, newest %s, oldest %s; want %d from m%d down to m4", len(c), c[0].mock.Name, c[len(c)-1].mock.Name, answeredPerKey, answeredPerKey+3)
	}
	for i := 0; i < answeredMaxKeys; i++ {
		l.note(keyedRecording(fmt.Sprintf("k%d", i), models.HTTP, i, fmt.Sprintf("/k%d", i), t0), t0)
	}
	if c := l.candidates([]string{"url:/a"}); len(c) != 0 {
		t.Fatalf("the oldest key outlived %d newer ones: %d kept", answeredMaxKeys, len(c))
	}
	if c := l.candidates([]string{"url:/k0"}); len(c) != 1 {
		t.Fatalf("a key among the newest %d was dropped", answeredMaxKeys)
	}

	// Across keys the newest wins: the manager compares answer times.
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.answered.note(keyedRecording("old", models.HTTP, 1, "/x", t0), t0)
	mm.answered.note(keyedRecording("new", models.HTTP, 2, "/y", t0), t0.Add(time.Second))
	got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/x", "url:/y"}, func(*models.Mock) bool { return true })
	if !ok || got.Mock.Name != "new" {
		t.Fatalf("found %v, want the one answered last", got.Mock)
	}
}

// Under exact serving a test's recording that answered through any door (a
// delete, or the door that keeps a stateful recording, MarkMockAsUsed) is
// found by a later test's lookup, the later test's own is not, and the
// recorded start is not known (exact serving stages no window). Without exact
// serving, a per-test recording MarkMockAsUsed keeps answered too.
func TestFindAnsweredEarlierUnderExactServing(t *testing.T) {
	byURL(t)
	all := func(*models.Mock) bool { return true }
	doors := map[string]func(mm *MockManager, m *models.Mock) bool{
		"DeleteFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.DeleteFilteredMock(*m) },
		"MarkMockAsUsed":     func(mm *MockManager, m *models.Mock) bool { return mm.MarkMockAsUsed(*m) },
	}
	for door, consume := range doors {
		t.Run(door, func(t *testing.T) {
			first := keyedRecording("mock-1", models.HTTP, 1, "/count", exactT0)
			second := keyedRecording("mock-2", models.HTTP, 2, "/count", exactT0.Add(10*time.Second))
			mm := stagedManager(t, []*models.Mock{first, second}, nil)
			mm.ServeExactTest("T1", []*models.Mock{first})
			pt, _ := mm.GetPerTestMocksInWindow()
			if len(pt) != 1 || !consume(mm, pt[0]) {
				t.Fatalf("the first test's recording did not answer (%d shown)", len(pt))
			}
			mm.ServeExactTest("T2", []*models.Mock{second})
			pt, _ = mm.GetPerTestMocksInWindow()
			if len(pt) != 1 || !consume(mm, pt[0]) {
				t.Fatal("the second test's recording did not answer")
			}
			got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all)
			if !ok || got.Mock.Name != first.Name {
				t.Fatalf("found %v (%v), want the first test's %s, not the open test's own", got.Mock, ok, first.Name)
			}
			if !got.TestRecordedStart.IsZero() {
				t.Fatalf("exact serving knows no recorded start, got %v", got.TestRecordedStart)
			}
		})
	}

	t.Run("MarkMockAsUsed without exact serving", func(t *testing.T) {
		t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
		kept := keyedRecording("mock-1", models.HTTP, 1, "/count", t0)
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		// A session recording answers every test that asks: one it served
		// after is no earlier test's answer.
		session := keyedRecording("mock-s", models.HTTP, 2, "/count", t0)
		session.TestModeInfo.IsFiltered, session.TestModeInfo.Lifetime = false, models.LifetimeSession
		mm.SetMocksWithWindow([]*models.Mock{kept}, []*models.Mock{session}, t0.Add(-time.Second), t0.Add(time.Second))
		staged, _ := mm.GetFilteredMocks()
		if len(staged) != 1 || !mm.MarkMockAsUsed(*staged[0]) {
			t.Fatal("the kept recording did not answer")
		}
		if !mm.MarkMockAsUsed(*session) {
			t.Fatal("the session recording did not answer")
		}
		mm.SetMocksWithWindow(nil, []*models.Mock{session}, t0.Add(5*time.Second), t0.Add(15*time.Second))
		if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok || got.Mock.Name != kept.Name {
			t.Fatalf("found %v (%v), want %s, not the session recording", got.Mock, ok, kept.Name)
		}
	})
}
