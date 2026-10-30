package learn

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A set whose payloads' derived data passes its budget (derivedBudget) still
// reads each earlier payload's instants once for the set: neither a later
// call of the same test nor a later test of the set reads them again.
//
// api-server's full round under keploy mock record (14 813 Mongo recordings,
// 335 MB of payloads) passes the 32 MiB budget within minutes. The words a
// set keeps of each recording fill it, and the learn step reads every
// earlier recording in the same order for each changed value of each call
// (earlierMoments, recordedMoments, appearedIn), so the recording read least
// recently is always the next one read: every read missed, and each call read
// the instants of every earlier payload anew, four times or more
// (payloadMoments: 76-86% of the agent's CPU in 30 s profiles). A pooled
// Mongo connection's handler then held its read request for minutes, and the
// end of TestSeededSuitesRunOnClient gave up on it after one watchdog period.
// A test's cache of texts had kept them for the test before the set kept them
// (main reads them once per test), and a recording the set let go no longer
// falls back to it.
func TestASetPastItsBudgetReadsEachPayloadsInstantsOnce(t *testing.T) {
	defer func(b int64) { derivedBudget = b }(derivedBudget)
	const n = 60
	at := eRecAt
	mocks := fillers(n, at)
	for i, m := range mocks {
		// Many words of its own, so the set's words pass the budget, and an
		// instant, so the echo reads each payload's instants.
		words := make([]string, 400)
		for j := range words {
			words[j] = fmt.Sprintf("w%dx%d", i, j)
		}
		row := m.Spec.PostgresV3.Query.Response.Rows[0]
		row[0].Value = strings.Join(words, " ")
		row[1].Value = at.Add(-time.Duration(n-i) * time.Minute).Format(time.RFC3339Nano)
	}
	rawOf := func() []Raw {
		raw := make([]Raw, len(mocks))
		for i, m := range mocks {
			raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
		}
		return raw
	}
	// A read of the clock (the echo reads every earlier payload's instants
	// for it) and a word that changed (the word rule reads their words).
	const clock, word = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:55:53.9Z"}, word: {Path: word, Type: String, Value: "a1b2c3d4"}}}
	live := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:56:54.1Z"}, word: {Path: word, Type: String, Value: "z9y8x7w6"}}}
	classify := func(dc *DriftCache) []Learned {
		dc.appClock = true
		return ClassifyFollowing(dc, []string{clock, word}, live, rec, at, nil, rawOf(), nil, nil, nil)
	}
	spans := instantSpans
	defer func() { instantSpans = spans }()
	read := 0
	instantSpans = func(s string) []instant {
		if strings.Contains(s, "filler") {
			read++
		}
		return spans(s)
	}

	want := classify(noSet(NewDriftCache()))
	derivedBudget = 1 << 40
	whole := NewTracker()
	whole.NewSet()
	whole.Begin()
	classify(whole.Drifts())
	if kept := whole.derived.keptBytes(); kept <= 64<<10 {
		t.Fatalf("the set's derived data kept whole is %d bytes: the row's payloads do not pass the 64 KiB budget", kept)
	}
	whole.End()

	derivedBudget = 64 << 10
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	read = 0
	first := classify(tr.Drifts())
	if read == 0 {
		t.Fatalf("the first call read no payload's instants: the row reads nothing")
	}
	read = 0
	second := classify(tr.Drifts())
	if read != 0 {
		t.Errorf("a second call of the test read earlier payloads' instants %d times again, want 0 (%d payloads): past its budget the set read every payload anew on every call", read, n)
	}
	tr.End()
	tr.Begin()
	read = 0
	next := classify(tr.Drifts())
	if read != 0 {
		t.Errorf("the set's second test read earlier payloads' instants %d times again, want 0 (%d payloads): what is read of a recording's payload alone is read once for the set", read, n)
	}
	tr.End()
	for i, got := range [][]Learned{first, second, next} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("call %d learned %v, want %v (a cache of no tracker's)", i+1, got, want)
		}
	}
}
