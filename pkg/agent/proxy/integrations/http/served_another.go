package http

import (
	"context"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// servedAsKey carries, on the context of a match that serves its call
// (decodeHTTP's replay), where matchCascade notes how it served the call
// (servedAs): the matcher's other reads of the store, which only diagnose
// a call (ownCall, answeredMatch), carry none, so nothing they match is
// reported as served.
type servedAsKey struct{}

// servedAs is how matchCascade served a call with the proven switch off:
// answer is "" for the call's own recording, or the answer that says the
// call was served the recording of another call (models.AnsweredAsAnother).
type servedAs struct {
	answer string
}

// withServedAs is ctx carrying a fresh servedAs, which matchCascade fills.
func withServedAs(ctx context.Context) (context.Context, *servedAs) {
	s := &servedAs{}
	return context.WithValue(ctx, servedAsKey{}, s), s
}

// noteServedAs notes, on ctx's servedAs when it carries one, how
// matchCascade served the call: with the recording of another path when
// idVaried (its second pass, which lets id segments differ, found the
// recording), else of another body when closest (the fuzzy fallback served a
// recording whose body is not the call's), else the call's own.
func noteServedAs(ctx context.Context, idVaried, closest bool) {
	s, _ := ctx.Value(servedAsKey{}).(*servedAs)
	if s == nil {
		return
	}
	switch {
	case idVaried:
		s.answer = models.AnswerOtherID
	case closest:
		s.answer = models.AnswerClosestBody
	default:
		s.answer = ""
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
func reportServedAnother(ctx context.Context, input *req, x *models.Mock, answer string) {
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
	call := places.summary(input.method, path)
	r := mismatch.NewReport(mismatch.ProtocolHTTP, call).WithDestination(dest).Build()
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
		livePath := "/"
		if input.url != nil && input.url.Path != "" {
			livePath = input.url.Path
		}
		r.Differs = idSegmentsDiffering(recordedPath, livePath)
		r.Diff = call + " was answered with the recording of " + recordedCall + ": no recording has the call's path, which differs from that recording's only at segments that look like ids (" + strings.Join(r.Differs, ", ") + ")"
		r.NextSteps = "Record the call (keploy mock record) so it is answered with its own recording, not another id's."
	case models.AnswerClosestBody:
		r.Differs = []string{"body"}
		r.Diff = call + " was answered with the recording of " + recordedCall + " whose request body is closest to the call's: no recording of the call's method and path has the call's body"
		r.NextSteps = "Record the call (keploy mock record) so it is answered with its own recording; or mark the body fields that change between runs as noise."
	}
	models.ReportAnswer(ctx, r)
}
