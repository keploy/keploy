package mock

import (
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// recordedTestWindows is, under the proven switch, the recorded window of
// every test of a mock set, the ones a replay gates included: what keploy
// test's staging sends the agent from its test cases
// (models.MockFilterParams.RecordedWindows), here from what keploy mock
// record wrote. A test's window runs from when it began in its recording
// (models.MappedTestCase.At, its scope window's start) to the last request
// or answer of the recordings its mapping lists (table, by test ID; decoded
// is every mapped mock the set decoded, held or not). A test with no begin
// starts at its earliest recording; a test with neither has none.
//
// The window starts at the begin even where a recording the test lists was
// sent before it (in flight when the test began), as keploy test's starts at
// its test case's request: the agent gives such a call to the test through
// the gap rule (models.WindowSchedule.InFlightInto), and a pool recording
// made in that gap, while no test was open, stays outside every window.
// keploy mock record gives a test every data-plane recording made inside its
// scope's window, so inside a window here is inside one there, and outside
// every window is a pool slice's: the learn step then reads a recording
// inside another test's window as that test's (integrations.TestWindows),
// never as an earlier place of the paired one, and a pool recording as
// before. A derived window ends at the test's last recording, not at its
// scope's end: a pool recording made after that, still inside the scope,
// reads as the pool's.
//
// Windows that overlap or touch (tests run in parallel, or a call a test
// left running answered inside the next one) are one window named by no
// test: the agent places a moment in the earliest-starting window that holds
// it (models.WindowSchedule.IndexOf), so two of one test's recordings could
// otherwise read as two tests', and a value one gave the other as noise.
// Read as one, each test's recordings are earlier places of the others',
// and a window no test names is no one test's (its stateful sequences walk
// every recording, as with no window).
func recordedTestWindows(mp *models.Mapping, table map[string][]string, decoded []*models.Mock) []models.TestWindow {
	byName := make(map[string]*models.Mock, len(decoded))
	for _, mk := range decoded {
		if mk != nil && mk.Name != "" {
			byName[mk.Name] = mk
		}
	}
	begins := map[string]time.Time{}
	tests := map[string]bool{}
	if mp != nil {
		for _, tc := range mp.TestCases {
			if tc.ID == "" {
				continue
			}
			tests[tc.ID] = true
			if b, ok := begins[tc.ID]; !tc.At.IsZero() && (!ok || b.IsZero() || tc.At.Before(b)) {
				begins[tc.ID] = tc.At
			}
		}
	}
	for id := range table {
		if id != "" {
			tests[id] = true
		}
	}
	var out []models.TestWindow
	for id := range tests {
		begin := begins[id]
		start, end := begin, begin
		for _, name := range table[id] {
			mk := byName[name]
			if mk == nil {
				continue
			}
			for _, t := range []time.Time{mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock} {
				if t.IsZero() {
					continue
				}
				if begin.IsZero() && (start.IsZero() || t.Before(start)) {
					start = t
				}
				if t.After(end) {
					end = t
				}
			}
		}
		if start.IsZero() {
			continue
		}
		out = append(out, models.TestWindow{TestCase: id, Start: start, End: end})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].TestCase < out[j].TestCase
	})
	merged := out[:0]
	for _, w := range out {
		if n := len(merged); n > 0 && !w.Start.After(merged[n-1].End) {
			last := &merged[n-1]
			if w.End.After(last.End) {
				last.End = w.End
			}
			last.TestCase = ""
			continue
		}
		merged = append(merged, w)
	}
	return merged
}
