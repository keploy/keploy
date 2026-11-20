package http

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
// record learned on each (MockSpec.ReqBodyNoise), compared both ways and
// whole (mocknoise.SameCallDrift, DiffBothWays), whichever noise mode the
// replay runs in: a JSON body is the same document, so key order and
// spacing are no difference while a field only one side holds, or an array
// of other elements, is; a form holds the same keys and values; any other
// body is the same bytes. Each is a recording of the very call. Each
// recording's body is read once (ownBodies), the call's once per call.
//
// A JSON recording whose print differs from the call's (callPrints) is
// another call's without the two being compared: the prints leave out at
// least what the comparison does not compare, so only a recording whose
// print is the call's, or says nothing, is compared (sameCallChecks counts
// them). Each recording is printed once (ownRead.printOf), the call (live,
// the match's read of it) once per noise the recordings were printed
// under, and only a recording compared is read whole (ownRead.json).
func (h *HTTP) sameCallBodies(candidates []*models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) []*models.Mock {
	prints := newCallPrints(live, userBodyNoise)
	var own []*models.Mock
	for _, m := range candidates {
		recorded, ok := h.ownBodies.read(m)
		if !ok || prints.another(m, recorded) {
			continue
		}
		sameCallChecks.Add(1)
		if len(mocknoise.SameCallDrift(httpNoiseAdapter{}, m, recorded.json(), true, live, userBodyNoise)) == 0 {
			own = append(own, m)
		}
	}
	return own
}

// bodyFieldsDiffering names, sorted, the request-body fields ("body.ts") at
// which live, the call's body, differs from x's, as sameCallBodies compares
// them, a field only the call holds among them: marking each as
// request-body noise makes x the call's own recording. None where the
// bodies have no fields to compare (neither JSON nor a form), or are not
// the same kind of JSON document.
func (h *HTTP) bodyFieldsDiffering(x *models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) []string {
	recorded, ok := h.ownBodies.read(x)
	return bodyFieldsOf(mocknoise.SameCallDrift(httpNoiseAdapter{}, x, recorded.json(), ok, live, userBodyNoise))
}

// bodyFieldsOf names, sorted, the request-body fields of drift
// (SameCallDrift's), the whole body ("body") not among them.
func bodyFieldsOf(drift map[string][]string) []string {
	var out []string
	for p := range drift {
		if strings.HasPrefix(p, "body.") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// noteFieldsOnOneSide notes, in how, a call served x, the one recording of
// its method and path left to serve it (the body-keys pass's one match, the
// fuzzy fallback's one candidate), that noteServedAs noted as served its own
// recording, as answered with the closest body (models.AnswerClosestBody)
// where a field only one of the two bodies holds, the call's or x's
// (fieldsOnOneSide), is a field they differ at outside the noise keploy.yml
// declares and the record learned (SameCallDrift, as sameCallBodies compares
// them): the call sent an input x's call did not, or lacks one it sent, so x
// is another call's recording. Its fields are then every field the two
// differ at, as for the closest of several. Where only values differ, x is
// still served as the call's own, with no report. Nothing is noted where how
// is nil (a match that only diagnoses the call) or noted another answer.
func (h *HTTP) noteFieldsOnOneSide(how *servedAs, x *models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) {
	if how == nil || how.answer != "" {
		return
	}
	recorded, ok := h.ownBodies.read(x)
	if !ok {
		return
	}
	oneSide := fieldsOnOneSide(x, recorded.json(), live)
	if len(oneSide) == 0 {
		return
	}
	drift := mocknoise.SameCallDrift(httpNoiseAdapter{}, x, recorded.json(), true, live, userBodyNoise)
	for p := range drift {
		if oneSide[p] {
			how.answer, how.fields = models.AnswerClosestBody, bodyFieldsOf(drift)
			return
		}
	}
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
// A how with no such answer (a call served its own recording, or one no
// pass served) reports nothing.
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
	r.ClosestMockReq = renderedOf(x)
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

// renderedOf is x's recorded request (x records one) as renderMockRequest
// renders it, for the report of a call served x: rendered once for each
// recorded request and kept (servedRenders) by what renderMockRequest reads
// of it (renderKey), so the calls served x are reported without rendering
// x again, and a recording whose request or redacted values are not what
// was rendered is rendered again.
func renderedOf(x *models.Mock) string {
	key := renderKey{places: placesKey(x.Spec.HTTPReq, x.Noise), method: x.Spec.HTTPReq.Method}
	if s, ok := servedRenders.get(key); ok {
		return s
	}
	rendersMade.Add(1)
	s := renderMockRequest(x.Spec.HTTPReq, x.Noise)
	servedRenders.put(key, s)
	return s
}

// renderKey is what renderMockRequest reads of a recording: its request's
// URL, query parameters, headers and body and the patterns of the values
// the obfuscator redacted (placesKey's digest of them), and its method.
type renderKey struct {
	places placesDigest
	method models.Method
}

// servedRenders are the recorded requests renderedOf rendered, by their
// renderKey.
var servedRenders keptRenders

// rendersMade counts the requests renderedOf rendered, so a row can count
// that a recording is rendered once however many calls it serves.
var rendersMade atomic.Int64

// maxKeptRenders bounds the rendered text keptRenders holds: past it, it
// starts again from none.
const maxKeptRenders = 8 << 20

// keptRenders are rendered requests by their renderKey, holding at most
// maxKeptRenders bytes of them. Safe for concurrent use.
type keptRenders struct {
	mu    sync.Mutex
	bytes int
	of    map[renderKey]string
}

func (k *keptRenders) get(key renderKey) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.of[key]
	return s, ok
}

func (k *keptRenders) put(key renderKey, s string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.of == nil || k.bytes+len(s) > maxKeptRenders {
		k.of, k.bytes = map[renderKey]string{}, 0
	}
	k.bytes += len(s)
	k.of[key] = s
}
