package integrations

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
)

// ONE CALL MORE THAN THE TEST RECORDED (under the proven switch).
//
// Under exact serving each test recording answers one call, and is then in
// no read: a call the test makes once more than it recorded finds none and
// misses (models.MissExtraCall). The miss names the recording the call
// repeats as its closest (models.MockMismatchReport.ClosestMock), never
// another recording in view: a recording of another request, diffed against
// the call, would read as a value to declare as noise, and that noise would
// answer the call with the other request's answer. Judging reads the name: a
// test whose only misses are such copies, made on the application's own
// clock, has the recording it repeats marked as its background work, which
// keploy then answers again in the test's window (AnsweredBackgroundReader).

// RepeatedRecording is, of copies, the open test's recordings of one call
// that already answered, the recording a call made once more repeats: the
// one recorded last. It is the newest answer the test recorded to the call,
// and a mark on it leaves the copies recorded before it required: a replay
// that makes the call as often as it was recorded is answered by them first,
// and one that makes it once less leaves only it unmade. Recorded order is
// the request's timestamp, which is stable across replays (a match changes
// SortOrder), then the name. nil when copies is empty.
func RepeatedRecording(copies []*models.Mock) *models.Mock {
	var last *models.Mock
	for _, m := range copies {
		if m == nil {
			continue
		}
		if last == nil || recordedAfter(m, last) {
			last = m
		}
	}
	return last
}

// recordedAfter reports whether a was recorded after b: by the request's
// timestamp, and by name at one timestamp.
func recordedAfter(a, b *models.Mock) bool {
	ta, tb := a.Spec.ReqTimestampMock, b.Spec.ReqTimestampMock
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.Name > b.Name
}

// MarkExtraCall makes report the miss of one call more than the open test
// recorded: recorded is how many times the test recorded the call, repeats
// the recording it repeats (RepeatedRecording), which answered already. The
// report names repeats as its closest and no other: no field differs from
// it that exact serving reads, so none is named and no noise is suggested,
// and the closest request rendered for a side-by-side diff, another
// recording's, is dropped. A nil report or repeats leaves report as it is.
func MarkExtraCall(report *models.MockMismatchReport, repeats *models.Mock, recorded int) {
	if report == nil || repeats == nil {
		return
	}
	report.MissKind, report.Recorded = models.MissExtraCall, max(recorded, 1)
	report.ClosestMock = repeats.Name
	report.FieldDiffs, report.Differs, report.ClosestMockReq = nil, nil, ""
	report.Diff = fmt.Sprintf("the call repeats %s, which answered it already", repeats.Name)
	report.NextSteps = fmt.Sprintf("This call repeats %s, which answered already: the test made it once more than the %d time(s) it was recorded. "+
		"Under exact serving (KEPLOY_PROVEN) a recording answers its call once, never again. "+
		"If the build now makes the call more often, record again; declaring noise would not answer it.",
		repeats.Name, max(recorded, 1))
}
