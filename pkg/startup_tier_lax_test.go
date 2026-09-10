package pkg

import (
	"context"
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
