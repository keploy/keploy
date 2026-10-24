package replay

import (
	"fmt"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// repeatRecordCommand is the command that records a test set again, which a
// repeat's note names (models.RepeatNote.Explain).
const repeatRecordCommand = "keploy record"

// markRepeatOf names, on each call of the set's results no mock answered
// that repeats an identical recording an earlier test made
// (UnmatchedCall.Repeat, set by the parser), the test whose call that
// recording answered at this replay (consumers: the tests that consumed each
// mock, in the order they ran), and writes the call's next step again with
// that test named. A recording no other test consumed is left unnamed. It
// measures both gaps start to start (models.RepeatNote.Anchor): at this
// replay from that answer to when the call's test began (replayStarts); at
// record from the newest identical per-test recording made before the test
// began then (recordAnchor over lookup, testCases), which may be another
// test's than the one that answered: that test's call was never made again
// while the cache stayed fresh at this replay. A call that is also a
// recording made elsewhere (NotOwned; markRecordedIn names its test first)
// keeps that recording as its closest, and its next step names both
// (models.RepeatNote.ExplainMiss). It returns one line per call it named, for
// the set's log.
func markRepeatOf(results []models.TestResult, consumers map[string][]string, testCases []*models.TestCase, lookup map[string]mockDisplayInfo, replayStarts map[string]time.Time) []string {
	began := recordedStarts(testCases)
	var recorded map[string][]recordedTwin
	var named []string
	for i := range results {
		r := &results[i]
		for j := range r.FailureInfo.UnmatchedCalls {
			c := &r.FailureInfo.UnmatchedCalls[j]
			if c.Repeat == nil || c.Repeat.Mock == "" {
				continue
			}
			if recorded == nil {
				recorded = recordedTwins(lookup)
			}
			// The note is the agent's: written on a copy, never through.
			n := c.Repeat.Clone()
			if n.Test == "" {
				n.Test = earlierConsumer(consumers[n.Mock], r.TestCaseID)
			}
			start := began[r.TestCaseID]
			last, ok := recordAnchor(recorded[lookup[n.Mock].requestOf()], start)
			if !ok {
				last = recordedTwin{name: n.Mock, recordedAt: n.RecordedAt}
			}
			n.Anchor(replayStarts[r.TestCaseID], start, last.name, last.recordedAt)
			c.Repeat = n
			c.NextSteps = c.Repeat.ExplainMiss(c.NotOwned, c.ClosestMock, c.RecordedIn, repeatRecordCommand)
			named = append(named, repeatLine(r.TestCaseID, c))
		}
	}
	return named
}

// recordedTwin is a per-test recording of the set, with when its request was
// recorded (recordAnchor).
type recordedTwin struct {
	name       string
	recordedAt time.Time
}

// recordedTwins is every per-test recording of lookup with a request
// fingerprint (models.Mock.RequestFingerprint) and a recorded time, by
// fingerprint, oldest first (names break a tie). A session or connection
// recording is in none: only a test's own recording says the application
// asked then, and "" is no request.
func recordedTwins(lookup map[string]mockDisplayInfo) map[string][]recordedTwin {
	out := map[string][]recordedTwin{}
	for name, info := range lookup {
		if info.lifetimeOf() != models.LifetimePerTest {
			continue
		}
		fp, at := info.requestOf(), info.recordedAtOf()
		if fp == "" || at.IsZero() {
			continue
		}
		out[fp] = append(out[fp], recordedTwin{name: name, recordedAt: at})
	}
	for _, list := range out {
		sort.Slice(list, func(a, b int) bool {
			if !list[a].recordedAt.Equal(list[b].recordedAt) {
				return list[a].recordedAt.Before(list[b].recordedAt)
			}
			return list[a].name < list[b].name
		})
	}
	return out
}

// recordAnchor is, of twins (oldest first), the one recorded last before
// start: at record, the identical call that last gave the application the
// answer before the test began, which an answer kept for a span of elapsed
// time was kept from. ok is false for none, or start unknown.
func recordAnchor(twins []recordedTwin, start time.Time) (recordedTwin, bool) {
	if start.IsZero() {
		return recordedTwin{}, false
	}
	for i := len(twins) - 1; i >= 0; i-- {
		if twins[i].recordedAt.Before(start) {
			return twins[i], true
		}
	}
	return recordedTwin{}, false
}

// recordedStarts is when each test of testCases began at record: its
// recorded request's time, where it has one.
func recordedStarts(testCases []*models.TestCase) map[string]time.Time {
	began := make(map[string]time.Time, len(testCases))
	for _, tc := range testCases {
		if tc == nil {
			continue
		}
		if req, _ := recordReqResTimestamps(tc); !req.IsZero() {
			began[tc.Name] = req
		}
	}
	return began
}

// markUnmadeRepeats is markRepeatOf's mirror, for a replay faster than the
// recording. It notes, on each unconsumed test mock of the set's results
// (FailureInfo.UnconsumedMocks, which the proven switch reports) that has no
// other explanation (no sibling consumed it, no concurrent share charged it),
// the newest identical recording (models.Mock.RequestFingerprint) another
// test of the set recorded before it that answered that test's call at this
// replay before this test began (UnconsumedMock.RepeatOf, with its sentence
// in NextSteps): the application likely still held that answer and did not
// ask again. consumed is the set's consumed mocks by name, whose Timestamp
// is when each first answered; replayStarts is when each test began at this
// replay (a test missing from it counts its result's Started, to the
// second). A session or connection recording is no earlier twin: only a
// test's own recording says that test made the call. The gap at record runs
// from the newest identical per-test recording made before this test began
// then (recordAnchor), as markRepeatOf's does. It returns one line per note,
// for the set's log.
func markUnmadeRepeats(results []models.TestResult, consumers map[string][]string, consumed map[string]models.MockState, lookup map[string]mockDisplayInfo, replayStarts map[string]time.Time, testCases []*models.TestCase) []string {
	var byRequest map[string][]answeredTwin
	var recorded map[string][]recordedTwin
	var recordStarts map[string]time.Time
	var named []string
	for i := range results {
		r := &results[i]
		for j := range r.FailureInfo.UnconsumedMocks {
			u := &r.FailureInfo.UnconsumedMocks[j]
			if u.ConsumedBy != "" || u.Concurrent != "" || u.WhyNot != "" || u.Unkept > 0 {
				continue
			}
			if byRequest == nil {
				byRequest = answeredTwins(consumers, consumed, lookup)
				recorded = recordedTwins(lookup)
				recordStarts = recordedStarts(testCases)
			}
			info := lookup[u.Name]
			began := replayStarts[r.TestCaseID]
			if began.IsZero() {
				began = time.Unix(r.Started, 0)
			}
			twin, by, ok := newestTwinBefore(byRequest[info.requestOf()], u.Name, r.TestCaseID, info.recordedAtOf(), began)
			if !ok {
				continue
			}
			n := models.NewRepeatNote(twin.name, twin.answeredAt, twin.recordedAt, time.Time{})
			start := recordStarts[r.TestCaseID]
			last, ok := recordAnchor(recorded[info.requestOf()], start)
			if !ok {
				last = recordedTwin{name: twin.name, recordedAt: twin.recordedAt}
			}
			n.Anchor(began, start, last.name, last.recordedAt)
			n.Test = by
			u.RepeatOf = n
			u.NextSteps = n.ExplainUnmade(repeatRecordCommand)
			line := fmt.Sprintf("%s: recorded %s was not called; identical to %s, answered for %s %.2fs before this test began at replay", r.TestCaseID, u.Name, n.Mock, by, n.ReplayGapSeconds)
			if n.RecordGapKnown {
				line += fmt.Sprintf(", %.2fs after %s at record", n.RecordGapSeconds, n.RecordedLast)
			}
			named = append(named, line)
		}
	}
	return named
}

// answeredTwin is a consumed per-test recording that may be an unconsumed
// one's earlier twin (markUnmadeRepeats).
type answeredTwin struct {
	name       string
	tests      []string
	answeredAt time.Time
	recordedAt time.Time
}

// answeredTwins is every consumed recording of the set with a request
// fingerprint and a known first answer, by fingerprint, oldest answer first
// (names break a tie). A recording of a kind with no fingerprint is in none:
// "" is no request.
func answeredTwins(consumers map[string][]string, consumed map[string]models.MockState, lookup map[string]mockDisplayInfo) map[string][]answeredTwin {
	out := map[string][]answeredTwin{}
	for name, tests := range consumers {
		st := consumed[name]
		if st.Timestamp == 0 {
			continue
		}
		if st.Lifetime != models.LifetimePerTest {
			continue
		}
		info := lookup[name]
		fp := info.requestOf()
		if fp == "" {
			continue
		}
		out[fp] = append(out[fp], answeredTwin{name: name, tests: tests, answeredAt: time.Unix(0, st.Timestamp), recordedAt: info.recordedAtOf()})
	}
	for _, list := range out {
		sort.Slice(list, func(a, b int) bool {
			if !list[a].answeredAt.Equal(list[b].answeredAt) {
				return list[a].answeredAt.Before(list[b].answeredAt)
			}
			return list[a].name < list[b].name
		})
	}
	return out
}

// newestTwinBefore is the twin of twins (oldest answer first) that answered
// last before began, for a test other than test, that is not mock itself and
// was recorded before it (recordedAt; either time unknown counts as before:
// the zero time is before any other),
// with the first other test it answered; ok is false for none.
func newestTwinBefore(twins []answeredTwin, mock, test string, recordedAt, began time.Time) (twin answeredTwin, by string, ok bool) {
	for i := len(twins) - 1; i >= 0; i-- {
		t := twins[i]
		if t.name == mock || !t.answeredAt.Before(began) {
			continue
		}
		if !recordedAt.IsZero() && !t.recordedAt.Before(recordedAt) {
			continue
		}
		if by = earlierConsumer(t.tests, test); by == "" {
			continue
		}
		return t, by, true
	}
	return answeredTwin{}, "", false
}

// earlierConsumer is the first of tests that is not test, "" for none.
func earlierConsumer(tests []string, test string) string {
	for _, t := range tests {
		if t != test {
			return t
		}
	}
	return ""
}

// repeatLine is the set log's line for a repeated call of test.
func repeatLine(test string, c *models.UnmatchedCall) string {
	n := c.Repeat
	by := n.Test
	if by == "" {
		by = "an earlier test"
	}
	when := "earlier"
	if n.ReplayGapFromStart {
		when = "before this test began"
	}
	line := fmt.Sprintf("%s: %s repeats %s, answered for %s %.2fs %s at replay", test, c.ActualSummary, n.Mock, by, n.ReplayGapSeconds, when)
	if n.RecordGapKnown {
		line += fmt.Sprintf(", %.2fs after %s at record", n.RecordGapSeconds, n.RecordedLast)
	}
	return line
}

// nameRepeats writes into the end-of-run mismatch rows of testSetID the
// notes markRepeatOf completed on results (the earlier test named, the gap
// at record measured) and the next steps it wrote with them, so the table
// the CLI prints says the same as the report.
func (tfs *TestFailureStore) nameRepeats(testSetID string, results []models.TestResult) {
	byTest := map[string]map[string]*models.UnmatchedCall{}
	for i := range results {
		for j := range results[i].FailureInfo.UnmatchedCalls {
			c := &results[i].FailureInfo.UnmatchedCalls[j]
			if c.Repeat == nil {
				continue
			}
			if byTest[results[i].TestCaseID] == nil {
				byTest[results[i].TestCaseID] = map[string]*models.UnmatchedCall{}
			}
			byTest[results[i].TestCaseID][c.Repeat.Mock] = c
		}
	}
	if len(byTest) == 0 {
		return
	}
	tfs.mu.Lock()
	defer tfs.mu.Unlock()
	for i := range tfs.failures {
		f := &tfs.failures[i]
		if f.TestSetID != testSetID || f.MismatchReport == nil || f.MismatchReport.Repeat == nil {
			continue
		}
		c := byTest[f.TestID][f.MismatchReport.Repeat.Mock]
		if c == nil {
			continue
		}
		rep := *f.MismatchReport
		rep.Repeat = c.Repeat.Clone()
		rep.NextSteps = c.NextSteps
		f.MismatchReport = &rep
	}
}
