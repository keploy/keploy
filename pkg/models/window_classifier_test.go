package models

import (
	"testing"
	"time"
)

func TestWindowClassifierMatchesDeriveWindowMappings(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

	defer RegisterIdleAnswer(HTTP, func(m *Mock) bool { return m.Name == "idle-in-win" })()
	defer RegisterAnswerPlaced(HTTP, func(m *Mock) bool { return m.Name == "placed-at-answer-in-win" || m.Name == "placed-at-answer-outside" })()

	mk := func(name string, kind Kind, req, res int, perTest bool) *Mock {
		meta := map[string]string{"type": "mocks"}
		if perTest {
			meta[MetadataLifetimeKey] = MetadataLifetimePerTest
		} else {
			meta[MetadataLifetimeKey] = MetadataLifetimeSession
		}
		m := &Mock{
			Name: name,
			Kind: kind,
			Spec: MockSpec{
				Metadata:         meta,
				ReqTimestampMock: at(req),
				ResTimestampMock: at(res),
			},
		}
		if req < 0 {
			m.Spec.ReqTimestampMock = time.Time{}
		}
		if res < 0 {
			m.Spec.ResTimestampMock = time.Time{}
		}
		m.DeriveLifetime()
		return m
	}

	windows := []TestWindow{
		{TestCase: "zero-start"}, // dropped
		{TestCase: "tc-1", Start: at(10), End: at(20)},
		{TestCase: "tc-overlap-long", Start: at(15), End: at(35)},
		{TestCase: "tc-overlap-short", Start: at(18), End: at(22)},
		{TestCase: "tc-inverted", Start: at(50), End: at(45)}, // clamped to [50, 50]
		{TestCase: "tc-zero-end", Start: at(60)},              // clamped to [60, 60]
		{TestCase: "tc-late", Start: at(80), End: at(90)},
	}

	mocks := []*Mock{
		mk("boot-before-all", HTTP, 2, 3, true),
		mk("in-tc1", HTTP, 12, 13, true),
		mk("in-overlap-tail", HTTP, 28, 29, true), // still covered by tc-overlap-long after tc-1 and tc-overlap-short ended
		mk("idle-in-win", HTTP, 16, 17, true),
		mk("gap-between", HTTP, 40, 41, true),
		mk("inflight-into-inverted", HTTP, 48, 50, true), // in-flight into tc-inverted (start 50)
		mk("exact-on-clamped", HTTP, 50, 51, true),
		mk("exact-on-zero-end", HTTP, 60, 61, true),
		mk("inflight-into-late", HTTP, 70, 82, true), // in-flight into tc-late
		mk("gap-before-late", HTTP, 70, 75, true),    // answered before tc-late
		mk("after-all", HTTP, 95, 96, true),
		mk("placed-at-answer-in-win", HTTP, 40, 85, true),  // placed at 85 -> inside tc-late
		mk("placed-at-answer-outside", HTTP, 12, 42, true), // placed at 42 -> outside all windows
		mk("session-outside", HTTP, 42, 43, false),         // non-per-test, still TestMock unless OutsideTestScopes
		mk("dns-phase", DNS, 43, 44, false),                // protocol phase -> OutsideTestScopes
		mk("zero-req", HTTP, -1, 44, true),                 // zero At -> skipped
		mk("in-flight-not-pertest", HTTP, 72, 85, false),   // session mock -> InFlightEligible=false -> Worker
	}

	wm := make([]WindowedMock, 0, len(mocks))
	for _, m := range mocks {
		wm = append(wm, NewWindowedMock(m))
	}
	derived := DeriveWindowMappings(windows, wm)
	wantWorker := make(map[string]bool, len(derived.Worker))
	for _, e := range derived.Worker {
		wantWorker[e.Name] = true
	}

	classifier := NewWindowClassifier(windows)
	for _, m := range mocks {
		got := classifier.IsWorker(m)
		want := wantWorker[m.Name]
		if got != want {
			t.Errorf("WindowClassifier.IsWorker(%q) = %v; want %v (DeriveWindowMappings)", m.Name, got, want)
		}
	}

	// Empty / all-zero windows: every valid TestMock is Worker.
	emptyDerived := DeriveWindowMappings([]TestWindow{{TestCase: "zero"}}, wm)
	wantEmpty := make(map[string]bool, len(emptyDerived.Worker))
	for _, e := range emptyDerived.Worker {
		wantEmpty[e.Name] = true
	}
	emptyClassifier := NewWindowClassifier([]TestWindow{{TestCase: "zero"}})
	for _, m := range mocks {
		if got, want := emptyClassifier.IsWorker(m), wantEmpty[m.Name]; got != want {
			t.Errorf("empty WindowClassifier.IsWorker(%q) = %v; want %v", m.Name, got, want)
		}
	}
}
