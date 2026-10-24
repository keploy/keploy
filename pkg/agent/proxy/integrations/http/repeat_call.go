package http

import (
	"context"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// repeatOfAnswered is, for a request no mock answered, the per-test recording
// of an earlier test of the set that already answered an identical request at
// this replay (integrations.AnsweredFinder), with when it answered; ok is
// false when there is none, when the store keeps no such history, and when
// the open test has a recording of its own under the call's request keys
// that it has not consumed (ownRecordingUnder): its diff is the miss's
// explanation then.
//
// Identical is the request as the strict comparison reads it (strictDiff),
// configured and learned noise applied, headers aside but for the
// X-Amz-Target, which names an AWS call's action: the method, destination,
// path, query and body. A header such as a trace id or a date differs
// between two calls an application makes for the same answer, and is no
// evidence that the answer it wanted differs.
func repeatOfAnswered(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (integrations.AnsweredCall, bool) {
	f, ok := mockDb.(integrations.AnsweredFinder)
	if !ok {
		return integrations.AnsweredCall{}, false
	}
	live := liveDestination(ctx, input)
	match := func(m *models.Mock) bool {
		return m.Spec.HTTPReq != nil && sameRequest(strictDiff(input, live, m, n))
	}
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	target := strings.Join(input.header.Values(amzTargetHeader), ",")
	keys := RequestKeysOf(input.method, path, target, n.loose())
	if ownRecordingUnder(mockDb, keys) {
		return integrations.AnsweredCall{}, false
	}
	return f.FindAnsweredEarlier(models.HTTP, keys, match)
}

// ownRecordingUnder reports whether the open test has a recording of its own
// under any of keys (models.RequestKeys) that no call of it consumed yet: a
// recording its call may be, changed. The miss then names that recording as
// its closest, with what differs, and gets no repeat note: an earlier test's
// identical recording does not explain a request the application changed, and
// a note there would hide the change behind a cache. A store whose per-test
// read fails counts as having one: no note is the side that hides nothing.
func ownRecordingUnder(mockDb integrations.MockMemDb, keys []string) bool {
	perTest, err := mockDb.GetPerTestMocksInWindow()
	if err != nil {
		return true
	}
	perTest = integrations.WithoutSpent(mockDb, perTest)
	owned := openTestOwner(mockDb, perTest)
	for _, m := range FilterHTTPMocks(perTest) {
		if m == nil || !owned(m) {
			continue
		}
		for _, k := range recordedRequestKeys(m) {
			if slices.Contains(keys, k) {
				return true
			}
		}
	}
	return false
}

// sameRequest reports whether a strict comparison's differences leave the
// request identical (repeatOfAnswered): none, or only in headers other than
// the X-Amz-Target.
func sameRequest(diffs []strictField) bool {
	target := "header." + strings.ToLower(amzTargetHeader)
	for _, d := range diffs {
		p := strings.ToLower(d.diff.Path)
		if !strings.HasPrefix(p, "header.") || p == target {
			return false
		}
	}
	return true
}

// markRepeat makes report the miss of a call identical to a, a recording an
// earlier test made that already answered at this replay: a is its closest
// mock, with no field named as differing, and the note (models.RepeatNote)
// says which recording and how long after its answer this call came. The
// replay measures the gaps at record and the likely cause at the set's end
// (markRepeatOf), from the set's recordings, which the agent does not hold.
func markRepeat(report *models.MockMismatchReport, a integrations.AnsweredCall) {
	m := a.Mock
	report.ClosestMock = m.Name
	report.FieldDiffs, report.Differs, report.Diff = nil, nil, ""
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.Repeat = models.NewRepeatNote(m.Name, a.AnsweredAt, m.Spec.ReqTimestampMock, report.At)
	report.NextSteps = report.Repeat.Explain(mismatch.RecordCommand())
}
