package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A place keploy.yml guards with patterns is decided by its patterns alone,
// though an older keploy learned it as noise whole at a text of more than
// one word: a live value outside them is refused with the patterns' reason,
// never with the one that says to record the test again, which is for noise
// that would excuse the place; a live value inside them is served. It is
// so at a JSON field, a form field, a multipart field and a header.
func TestReplayReadsAGuardedPlaceByItsPatternsNotByAnOlderKeploysWholeNoise(t *testing.T) {
	const notePat, tracePat = `^lead r-\d+ created$`, `^tenant=acme; span=s-\d+$`
	body := requestBodyNoise(map[string][]string{"note": {notePat}}).Body
	header := map[string][]string{"x-trace": {tracePat}}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	multipart := map[string]string{"Content-Type": "multipart/form-data; boundary=" + noteBoundary}
	for _, c := range []struct {
		name, place, pat string
		call             func(v string) *req
		rec, in, out     string
	}{
		{"a JSON field", "body.note", notePat, func(v string) *req {
			return live("POST", "/v1/leads", `{"note":"`+v+`","item":"book"}`, map[string]string{"Content-Type": "application/json"})
		}, "lead r-1 created", "lead r-7 created", "deal x-3 deleted"},
		{"a form field", "body.note", notePat, func(v string) *req {
			return live("POST", "/v1/leads", "note="+url.QueryEscape(v)+"&item=book", form)
		}, "lead r-1 created", "lead r-7 created", "deal x-3 deleted"},
		{"a multipart field", "body.note", notePat, func(v string) *req { return live("POST", "/v1/leads", multipartNote(v), multipart) },
			"lead r-1 created", "lead r-7 created", "deal x-3 deleted"},
		{"a header", "header.x-trace", tracePat, func(v string) *req { return live("GET", "/v1/leads", "", map[string]string{"X-Trace": v}) },
			"tenant=acme; span=s-1", "tenant=acme; span=s-7", "tenant=evil; span=x-3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := c.call(c.rec)
			m := learnRecorded("mock-0", 0, in.method, in.url.String(), string(in.body), headerOf(in), `{"ok":true}`)
			m.Spec.ReqBodyNoise = map[string][]string{c.place: {}}
			if got, diag := strictMatch(t, c.call(c.in), header, body, nil, m); got == nil {
				t.Fatalf("%q, inside keploy.yml's patterns, refused (differs %v); want served", c.in, differs(diag))
			}
			got, diag := strictMatch(t, c.call(c.out), header, body, nil, m)
			if got != nil || diag == nil {
				t.Fatalf("%q, outside keploy.yml's patterns, served; want refused", c.out)
			}
			want := []models.FieldReason{{Field: c.place, Why: matcher.PatternWhy(c.place, []string{c.pat}, "live", matcher.StringShape(len(c.out)))}}
			if got := reasonsOf(diag.differs); !reflect.DeepEqual(got, want) {
				t.Fatalf("reasons %+v; want the patterns' %+v alone", got, want)
			}
		})
	}
}

// multipartUpload is a multipart form of a file part, upload, and a field,
// item.
func multipartUpload(data string) string {
	return "--" + noteBoundary + "\r\nContent-Disposition: form-data; name=\"upload\"; filename=\"lead.txt\"\r\nContent-Type: text/plain\r\n\r\n" + data +
		"\r\n--" + noteBoundary + "\r\nContent-Disposition: form-data; name=\"item\"\r\n\r\nbook\r\n--" + noteBoundary + "--\r\n"
}

// Learned noise excuses no file part of a multipart form, so noise an older
// keploy learned whole at one is no noise that no longer excuses it: a
// changed file is refused, naming its part, with no reason to record the
// test again.
func TestReplayGivesAChangedFilePartNoReasonOfAnOlderKeploysWholeNoise(t *testing.T) {
	ct := map[string]string{"Content-Type": "multipart/form-data; boundary=" + noteBoundary}
	in := live("POST", "/v1/leads", multipartUpload("lead r-1 created"), ct)
	m := learnRecorded("mock-0", 0, in.method, in.url.String(), string(in.body), headerOf(in), `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.upload": {}}
	got, diag := strictMatch(t, live("POST", "/v1/leads", multipartUpload("deal r-2 deleted"), ct), nil, nil, nil, m)
	if got != nil || diag == nil || !hasField(differs(diag), "body.upload") {
		t.Fatalf("served %v (differs %v); want refused, naming body.upload", got != nil, differs(diag))
	}
	if r := reasonsOf(diag.differs); len(r) != 0 {
		t.Fatalf("reasons %+v; want none", r)
	}
}

// A words pattern names the words of its one text, at that text's own
// place: one written at a field's name in the request matcher's dialect
// (an array's element field, body.items[].note) excuses no element's text
// whole. Record's full round learns the element's changed words at its own
// place, so the rest of it is still compared at replay.
func TestLearnMatchAWordsPatternAtADialectNameExcusesNoTextWhole(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	m := learnRecorded("mock-0", 0, "POST", "/v1/leads", `{"items":[{"note":"lead r-1 created"}]}`, nil, `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.items[].note": {learn.NoiseWordsPattern([]int{1})}}
	db := &learnDb{perTest: []*models.Mock{m}, observe: tr.ServedMock}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/leads", `{"items":[{"note":"deal r-2 deleted"}]}`, nil), db, n, opts)
	if err != nil || served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("served %v (miss %v, err %v); want served, its changed words learned", served != nil, miss != nil, err)
	}
	want := map[string][]string{
		"body.items[].note":  {learn.NoiseWordsPattern([]int{1})},
		"body.items[0].note": {learn.NoiseWordsPattern([]int{0, 1, 2})},
	}
	if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
		t.Fatalf("learned %v; want %v", got, want)
	}
}

// keploy.yml's patterns decide a place they guard, though the mock learned
// the words of its text that changed: a live value that differs from the
// recorded one only in a learned word, but is outside the patterns, is
// refused with the patterns' reason; a live value inside them is served. It
// is so at a JSON field, a form field, a multipart field and a header.
func TestReplayReadsAGuardedPlaceByItsPatternsNotByItsLearnedWords(t *testing.T) {
	const pat = `^lead r-\d+ created$`
	body := requestBodyNoise(map[string][]string{"note": {pat}}).Body
	header := map[string][]string{"x-trace": {pat}}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	multipart := map[string]string{"Content-Type": "multipart/form-data; boundary=" + noteBoundary}
	const rec, in, out = "lead r-1 created", "lead r-7 created", "lead x-3 created"
	for _, c := range []struct {
		name, place string
		call        func(v string) *req
	}{
		{"a JSON field", "body.note", func(v string) *req {
			return live("POST", "/v1/leads", `{"note":"`+v+`","item":"book"}`, map[string]string{"Content-Type": "application/json"})
		}},
		{"a form field", "body.note", func(v string) *req { return live("POST", "/v1/leads", "note="+url.QueryEscape(v)+"&item=book", form) }},
		{"a multipart field", "body.note", func(v string) *req { return live("POST", "/v1/leads", multipartNote(v), multipart) }},
		{"a header", "header.x-trace", func(v string) *req { return live("GET", "/v1/leads", "", map[string]string{"X-Trace": v}) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.call(rec)
			m := learnRecorded("mock-0", 0, r.method, r.url.String(), string(r.body), headerOf(r), `{"ok":true}`)
			// The mock learned that word 1, "r-1", changed.
			m.Spec.ReqBodyNoise = map[string][]string{c.place: {learn.NoiseWordsPattern([]int{1})}}
			if got, diag := strictMatch(t, c.call(in), header, body, nil, m); got == nil {
				t.Fatalf("%q, inside keploy.yml's patterns, refused (differs %v); want served", in, differs(diag))
			}
			got, diag := strictMatch(t, c.call(out), header, body, nil, m)
			if got != nil || diag == nil {
				t.Fatalf("%q, outside keploy.yml's patterns though it differs only in the learned word, served; want refused", out)
			}
			want := []models.FieldReason{{Field: c.place, Why: matcher.PatternWhy(c.place, []string{pat}, "live", matcher.StringShape(len(out)))}}
			if got := reasonsOf(diag.differs); !reflect.DeepEqual(got, want) {
				t.Fatalf("reasons %+v; want the patterns' %+v alone", got, want)
			}
		})
	}
}

// A miss at a text an older keploy learned as noise whole names no
// keploy.yml entry to change: the user declared none there, so the hint
// offers adding the field, beside recording the test again.
func TestAMissAtAnOlderKeploysWholeNoiseOffersNoKeployYmlEntryToChange(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	in := live("POST", "/v1/leads", `{"note":"lead r-1 created"}`, ct)
	m := learnRecorded("mock-0", 0, in.method, in.url.String(), string(in.body), headerOf(in), `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.note": {}}
	const liveBody = `{"note":"deal r-2 deleted"}`
	call := live("POST", "/v1/leads", liveBody, ct)
	got, diag := strictMatch(t, call, nil, nil, nil, m)
	if got != nil || diag == nil {
		t.Fatal("served; want refused")
	}
	if want := []models.FieldReason{{Field: "body.note", Why: staleWholeWhy("body.note")}}; !reflect.DeepEqual(reasonsOf(diag.differs), want) {
		t.Fatalf("reasons %+v; want %+v", reasonsOf(diag.differs), want)
	}
	request, _ := http.NewRequest(call.method, call.url.String(), strings.NewReader(liveBody))
	request.Header.Set("Content-Type", ct["Content-Type"])
	r := newHTTP().buildHTTPMismatchReport(request, []byte(liveBody), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	if strings.Contains(r.NextSteps, "change the request-body fields' entries") || !strings.Contains(r.NextSteps, "add the request-body fields under test.globalNoise.global.requestbody") {
		t.Fatalf("hint %q; want it to offer adding body.note, never changing an entry keploy.yml lacks", r.NextSteps)
	}
}

// A words pattern written at a field's name in the request matcher's
// dialect excuses no element's text whole, of one word either: an element
// whose one-word text changed is learned as noise at its own place.
func TestLearnMatchAWordsPatternAtADialectNameExcusesNoOneWordTextWhole(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	m := learnRecorded("mock-0", 0, "POST", "/v1/leads", `{"items":[{"note":"lead r-1 created"},{"note":"lead"}]}`, nil, `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.items[].note": {learn.NoiseWordsPattern([]int{1})}}
	db := &learnDb{perTest: []*models.Mock{m}, observe: tr.ServedMock}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/leads", `{"items":[{"note":"lead r-2 created"},{"note":"deal"}]}`, nil), db, n, opts)
	if err != nil || served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("served %v (miss %v, err %v); want served, its changes learned", served != nil, miss != nil, err)
	}
	want := map[string][]string{
		"body.items[].note":  {learn.NoiseWordsPattern([]int{1})},
		"body.items[0].note": {learn.NoiseWordsPattern([]int{1})},
		"body.items[1].note": {},
	}
	if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
		t.Fatalf("learned %v; want %v", got, want)
	}
}
