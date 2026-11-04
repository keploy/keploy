package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// derivedStage is a mock of the store derived from the recording source
// names ("<test>/<mock>"), recorded at at, making path's call.
func derivedStage(name, source, path string, at time.Time) *models.Mock {
	m := stageMock(name, path, at)
	m.Spec.Authored = &models.Authored{How: models.AuthoredDerived, Reason: "declined card", Source: source}
	return m
}

// replacingStore is a recording a replay serves by windows: boot, recorded at
// the app's start, and sess, a session call every test is served, each the
// source of a mock derived into test X's window [stageT0, +2s]; test Y, a
// minute later, holds no derived mock.
func replacingStore(t *testing.T, strict string) (*Agent, *recordingProxy) {
	t.Helper()
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", strict)
	p := &recordingProxy{}
	a := diskAgent(t, p,
		stageMock("boot", "/boot", stageT0.Add(-5*time.Second)),
		stageMock("x-own", "/x", stageT0.Add(200*time.Millisecond)),
		derivedStage("d-boot", "startup:/boot", "/boot", stageT0.Add(400*time.Millisecond)),
		derivedStage("d-sess", "pkg::TestA/sess", "/sess", stageT0.Add(600*time.Millisecond)),
		stageMock("y-own", "/y", stageT0.Add(time.Minute+200*time.Millisecond)))
	sess := stageMock("sess", "/sess", stageT0.Add(-10*time.Second))
	sess.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true}
	s, _ := a.clientMocks.Load(uint64(0))
	s.(*ClientMockStorage).unfiltered = []*models.Mock{sess}
	return a, p
}

// A replay serves a test whose window holds a mock derived from a recording
// that test would be served (one recorded at the app's start, a session
// call) the derived mock in that recording's place: the test's staging
// holds neither source, in either pool, while the next test's, and the pool
// staged between tests, hold each as before. In either window mode.
func TestADerivedMockReplacesItsSourceInItsOwnTestAlone(t *testing.T) {
	for _, strict := range []string{"0", "1"} {
		t.Run("KEPLOY_STRICT_MOCK_WINDOW="+strict, func(t *testing.T) {
			a, p := replacingStore(t, strict)
			ctx := context.Background()
			x := models.MockFilterParams{AfterTime: stageT0, BeforeTime: stageT0.Add(2 * time.Second), StrictMockWindow: strict == "1"}
			if err := a.UpdateMockParams(ctx, x); err != nil {
				t.Fatal(err)
			}
			got := p.windowed[len(p.windowed)-1]
			for _, d := range []string{"d-boot", "d-sess", "x-own"} {
				if !slices.Contains(got[0], d) {
					t.Fatalf("test X is not served its own %s: staged %v", d, got)
				}
			}
			for _, src := range []string{"boot", "sess"} {
				if slices.Contains(got[0], src) || slices.Contains(got[1], src) {
					t.Fatalf("test X, whose derived mock replaces %s, is still served it: staged %v", src, got)
				}
			}

			y := models.MockFilterParams{AfterTime: stageT0.Add(time.Minute), BeforeTime: stageT0.Add(time.Minute + 2*time.Second), StrictMockWindow: strict == "1"}
			if err := a.UpdateMockParams(ctx, y); err != nil {
				t.Fatal(err)
			}
			got = p.windowed[len(p.windowed)-1]
			if !slices.Contains(got[0], "boot") && !slices.Contains(got[1], "boot") {
				t.Fatalf("test Y is not served the start-up recording boot: staged %v", got)
			}
			if !slices.Contains(got[1], "sess") {
				t.Fatalf("test Y is not served the session recording sess: staged %v", got)
			}

			if err := a.stageScope(ctx, nil, false); err != nil {
				t.Fatal(err)
			}
			if between := p.scoped[len(p.scoped)-1]; !slices.Contains(between[1], "sess") {
				t.Fatalf("the pool staged between tests lost sess: staged %v", between)
			}
		})
	}
}

// Staged by its mapping, as the proven switch stages a test, the test is
// served its derived mock and not its source either.
func TestADerivedMockReplacesItsSourceInATestStagedByItsMapping(t *testing.T) {
	a, p := replacingStore(t, "1")
	if err := a.stageScope(context.Background(), []string{"x-own", "d-boot", "d-sess"}, true); err != nil {
		t.Fatal(err)
	}
	got := p.scoped[len(p.scoped)-1]
	if !slices.Contains(got[0], "d-sess") {
		t.Fatalf("the test is not served its own d-sess: staged %v", got)
	}
	for _, src := range []string{"boot", "sess"} {
		if slices.Contains(got[0], src) || slices.Contains(got[1], src) {
			t.Fatalf("the test, whose derived mock replaces %s, is still served it: staged %v", src, got)
		}
	}
	if err := a.stageScope(context.Background(), []string{"y-own"}, true); err != nil {
		t.Fatal(err)
	}
	if got = p.scoped[len(p.scoped)-1]; !slices.Contains(got[1], "sess") {
		t.Fatalf("a test holding no derived mock is not served the session recording sess: staged %v", got)
	}
}

// keploy mock replay stages a test by its mapping with the window of the
// test's run, which holds none of its recorded times: the test's own are
// the mocks its mapping names, wherever they were placed, so its derived
// mocks still replace their sources.
func TestADerivedMockReplacesItsSourceInATestStagedByItsMappingInItsRunsWindow(t *testing.T) {
	a, p := replacingStore(t, "1")
	run := stageT0.Add(time.Hour)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: run, BeforeTime: run.Add(2 * time.Second),
		UseMappingBased: true, MockMapping: []string{"x-own", "d-boot", "d-sess"}, StrictMockWindow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := p.windowed[len(p.windowed)-1]
	if !slices.Contains(got[0], "d-sess") {
		t.Fatalf("the test is not served its own d-sess: staged %v", got)
	}
	for _, src := range []string{"boot", "sess"} {
		if slices.Contains(got[0], src) || slices.Contains(got[1], src) {
			t.Fatalf("the test, whose derived mock replaces %s, is still served it: staged %v", src, got)
		}
	}
}
