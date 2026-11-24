package agent

import (
	"context"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type gapTestProxy struct {
	proxyPkg.Proxy
	mm *proxyPkg.MockManager
}

func (p *gapTestProxy) SeedStartupCutoff(t time.Time) { p.mm.SeedStartupCutoff(t) }
func (p *gapTestProxy) SeedRecordedWindows(ws []models.TestWindow) {
	p.mm.SeedRecordedWindows(ws)
}
func (p *gapTestProxy) InFlightFrom(start time.Time) (time.Time, bool) {
	return p.mm.InFlightFrom(start)
}
func (p *gapTestProxy) GapAfter(start time.Time) (time.Time, time.Time, bool) {
	return p.mm.GapAfter(start)
}
func (p *gapTestProxy) SetMocksWithWindow(_ context.Context, filtered, unfiltered []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(filtered, unfiltered, start, end)
	return nil
}

func TestAgentLoadsPostWindowGapMocksInMappingAndStrictWindowModes(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 13, 25, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	windows := []models.TestWindow{
		{TestCase: "post-async", Start: at(1000), End: at(1010)},
		{TestCase: "wait-async", Start: at(1050), End: at(1200)},
	}
	gapMock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-gap-http",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                         "HTTP_CLIENT",
				models.MetadataInterTestGapKey: "true",
			},
			ReqTimestampMock: at(1020),
			ResTimestampMock: at(1030),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	unstampedGapMock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-unstamped-gap",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			ReqTimestampMock: at(1035),
			ResTimestampMock: at(1040),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	waitMock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-wait-sqs",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			ReqTimestampMock: at(1100),
			ResTimestampMock: at(1120),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}

	for _, useMapping := range []bool{false, true} {
		name := "strict-window"
		if useMapping {
			name = "mapping-mode"
		}
		t.Run(name, func(t *testing.T) {
			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			px := &gapTestProxy{mm: mm}
			a := &Agent{
				logger: zap.NewNop(),
				Proxy:  px,
			}
			a.clientMocks.Store(uint64(0), &ClientMockStorage{
				filtered: []*models.Mock{gapMock.DeepCopy(), unstampedGapMock.DeepCopy(), waitMock.DeepCopy()},
			})
			mm.ResetForReplaySession()

			// Stage set with recorded windows.
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:              models.BaseTime,
				BeforeTime:             time.Now(),
				StrictMockWindow:       true,
				FirstRecordedTestStart: at(1000),
				RecordedWindows:        windows,
			}); err != nil {
				t.Fatalf("initial staging: %v", err)
			}

			// Open post-async (in mapping mode with ExactMapping=true and empty MockMapping).
			// Only the interTestGap-stamped mock loads; unstamped-gap stays out.
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:        at(1000),
				BeforeTime:       at(1010),
				StrictMockWindow: true,
				UseMappingBased:  useMapping,
				ExactMapping:     useMapping,
			}); err != nil {
				t.Fatalf("post-async staging: %v", err)
			}
			got, err := mm.GetPerTestMocksInWindow()
			if err != nil || len(got) != 1 || got[0].Name != "mock-gap-http" {
				t.Fatalf("post-async per-test mocks = %v (err=%v); want [mock-gap-http]", got, err)
			}

			// Open wait-async without consuming mock-gap-http: single-step handoff
			// keeps mock-gap-http reachable alongside wait-async's own mock.
			var waitMapping []string
			if useMapping {
				waitMapping = []string{"mock-wait-sqs"}
			}
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:        at(1050),
				BeforeTime:       at(1200),
				StrictMockWindow: true,
				UseMappingBased:  useMapping,
				MockMapping:      waitMapping,
			}); err != nil {
				t.Fatalf("wait-async staging: %v", err)
			}
			got, err = mm.GetPerTestMocksInWindow()
			if err != nil || len(got) != 2 {
				t.Fatalf("wait-async per-test mocks = %d (err=%v); want 2 ([mock-wait-sqs, mock-gap-http])", len(got), err)
			}

			// Once consumed (in TotalConsumedMocks), re-opening post-async drops it.
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:        at(1000),
				BeforeTime:       at(1010),
				StrictMockWindow: true,
				UseMappingBased:  useMapping,
				ExactMapping:     useMapping,
				TotalConsumedMocks: map[string]models.MockState{
					"mock-gap-http": {Name: "mock-gap-http", Usage: models.Deleted},
				},
			}); err != nil {
				t.Fatalf("post-async consumed staging: %v", err)
			}
			got, err = mm.GetPerTestMocksInWindow()
			if err != nil || len(got) != 0 {
				t.Fatalf("consumed mock-gap-http reappeared: %v (err=%v)", got, err)
			}
		})
	}
}
