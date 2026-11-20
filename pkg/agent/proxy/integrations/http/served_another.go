package http

import (
	"context"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// servedAs is how matchCascade served a call with the proven switch off
// (matchServing): answer is "" for the call's own recording, or the answer
// that says the call was served the recording of another call
// (models.AnsweredAsAnother).
type servedAs struct {
	answer string
}

// noteServedAs notes, in how when it is not nil, how matchCascade served
// the call: with the recording of another path when idVaried (its second
// pass, which lets id segments differ, found the recording), else of
// another body when closest (the fuzzy fallback served the closest of
// several recordings), else the call's own.
func noteServedAs(how *servedAs, idVaried, closest bool) {
	if how == nil {
		return
	}
	switch {
	case idVaried:
		how.answer = models.AnswerOtherID
	case closest:
		how.answer = models.AnswerClosestBody
	default:
		how.answer = ""
	}
}

// idSegmentsDiffering names the segments at which recorded and live, two
// paths of as many segments, differ, as the learn step names a path's
// places (url.path.<i>, the first segment after the leading "/" at 0).
func idSegmentsDiffering(recorded, live string) []string {
	rs := strings.Split(strings.TrimPrefix(recorded, "/"), "/")
	ls := strings.Split(strings.TrimPrefix(live, "/"), "/")
	var out []string
	for i := range rs {
		if i < len(ls) && rs[i] != ls[i] {
			out = append(out, "url.path."+strconv.Itoa(i))
		}
	}
	return out
}

// reportServedAnother reports, in-band (models.ReportAnswer), a call
// matchCascade served with the recording x of another call (answer, a
// models.AnsweredAsAnother answer): ClosestMock names x, Diff says which
// call x recorded, Differs where the two differ, and NextSteps how to give
// the call its own recording. The call is served either way; the report is
// what keeps keploy mock replay from counting it as the call's own answer.
// The call and x's call are named as the miss's report names them: the
// segments x redacted stay redacted.
//
// The report carries the round its call was made in (call, where the
// request's first read of the round found it: integrations.StampMark), as a
// miss's does, so the proxy places it by that mark. With the switch off,
// the one mode a call is served so, the decode marks no call (the zero
// CallRound), its misses included: the report is placed as they are, by
// when it reaches the proxy.
func reportServedAnother(ctx context.Context, input *req, x *models.Mock, answer string, call integrations.CallRound) {
	if !models.AnsweredAsAnother(answer) || x == nil || x.Spec.HTTPReq == nil {
		return
	}
	path, dest := "/", input.header.Get("Host")
	if input.url != nil {
		if escaped := input.url.EscapedPath(); escaped != "" {
			path = escaped
		}
		if dest == "" {
			dest = input.url.Host
		}
	}
	places := placesOf(x)
	liveCall := places.summary(input.method, path)
	r := mismatch.NewReport(mismatch.ProtocolHTTP, liveCall).WithDestination(dest).Build()
	r.Operation = models.HTTPOperationLabel(liveRequest(nil, input))
	r.Answer, r.ClosestMock = answer, x.Name
	r.PathTag = models.URLPathTag(ctx, input.method, input.url)
	r.ClosestMockReq = renderMockRequest(x.Spec.HTTPReq, x.Noise)
	recordedPath, _ := recordedURLPath(x.Spec.HTTPReq.URL)
	if recordedPath == "" {
		recordedPath = "/"
	}
	recordedCall := string(x.Spec.HTTPReq.Method) + " " + recordedPath
	switch answer {
	case models.AnswerOtherID:
		livePath := ""
		if input.url != nil {
			livePath = input.url.Path
		}
		r.Differs = idSegmentsDiffering(recordedPath, livePath)
		r.Diff = liveCall + " was answered with the recording of " + recordedCall + ": no recording has the call's path, which differs from that recording's only at segments that look like ids (" + strings.Join(r.Differs, ", ") + ")"
		r.NextSteps = "Record the call (keploy mock record) so it is answered with its own recording, not another id's."
	case models.AnswerClosestBody:
		r.Differs = []string{"body"}
		r.Diff = liveCall + " was answered with the recording of " + recordedCall + " whose request body is closest to the call's: no recording of the call's method and path has the call's body"
		r.NextSteps = "Record the call (keploy mock record) so it is answered with its own recording; or mark the body fields that change between runs as noise."
	}
	integrations.StampMark(r, call)
	models.ReportAnswer(ctx, r)
}
