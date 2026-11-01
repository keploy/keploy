package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// Rows for a variable place that names a word of a value (variables.ParseAddress):
// another protocol's learn step binds a value an HTTP call sent at the word of
// the HTTP recording's field that holds it (a Mongo insert copies the app id
// an OpenBao PUT sent inside a longer text). HTTP's replay reads such a place
// as a word address, as Mongo's does: a place the recording holds as written
// stays whole.

// wordedCreate: the PUT sends the app's id as the third word of a note; the
// read-back asks for it and is answered with it.
func wordedCreate() []*models.Mock {
	return []*models.Mock{
		at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"note":"made by appRec7 at boot"}}`, 204, `{}`,
			models.MockVariables{"appId": bindV("appRec7", "body.data.note#2")}), 1),
		at(varMock("mock-2", "GET", "http://store/apps/appRec7", "", 200, `{"id":"appRec7","msg":"hello appRec7"}`,
			models.MockVariables{"appId": useV("appRec7", []string{"url.path[1]"}, "body.id", "body.msg#1")}), 2),
	}
}

func TestVariables_ABindAtAWordOfABodyFieldIsRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: wordedCreate()}

	ok, stub, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at boot"}}`))
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the PUT should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if b, bound := db.table.Lookup("appId"); !bound || b.Value != "appLive9" || b.Recorded != "appRec7" || b.Mock != "mock-1" || b.Place != "body.data.note#2" {
		t.Fatalf("appId bound as %+v (%v), want appLive9 at mock-1 body.data.note#2", b, bound)
	}
	ok, stub, diag = matchVar(t, h, db, liveReq("GET", "/apps/appLive9", ""))
	if !ok || stub.Name != "mock-2" {
		t.Fatalf("the read-back of this run's id should be answered by mock-2: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if want := `{"id":"appLive9","msg":"hello appLive9"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
}

// The api-server shape: the word is its field's whole text.
func TestVariables_ABindAtTheOneWordOfAFieldIsRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"lastUsedAt":"1730000000","name":"app"}}`, 204, `{}`,
			models.MockVariables{"createdAt": bindV("1730000000", "body.data.lastUsedAt#0")}), 1),
	}}
	ok, stub, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"lastUsedAt":"1730000555","name":"app"}}`))
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the PUT should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if b, bound := db.table.Lookup("createdAt"); !bound || b.Value != "1730000555" || b.Place != "body.data.lastUsedAt#0" {
		t.Fatalf("createdAt bound as %+v (%v)", b, bound)
	}
}

// Every byte of the field outside its variable's word is compared as recorded.
func TestVariables_TheRestOfAWordedFieldIsCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: wordedCreate()}
	for _, note := range []string{"made by appLive9 at noon", "made by appLive9", "made-by appLive9 at boot", "made by app Live9 at boot"} {
		if ok, stub, _ := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"`+note+`"}}`)); ok {
			t.Fatalf("note %q: answered by %s; only the word of the variable may differ", note, stub.Name)
		}
	}
	if _, bound := db.table.Lookup("appId"); bound {
		t.Fatal("a call that is not the recording bound appId")
	}
}

func TestVariables_TwoWordsOfOneFieldAreTwoPlaces(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/links", `{"what":"from appRec7 to ordRec3"}`, 201, `{"link":"appRec7-ordRec3"}`,
			models.MockVariables{
				"appId": bindV("appRec7", "body.what#1", "body.link#0"),
				"ordId": bindV("ordRec3", "body.what#3", "body.link#1"),
			}), 1),
	}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/links", `{"what":"from appLive9 to ordLive4"}`))
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the POST should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if want := `{"link":"appLive9-ordLive4"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
	for name, want := range map[string]string{"appId": "appLive9", "ordId": "ordLive4"} {
		if b, bound := db.table.Lookup(name); !bound || b.Value != want {
			t.Fatalf("%s bound as %+v (%v), want %s", name, b, bound, want)
		}
	}
}

// A word of a query value, a header value and a URL path segment, and one
// under a decode step (a cookie's value); a step's value whole (a cookie's).
func TestVariables_AWordOfAQueryHeaderSegmentOrCookie(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	rec := varMock("mock-2", "GET", "http://store/apps/app-appRec7?ref=v1.appRec7", "", 200, `{}`,
		models.MockVariables{"appId": useV("appRec7", []string{"url.path[1]#1", "url.query.ref#1", "header.X-App#0", "header.Cookie|cookie.sid#1", "header.Cookie|cookie.uid"})})
	rec.Spec.HTTPReq.Header["X-App"] = "appRec7"
	rec.Spec.HTTPReq.Header["Cookie"] = "theme=dark; sid=s.appRec7; uid=appRec7"
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"note":"made by appRec7 at boot"}}`, 204, `{}`,
			models.MockVariables{"appId": bindV("appRec7", "body.data.note#2")}), 1),
		at(rec, 2),
	}}
	if ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at boot"}}`)); !ok {
		t.Fatalf("the PUT should be answered: %+v", diag)
	}
	read := func(cookie, path string) *req {
		r := liveReq("GET", path, "")
		r.header.Set("X-App", "appLive9")
		r.header.Set("Cookie", cookie)
		return r
	}
	if ok, stub, _ := matchVar(t, h, db, read("theme=dark; sid=s.appOther; uid=appLive9", "/apps/app-appLive9?ref=v1.appLive9")); ok {
		t.Fatalf("a cookie that holds another value was answered by %s", stub.Name)
	}
	if ok, stub, _ := matchVar(t, h, db, read("theme=dark; sid=s.appLive9; uid=appLive9", "/apps/app-appLive9?ref=v2.appLive9")); ok {
		t.Fatalf("a query value that differs outside its word was answered by %s", stub.Name)
	}
	ok, stub, diag := matchVar(t, h, db, read("theme=dark; sid=s.appLive9; uid=appLive9", "/apps/app-appLive9?ref=v1.appLive9"))
	if !ok || stub.Name != "mock-2" {
		t.Fatalf("the read-back should be answered by mock-2: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
}

// A place the recording holds as written is that place whole, as it was
// read before word addresses: a JSON key and a query name that end in
// #<digits>.
func TestVariables_APlaceTheRecordingHoldsAsWrittenIsWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/tags?page%231=pgRec1", `{"tag#0":"tagRec5 x"}`, 201, `{"tag#0":"tagRec5 x"}`,
		models.MockVariables{
			"tag":  bindV("tagRec5 x", "body.tag#0", "body.tag#0", "header.X-Tag#0"),
			"page": bindV("pgRec1", "url.query.page#1"),
		})
	m.Spec.HTTPResp.Header["X-Tag#0"] = "tagRec5 x"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	live := liveReq("POST", "/tags", `{"tag#0":"tagLive8 y"}`)
	live.url.RawQuery = "page%231=pgLive2"
	ok, stub, diag := matchVar(t, h, db, live)
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the POST should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if want := `{"tag#0":"tagLive8 y"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
	if got := stub.Spec.HTTPResp.Header["X-Tag#0"]; got != "tagLive8 y" {
		t.Fatalf("header X-Tag#0 answered %q, want tagLive8 y (the header whole)", got)
	}
	if b, _ := db.table.Lookup("page"); b.Value != "pgLive2" {
		t.Fatalf("page bound as %+v, want pgLive2 (the query parameter page#1 whole)", b)
	}
}

// A word is a word of a string. A call that sends a number, a boolean or
// null where the recording holds the string a word place names, and is the
// recording otherwise, is the mock's with that value's type changed: the
// same evidence as at a whole place (a variable_mismatch the miss report
// shows), as Mongo's replay gives of both, and nothing is bound.
func TestVariables_AWordOfAFieldOfAnotherTypeIsATypeChange(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	for _, bind := range []string{"body.data.lastUsedAt", "body.data.lastUsedAt#0"} {
		db := &varDb{perTest: []*models.Mock{
			at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"lastUsedAt":"1730000000"},"x":"rec"}`, 204, `{}`,
				models.MockVariables{"createdAt": bindV("1730000000", bind)}), 1),
		}}
		for _, sent := range []string{`1730000555`, `true`, `null`} {
			ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"lastUsedAt":`+sent+`},"x":"rec"}`))
			p := wantProblem(t, ok, diag, "the call sent "+sent+` where the recording has "1730000000", a value of another JSON type`)
			if p.kind != models.DiffKindTypeChanged || p.place != bind {
				t.Fatalf("%s at %s: evidence %+v; want a type change at the place", sent, bind, p)
			}
			// The rest of the call is still compared.
			if ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"lastUsedAt":`+sent+`},"x":"other"}`)); ok || diag != nil && diag.variable != nil {
				t.Fatalf("%s at %s with another field changed: served %v, evidence %+v; want a plain miss", sent, bind, ok, diag)
			}
		}
		if _, bound := db.table.Lookup("createdAt"); bound {
			t.Fatal("createdAt was bound at a value of another type")
		}
	}
}

// One word declared twice is refused as any place declared twice; two words
// of one field are two places (above).
func TestVariables_AWordDeclaredTwiceIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"note":"made by appRec7 at boot"}}`, 204, `{}`,
			models.MockVariables{"a": bindV("appRec7", "body.data.note#2"), "b": useV("appRec7", []string{`body["data"].note#2`})}), 1),
	}}
	ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at boot"}}`))
	wantProblem(t, ok, diag, "declared twice")
}

// A word of a header's value is answered, and a field a variable answers
// whole holds the word another answers at it.
func TestVariables_AnAnswerAtAWordOfAHeaderOrOfAWholeAnswer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/apps", `{"note":"made by appRec7 at boot"}`, 201, `{"id":"appRec7","self":"appRec7"}`,
		models.MockVariables{"appId": bindV("appRec7", "body.note#2", "header.Location#1", "body.self#0", "body.self", "body.id")})
	m.Spec.HTTPResp.Header["Location"] = "/apps/appRec7"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"note":"made by appLive9 at boot"}`))
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the POST should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if got := stub.Spec.HTTPResp.Header["Location"]; got != "/apps/appLive9" {
		t.Fatalf("Location answered %q, want /apps/appLive9", got)
	}
	if want := `{"id":"appLive9","self":"appLive9"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
}

// What keploy cannot follow at a word is refused with its evidence, never
// served as recorded: a word of a number, a word under a step it cannot
// write back (a JWT claim: the signature would break), and on the route of
// a word of a URL segment.
func TestVariables_AWordKeployCannotFollowIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	const jwtRec = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhcHBSZWM3In0.c2ln"   // {"sub":"appRec7"}
	const jwtLive = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhcHBMaXZlOSJ9.c2lo" // {"sub":"appLive9"}
	jwt := varMock("mock-1", "GET", "http://store/me", "", 200, `{}`,
		models.MockVariables{"appId": bindV("appRec7", "header.Authorization|jwt.sub#0")})
	jwt.Spec.HTTPReq.Header["Authorization"] = jwtRec
	db := &varDb{perTest: []*models.Mock{at(jwt, 1)}}
	r := liveReq("GET", "/me", "")
	r.header.Set("Authorization", jwtLive)
	ok, _, diag := matchVar(t, h, db, r)
	wantProblem(t, ok, diag, "cannot put variable appId's recorded value \"appRec7\" back at header.Authorization|jwt.sub#0")

	db = &varDb{perTest: []*models.Mock{at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"n":1730000000}`, 204, `{}`,
		models.MockVariables{"n": bindV("1730000000", "body.n#0")}), 1)}}
	ok, _, diag = matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"n":1730000555}`))
	wantProblem(t, ok, diag, "which is no string to hold a word of")

	db = &varDb{perTest: []*models.Mock{at(varMock("mock-1", "GET", "http://store/apps/app-appRec7", "", 200, `{}`,
		models.MockVariables{"appId": useV("appRec7", []string{"url.path[1]#1"}), "other": {Use: []string{"body.x"}}}), 1)}}
	ok, _, diag = matchVar(t, h, db, liveReq("GET", "/apps/app-appLive9", ""))
	wantProblem(t, ok, diag, "variable other has no recorded value")
}

// The field diffs of a call that does not fit leave out only the variable's
// word of a worded field: the rest of the field is diffed as it was sent.
func TestVariables_TheDiffOfAWordedFieldKeepsTheRestAsSent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_, body := asRecordedForDiff(httpRequest("PUT", "/secrets/app"), []byte(`{"data":{"note":"made by appLive9 at noon"}}`), wordedCreate()[0])
	if want := `{"data":{"note":"made by appRec7 at noon"}}`; string(body) != want {
		t.Fatalf("diffed %s, want %s", body, want)
	}
}

// An answer at a word that keploy cannot write as recorded is refused with
// its evidence, never served as recorded: the recorded answer holds another
// word there, a value another variable answers whole (in the body or a
// header) holds another word there, a body value of another JSON type, and
// a header recorded on more than one line.
func TestVariables_AWordAnswerKeployCannotWriteIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	for _, c := range []struct {
		name, rec, resp string
		header          map[string]string
		lines           models.HeaderLineLengths
		vars            models.MockVariables
		why             string
	}{
		{name: "another word", rec: "appRec7", resp: `{"msg":"hello appRec7"}`,
			vars: models.MockVariables{"a": bindV("appRec7", "body.a", "body.msg#0")},
			why:  `the recorded response holds "hello appRec7" there, not the variable's recorded value`},
		{name: "whole body", rec: "appRec7", resp: `{"self":"appRec7"}`,
			vars: models.MockVariables{"a": bindV("appRec7", "body.a", "body.self"), "b": bindV("appRec7", "body.b", "body.self#0")},
			why:  "another variable answers that value whole, with another word there"},
		{name: "whole header", rec: "appRec7", resp: `{}`, header: map[string]string{"Location": "appRec7"},
			vars: models.MockVariables{"a": bindV("appRec7", "body.a", "header.Location"), "b": bindV("appRec7", "body.b", "header.Location#0")},
			why:  "another variable answers that value whole, with another word there"},
		{name: "number", rec: "1730000000", resp: `{"n":1730000000}`,
			vars: models.MockVariables{"a": bindV("1730000000", "body.a", "body.n#0")},
			why:  "the recorded response holds no string there to hold a word of"},
		{name: "two lines", rec: "appRec7", resp: `{}`, header: map[string]string{"Location": "appRec7"}, lines: models.HeaderLineLengths{"Location": {3, 4}},
			vars: models.MockVariables{"a": bindV("appRec7", "body.a", "header.Location#0")},
			why:  "the recorded response holds no one value of that header"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := varMock("mock-1", "POST", "http://store/apps", `{"a":"`+c.rec+`","b":"`+c.rec+`"}`, 201, c.resp, c.vars)
			for k, v := range c.header {
				m.Spec.HTTPResp.Header[k] = v
			}
			m.Spec.HTTPResp.HeaderLineLengths = c.lines
			db := &varDb{perTest: []*models.Mock{at(m, 1)}}
			b := c.rec // a field of no variable is sent as recorded
			if _, ok := c.vars["b"]; ok {
				b = "bLive3"
			}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"1730000555","b":"`+b+`"}`))
			wantProblem(t, ok, diag, c.why)
		})
	}
}

// The learn step reads a mock's variable places as the replay does
// (learn.VariableLeafPath): a word address is a place it can read (no
// unreadPlace, which would make every call of the operation miss and warn
// that a valid place be corrected), and it names its field's leaf. The check
// that a call is exactly another test's recording (exactlyElsewhere) then
// excuses that field, and a segment word files the recording under the
// learned request key (variableKeyed). A place the recording holds as
// written names its own leaf.
func TestVariables_TheLearnStepReadsAWordAtItsField(t *testing.T) {
	for _, c := range []struct {
		name, url, body string
		v               models.MockVariable
		leaf            string
		keyed           bool
	}{
		{name: "segment word", url: "http://store/apps/app-appRec7", v: useV("appRec7", []string{"url.path[1]#1"}), leaf: "url.path.1", keyed: true},
		{name: "body word", url: "http://store/secrets/app", body: `{"data":{"note":"made by appRec7 at boot"}}`, v: bindV("appRec7", "body.data.note#2"), leaf: "body.data.note"},
		{name: "query word", url: "http://store/apps?ref=by+appRec7", v: bindV("appRec7", "url.query.ref#1"), leaf: "url.query.ref"},
		{name: "cookie", url: "http://store/apps", v: bindV("appRec7", "header.Cookie|cookie.uid"), leaf: "header.cookie"},
		{name: "legacy key", url: "http://store/secrets/app", body: `{"a#0":"appRec7","a":"x appRec7"}`, v: bindV("appRec7", "body.a#0"), leaf: "body.a#0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := varMock("mock-1", "PUT", c.url, c.body, 204, `{}`, models.MockVariables{"appId": c.v})
			m.Spec.HTTPReq.Header["Cookie"] = "uid=appRec7; theme=dark"
			places, err := variablePlaces(m)
			if err != nil {
				t.Fatalf("the learn step cannot read the place: %v", err)
			}
			if len(places) != 1 || !places[c.leaf] {
				t.Fatalf("places %v; want the leaf %s alone", places, c.leaf)
			}
			if got := variableKeyed(m, 2); got != c.keyed {
				t.Fatalf("filed under the learned key: %v, want %v", got, c.keyed)
			}
		})
	}
}

// Learned noise at the field a word variable stands in is not applied at
// replay, as MergeLearned no longer writes it: a variable's place is never
// noise. The rest of the field is compared: a call that changes another word
// of it is not the mock's call; one that changes the word alone binds it.
func TestVariables_NoiseAtAWordedFieldLeavesTheRestCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := wordedCreate()[0]
	m.Spec.ReqBodyNoise = map[string][]string{"body.data.note": nil}
	db := &varDb{perTest: []*models.Mock{m}}
	if ok, _, _ := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at noon"}}`)); ok {
		t.Fatal("served a call that changes the field outside the variable's word")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at boot"}}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if b, bound := db.table.Lookup("appId"); !bound || b.Value != "appLive9" {
		t.Fatalf("bound %+v, want appLive9", b)
	}
}

// Whether a recorded request holds a place (variables.InRequest, which reads
// a mock's variable places for its replay and its learn step) is what the
// replay reads there (recordedView): one value at a segment, a query name, a
// header or a body path.
func TestVariables_TheRecordingHoldsWhatTheReplayReads(t *testing.T) {
	r := &models.HTTPReq{
		Method: "PUT",
		URL:    "http://store/apps/a%2Fb/x?ref=one&two=1&two=2&page%231=p",
		Header: map[string]string{"X-App": "v", "x-other": "w"},
		Body:   `{"a":{"b":[1,"x"]},"a#0":"y","n":null}`,
	}
	holds := variables.InRequest(r)
	rec, _ := recordedView(r)
	for _, s := range []string{"url.path[0]", "url.path[1]", "url.path[2]", "url.path[3]", "url.query.ref", "url.query.two", `url.query.page\#1`, "url.query.none",
		"header.X-App", "header.x-app", "header.X-Other", "header.None", `body.a.b[1]`, `body.a.b[2]`, "body.a", `body["a#0"]`, "body.n", "body.none"} {
		p, err := variables.Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		_, want := rec.leaf(p)
		if got := holds(p); got != want {
			t.Errorf("%s: InRequest holds it %v, the replay reads it %v", s, got, want)
		}
	}
	if variables.InRequest(nil)(variables.Place{Part: variables.PartBody}) {
		t.Error("no request holds a place")
	}
}

// A word answer whose live value is no one word is refused, never served
// with the word dropped or the value split.
func TestVariables_AWordAnswerOfAValueThatIsNoWordIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/apps", `{"a":"appRec7"}`, 201, `{"msg":"hello appRec7"}`,
		models.MockVariables{"a": bindV("appRec7", "body.a", "body.msg#1")})
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"app Live9"}`))
	if ok {
		t.Fatalf("served %s", stub.Spec.HTTPResp.Body)
	}
	wantProblem(t, ok, diag, `the value "app Live9" cannot stand at that word`)
}

// Words of two body strings are each written into their own string.
func TestVariables_WordAnswersInTwoBodyStrings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/apps", `{"a":"appRec7"}`, 201, `{"x":"hello appRec7","y":"bye appRec7"}`,
		models.MockVariables{"a": bindV("appRec7", "body.a", "body.x#1", "body.y#1")})
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"appLive9"}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if want := `{"x":"hello appLive9","y":"bye appLive9"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
}

// An echo bound at a word of a request text is the word the call sends, and
// answers it.
func TestVariables_AnEchoAtAWord(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	v := bindV("reqRec7", "body.note#1", "body.id")
	v.Echo = true
	m := varMock("mock-1", "POST", "http://store/apps", `{"note":"req reqRec7"}`, 201, `{"id":"reqRec7"}`, models.MockVariables{"rid": v})
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"note":"req reqLive9"}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if want := `{"id":"reqLive9"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
}

// A digest header a variable's word is answered in holds that word: it is
// not recomputed over the body (its note is dropped from the served copy).
func TestVariables_ADigestHeaderAWordIsAnsweredInIsNotRecomputed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"tag":"tagRec7"}`, 201, `{"id":"r1"}`,
		models.MockVariables{"tag": bindV("tagRec7", "body.tag", "header.ETag#1")}), 1)
	m.Spec.HTTPResp.Header["ETag"] = `W/"tagRec7"`
	m.Spec.Metadata = map[string]string{models.MetadataBodyDigestsKey: "etag=etag/md5/s"}
	db := &varDb{perTest: []*models.Mock{m}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"tag":"tagLive9"}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if _, noted := stub.Spec.Metadata[models.MetadataBodyDigestsKey]; noted {
		t.Fatalf("the served copy still notes the ETag as a digest: %v", stub.Spec.Metadata)
	}
	raw, err := h.buildMockResponseBytes(stub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Etag: W/\"tagLive9\"\r\n") && !strings.Contains(string(raw), "ETag: W/\"tagLive9\"\r\n") {
		t.Fatalf("served %q; want the ETag holding the live word", raw)
	}
}

// A word answer keeps every recorded byte of the text outside the word:
// HTML characters as recorded (escaped or not) and non-ASCII letters.
func TestVariables_AWordAnswerKeepsTheRestOfTheTextAsRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	esc := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`, "é", `\u00e9`)
	for _, resp := range []string{`{"msg":"<b>hello appRec7</b> & café"}`, esc.Replace(`{"msg":"<b>hello appRec7</b> & café"}`)} {
		m := varMock("mock-1", "PUT", "http://store/secrets/app", `{"note":"a<b & café appRec7 \"q\""}`, 200, resp,
			models.MockVariables{"appId": bindV("appRec7", "body.note#3", "body.msg#2")})
		db := &varDb{perTest: []*models.Mock{at(m, 1)}}
		ok, stub, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"note":"a<b & café appLive9 \"q\""}`))
		if !ok {
			t.Fatalf("not served: %+v", diag)
		}
		if want := strings.Replace(resp, "appRec7", "appLive9", 1); stub.Spec.HTTPResp.Body != want {
			t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
		}
	}
}
