package http

import (
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// oneRecording is a store that holds one recording of mk(body), answered
// {"hit":1}.
func oneRecording(t *testing.T, h *HTTP, mk func(string) string, body string) *mockMemDb {
	t.Helper()
	return &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
		recordedAs(t, h, "mock-one-1", 1, mk(body), jsonAnswer(200, `{"hit":1}`), nil)}}
}

// When one recording of a call's method and path is left to serve it (the
// body-keys pass's one match for JSON, the fuzzy fallback's one candidate
// for a form), a field only one of the two bodies holds, the call's or the
// recording's, outside noise, makes the recording another call's: with the
// proven switch off the call is still served it, and reported closest_body,
// naming every field the two differ at. The next step's keploy.yml noise,
// read as keploy reads keploy.yml, makes the recording the call's own, and
// the call is then served with no report. Under the switch the call is not
// served the recording, and nothing reports it answered. Before, each was
// served as the call's own, with no report.
func TestTheOneRecordingLeftIsAnotherCallsWhereAFieldIsOnOneSide(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recorded  string
		call      string
		form      bool
		noise     map[string][]string // keploy.yml's request-body noise
		differs   []string
		nextNoise bool // the next step's noise makes the recording the call's own
	}{
		{name: "the quickstart's POST: a field only the call has", recorded: `{"name":"ada"}`, call: `{"name":"ada","role":"admin"}`, differs: []string{"body.role"}, nextNoise: true},
		{name: "a nested field only the recording has", recorded: `{"user":{"name":"ada","role":"admin"}}`, call: `{"user":{"name":"ada"}}`, differs: []string{"body.user.role"}, nextNoise: true},
		{name: "a null field only the call has", recorded: `{"name":"ada"}`, call: `{"name":"ada","role":null}`, differs: []string{"body.role"}, nextNoise: true},
		{name: "a field on one side beside a value that differs", recorded: `{"name":"ada"}`, call: `{"name":"bob","role":"admin"}`, differs: []string{"body.name", "body.role"}, nextNoise: true},
		{name: "a form key only the call has", recorded: `q=ada`, call: `q=ada&admin=1`, form: true, differs: []string{"body.admin"}, nextNoise: true},
		{name: "a form key only the recording has", recorded: `q=ada&admin=1`, call: `q=ada`, form: true, differs: []string{"body.admin"}, nextNoise: true},
		{name: "a form key only the call has, written encoded", recorded: `q=ada`, call: `q=ada&ad%6din=1`, form: true, differs: []string{"body.admin"}, nextNoise: true},
		{name: "a form key only the call has, escaped in capitals", recorded: `q=ada`, call: `q=ada&ad%6Din=1`, form: true, differs: []string{"body.admin"}, nextNoise: true},
		{name: "a form key only the call has, named in capitals", recorded: `q=ada`, call: `q=ada&userId=1`, form: true, differs: []string{"body.userId"}, nextNoise: true},
		{name: "a form key only the call has, guarded by its decoded name", recorded: `q=ada`, call: `q=ada&ad%6Din=1`, form: true, noise: map[string][]string{"admin": {"^1$"}}, differs: []string{"body.admin"}},
	} {
		mk := searchCall
		if tc.form {
			mk = formSearchCall
		}
		opts := models.OutgoingOptions{}
		if tc.noise != nil {
			opts.NoiseConfig = map[string]map[string][]string{"requestbody": tc.noise}
		}
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			got := serveReportedWith(t, h, oneRecording(t, h, mk, tc.recorded), opts, mk(tc.call))
			a := got.answers()
			if got.status != 200 || got.body != `{"hit":1}` || len(a) != 1 || a[0].Answer != models.AnswerClosestBody || !slices.Equal(a[0].Differs, tc.differs) || a[0].ClosestMock != "mock-one-1" {
				t.Fatalf("%s against the one recording %s got %d %q with answer reports %+v, want it served, and one %q of mock-one-1 naming %q", tc.call, tc.recorded, got.status, got.body, a, models.AnswerClosestBody, tc.differs)
			}
			if !tc.nextNoise {
				return
			}
			next := models.OutgoingOptions{NoiseConfig: keployYmlRequestBodyNoise(t, a[0].NextSteps)}
			h = newHTTP()
			again := serveReportedWith(t, h, oneRecording(t, h, mk, tc.recorded), next, mk(tc.call))
			if again.status != 200 || again.body != `{"hit":1}` || len(again.answers()) != 0 {
				t.Fatalf("with the next step's keploy.yml noise %v, %s got %d %q with answer reports %+v, want the recording served as its own", next.NoiseConfig, tc.call, again.status, again.body, again.answers())
			}
		})
		t.Run(tc.name+", proven switch", func(t *testing.T) {
			setProven(t, true)
			h := newHTTP()
			got := serveReportedWith(t, h, oneRecording(t, h, mk, tc.recorded), opts, mk(tc.call))
			if a := got.answers(); len(a) != 0 {
				t.Fatalf("%s was reported answered under the switch: %+v", tc.call, a)
			}
			if got.status == 200 && got.body == `{"hit":1}` {
				t.Fatalf("%s was served the recording of %s under the switch", tc.call, tc.recorded)
			}
		})
	}
}

// The one recording left serves the call as its own, with no report, where
// no field is on one side only outside noise: the bodies are the same
// outside noise, keploy.yml's request-body noise covers the field only one
// holds, or only values differ (a value alone is no new field). In each
// noise mode the switch leaves off.
func TestTheOneRecordingLeftIsTheCallsOwnWithNoFieldOnOneSide(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		call     string
		form     bool
		noise    map[string][]string
	}{
		{name: "the same body, keys in another order", recorded: `{"name":"ada","age":3}`, call: `{"age":3,"name":"ada"}`},
		{name: "noise covers the field only the call has", recorded: `{"name":"ada"}`, call: `{"name":"ada","role":"admin"}`, noise: map[string][]string{"role": {}}},
		{name: "noise covers the field only the call has, and a value differs", recorded: `{"name":"ada"}`, call: `{"name":"bob","role":"admin"}`, noise: map[string][]string{"role": {}}},
		{name: "noise covers a nested field only the recording has", recorded: `{"user":{"name":"ada","role":"admin"}}`, call: `{"user":{"name":"ada"}}`, noise: map[string][]string{"user.role": {}}},
		{name: "noise covers the form key only the call has", recorded: `q=ada`, call: `q=ada&admin=1`, form: true, noise: map[string][]string{"admin": {}}},
		{name: "noise covers the form key in capitals only the call has", recorded: `q=ada`, call: `q=ada&userId=1`, form: true, noise: map[string][]string{"userId": {}}},
		{name: "noise covers the form key in capitals only the recording has", recorded: `q=ada&userId=1`, call: `q=ada`, form: true, noise: map[string][]string{"userId": {}}},
		{name: "noise covers the form key the call escapes", recorded: `q=ada`, call: `q=ada&ad%6Din=1`, form: true, noise: map[string][]string{"admin": {}}},
		{name: "a value alone differs", recorded: `{"name":"ada"}`, call: `{"name":"bob"}`},
		{name: "a form value alone differs", recorded: `q=ada&n=1`, call: `q=bob&n=1`, form: true},
		{name: "a form key written another way", recorded: `q=ada&ad%6Din=1`, call: `q=ada&admin=1`, form: true},
		{name: "a form key written another way, by the call", recorded: `q=ada&admin=1`, call: `q=ada&ad%6Din=1`, form: true},
	} {
		mk := searchCall
		if tc.form {
			mk = formSearchCall
		}
		opts := models.OutgoingOptions{}
		if tc.noise != nil {
			opts.NoiseConfig = map[string]map[string][]string{"requestbody": tc.noise}
		}
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			got := serveReportedWith(t, h, oneRecording(t, h, mk, tc.recorded), opts, mk(tc.call))
			if got.status != 200 || got.body != `{"hit":1}` || len(got.answers()) != 0 {
				t.Fatalf("%s against the one recording %s got %d %q with answer reports %+v, want it served as its own with none", tc.call, tc.recorded, got.status, got.body, got.answers())
			}
		})
	}
}

// A call of another id whose body also holds a field the one recording
// lacks is reported once, as answered with another id's recording: the path
// that differs is what the report names, as for every pass that serves
// another id's recording.
func TestAnotherIDsOneRecordingWithAFieldOnOneSideIsOtherID(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
		recordedAs(t, h, "mock-order-1", 1, orderOfID("1", "application/json", `{"name":"ada"}`), jsonAnswer(200, `{"order":1}`), nil)}}
	got := serveReported(t, h, db, orderOfID("7", "application/json", `{"name":"ada","role":"admin"}`))
	a := got.answers()
	if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerOtherID || !slices.Equal(a[0].Differs, []string{"url.path.1"}) {
		t.Fatalf("the call got %d %q with answer reports %+v, want one %q at url.path.1", got.status, got.body, a, models.AnswerOtherID)
	}
}

// noteFieldsOnOneSide notes nothing for a match that only diagnoses a call
// (no how), for a call already noted another answer, or for a recording
// that records no request.
func TestNoteFieldsOnOneSideNotesOnlyAServedCallsOwn(t *testing.T) {
	h := newHTTP()
	x := recordedAs(t, h, "mock-one-1", 1, searchCall(`{"name":"ada"}`), jsonAnswer(200, `{"hit":1}`), nil)
	call := []byte(`{"name":"ada","role":"admin"}`)
	h.noteFieldsOnOneSide(nil, x, call, nil)
	other := servedAs{answer: models.AnswerOtherID}
	h.noteFieldsOnOneSide(&other, x, call, nil)
	if other.answer != models.AnswerOtherID || other.fields != nil {
		t.Fatalf("a call noted %q became %+v", models.AnswerOtherID, other)
	}
	var none servedAs
	h.noteFieldsOnOneSide(&none, &models.Mock{Name: "no-request"}, call, nil)
	if none.answer != "" {
		t.Fatalf("a recording with no request noted %+v", none)
	}
	var own servedAs
	h.noteFieldsOnOneSide(&own, x, call, nil)
	if own.answer != models.AnswerClosestBody || !slices.Equal(own.fields, []string{"body.role"}) {
		t.Fatalf("the call noted %+v, want %q at body.role", own, models.AnswerClosestBody)
	}
	for i := range 3 { // the fields are found in map order
		var again servedAs
		h.noteFieldsOnOneSide(&again, x, []byte(`{"name":"bob","role":"admin","z":`+strconv.Itoa(i)+`}`), nil)
		if want := []string{"body.name", "body.role", "body.z"}; !slices.Equal(again.fields, want) {
			t.Fatalf("the call noted %+v, want fields %q", again, want)
		}
	}
}
