package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg"
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
			// The report's expected side, where a mapping names the answer
			// never read, lists the answer read and not that one.
			if tc.recorded {
				if got.MockMismatches == nil {
					t.Fatal("test-1 has no expected-vs-consumed report")
				}
				var expected []string
				for _, m := range got.MockMismatches.ExpectedMocks {
					expected = append(expected, m.Name)
				}
				if len(expected) != 1 || expected[0] != "mock-a" {
					t.Fatalf("test-1's report expects %v; want [mock-a], not the answer never read", expected)
				}
			}
		})
	}
}

// The mismatch report never expects an answer the client never read, on
// either replay path, whatever mapping names one: the regular path's
// expected mocks (buildExpectedMockInfos) and a streaming test's
// (reportedExpectedNames). Every other name it lists as before.
func TestTheMismatchReportExpectsNoAnswerTheClientNeverRead(t *testing.T) {
	undelivered := map[string]bool{"mock-unread": true}
	entries := []models.MockEntry{{Name: "mock-a", Kind: string(models.HTTP)}, {Name: "mock-unread", Kind: string(models.HTTP)}, {Name: "mock-b", Kind: string(models.HTTP)}}
	got := buildExpectedMockInfos(entries, nil, undelivered)
	want := []models.MockMismatchMock{{Name: "mock-a", Kind: string(models.HTTP)}, {Name: "mock-b", Kind: string(models.HTTP)}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the regular path expects %v; want %v", got, want)
	}
	if got := buildExpectedMockInfos(entries, nil, nil); len(got) != 3 {
		t.Fatalf("with no answer never read the regular path expects %v; want all three", got)
	}
	names := reportedExpectedNames([]string{"mock-a", "mock-unread", "mock-b"}, undelivered)
	if len(names) != 2 || names[0] != "mock-a" || names[1] != "mock-b" {
		t.Fatalf("a streaming test's report expects %v; want [mock-a mock-b]", names)
	}
	if names := reportedExpectedNames([]string{"mock-a", "mock-b"}, undelivered); len(names) != 2 {
		t.Fatalf("a streaming test's report expects %v; want both", names)
	}
}

// A failing streaming test whose recorded mapping names an answer the client
// never read lists no such name on the mismatch report's expected side, under
// either switch: the answer read and the call it made instead are listed as
// before. The app calls mock-b in place of mock-a, so its mock set diverges
// from its mapping, and answers the stream wrong. With the switch off, the
// same test answered right passes and is not listed.
func TestAFailingStreamingTestsReportExpectsNoAnswerTheClientNeverRead(t *testing.T) {
	for _, tc := range []struct {
		on, right bool
	}{{false, false}, {true, false}, {false, true}} {
		t.Run(fmt.Sprintf("switch on %v, answered right %v", tc.on, tc.right), func(t *testing.T) {
			setSwitch(t, tc.on)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.streaming("test-1")
			h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), unreadAt("mock-unread", 1020), httpAt("mock-b", 1030)}
			mappingFile(t, h, map[string][]models.MockEntry{"test-1": {
				{Name: "mock-a", Kind: string(models.HTTP)},
				{Name: "mock-unread", Kind: string(models.HTTP)},
			}})
			made := models.MockState{Name: "mock-b", Kind: models.HTTP, Usage: models.Deleted, Lifetime: models.LifetimePerTest,
				ReqTimestampMock: models.FormatMockTimestamp(attrAt(1030)), ResTimestampMock: models.FormatMockTimestamp(attrAt(1031))}
			answer := func(c *models.TestCase) *pkg.StreamingHTTPResponse {
				code, body, header := 500, `{"ok":false}`, map[string]string{"Content-Type": "application/json"}
				if tc.right {
					code, body, header = c.HTTPResp.StatusCode, c.HTTPResp.Body, c.HTTPResp.Header
				}
				return &pkg.StreamingHTTPResponse{StatusCode: code, StatusMessage: http.StatusText(code), Header: header,
					Reader: io.NopCloser(strings.NewReader(body)), StreamConfig: pkg.HTTPStreamConfig{Mode: pkg.HTTPStreamModeNone}}
			}
			h.replayer.hookImpl = nonStreamAnswerHooks{
				prHooks: prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {made}})},
				answers: map[string]func(*models.TestCase) *pkg.StreamingHTTPResponse{"test-1": answer},
			}
			_ = h.run(t)
			if passed := resultOf(t, h, "test-1").Status == models.TestStatusPassed; passed != tc.right {
				t.Fatalf("the streaming test passed %v; want %v", passed, tc.right)
			}
			var reported []TestFailure
			for _, f := range h.replayer.mockMismatchFailures.failures {
				if f.TestID == "test-1" {
					reported = append(reported, f)
				}
			}
			if tc.right {
				if len(reported) != 0 {
					t.Fatalf("the report lists the passing test's mismatch %d time(s); want none", len(reported))
				}
				return
			}
			if len(reported) != 1 {
				t.Fatalf("the report lists test-1's mismatch %d time(s); want once", len(reported))
			}
			got := reported[0]
			if len(got.ExpectedMocks) != 1 || got.ExpectedMocks[0] != "mock-a" {
				t.Fatalf("the report expects %v; want [mock-a], not the answer never read", got.ExpectedMocks)
			}
			// The harness answers each read of the window's consumption, so
			// the call made may be listed once a read.
			if len(got.ActualMocks) == 0 || slices.ContainsFunc(got.ActualMocks, func(n string) bool { return n != "mock-b" }) {
				t.Fatalf("the report lists consumed %v; want mock-b, the call made", got.ActualMocks)
			}
		})
	}
}
