package replay

import (
	"fmt"
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
// that test named. A recording no other test consumed is left unnamed. The
// gap at record is measured again from the call's own test's recorded
// request (testCases), where it has one: the agent knows the test's recorded
// start only when it stages the test by its window, which exact serving
// does not. It returns one line per call it named, for the set's log.
func markRepeatOf(results []models.TestResult, consumers map[string][]string, testCases []*models.TestCase) []string {
	began := make(map[string]time.Time, len(testCases))
	for _, tc := range testCases {
		if tc == nil {
			continue
		}
		if req, _ := recordReqResTimestamps(tc); !req.IsZero() {
			began[tc.Name] = req
		}
	}
	var named []string
	for i := range results {
		r := &results[i]
		for j := range r.FailureInfo.UnmatchedCalls {
			c := &r.FailureInfo.UnmatchedCalls[j]
			if c.Repeat == nil || c.Repeat.Mock == "" {
				continue
			}
			// The note is the agent's: written on a copy, never through.
			n := c.Repeat.Clone()
			if n.Test == "" {
				n.Test = earlierConsumer(consumers[n.Mock], r.TestCaseID)
			}
			if start := began[r.TestCaseID]; !start.IsZero() && !n.RecordedAt.IsZero() {
				gap := models.NewRepeatNote(n.Mock, time.Time{}, n.RecordedAt, time.Time{}, start)
				n.RecordGapSeconds, n.RecordGapKnown = gap.RecordGapSeconds, gap.RecordGapKnown
			}
			c.Repeat = n
			c.NextSteps = c.Repeat.Explain(repeatRecordCommand)
			named = append(named, repeatLine(r.TestCaseID, c))
		}
	}
	return named
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
	line := fmt.Sprintf("%s: %s repeats %s, answered for %s %.2fs earlier at replay", test, c.ActualSummary, n.Mock, by, n.ReplayGapSeconds)
	if n.RecordGapKnown {
		line += fmt.Sprintf(", %.2fs apart at record", n.RecordGapSeconds)
	}
	return line
}

// nameRepeats writes into the end-of-run mismatch rows of testSetID the
// notes markRepeatOf completed on results (the earlier test named, the gap
// at record measured), so the table the CLI prints says the same as the
// report.
func (tfs *TestFailureStore) nameRepeats(testSetID string, results []models.TestResult) {
	byTest := map[string]map[string]*models.RepeatNote{}
	for i := range results {
		for _, c := range results[i].FailureInfo.UnmatchedCalls {
			if c.Repeat == nil {
				continue
			}
			if byTest[results[i].TestCaseID] == nil {
				byTest[results[i].TestCaseID] = map[string]*models.RepeatNote{}
			}
			byTest[results[i].TestCaseID][c.Repeat.Mock] = c.Repeat
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
		n := byTest[f.TestID][f.MismatchReport.Repeat.Mock]
		if n == nil {
			continue
		}
		rep := *f.MismatchReport
		rep.Repeat = n.Clone()
		rep.NextSteps = rep.Repeat.Explain(repeatRecordCommand)
		f.MismatchReport = &rep
	}
}
