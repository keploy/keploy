package replay

import (
	"context"
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A repeated call is named with the first other test that consumed its
// recording, never its own test, and its next step is written again with that
// test; a recording no other test consumed stays unnamed, and a call with no
// note is left as it was. The end-of-run rows say the same.
func TestMarkRepeatOfNamesTheEarlierTest(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	note := func(mock string) *models.RepeatNote {
		return anchoredNote(mock, rec, rec, rec.Add(6*time.Second), rec.Add(2*time.Second))
	}
	named := func(mock, test string) *models.RepeatNote {
		n := note(mock)
		n.Test = test
		return n
	}
	results := []models.TestResult{
		{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: note("mock-7")},
			{ActualSummary: "GET /alone", ClosestMock: "mock-8", Repeat: note("mock-8")},
			{ActualSummary: "GET /other", NextSteps: "as it was"},
			{ActualSummary: "GET /named", ClosestMock: "mock-6", Repeat: named("mock-6", "test-4")},
		}}},
	}
	consumers := map[string][]string{"mock-7": {"test-9", "test-3", "test-5"}, "mock-8": {"test-9"}, "mock-6": {"test-2", "test-4"}}
	store := NewTestFailureStore()
	for _, c := range results[0].FailureInfo.UnmatchedCalls {
		store.AddUnmatchedCallForTest("set-0", "test-9", c)
	}
	// Another set's row of a test of the same name, repeating the same
	// recording name: another set's recordings, which this set's end does
	// not name.
	store.AddUnmatchedCallForTest("set-1", "test-9", models.UnmatchedCall{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: note("mock-7"), NextSteps: "set-1's"})
	keep := results[0].FailureInfo.UnmatchedCalls[0].Repeat

	lines := markRepeatOf(results, consumers, nil, nil, nil)
	calls := results[0].FailureInfo.UnmatchedCalls
	if calls[0].Repeat.Test != "test-3" || !strings.Contains(calls[0].NextSteps, "test test-3's call") {
		t.Fatalf("the repeat is named %q: %s", calls[0].Repeat.Test, calls[0].NextSteps)
	}
	if keep.Test != "" {
		t.Fatal("naming the test wrote into the note the agent's report shares")
	}
	if calls[1].Repeat.Test != "" || !strings.Contains(calls[1].NextSteps, "an earlier test of this set") {
		t.Fatalf("a recording only its own test consumed was named %q", calls[1].Repeat.Test)
	}
	if calls[2].NextSteps != "as it was" {
		t.Fatalf("a call with no note was rewritten: %q", calls[2].NextSteps)
	}
	// A note that names its earlier test already keeps it, whoever else
	// consumed the recording first.
	if calls[3].Repeat.Test != "test-4" || !strings.Contains(calls[3].NextSteps, "test test-4's call") {
		t.Fatalf("a note that named test-4 now names %q: %s", calls[3].Repeat.Test, calls[3].NextSteps)
	}
	if len(lines) != 3 || !strings.Contains(lines[0], "test-9: GET /count repeats mock-7, answered for test-3 6.00s before this test began at replay, 2.00s after mock-7 at record") {
		t.Fatalf("the set log lines are %q", lines)
	}

	store.nameRepeats("set-0", results)
	rows := store.GetFailures()
	if rows[0].MismatchReport.Repeat.Test != "test-3" || !strings.Contains(rows[0].MismatchReport.NextSteps, "test-3") {
		t.Fatalf("the end-of-run row says %+v", rows[0].MismatchReport.Repeat)
	}
	if rows[1].MismatchReport.Repeat.Test != "" || rows[2].MismatchReport.NextSteps != "as it was" {
		t.Fatalf("rows that name no earlier test changed: %+v, %q", rows[1].MismatchReport.Repeat, rows[2].MismatchReport.NextSteps)
	}
	if rows[4].TestSetID != "set-1" || rows[4].MismatchReport.Repeat.Test != "" || rows[4].MismatchReport.NextSteps != "set-1's" {
		t.Fatalf("another set's row was named by this set's end: %+v, %q", rows[4].MismatchReport.Repeat, rows[4].MismatchReport.NextSteps)
	}
}

// The gap at record is measured to the call's own test's recorded request
// where the replay has it (the agent, which does not hold the set's
// recordings, measures none), and the gap at replay to when the test began;
// the agent's note is not written through. With no recorded request the
// note's gap, or none, stays. The end-of-run rows carry the gap measured,
// named test or not.
func TestMarkRepeatOfMeasuresTheRecordGapFromTheTest(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	unknown := models.NewRepeatNote("mock-7", rec, rec, rec.Add(70*time.Second))
	results := []models.TestResult{
		{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: unknown},
		}}},
		{TestCaseID: "test-12", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: anchoredNote("mock-7", rec, rec, rec.Add(80*time.Second), rec.Add(3*time.Second))},
		}}},
	}
	store := NewTestFailureStore()
	store.AddUnmatchedCallForTest("set-0", "test-9", results[0].FailureInfo.UnmatchedCalls[0])
	tc := func(name string, at time.Time) *models.TestCase {
		return &models.TestCase{Name: name, HTTPReq: models.HTTPReq{Timestamp: at}}
	}
	// test-9 began 40 s after the recording at record, and no other test
	// consumed it (the note stays unnamed); test-12 has no recorded request
	// time.
	markRepeatOf(results, map[string][]string{"mock-7": {"test-9"}}, []*models.TestCase{tc("test-9", rec.Add(40*time.Second)), tc("test-12", time.Time{}), nil}, nil, map[string]time.Time{"test-9": rec.Add(70 * time.Second)})
	got := results[0].FailureInfo.UnmatchedCalls[0]
	if !got.Repeat.RecordGapKnown || got.Repeat.RecordGapSeconds != 40 || got.Repeat.RecordedLast != "mock-7" || !got.Repeat.ReplayGapFromStart || !strings.Contains(got.NextSteps, "at record this test began 40.00s after it,") {
		t.Fatalf("the gap at record is %+v: %s", got.Repeat, got.NextSteps)
	}
	if likely, known := got.Repeat.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("70s at replay after 40s at record is not read as a span that ran out: %v %v", likely, known)
	}
	if unknown.RecordGapKnown || unknown.ReplayGapFromStart {
		t.Fatal("measuring the gap wrote into the note the agent's report shares")
	}
	if n := results[1].FailureInfo.UnmatchedCalls[0].Repeat; !n.RecordGapKnown || n.RecordGapSeconds != 3 {
		t.Fatalf("a test with no recorded request lost the agent's gap: %+v", n)
	}
	store.nameRepeats("set-0", results)
	if row := store.GetFailures()[0].MismatchReport; !row.Repeat.RecordGapKnown || row.Repeat.RecordGapSeconds != 40 {
		t.Fatalf("the end-of-run row's note is %+v", row.Repeat)
	}
}

// At record the cache is refilled in between: test-5 made the call again
// (mock-8) after test-3's (mock-7), and test-9 made none. At this replay
// test-5's call was never made again, the cache still fresh, so mock-7 is
// the one that answered last; the gap at record runs from mock-8, the newest
// identical per-test recording made before test-9 began, and names it. A
// newer identical session recording, one made after test-9 began, and one of
// another request are no anchor.
func TestMarkRepeatOfAnchorsOnTheNewestIdenticalRecording(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	replay := rec.Add(time.Hour)
	mk := func(name, query string, at time.Time, lt models.Lifetime) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at,
			HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://sap/count" + query}}}
		m.TestModeInfo.Lifetime = lt
		return m
	}
	lookup := map[string]mockDisplayInfo{}
	addMockDisplayInfo(lookup, []*models.Mock{
		mk("mock-7", "", rec, models.LifetimePerTest),                             // test-3's, answered at replay
		mk("mock-8", "", rec.Add(4*time.Second), models.LifetimePerTest),          // test-5's refill
		mk("mock-11", "", rec.Add(5*time.Second), models.LifetimeSession),         // a session recording
		mk("mock-12", "?x=1", rec.Add(8*time.Second), models.LifetimePerTest),     // another request
		mk("mock-13", "", rec.Add(9*time.Second), models.LifetimePerTest),         // test-9 began before it
		mk("mock-14", "", rec.Add(8500*time.Millisecond), models.LifetimePerTest), // the newest, at the start's instant less 0.5s
	})
	// test-9 began at record 9 s after mock-7, 0.5 s after mock-14; at
	// replay 6.5 s after mock-7 answered.
	run := func(drop ...string) *models.RepeatNote {
		l := map[string]mockDisplayInfo{}
		for k, v := range lookup {
			l[k] = v
		}
		for _, d := range drop {
			delete(l, d)
		}
		results := []models.TestResult{{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: models.NewRepeatNote("mock-7", replay, rec, replay.Add(7*time.Second))},
		}}}}
		tcs := []*models.TestCase{{Name: "test-9", HTTPReq: models.HTTPReq{Timestamp: rec.Add(9 * time.Second)}}}
		markRepeatOf(results, map[string][]string{"mock-7": {"test-3"}}, tcs, l, map[string]time.Time{"test-9": replay.Add(6500 * time.Millisecond)})
		return results[0].FailureInfo.UnmatchedCalls[0].Repeat
	}
	if n := run(); n.RecordedLast != "mock-14" || n.RecordGapSeconds != 0.5 {
		t.Fatalf("anchored on %q, %.2fs; want mock-14, 0.50s", n.RecordedLast, n.RecordGapSeconds)
	}
	n := run("mock-14")
	if n.RecordedLast != "mock-8" || n.RecordGapSeconds != 5 || n.ReplayGapSeconds != 6.5 || !n.ReplayGapFromStart {
		t.Fatalf("the note is %+v, want 6.50s at replay from test-9's start and 5.00s at record from mock-8", n)
	}
	if likely, known := n.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("6.5s at replay after a refill 5s before the test at record is not an expiry: %v %v", likely, known)
	}
	if s := n.Explain(repeatRecordCommand); !strings.Contains(s, "at record this test began 5.00s after mock-8, the newest identical recording made before it") || !strings.Contains(s, "likely kept") {
		t.Fatalf("the next step: %s", s)
	}
	// With no identical per-test recording but the twin before the test, the
	// gap runs from the twin, and here says the cause is less likely.
	if n := run("mock-14", "mock-8"); n.RecordedLast != "mock-7" || n.RecordGapSeconds != 9 {
		t.Fatalf("with the refill gone, anchored on %q, %.2fs; want mock-7, 9.00s", n.RecordedLast, n.RecordGapSeconds)
	}
	// A twin the lookup does not know is measured from its own recording.
	if n := run("mock-7", "mock-8", "mock-14"); n.RecordedLast != "mock-7" || n.RecordGapSeconds != 9 {
		t.Fatalf("a twin the lookup lacks is anchored on %q, %.2fs; want mock-7, 9.00s", n.RecordedLast, n.RecordGapSeconds)
	}
}

// The mirror: a recorded call the replay never made is named with the newest
// identical recording (by fingerprint) that answered another test's call
// before this test began at replay and was recorded before it, with that
// test and both gaps from the controlled timeline; every recording that is
// no such twin is passed over, and an unconsumed recording with another
// explanation, or of no fingerprinted kind, is left as it was.
func TestMarkUnmadeRepeatsNamesTheEarlierTwin(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	replay := rec.Add(time.Hour)
	http := func(name, body string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at,
			HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://sap/rates", Body: body}}}
	}
	session := func(name string, at time.Time) *models.Mock {
		m := http(name, "", at)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		return m
	}
	lookup := map[string]mockDisplayInfo{}
	addMockDisplayInfo(lookup, []*models.Mock{
		http("mock-1", "", rec.Add(time.Second)),      // test-1's: the oldest twin
		http("mock-2", "", rec.Add(2*time.Second)),    // test-2's: the newest twin before test-9
		http("mock-3", "", rec.Add(3*time.Second)),    // test-3's: answered after test-9 began
		http("mock-4", "", rec.Add(4*time.Second)),    // test-9's own: consumed by it alone
		http("mock-5", "", rec.Add(80*time.Second)),   // test-5's: recorded after mock-9
		session("mock-6", rec.Add(5*time.Second)),     // a session recording
		http("mock-7", "other", rec.Add(time.Second)), // another request
		http("mock-9", "", rec.Add(70*time.Second)),   // test-9's unconsumed recording
		http("mock-10", "", rec.Add(71*time.Second)),  // test-9's, consumed by a sibling
		{Name: "pg-1", Kind: models.Postgres},
		{Name: "pg-0", Kind: models.Postgres},
	})
	served := func(name string, after time.Duration, lt models.Lifetime) models.MockState {
		return models.MockState{Name: name, Timestamp: replay.Add(after).UnixNano(), Lifetime: lt}
	}
	consumed := map[string]models.MockState{
		"mock-1": served("mock-1", time.Second, models.LifetimePerTest),
		"mock-2": served("mock-2", 2*time.Second, models.LifetimePerTest),
		"mock-3": served("mock-3", 9*time.Second, models.LifetimePerTest),
		"mock-4": served("mock-4", 3*time.Second, models.LifetimePerTest),
		"mock-5": served("mock-5", 3*time.Second, models.LifetimePerTest),
		"mock-6": served("mock-6", 3500*time.Millisecond, models.LifetimeSession),
		"mock-7": served("mock-7", 3*time.Second, models.LifetimePerTest),
		"pg-0":   served("pg-0", 3*time.Second, models.LifetimePerTest),
	}
	consumers := map[string][]string{
		"mock-1": {"test-1"}, "mock-2": {"test-2"}, "mock-3": {"test-3"}, "mock-4": {"test-9"},
		"mock-5": {"test-5"}, "mock-6": {"test-6"}, "mock-7": {"test-7"}, "pg-0": {"test-1"},
	}
	unconsumed := []models.UnconsumedMock{
		{Name: "mock-9", Kind: "Http"},
		{Name: "mock-10", Kind: "Http", ConsumedBy: "test-8"},
		{Name: "pg-1", Kind: "Postgres"},
		{Name: "mock-9", Kind: "Http", Concurrent: "test-8"},
		{Name: "mock-9", Kind: "Http", WhyNot: models.ConcurrentNotSharedKind},
		{Name: "mock-9", Kind: "Http", Unkept: 1},
	}
	results := []models.TestResult{{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnconsumedMocks: unconsumed}}}
	testCases := []*models.TestCase{{Name: "test-9", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: rec.Add(69 * time.Second)}}}
	starts := map[string]time.Time{"test-9": replay.Add(4 * time.Second)}

	named := markUnmadeRepeats(results, consumers, consumed, lookup, starts, testCases)
	u := results[0].FailureInfo.UnconsumedMocks
	n := u[0].RepeatOf
	if n == nil || n.Mock != "mock-2" || n.Test != "test-2" {
		t.Fatalf("the unmade call's twin is %+v, want mock-2 of test-2", n)
	}
	// At record the gap runs from the newest identical per-test recording
	// before test-9 began (mock-4), not from the twin.
	if n.ReplayGapSeconds != 2 || !n.RecordGapKnown || n.RecordGapSeconds != 65 || n.RecordedLast != "mock-4" {
		t.Fatalf("the gaps are %+v, want 2s at replay and 65s at record, from mock-4", n)
	}
	if !strings.Contains(u[0].NextSteps, "test test-2's call") || !strings.Contains(u[0].NextSteps, "likely still held") {
		t.Fatalf("the unmade call's next step: %s", u[0].NextSteps)
	}
	for _, other := range u[1:] {
		if other.RepeatOf != nil || other.NextSteps != "" {
			t.Fatalf("a recording with another explanation, or of no fingerprinted kind, was noted: %+v", other)
		}
	}
	if len(named) != 1 || named[0] != "test-9: recorded mock-9 was not called; identical to mock-2, answered for test-2 2.00s before this test began at replay, 65.00s after mock-4 at record" {
		t.Fatalf("the set log lines are %q", named)
	}

	// Without its replay start the test counts its result's Started, to the
	// second; with every twin answered after that, nothing is noted.
	again := []models.TestResult{{TestCaseID: "test-9", Started: replay.Unix(), FailureInfo: models.FailureInfo{UnconsumedMocks: []models.UnconsumedMock{{Name: "mock-9"}}}}}
	if named := markUnmadeRepeats(again, consumers, consumed, lookup, nil, testCases); len(named) != 0 || again[0].FailureInfo.UnconsumedMocks[0].RepeatOf != nil {
		t.Fatalf("a twin answered after the test began was named: %q", named)
	}
	again[0].Started = replay.Add(2500 * time.Millisecond).Unix() // 2s, so mock-2, answered at 2s, is not before it
	if markUnmadeRepeats(again, consumers, consumed, lookup, nil, testCases); again[0].FailureInfo.UnconsumedMocks[0].RepeatOf == nil || again[0].FailureInfo.UnconsumedMocks[0].RepeatOf.Mock != "mock-1" {
		t.Fatalf("the result's Started was not the fallback: %+v", again[0].FailureInfo.UnconsumedMocks[0].RepeatOf)
	}
}

// The twin chosen is the one that answered last before the test began, of a
// test other than its own, never the recording itself, and recorded before
// it, where both times are known: a time unknown on either side counts as
// before. Twins answered at one instant are told apart by name, so the
// choice does not hang on map order.
func TestNewestTwinBefore(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	began := rec.Add(time.Hour)
	twin := func(name string, answered time.Duration, recorded time.Time, tests ...string) answeredTwin {
		return answeredTwin{name: name, tests: tests, answeredAt: began.Add(-answered), recordedAt: recorded}
	}
	for _, c := range []struct {
		name       string
		twins      []answeredTwin
		recordedAt time.Time
		want, by   string
	}{
		{"the recording itself is no twin", []answeredTwin{twin("a", 2*time.Second, rec, "t1"), twin("self", time.Second, time.Time{}, "t8")}, time.Time{}, "a", "t1"},
		{"an unknown recorded time of the call rules no twin out", []answeredTwin{twin("a", time.Second, rec.Add(time.Hour), "t1")}, time.Time{}, "a", "t1"},
		{"an unknown recorded time of the twin counts as before", []answeredTwin{twin("a", time.Second, time.Time{}, "t1")}, rec, "a", "t1"},
		{"the test's own recording is skipped for an earlier one", []answeredTwin{twin("a", 2*time.Second, rec, "t9", "t1"), twin("b", time.Second, rec, "t9")}, rec.Add(time.Minute), "a", "t1"},
		{"none answered before the test", []answeredTwin{twin("a", 0, rec, "t1")}, rec.Add(time.Minute), "", ""},
	} {
		got, by, ok := newestTwinBefore(c.twins, "self", "t9", c.recordedAt, began)
		if got.name != c.want || by != c.by || ok != (c.want != "") {
			t.Errorf("%s: got %q by %q (ok %v), want %q by %q", c.name, got.name, by, ok, c.want, c.by)
		}
	}

	lookup := map[string]mockDisplayInfo{}
	consumed := map[string]models.MockState{}
	consumers := map[string][]string{}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("tie-%d", i)
		addMockDisplayInfo(lookup, []*models.Mock{{Name: name, Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://sap/x"}}}})
		consumed[name] = models.MockState{Name: name, Timestamp: began.Add(-time.Second).UnixNano()}
		consumers[name] = []string{fmt.Sprintf("t%d", i)}
	}
	addMockDisplayInfo(lookup, []*models.Mock{{Name: "late", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://sap/x"}}}})
	consumers["late"] = []string{"t8"} // answered when, unknown: no twin
	twins := answeredTwins(consumers, consumed, lookup)
	fp := lookup["tie-0"].requestOf()
	if list := twins[fp]; len(list) != 8 || list[7].name != "tie-7" {
		t.Fatalf("the tied twins are %+v, want the eight in name order and no twin whose answer time is unknown", list)
	}
}

// A recording whose payload a read left on disk is named as one held in
// memory is: the lookup asks the mock for its kept fingerprint and recorded
// time.
func TestMockLookupKeepsTheRequestOfAMockOnDisk(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	mk := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at, HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://sap/rates"}}}
	}
	held, disk := mk("held"), mk("disk")
	disk.LeavePayloadOnDisk(noPayloads{}, models.PayloadAt{Name: "disk"})
	lookup := map[string]mockDisplayInfo{}
	addMockDisplayInfo(lookup, []*models.Mock{held, disk})
	if lookup["disk"].onDisk == nil {
		t.Fatal("the mock on disk was not kept as such")
	}
	if fp := lookup["held"].requestOf(); fp == "" || lookup["disk"].requestOf() != fp {
		t.Fatal("a mock on disk lost the fingerprint of its request")
	}
	if !lookup["disk"].recordedAtOf().Equal(at) || !lookup["held"].recordedAtOf().Equal(at) {
		t.Fatal("the lookup lost when a request was recorded")
	}
}

// noPayloads is a mock file no test reads back.
type noPayloads struct{}

func (noPayloads) LoadPayload(context.Context, models.PayloadAt) (*models.Mock, error) {
	return nil, models.ErrPayloadFileReplaced
}
func (noPayloads) SetPayloadMutator(models.PayloadMutator) {}
func (noPayloads) PayloadMutator() models.PayloadMutator   { return nil }

// anchoredNote is the note of a call identical to mock, which answered at
// answered and was recorded at rec, in a test that began at replayStart at
// this replay and at recordStart at record, measured from mock itself
// (models.RepeatNote.Anchor).
func anchoredNote(mock string, answered, rec, replayStart, recordStart time.Time) *models.RepeatNote {
	n := models.NewRepeatNote(mock, answered, rec, time.Time{})
	n.Anchor(replayStart, recordStart, mock, rec)
	return n
}

// RunTestSet completes the parsers' repeat notes once the set has run, with
// the set's own results, consumers, test cases, mock lookup and test starts
// (markRepeatOf); writes them into the end-of-run rows in the branch that
// named any, after they are complete (nameRepeats), so the table says what
// the report says; names the mirror on unconsumed recordings
// (markUnmadeRepeats); and notes when each test began, on the main path and
// the streaming one (replayStarts), which is what lets a note judge its cause
// from two gaps that both run to a test's start. Each seam's behaviour is
// tested above; this pins that RunTestSet calls it (depresult_wiring_test.go
// says why the wiring is read from the source).
func TestRunTestSetWiresTheRepeatNotes(t *testing.T) {
	fset, fn := runTestSetSource(t)
	for _, c := range []struct {
		call string
		want []string
	}{
		{"markRepeatOf", []string{"testCaseResults", "consumers", "testCases", "mockLookup", "replayStarts"}},
		{"markUnmadeRepeats", []string{"testCaseResults", "consumers", "totalConsumedMocks", "mockLookup", "replayStarts", "testCases"}},
	} {
		sites := callArgIdents(fn, c.call)
		if len(sites) != 1 {
			t.Fatalf("RunTestSet calls %s %d times, want once", c.call, len(sites))
		}
		for _, w := range c.want {
			if !sites[0][w] {
				t.Errorf("RunTestSet's %s no longer passes %s: it passes %v", c.call, w, sortedKeys(sites[0]))
			}
		}
	}
	var named []string
	ast.Inspect(fn, func(n ast.Node) bool {
		s, ok := n.(*ast.IfStmt)
		if !ok || s.Init == nil {
			return true
		}
		init, ok := s.Init.(*ast.AssignStmt)
		if !ok || len(init.Rhs) != 1 {
			return true
		}
		if call, ok := init.Rhs[0].(*ast.CallExpr); !ok || calleeName(call) != "markRepeatOf" {
			return true
		}
		for _, st := range s.Body.List {
			if es, ok := st.(*ast.ExprStmt); ok {
				if call, ok := es.X.(*ast.CallExpr); ok && calleeName(call) == "nameRepeats" {
					named = append(named, printNode(t, fset, call))
				}
			}
		}
		return false
	})
	if len(named) != 1 || named[0] != "r.mockMismatchFailures.nameRepeats(testSetID, testCaseResults)" {
		t.Fatalf("in the branch markRepeatOf named calls in, RunTestSet writes the end-of-run rows with %v, want one r.mockMismatchFailures.nameRepeats(testSetID, testCaseResults)", named)
	}
	var writes []string
	ast.Inspect(fn, func(n ast.Node) bool {
		a, ok := n.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != 1 {
			return true
		}
		if ix, ok := a.Lhs[0].(*ast.IndexExpr); ok {
			if id, ok := ix.X.(*ast.Ident); ok && id.Name == "replayStarts" {
				writes = append(writes, printNode(t, fset, a))
			}
		}
		return true
	})
	sort.Strings(writes)
	if want := []string{"replayStarts[tc.Name] = started", "replayStarts[testCase.Name] = started"}; strings.Join(writes, "; ") != strings.Join(want, "; ") {
		t.Fatalf("RunTestSet notes the tests' starts as %v, want %v: without a start a note's replay gap runs to the call, and its cause is not judged", writes, want)
	}
}
