package http

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// servedAs is how matchCascade served a call with the proven switch off
// (matchServing): answer is "" for the call's own recording, or the answer
// that says the call was served the recording of another call
// (models.AnsweredAsAnother); fields, for models.AnswerClosestBody, are the
// body fields the call differs from that recording at
// (bodyFieldsDiffering).
type servedAs struct {
	answer string
	fields []string
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

// sameCallBodies are the candidates whose request body is the call's (body)
// outside the noise keploy.yml declares (userBodyNoise) and the noise the
// record learned on each (MockSpec.ReqBodyNoise), as strict enforcement
// compares them (mocknoise.Engine.StrictReject), whichever noise mode the
// replay runs in: a JSON body field by field, so key order and spacing are
// no difference; a form key by key; any other body byte for byte. Each is a
// recording of the very call.
func (h *HTTP) sameCallBodies(candidates []*models.Mock, body []byte, userBodyNoise map[string][]string) []*models.Mock {
	eng := mocknoise.New(h.MockNoise(), false, true)
	live := mocknoise.NewJSONBody(body)
	var own []*models.Mock
	for _, m := range candidates {
		recorded, ok := h.MockNoise().RecordedBody(m)
		if allowed, _ := eng.StrictRejectRecordedBody(m, recorded, ok, live, userBodyNoise); allowed {
			own = append(own, m)
		}
	}
	return own
}

// bodyFieldsDiffering names, sorted, the request-body fields ("body.ts") at
// which body, the call's, differs from x's, outside the noise sameCallBodies
// reads: marking each as request-body noise makes x the call's own
// recording. None where the bodies have no fields to compare (neither JSON
// nor a form).
func (h *HTTP) bodyFieldsDiffering(x *models.Mock, body []byte, userBodyNoise map[string][]string) []string {
	recorded, ok := h.MockNoise().RecordedBody(x)
	_, drift := mocknoise.New(h.MockNoise(), false, true).StrictRejectRecorded(x, recorded, ok, body, userBodyNoise)
	var out []string
	for p := range drift {
		if strings.HasPrefix(p, "body.") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// urlNoiseEntry is the keploy.yml url-noise key (test.globalNoise.global.url,
// a regex MatchURLPath masks a path with) that makes recorded, a recorded
// path, match each path that differs from it only at the segments differs
// names (idSegmentsDiffering's url.path.<i>): anchored to the whole path,
// every other segment as it is, an all-digit segment as [0-9]+ and any other
// as [^/]+. It is written Go-quoted, which YAML reads as the same string.
func urlNoiseEntry(recorded string, differs []string) string {
	varied := map[string]bool{}
	for _, d := range differs {
		varied[strings.TrimPrefix(d, "url.path.")] = true
	}
	segs := strings.Split(strings.TrimPrefix(recorded, "/"), "/")
	for i, s := range segs {
		switch {
		case !varied[strconv.Itoa(i)]:
			segs[i] = regexp.QuoteMeta(s)
		case reSegAllDigits.MatchString(s):
			segs[i] = "[0-9]+"
		default:
			segs[i] = "[^/]+"
		}
	}
	return strconv.Quote("^/" + strings.Join(segs, "/") + "$")
}

// requestBodyNoiseEntries are the keploy.yml request-body noise entries
// (test.globalNoise.global.requestbody, root-relative keys) that excuse the
// body fields fields names ("body.ts"), each Go-quoted, which YAML reads as
// the same string, with no patterns: `"ts": []`.
func requestBodyNoiseEntries(fields []string) string {
	entries := make([]string, 0, len(fields))
	for _, f := range fields {
		entries = append(entries, strconv.Quote(strings.TrimPrefix(f, "body."))+": []")
	}
	return strings.Join(entries, ", ")
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
// matchCascade served with the recording x of another call (how: its
// answer, a models.AnsweredAsAnother answer, and for a body the fields it
// differs at): ClosestMock names x, Diff says which call x recorded, Differs
// where the two differ, and NextSteps the two ways to give the call its own
// recording: record it, a new input; or, where what differs changes on every
// run, the keploy.yml noise entry that makes x its own (urlNoiseEntry,
// requestBodyNoiseEntries). The call is served either way; the report is
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
func reportServedAnother(ctx context.Context, input *req, x *models.Mock, how servedAs, call integrations.CallRound) {
	answer := how.answer
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
		r.NextSteps = "If the call is a new input, record it (keploy mock record) so it is answered with its own recording. " +
			"If the app makes a new id there on every run, mark it as url noise in keploy.yml, under test.globalNoise.global.url: {" + urlNoiseEntry(recordedPath, r.Differs) + ": []}; each run's call is then answered with this recording as its own."
	case models.AnswerClosestBody:
		r.Differs = how.fields
		if len(r.Differs) == 0 {
			r.Differs = []string{"body"}
		}
		r.Diff = liveCall + " was answered with the recording of " + recordedCall + " whose request body is closest to the call's: no recording of the call's method and path has the call's body outside its noise"
		r.NextSteps = "If the call is a new input, record it (keploy mock record) so it is answered with its own recording."
		if len(how.fields) > 0 {
			r.NextSteps += " If the fields it differs at (" + strings.Join(how.fields, ", ") + ") change on every run, mark them as noise in keploy.yml, under test.globalNoise.global.requestbody: {" + requestBodyNoiseEntries(how.fields) + "}; the call is then answered with this recording as its own."
		}
	}
	integrations.StampMark(r, call)
	models.ReportAnswer(ctx, r)
}
