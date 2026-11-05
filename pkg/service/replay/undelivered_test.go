package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// unreadAt is httpAt tagged as an answer the client never read
// (models.MetadataUndeliveredKey): its parser never serves it.
func unreadAt(name string, ms int) *models.Mock {
	m := httpAt(name, ms)
	m.Spec.Metadata = map[string]string{models.MetadataUndeliveredKey: models.MetadataUndelivered}
	return m
}

// An answer the client never read is no call the application must make
// again: a set whose application made every call it read passes, though the
// recording holds an unread answer at its boot and one inside test-1, and
// nothing names either as a call it never made. Under the switch, whether
// keploy record wrote the set's mappings.yaml (listing the unread answers in
// the worker scope and in test-1's mapping, as a mapping written before the
// tag or by another writer can) or the replay derives them from the windows;
// with the switch off, under --assert-dependencies, with test-1's mapping
// listing the one inside its window.
func TestAnAnswerTheClientNeverReadIsNoCallTheVerdictRequires(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on       bool
		recorded bool
	}{
		{"switch on, mapped as record wrote it", true, true},
		{"switch on, mapped from the windows", true, false},
		{"switch off, --assert-dependencies", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.on)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.replayer.config.Test.AssertDependencies = !tc.on
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{unreadAt("mock-unread-boot", 500), httpAt("mock-a", 1010), unreadAt("mock-unread-in-test", 1020)}
			var recorded map[string][]models.MockEntry
			if tc.recorded {
				recorded = map[string][]models.MockEntry{"test-1": {
					{Name: "mock-a", Kind: string(models.HTTP)},
					{Name: "mock-unread-in-test", Kind: string(models.HTTP)},
				}}
			}
			db := mappingFile(t, h, recorded)
			if tc.recorded && tc.on {
				if err := db.UpsertWorker(context.Background(), "test-set-0", []models.MockEntry{{Name: "mock-unread-boot", Kind: string(models.HTTP)}}); err != nil {
					t.Fatal(err)
				}
			}
			made := models.MockState{Name: "mock-a", Kind: models.HTTP, Usage: models.Deleted, Lifetime: models.LifetimePerTest,
				ReqTimestampMock: models.FormatMockTimestamp(attrAt(1010)), ResTimestampMock: models.FormatMockTimestamp(attrAt(1011))}
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {made}})}
			if got := h.run(t); got != models.TestSetStatusPassed {
				t.Fatalf("set %s, want PASSED (worker-scope call named %d time(s))", got, workerNamed(logs))
			}
			got := resultOf(t, h, "test-1")
			if got.Status != models.TestStatusPassed || got.FailureInfo.UnconsumedMocks != nil {
				t.Fatalf("test-1: %s, unconsumed %+v; want PASSED, none", got.Status, got.FailureInfo.UnconsumedMocks)
			}
			if n := workerNamed(logs); n != 0 {
				t.Fatalf("the set's end named a worker-scope call never made %d time(s)", n)
			}
		})
	}
}
