package replay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestReadThroughMockState_ExcludedFromTestOwnershipAndPerTestAssertion(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)

			rtState := models.MockState{
				Name:        "mock-rt-0123456789abcdef",
				Kind:        models.Mongo,
				Usage:       models.Updated,
				Type:        "mocks",
				ReadThrough: true,
				Lifetime:    models.LifetimePerTest,
			}
			ownState := models.MockState{
				Name:     "mock-own-1",
				Kind:     models.Mongo,
				Usage:    models.Deleted,
				Type:     "mocks",
				Lifetime: models.LifetimePerTest,
			}

			// 1. consumedOutsideAssertion and streaming isMockSubsetWithConfig
			//    must NEVER treat a read-through consume as an unexpected per-test mismatch.
			if !consumedOutsideAssertion(rtState) {
				t.Fatal("read-through MockState must be outside per-test dependency assertion")
			}
			if consumedOutsideAssertion(ownState) {
				t.Fatal("ordinary per-test MockState must remain inside per-test dependency assertion")
			}
			if !isMockSubsetWithConfig([]models.MockState{ownState, rtState}, []string{"mock-own-1"}) {
				t.Fatal("isMockSubsetWithConfig must ignore read-through consumes so streaming tests are not falsely demoted")
			}

			// 2. mapConsumed must file read-through consumes under Startup, NEVER
			//    as test-owned mocks in mappings.yaml.
			attr := newCarryOverAttribution(nil)
			mapping := &models.Mapping{
				TestSetID: "test-set-0",
				TestCases: []models.MappedTestCase{{ID: "test-1"}},
			}
			attr.mapConsumed(mapping, "test-1", t0, t0.Add(time.Second), []models.MockState{ownState, rtState})

			for _, m := range mapping.TestCases[0].Mocks {
				if m.Name == rtState.Name {
					t.Fatalf("read-through mock %s must NEVER be owned by test-1 in mappings.yaml", rtState.Name)
				}
			}
			var inStartup bool
			for _, m := range mapping.Startup {
				if m.Name == rtState.Name {
					inStartup = true
				}
			}
			if !inStartup {
				t.Fatalf("read-through mock %s should be recorded in mapping.Startup, got %v", rtState.Name, mapping.Startup)
			}

			// 3. Startup-phase read-through consumes MUST be classified as
			//    reusable/startup by isReusableTierState and partitionInitialConsumed,
			//    never misrouted to rearm.
			if !isReusableTierState(rtState) {
				t.Fatal("isReusableTierState must return true for ReadThrough MockState")
			}
			startupPart, rearmPart := partitionInitialConsumed([]models.MockState{rtState, ownState})
			if len(startupPart) != 1 || startupPart[0].Name != rtState.Name {
				t.Fatalf("expected read-through state in startup partition, got %+v", startupPart)
			}
			if len(rearmPart) != 1 || rearmPart[0].Name != ownState.Name {
				t.Fatalf("expected only ordinary per-test state in rearm partition, got %+v", rearmPart)
			}

			// 4. windowOwnershipOf must skip valid read-through mocks so they
			//    are never assigned to any test case or worker window ownership.
			rtMock := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "mock-rt-window-skip",
				Kind:    models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{"type": "mocks"},
					HTTPReq: &models.HTTPReq{
						Method: models.Method("GET"),
						URL:    "http://cfg.internal/v1/flags",
						Header: map[string]string{"Host": "cfg.internal"},
					},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
					ReqTimestampMock: t0.Add(100 * time.Millisecond),
					ResTimestampMock: t0.Add(150 * time.Millisecond),
				},
			}
			if !models.MarkReadThrough(rtMock) {
				t.Fatal("MarkReadThrough failed for HTTP GET mock")
			}
			ownMock := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "mock-own-window",
				Kind:    models.HTTP,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"type": "mocks"},
					ReqTimestampMock: t0.Add(200 * time.Millisecond),
					ResTimestampMock: t0.Add(250 * time.Millisecond),
				},
			}
			tc := &models.TestCase{
				Name: "test-1",
				HTTPReq: models.HTTPReq{
					Timestamp: t0,
				},
				HTTPResp: models.HTTPResp{
					Timestamp: t0.Add(time.Second),
				},
			}
			replayer := &Replayer{logger: zap.NewNop()}
			ownedByTest, workerEntries, _ := replayer.windowOwnershipOf("test-set-0", []*models.TestCase{tc}, "test", nil, []*models.Mock{rtMock, ownMock})
			for _, e := range ownedByTest["test-1"] {
				if e.Name == rtMock.Name {
					t.Fatalf("windowOwnershipOf must skip read-through mock %s", rtMock.Name)
				}
			}
			for _, e := range workerEntries {
				if e.Name == rtMock.Name {
					t.Fatalf("windowOwnershipOf must not assign read-through mock %s to worker scope", rtMock.Name)
				}
			}
		})
	}
}
