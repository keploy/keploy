package pkg

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A connection failure recorded in one test must never fail a connect in
// another, so the agent's window filters serve one only from its own window,
// or from the startup band when it predates the first test, and drop it in
// every other case, in strict mode and lax. Each case below is a rule that
// hands an ordinary per-test mock to tests it was not recorded in.
func TestWindowBoundMocksAreServedOnlyInTheirOwnWindow(t *testing.T) {
	prevOverride, prevExplicitOff := strictWindowEnvOverride, strictWindowEnvExplicitOff
	strictWindowEnvOverride, strictWindowEnvExplicitOff = false, false
	t.Cleanup(func() {
		strictWindowEnvOverride, strictWindowEnvExplicitOff = prevOverride, prevExplicitOff
	})

	first := time.Date(2026, 10, 8, 10, 0, 10, 0, time.UTC) // the first test window starts
	after := first.Add(10 * time.Second)                    // this test's window
	before := after.Add(2 * time.Second)

	mock := func(kind models.Kind, req time.Time, edit func(*models.Mock)) *models.Mock {
		m := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "m",
			Kind:    kind,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				ReqTimestampMock: req,
				ResTimestampMock: req.Add(time.Millisecond),
			},
		}
		if kind == models.ConnectionFailure {
			m.Spec.ConnFailure = &models.ConnFailureSpec{Address: "127.0.0.1:5432", Phase: models.ConnFailurePhaseConnect, Outcome: models.ConnFailureRefused}
		}
		if edit != nil {
			edit(m)
		}
		m.DeriveLifetime()
		return m
	}
	tag := func(v string) func(*models.Mock) {
		return func(m *models.Mock) { m.Spec.Metadata["type"] = v }
	}

	const (
		window  = "window"
		startup = "startup"
		session = "session"
		dropped = "dropped"
	)
	for _, tc := range []struct {
		name     string
		req      time.Time
		edit     func(*models.Mock)
		strict   bool
		want     string // for a connection failure
		ordinary string // for a Mongo mock in the same place, two-tier: why the rule matters
	}{
		{"in its window", after.Add(time.Second), nil, false, window, window},
		{"recorded at boot", first.Add(-time.Second), nil, false, startup, window},
		{"from an earlier test, lax", first.Add(time.Second), nil, false, dropped, session},
		{"from a later test, lax", before.Add(time.Second), nil, false, dropped, session},
		{"from an earlier test, strict", first.Add(time.Second), nil, true, dropped, dropped},
		{"tagged config, in its window", after.Add(time.Second), tag("config"), false, window, session},
		{"tagged connection, in its window", after.Add(time.Second), func(m *models.Mock) {
			m.Spec.Metadata["type"], m.Spec.Metadata["connID"] = "connection", "c1"
		}, false, window, session},
		{"tagged config, out of its window", first.Add(time.Second), tag("config"), false, dropped, session},
		{"session lifetime set after derivation (a mutator)", after.Add(time.Second), func(m *models.Mock) {
			m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimeSession, true
		}, false, window, session},
		{"no timestamps", time.Time{}, func(m *models.Mock) { m.Spec.ResTimestampMock = time.Time{} }, false, dropped, window},
		{"response before request", after.Add(time.Second), func(m *models.Mock) {
			m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-time.Millisecond)
		}, false, dropped, dropped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// filterByTimeStampTierAware (through its exported entry) puts the
			// startup band in the per-test slice; the three-tier filter has a
			// slice of its own for it.
			wantTwo := tc.want
			if wantTwo == startup {
				wantTwo = window
			}
			f, u := FilterPerTestAndLaxPromotedTierAware(context.Background(), zap.NewNop(),
				[]*models.Mock{mock(models.ConnectionFailure, tc.req, tc.edit)}, after, before, tc.strict, first)
			if got := placeOf(t, f, u, nil); got != wantTwo {
				t.Errorf("two-tier filter: %s, want %s", got, wantTwo)
			}
			f3, u3, s3 := FilterByTimeStampThreeTier(context.Background(), zap.NewNop(),
				[]*models.Mock{mock(models.ConnectionFailure, tc.req, tc.edit)}, after, before, tc.strict, first)
			if got := placeOf(t, f3, u3, s3); got != tc.want {
				t.Errorf("three-tier filter: %s, want %s", got, tc.want)
			}
			for _, m := range append(append(f, f3...), s3...) {
				if m.TestModeInfo.Lifetime != models.LifetimePerTest {
					t.Errorf("a served connection failure must be per-test, is %v", m.TestModeInfo.Lifetime)
				}
			}

			// Control: an ordinary per-test kind in the same place.
			fo, uo := FilterPerTestAndLaxPromotedTierAware(context.Background(), zap.NewNop(),
				[]*models.Mock{mock(models.Mongo, tc.req, tc.edit)}, after, before, tc.strict, first)
			if got := placeOf(t, fo, uo, nil); got != tc.ordinary {
				t.Errorf("control: a Mongo mock went to %s, want %s", got, tc.ordinary)
			}
		})
	}
}

// With no window there is nothing to place a connection failure in, but it
// still goes where per-test mocks go, never to the session pool.
func TestWindowBoundMocksStayPerTestWithoutAWindow(t *testing.T) {
	m := &models.Mock{
		Version: "api.keploy.io/v1beta1", Name: "cf", Kind: models.ConnectionFailure,
		Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}},
	}
	f, u, s := FilterByTimeStampThreeTier(context.Background(), zap.NewNop(), []*models.Mock{m}, time.Time{}, time.Time{}, false, time.Time{})
	if got := placeOf(t, f, u, s); got != "window" {
		t.Fatalf("an untimed config-tagged connection failure went to %s, want the per-test slice", got)
	}
	if f[0].TestModeInfo.Lifetime != models.LifetimePerTest {
		t.Fatalf("lifetime %v, want per-test", f[0].TestModeInfo.Lifetime)
	}
	g := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "g", Kind: models.GENERIC, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	f, u, s = FilterByTimeStampThreeTier(context.Background(), zap.NewNop(), []*models.Mock{g}, time.Time{}, time.Time{}, false, time.Time{})
	if got := placeOf(t, f, u, s); got != "session" {
		t.Fatalf("control: a config-tagged Generic mock went to %s, want session", got)
	}
}

func placeOf(t *testing.T, filtered, unfiltered, startup []*models.Mock) string {
	t.Helper()
	switch n := len(filtered) + len(unfiltered) + len(startup); {
	case n == 0:
		return "dropped"
	case n > 1:
		t.Fatalf("one mock in, %d out", n)
	case len(filtered) == 1:
		return "window"
	case len(unfiltered) == 1:
		return "session"
	case len(startup) == 1:
		return "startup"
	}
	return "unreachable"
}
