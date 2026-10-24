package http

import (
	"context"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// repeatOfAnswered is, for a request no mock answered, the per-test recording
// of an earlier test of the set that already answered an identical request at
// this replay (integrations.AnsweredFinder), with when it answered; ok is
// false when there is none, or the store keeps no such history.
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
	return f.FindAnsweredEarlier(models.HTTP, RequestKeysOf(input.method, path, target, n.loose()), match)
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
// says which recording, the gaps at replay and at record, and the likely
// cause.
func markRepeat(report *models.MockMismatchReport, a integrations.AnsweredCall) {
	m := a.Mock
	report.ClosestMock = m.Name
	report.FieldDiffs, report.Differs, report.Diff = nil, nil, ""
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.Repeat = models.NewRepeatNote(m.Name, a.AnsweredAt, m.Spec.ReqTimestampMock, report.At, a.TestRecordedStart)
	report.NextSteps = report.Repeat.Explain(mismatch.RecordCommand())
}
