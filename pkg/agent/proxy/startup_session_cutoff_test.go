package proxy

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// startUpSessionMocks are a set's session mocks, its connections' handshakes,
// as a recording that keeps opening connections while its tests run holds
// them: one from the app's start-up, one with no recorded time, one recorded
// exactly as the first test (winStart(1)) began, and one inside test-2.
func startUpSessionMocks() []*models.Mock {
	return []*models.Mock{
		newMockForTest("boot-handshake", pms(300), models.LifetimeSession),
		newMockForTest("untimed-config", time.Time{}, models.LifetimeSession),
		newMockForTest("handshake-at-first-test", winStart(1), models.LifetimeSession),
		newMockForTest("handshake-in-test-2", pms(2050), models.LifetimeSession),
	}
}

func sessionScopedNames(t *testing.T, mm *MockManager) []string {
	t.Helper()
	got, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, mk := range got {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

// An initial staging copies into the start-up tier only the session mocks
// recorded before the set's first recorded test, the app's own start-up, and
// those with no recorded time. One recorded while the tests ran (a connection
// opened then) is served from the session tier alone, which keeps every one:
// a recording that keeps opening connections no longer grows the start-up
// tier of every following round (k8s-proxy#989). The per-test mocks the
// staging is handed all go in, as before. With no first test known, every
// session mock is copied, as before.
func TestInitialStagingCopiesOnlyStartUpSessionMocksIntoTheStartUpTier(t *testing.T) {
	allSession := []string{"boot-handshake", "handshake-at-first-test", "handshake-in-test-2", "untimed-config"}
	startUpOnly := []string{"boot-handshake", "boot-query", "untimed-config"}
	for _, tc := range []struct {
		name    string
		seed    func(mm *MockManager)
		startup []string
	}{
		{"cutoff seeded", func(mm *MockManager) { mm.SeedStartupCutoff(winStart(1)) }, startUpOnly},
		{"recorded windows seeded", func(mm *MockManager) { mm.SeedRecordedWindows(windowsOf("test-1", "test-2")) }, startUpOnly},
		{"no first test known", func(*MockManager) {}, append([]string{"boot-query"}, allSession...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachSwitchState(t, func(t *testing.T) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				mm.ResetForReplaySession()
				tc.seed(mm)
				perTest := []*models.Mock{newMockForTest("boot-query", pms(400), models.LifetimePerTest)}
				mm.SetMocksWithWindow(perTest, startUpSessionMocks(), models.BaseTime, time.Now())

				want := append([]string(nil), tc.startup...)
				sort.Strings(want)
				if got := startupNames(t, mm); !equalStrings(got, want) {
					t.Fatalf("the start-up tier holds %v; want %v", got, want)
				}
				if got := sessionScopedNames(t, mm); !equalStrings(got, allSession) {
					t.Fatalf("the session tier holds %v; want every session mock %v", got, allSession)
				}
			})
		})
	}
}

// sessionMockForBoot copies a session mock into the start-up tier when it was
// placed before the cutoff or when nothing tells: no cutoff, or no time on the
// mock.
func TestSessionMockForBoot(t *testing.T) {
	cutoff := winStart(1)
	for _, tc := range []struct {
		name   string
		mk     *models.Mock
		cutoff time.Time
		want   bool
	}{
		{"before the cutoff", newMockForTest("m", pms(999), models.LifetimeSession), cutoff, true},
		{"at the cutoff", newMockForTest("m", cutoff, models.LifetimeSession), cutoff, false},
		{"after the cutoff", newMockForTest("m", pms(5000), models.LifetimeSession), cutoff, false},
		{"no recorded time", newMockForTest("m", time.Time{}, models.LifetimeSession), cutoff, true},
		{"no cutoff known", newMockForTest("m", pms(5000), models.LifetimeSession), time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionMockForBoot(tc.mk, tc.cutoff); got != tc.want {
				t.Fatalf("sessionMockForBoot = %v; want %v", got, tc.want)
			}
		})
	}
}
