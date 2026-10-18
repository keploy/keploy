package pkg

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A bootstrap mock — one recorded BEFORE the first test window — must be
// preserved in the filtered slice in BOTH strict and lax mode, because
// MockManager.SetMocksWithWindow routes exactly that band into the startup
// tree, and the tier-aware dispatcher reads it via GetStartupMocks.
//
// Lax is the case that regressed: agentStrict is permanently false on a
// WindowedProxy, so on every windowed replay the preservation (which used to
// live inside the strict branch) never ran, the mock was diverted into the
// session/unfiltered pool that the mongo tier-aware path never consults for
// bootstrap traffic, and the startup tier stayed empty for the whole run.
func TestStartupBandIsPreservedInBothStrictAndLaxModes(t *testing.T) {
	first := time.Date(2026, 9, 1, 12, 0, 10, 0, time.UTC) // first window start
	after := first                                         // this test's window
	before := first.Add(2 * time.Second)
	bootTS := first.Add(-3 * time.Second) // recorded during app bootstrap

	newBootstrapMock := func() *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "bootstrap-find-schema",
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				ReqTimestampMock: bootTS,
				ResTimestampMock: bootTS.Add(time.Millisecond),
			},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
		}
	}

	// strictWindowEnabled ORs in a process-wide env override, so strict=false is
	// not lax on a machine or lane with KEPLOY_STRICT_MOCK_WINDOW=1 set — the lax
	// subtest would silently pass even with the bug restored. Pin both knobs off.
	prevOverride, prevExplicitOff := strictWindowEnvOverride, strictWindowEnvExplicitOff
	strictWindowEnvOverride, strictWindowEnvExplicitOff = false, false
	t.Cleanup(func() {
		strictWindowEnvOverride, strictWindowEnvExplicitOff = prevOverride, prevExplicitOff
	})

	for _, strict := range []bool{true, false} {
		name := "lax"
		if strict {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			filtered, unfiltered := FilterPerTestAndLaxPromotedTierAware(
				context.Background(), zap.NewNop(),
				[]*models.Mock{newBootstrapMock()}, after, before, strict, first)

			if len(filtered) != 1 {
				t.Fatalf("bootstrap mock not preserved for the startup tier: filtered=%d unfiltered=%d; "+
					"SetMocksWithWindow only builds the startup tree from the filtered slice, so a mock "+
					"diverted here leaves the tier empty and bootstrap queries miss with candidates:0",
					len(filtered), len(unfiltered))
			}
			if !filtered[0].TestModeInfo.IsFiltered {
				t.Fatal("preserved bootstrap mock must be marked IsFiltered so the startup partition claims it")
			}
		})
	}
}

// A per-test mock in flight when the window began (sent after inFlightFrom,
// the end of the gap before the window, and answered at or after its start)
// is kept in the per-test slice by the user's window mode, not by the agent's
// own strictness: on a windowed proxy (the default path) the agent's filter is
// never strict, and the strict window (MockFilterParams.StrictMockWindow, on
// by default) keeps it per-test, stated lifetime or not. In lax mode one whose
// recorder stated its lifetime is kept, and an unstated one is promoted to the
// session pool with every other unstated per-test mock out of the window, as
// before. One sent and answered in the gap, or sent at or before the gap's
// start, is not kept, nor is a session mock, a connection failure or a
// carry-over mock (the carry-over tier keeps that one reachable from its own
// release window), and with no inFlightFrom nothing changes.
func TestAMockInFlightWhenTheWindowBeganIsKeptPerTest(t *testing.T) {
	t.Cleanup(models.RegisterCarryOver(models.HTTP, func(m *models.Mock) bool { return m.Name == "carry" }))
	first := time.Date(2026, 10, 17, 6, 56, 0, 0, time.UTC)
	gapFrom := first.Add(100 * time.Millisecond) // the previous test's end
	after, before := first.Add(time.Second), first.Add(1100*time.Millisecond)
	mk := func(name string, req, res time.Duration, stated bool) *models.Mock {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP,
			Spec: models.MockSpec{Metadata: map[string]string{"type": "mocks"},
				ReqTimestampMock: first.Add(req), ResTimestampMock: first.Add(res)}}
		if stated {
			m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			m.DeriveLifetime()
		} else {
			m.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}
		}
		return m
	}
	prevOverride, prevExplicitOff := strictWindowEnvOverride, strictWindowEnvExplicitOff
	strictWindowEnvOverride, strictWindowEnvExplicitOff = false, false
	t.Cleanup(func() {
		strictWindowEnvOverride, strictWindowEnvExplicitOff = prevOverride, prevExplicitOff
	})
	namesOf := func(ms []*models.Mock) []string {
		names := []string{}
		for _, m := range ms {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return names
	}
	for _, mode := range []struct{ strict, strictWindow bool }{
		{false, true},  // a windowed proxy, the strict window (the default)
		{false, false}, // a windowed proxy, lax mode
		{true, true},   // an agent that filters strictly itself
	} {
		strict, strictWindow := mode.strict, mode.strictWindow
		for _, stated := range []bool{true, false} {
			for _, from := range []time.Time{gapFrom, {}} {
				failure := mk("failure", 500*time.Millisecond, 1050*time.Millisecond, stated)
				failure.Kind = models.ConnectionFailure
				session := mk("session", 520*time.Millisecond, 1050*time.Millisecond, stated)
				session.TestModeInfo.Lifetime = models.LifetimeSession
				carry := mk("carry", 540*time.Millisecond, 1050*time.Millisecond, stated)
				filtered, promoted := FilterPerTestAndLaxPromotedInFlightAware(context.Background(), zap.NewNop(), []*models.Mock{
					mk("in-flight", 500*time.Millisecond, 1050*time.Millisecond, stated),
					mk("at-gap-start", 100*time.Millisecond, 1050*time.Millisecond, stated),
					mk("gap-only", 600*time.Millisecond, 700*time.Millisecond, stated),
					mk("before-gap", 50*time.Millisecond, 1050*time.Millisecond, stated),
					failure, session, carry,
				}, after, before, strict, first, from, strictWindow)
				kept := !from.IsZero() && (strictWindow || stated)
				wantKept, wantPromoted := []string{}, []string{"session"}
				if kept {
					wantKept = []string{"in-flight"}
				}
				if !strict && !stated {
					wantPromoted = []string{"at-gap-start", "before-gap", "carry", "gap-only", "session"}
					if !kept {
						wantPromoted = []string{"at-gap-start", "before-gap", "carry", "gap-only", "in-flight", "session"}
					}
				}
				if got := namesOf(filtered); !reflect.DeepEqual(got, wantKept) {
					t.Fatalf("strict=%v strictWindow=%v stated=%v inFlightFrom=%v: per-test slice %v, want %v", strict, strictWindow, stated, from, got, wantKept)
				}
				if got := namesOf(promoted); !reflect.DeepEqual(got, wantPromoted) {
					t.Fatalf("strict=%v strictWindow=%v stated=%v inFlightFrom=%v: session pool %v, want %v", strict, strictWindow, stated, from, got, wantPromoted)
				}
			}
		}
	}
}
