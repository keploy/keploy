package proxy

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// scopeMock is a per-test mock recorded at at, as the agent stages one.
func scopeMock(name string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api/" + name, ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: name},
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

func sessionMock(name string) *models.Mock {
	m := scopeMock(name, time.Time{})
	m.Spec.Metadata = map[string]string{"type": "config"}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	return m
}

func scopeNames(ms []*models.Mock, _ error) []string {
	out := []string{}
	for _, m := range ms {
		if m != nil {
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out
}

func sameNames(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func newScopeManager(t *testing.T) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	return mm
}

// A test that keploy mock replay serves by its mapping has no recorded-time
// window: while it runs, the manager reports a test window open and a test
// fired, so tier-aware parsers route its calls to its per-test tier, and the
// per-test tier holds exactly what was staged, whatever the mocks' recorded
// times. Its end goes back to the routing before it, with the pool in the
// startup tier, as a BaseTime staging leaves it.
func TestSetScopeMocksOpensAWindowWithoutTimeBounds(t *testing.T) {
	mm := newScopeManager(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	mm.SetMocksWithWindow([]*models.Mock{scopeMock("a", t0), scopeMock("b", t0.Add(time.Second))}, []*models.Mock{sessionMock("cfg")}, models.BaseTime, time.Now())
	epoch := mm.StagingEpoch()

	// Recorded long ago, and in the future (a recorder with a clock ahead).
	staged := []*models.Mock{scopeMock("b", t0.Add(time.Second)), scopeMock("poll", time.Now().Add(time.Hour))}
	mm.SetScopeMocks(staged, []*models.Mock{sessionMock("cfg")}, true)

	if snap := mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("an open test reads %+v, want a window active and a test fired", snap)
	}
	if !mm.IsTestWindowActive() || !mm.HasFirstTestFired() {
		t.Fatalf("an open test reads active=%v fired=%v, want both", mm.IsTestWindowActive(), mm.HasFirstTestFired())
	}
	if s, e := mm.CurrentTestWindow(); !s.IsZero() || !e.IsZero() {
		t.Fatalf("an open test has time bounds [%v, %v], want none", s, e)
	}
	if !mm.FirstTestWindowStart().IsZero() {
		t.Fatalf("an open test set the startup cutoff to %v", mm.FirstTestWindowStart())
	}
	sameNames(t, "per-test tier in the window", scopeNames(mm.GetPerTestMocksInWindow()), "b", "poll")
	sameNames(t, "startup tier", scopeNames(mm.GetStartupMocks()), nil...)
	sameNames(t, "session tier", scopeNames(mm.GetSessionScopedMocks()), "cfg")
	if mm.StagingEpoch() == epoch {
		t.Fatal("an open test did not start a new staging epoch")
	}

	epoch = mm.StagingEpoch()
	mm.SetScopeMocks([]*models.Mock{scopeMock("a", t0)}, []*models.Mock{sessionMock("cfg")}, false)
	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("between tests reads %+v, want no window and no test fired, as before the first test", snap)
	}
	sameNames(t, "per-test tier between tests", scopeNames(mm.GetFilteredMocks()), nil...)
	sameNames(t, "startup tier between tests", scopeNames(mm.GetStartupMocks()), "a", "cfg")
	if mm.StagingEpoch() == epoch {
		t.Fatal("an end did not start a new staging epoch")
	}
}

// The staging that ends a test leaves out every per-test mock served while
// the test was open, whether consumed out of its tier (Deleted) or used where
// it stays (MarkMockAsUsed, as Postgres v3 and the cursor-serving parsers
// record a serve); what was served outside a test, and the reusable tier,
// stay. The set's boundary forgets them.
func TestSetScopeMocksEndLeavesOutWhatTheTestServed(t *testing.T) {
	mm := newScopeManager(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	pool := func() []*models.Mock {
		return []*models.Mock{scopeMock("boot", t0.Add(-time.Minute)), scopeMock("a1", t0), scopeMock("a2", t0.Add(time.Second)), scopeMock("b", t0.Add(3*time.Second))}
	}
	session := func() []*models.Mock { return []*models.Mock{sessionMock("cfg")} }
	mm.SetMocksWithWindow(pool(), session(), models.BaseTime, time.Now())
	// Served before any test: from the startup tier, re-armed by stagings.
	startup, _ := mm.GetStartupMocks()
	for _, m := range startup {
		if m.Name == "boot" {
			mm.MarkMockAsUsed(*m)
		}
	}

	mm.SetScopeMocks([]*models.Mock{scopeMock("a1", t0), scopeMock("a2", t0.Add(time.Second))}, session(), true)
	per, _ := mm.GetPerTestMocksInWindow()
	for _, m := range per {
		switch m.Name {
		case "a1":
			if !mm.DeleteFilteredMock(*m) {
				t.Fatal("could not consume a1")
			}
		case "a2":
			mm.MarkMockAsUsed(*m)
		}
	}
	cfg, _ := mm.GetSessionScopedMocks()
	mm.MarkMockAsUsed(*cfg[0])
	mm.SetScopeMocks(pool(), session(), false)
	sameNames(t, "the pool between tests", scopeNames(mm.GetStartupMocks()), "boot", "b", "cfg")

	// The next test's end leaves out what every test served, not only its own.
	mm.SetScopeMocks([]*models.Mock{scopeMock("b", t0.Add(3*time.Second))}, session(), true)
	per, _ = mm.GetPerTestMocksInWindow()
	if len(per) != 1 || !mm.DeleteFilteredMock(*per[0]) {
		t.Fatalf("could not consume b from %v", scopeNames(per, nil))
	}
	mm.SetScopeMocks(pool(), session(), false)
	sameNames(t, "the pool between tests after both tests", scopeNames(mm.GetStartupMocks()), "boot", "cfg")

	// A test run again is staged its own mocks afresh.
	mm.SetScopeMocks([]*models.Mock{scopeMock("a1", t0), scopeMock("a2", t0.Add(time.Second))}, session(), true)
	sameNames(t, "a second run's tier", scopeNames(mm.GetPerTestMocksInWindow()), "a1", "a2")

	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(pool(), session(), models.BaseTime, time.Now())
	mm.SetScopeMocks(pool(), session(), false)
	sameNames(t, "the next set's pool between tests", scopeNames(mm.GetStartupMocks()), "boot", "a1", "a2", "b", "cfg")
}

// The scope is window state: every other staging replaces it, the next set's
// boundary staging, a mid-set BaseTime staging and a real test window alike,
// and so does setting the window.
func TestSetScopeMocksIsReplacedByTheNextWindow(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	mm := newScopeManager(t)
	mm.SetScopeMocks([]*models.Mock{scopeMock("a", t0)}, nil, true)
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow([]*models.Mock{scopeMock("a", t0)}, nil, models.BaseTime, time.Now())
	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("the next set's staging reads %+v, want no window and no test fired", snap)
	}

	// A BaseTime staging mid-set moves the pool to the startup tier: a test
	// left open over it would route the test's calls to an empty tier.
	mm = newScopeManager(t)
	mm.SetScopeMocks([]*models.Mock{scopeMock("a", t0)}, nil, true)
	mm.SetMocksWithWindow([]*models.Mock{scopeMock("a", t0)}, nil, models.BaseTime, time.Now())
	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("a BaseTime staging over an open test reads %+v, want no window and no test fired", snap)
	}

	mm = newScopeManager(t)
	mm.SetScopeMocks([]*models.Mock{scopeMock("a", t0)}, nil, true)
	mm.SetMocksWithWindow([]*models.Mock{scopeMock("a", t0)}, nil, t0.Add(-time.Second), t0.Add(time.Second))
	if s, e := mm.CurrentTestWindow(); !s.Equal(t0.Add(-time.Second)) || !e.Equal(t0.Add(time.Second)) {
		t.Fatalf("the real window reads [%v, %v]", s, e)
	}
	mm.windowMu.RLock()
	open := mm.scopeOpen
	mm.windowMu.RUnlock()
	if open {
		t.Fatal("a real window's staging left the scope open")
	}

	mm = newScopeManager(t)
	mm.SetScopeMocks([]*models.Mock{scopeMock("a", t0)}, nil, true)
	mm.SetCurrentTestWindow(time.Time{}, time.Time{})
	if mm.IsTestWindowActive() {
		t.Fatal("clearing the window left the scope open")
	}
}
