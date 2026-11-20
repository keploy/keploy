package http

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// formSearchCall is searchCall with a form body.
func formSearchCall(body string) string {
	return "POST /search HTTP/1.1\r\nHost: users.local\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// keployYmlRequestBodyNoise is the request-body noise keploy reads, as the
// CLI reads keploy.yml (loader.Read, then Unmarshal), from a keploy.yml whose
// test.globalNoise.global.requestbody is the flow mapping the next step next
// names after its "test.globalNoise.global.requestbody: ".
func keployYmlRequestBodyNoise(t *testing.T, next string) map[string]map[string][]string {
	t.Helper()
	const marker = "test.globalNoise.global.requestbody: "
	i := strings.Index(next, marker)
	if i < 0 {
		t.Fatalf("the next step %q does not name %q", next, marker)
	}
	rest := next[i+len(marker):]
	end := strings.Index(rest, "}")
	if !strings.HasPrefix(rest, "{") || end < 0 {
		t.Fatalf("the next step %q names no YAML mapping after %q", next, marker)
	}
	dir := t.TempDir()
	yml := "test:\n  globalNoise:\n    global:\n      requestbody: " + rest[:end+1] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.SetConfigType("yml")
	files := loader.New(v)
	if err := files.Read(loader.Find(dir)); err != nil {
		t.Fatalf("keploy refuses the keploy.yml %q: %v", yml, err)
	}
	cfg := config.New()
	if err := files.Unmarshal(cfg); err != nil {
		t.Fatalf("keploy refuses the keploy.yml %q: %v", yml, err)
	}
	return cfg.Test.GlobalNoise.Global
}

// A call that sends an input no recording of its method and path has is not
// any recording's own, whichever side holds the field: a field only the call
// holds (at the top, nested, in a form) is a difference as a field only the
// recording holds is, and so is an array with another element anywhere or
// of another length. With the proven switch off the call is served the
// closest recording and reported closest_body, in the default noise mode,
// naming the field; the next step's entries, read as
// keploy reads keploy.yml, make the recordings the call's own, and the call
// is then served with no report. Under the switch it is reported answered by
// no recording. Before, a call whose body was a recording's plus a field was
// served that recording as its own, with no report.
func TestACallWithAnInputNoRecordingHasIsServedAnotherCallsRecording(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded []string
		call     string
		form     bool
		names    string // a field the report names
	}{
		{name: "a field only the call has", recorded: []string{`{"q":"ada","x":1}`, `{"q":"alan","x":1}`}, call: `{"q":"ada","x":1,"admin":true}`, names: "body.admin"},
		{name: "a nested field only the call has", recorded: []string{`{"q":"ada","f":{"x":1}}`, `{"q":"alan","f":{"x":1}}`}, call: `{"q":"ada","f":{"x":1,"y":2}}`, names: "body.f.y"},
		{name: "the quickstart's POST shape", recorded: []string{`{"name":"ada"}`, `{"name":"alan"}`}, call: `{"name":"ada","role":"admin"}`, names: "body.role"},
		{name: "a form key only the call has", recorded: []string{`q=ada&ts=1`, `q=alan&ts=1`}, call: `q=ada&ts=1&admin=1`, form: true, names: "body.admin"},
		{name: "an array element, not the last", recorded: []string{`{"ids":[3,2]}`, `{"ids":[5,6]}`}, call: `{"ids":[1,2]}`, names: "body.ids[]"},
		{name: "an array of another length", recorded: []string{`{"ids":[1,1]}`, `{"ids":[2]}`}, call: `{"ids":[1]}`, names: "body.ids[]"},
	} {
		mk := searchCall
		if tc.form {
			mk = formSearchCall
		}
		recordings := func(t *testing.T, h *HTTP) *mockMemDb {
			db := &mockMemDb{updateUnFilteredReturn: true}
			for i, b := range tc.recorded {
				db.mocks = append(db.mocks, recordedAs(t, h, "mock-search-"+strconv.Itoa(i+1), i+1, mk(b), jsonAnswer(200, `{"hit":`+strconv.Itoa(i+1)+`}`), nil))
			}
			return db
		}
		for _, mode := range []struct {
			name string
			opts models.OutgoingOptions
		}{{"default noise mode", models.OutgoingOptions{}}} {
			t.Run(tc.name+", "+mode.name, func(t *testing.T) {
				setProven(t, false)
				h := newHTTP()
				got := serveReportedWith(t, h, recordings(t, h), mode.opts, mk(tc.call))
				a := got.answers()
				if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerClosestBody || !slices.Contains(a[0].Differs, tc.names) {
					t.Fatalf("%s got %d %q with answer reports %+v, want one %q naming %s", tc.call, got.status, got.body, a, models.AnswerClosestBody, tc.names)
				}
				opts := mode.opts
				opts.NoiseConfig = keployYmlRequestBodyNoise(t, a[0].NextSteps)
				h = newHTTP()
				again := serveReportedWith(t, h, recordings(t, h), opts, mk(tc.call))
				if again.status != 200 || len(again.answers()) != 0 {
					t.Fatalf("with the next step's keploy.yml noise %v, %s got %d %q with answer reports %+v, want a recording served as its own", opts.NoiseConfig, tc.call, again.status, again.body, again.answers())
				}
			})
		}
		t.Run(tc.name+", proven switch", func(t *testing.T) {
			setProven(t, true)
			h := newHTTP()
			got := serveReported(t, h, recordings(t, h), mk(tc.call))
			if a := got.answers(); len(a) != 0 {
				t.Fatalf("%s was reported answered under the switch: %+v", tc.call, a)
			}
			if got.status == 200 && strings.HasPrefix(got.body, `{"hit":`) {
				t.Fatalf("%s was served %s under the switch", tc.call, got.body)
			}
		})
	}
}

// A field only the call holds that keploy.yml declares noise is no
// difference: the call is served its own recording with no report.
func TestAFieldOnlyTheCallHasThatNoiseCoversIsNoDifference(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil, `{"q":"ada","x":1}`, `{"q":"alan","x":1}`)
	got := serveReportedWith(t, h, db, models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"requestbody": {"admin": {}}}}, searchCall(`{"q":"ada","x":1,"admin":true}`))
	if got.status != 200 || got.body != `{"hit":1}` || len(got.answers()) != 0 {
		t.Fatalf("the call got %d %q with answer reports %+v, want its own recording and none", got.status, got.body, got.answers())
	}
}

// formKeysOnlyLive names each key the live form holds that the recorded one
// holds neither as written nor by its decoded name, by its decoded name; a
// key an entry with no patterns names, as written or by its decoded name in
// any case, is none, but an entry naming id keys does not name a key only
// the live form holds.
func TestFormKeysOnlyLive(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live string
		known           map[string][]string
		want            []string
	}{
		{name: "the same keys", rec: "q=ada&ts=1", live: "ts=2&q=bob"},
		{name: "a key only the call has", rec: "q=ada", live: "q=ada&admin=1&&", want: []string{"body.admin"}},
		{name: "a key with no value", rec: "q=ada", live: "q=ada&admin", want: []string{"body.admin"}},
		{name: "a key written another way", rec: "a%20b=1", live: "a+b=1"},
		{name: "a key known with no patterns", rec: "q=ada", live: "q=ada&admin=1", known: map[string][]string{"body.admin": {}}},
		{name: "a key known with patterns", rec: "q=ada", live: "q=ada&admin=1", known: map[string][]string{"body.admin": {"^1$"}}, want: []string{"body.admin"}},
		{name: "an escaped key known with patterns as written", rec: "q=ada", live: "q=ada&a%20b=1", known: map[string][]string{"body.a%20b": {"^1$"}}, want: []string{"body.a b"}},
		{name: "a guarded key by its decoded name", rec: "q=ada", live: "q=ada&a%20b=1", known: map[string][]string{"body.a b": {"^1$"}}, want: []string{"body.a b"}},
		{name: "an escaped key", rec: "q=ada", live: "q=ada&ad%6Din=1", want: []string{"body.admin"}},
		{name: "a key in capitals known in lower case", rec: "q=ada", live: "q=ada&userId=1", known: map[string][]string{"body.userid": {}}},
		{name: "an escaped key known by its decoded name", rec: "q=ada", live: "q=ada&ad%6Din=1", known: map[string][]string{"body.admin": {}}},
		{name: "an escaped key known as written", rec: "q=ada", live: "q=ada&ad%6Din=1", known: map[string][]string{"body.ad%6Din": {}}},
		{name: "an id key and an entry naming id keys", rec: "q=ada", live: "q=ada&1234567890=1", known: map[string][]string{"body.{id}": {}}, want: []string{"body.1234567890"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for k := range formKeysOnlyLive(tc.rec, tc.live, tc.known) {
				got = append(got, k)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// SameCallDrift: a recording with no body drifts nowhere; bodies with no
// fields drift whole unless they are the same bytes; and keploy.yml's
// entries with patterns decide the fields they guard before a learned
// container that holds them.
func TestSameCallDrift(t *testing.T) {
	h := newHTTP()
	text := func(body string) *models.Mock {
		return recordedAs(t, h, "m", 1, "POST /t HTTP/1.1\r\nHost: users.local\r\nContent-Type: text/plain\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body, jsonAnswer(200, `{}`), nil)
	}
	m := text("abc")
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, m, mocknoise.NewJSONBody([]byte("abc")), false, mocknoise.NewJSONBody([]byte("abd")), nil); d != nil {
		t.Errorf("a recording with no body drifts at %v", d)
	}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, m, mocknoise.NewJSONBody([]byte("abc")), true, mocknoise.NewJSONBody([]byte("abc")), nil); d != nil {
		t.Errorf("the same text drifts at %v", d)
	}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, m, mocknoise.NewJSONBody([]byte("abc")), true, mocknoise.NewJSONBody([]byte("abd")), nil); len(d) != 1 || d["body"] != nil {
		t.Errorf("another text drifts at %v, want the whole body", d)
	}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, m, mocknoise.NewJSONBody([]byte(`[1]`)), true, mocknoise.NewJSONBody([]byte(`{"a":1}`)), nil); len(d) != 1 || d["body"] != nil {
		t.Errorf("JSON of another kind drifts at %v, want the whole body", d)
	}
	j := recordedAs(t, h, "j", 1, searchCall(`{"user":{"id":"u-1"}}`), jsonAnswer(200, `{}`), nil)
	j.Spec.ReqBodyNoise = map[string][]string{"body.user": {}}
	user := map[string][]string{"id": {"^u-[0-9]+$"}}
	rec := mocknoise.NewReadJSONBody([]byte(`{"user":{"id":"u-1"}}`))
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, j, rec, true, mocknoise.NewJSONBody([]byte(`{"user":{"id":"u-7"}}`)), user); d != nil {
		t.Errorf("an id inside its patterns drifts at %v", d)
	}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, j, rec, true, mocknoise.NewJSONBody([]byte(`{"user":{"id":"x"}}`)), user); len(d) != 1 || d["body.user.id"] == nil {
		t.Errorf("an id outside its patterns, in a learned container, drifts at %v, want body.user.id", d)
	}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, j, rec, true, mocknoise.NewJSONBody([]byte(`{"user":{"id":"x"}}`)), nil); d != nil {
		t.Errorf("a learned container with no keploy.yml patterns drifts at %v", d)
	}
}

// Each recording's body is read once however many calls compare it, read
// again when the recording's body is not the one it was read from, and the
// kept bodies start again from none past ownBodiesMaxBytes.
func TestEachRecordingsBodyIsReadOnce(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil, `{"q":"ada","x":1}`, `{"q":"alan","x":1}`)
	before := ownBodyReads.Load()
	for _, call := range []string{`{"q":"bob","x":1}`, `{"q":"eve","x":1}`, `{"q":"ada","x":1,"y":2}`} {
		if got := serveReported(t, h, db, searchCall(call)); got.status != 200 {
			t.Fatalf("%s got %d %q", call, got.status, got.body)
		}
	}
	if n := ownBodyReads.Load() - before; n != 2 {
		t.Fatalf("three calls read the two recordings' bodies %d times, want 2", n)
	}

	var o ownBodies
	m := &models.Mock{Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: `{"a":1}`}}}
	first, _ := o.read(m)
	m.Spec.HTTPReq.Body = `{"a":2}`
	second, _ := o.read(m)
	if v, _ := second.Decoded(); v.(map[string]any)["a"] != float64(2) || first == second {
		t.Fatalf("a recording whose body changed was not read again: %v", v)
	}
	if _, ok := o.read(&models.Mock{}); ok {
		t.Fatal("a recording of no request has a body")
	}
	big := &models.Mock{Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: `"` + strings.Repeat("x", ownBodiesMaxBytes) + `"`}}}
	o.read(big)
	if len(o.of) != 1 || o.bytes != len(big.Spec.HTTPReq.Body) {
		t.Fatalf("past ownBodiesMaxBytes ownBodies keeps %d bodies of %d bytes, want only the last", len(o.of), o.bytes)
	}
	o.read(m)
	if len(o.of) != 1 {
		t.Fatalf("ownBodies keeps %d bodies past its bound, want 1", len(o.of))
	}
}

// DiffBothWays reads a body as JSON only where both bodies are JSON: a form
// whose live body reads as JSON is compared key by key. A recording of no
// request has no fields to compare, and no recording of no request is the
// call's own.
func TestDiffBothWaysReadsJSONOnlyWhereBothAre(t *testing.T) {
	h := newHTTP()
	m := recordedAs(t, h, "f", 1, formSearchCall("a=1"), jsonAnswer(200, `{}`), nil)
	d, comparable := httpNoiseAdapter{}.DiffBothWays(m, mocknoise.NewJSONBody([]byte("a=1")), mocknoise.NewJSONBody([]byte("1")), nil, nil, nil)
	if !comparable || len(d) != 2 || d["body.a"] == nil || d["body.1"] == nil {
		t.Errorf("the form a=1 against the form 1 drifts at %v (comparable %v), want body.a and body.1", d, comparable)
	}
	if d, comparable := (httpNoiseAdapter{}).DiffBothWays(&models.Mock{}, mocknoise.NewJSONBody([]byte("{}")), mocknoise.NewJSONBody([]byte("{}")), nil, nil, nil); d != nil || comparable {
		t.Errorf("a recording of no request drifts at %v (comparable %v)", d, comparable)
	}
	if own := h.sameCallBodies([]*models.Mock{{Name: "none"}}, []byte(`{"q":1}`), nil); len(own) != 0 {
		t.Errorf("a recording of no request is the call's own: %v", own)
	}
	if f := h.bodyFieldsDiffering(&models.Mock{Name: "none"}, []byte(`{"q":1}`), nil); f != nil {
		t.Errorf("a recording of no request differs at %v", f)
	}
}

// keploy.yml's entries with patterns decide only the fields they guard: a
// learned container still excuses a field in it that no entry guards.
func TestALearnedContainerExcusesAFieldNoPatternGuards(t *testing.T) {
	h := newHTTP()
	j := recordedAs(t, h, "j", 1, searchCall(`{"user":{"id":"u-1"}}`), jsonAnswer(200, `{}`), nil)
	j.Spec.ReqBodyNoise = map[string][]string{"body.user": {}}
	other := map[string][]string{"other": {"^u-[0-9]+$"}}
	if d := mocknoise.SameCallDrift(httpNoiseAdapter{}, j, mocknoise.NewReadJSONBody([]byte(`{"user":{"id":"u-1"}}`)), true, mocknoise.NewJSONBody([]byte(`{"user":{"id":"x"}}`)), other); d != nil {
		t.Errorf("a field in a learned container no entry guards drifts at %v", d)
	}
}
