package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
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

// Every byte of the field outside its variable's word is compared as
// recorded. A call that is the recording but there misses with that field
// named; one that differs elsewhere too is another call, of no evidence.
func TestVariables_TheRestOfAWordedFieldIsCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: wordedCreate()}
	for _, note := range []string{"made by appLive9 at noon", "made by appLive9", "made-by appLive9 at boot", "made by app Live9 at boot"} {
		ok, stub, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"`+note+`"}}`))
		if ok {
			t.Fatalf("note %q: answered by %s; only the word of the variable may differ", note, stub.Name)
		}
		if p := wantWorded(t, diag, "body.data.note", "appId"); p.expected != "made by appRec7 at boot" || p.actual != note {
			t.Fatalf("note %q: evidence %+v; want the recorded note and the one sent", note, p)
		}
	}
	if ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/other", `{"data":{"note":"made by appLive9 at noon"}}`)); ok || diag != nil && diag.variable != nil {
		t.Fatalf("a call to another path: served %v, evidence %+v; want a plain miss", ok, diag)
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

// A word place where the call sends a string with no such word is no type
// change: the call is not the mock's (a plain miss), and nothing binds.
func TestVariables_AStringWithNoWordThereIsAPlainMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"data":{"note":"made by appRec7 at boot"}}`, 204, `{}`,
			models.MockVariables{"appId": bindV("appRec7", "body.data.note#2")}), 1),
	}}
	for _, sent := range []string{`"made by"`, `""`} {
		ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":`+sent+`}}`))
		if ok || diag != nil && diag.variable != nil {
			t.Fatalf("note %s: served %v, evidence %+v; want a plain miss", sent, ok, diag)
		}
	}
	if _, bound := db.table.Lookup("appId"); bound {
		t.Fatal("appId was bound at a string with no word there")
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
	ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at noon"}}`))
	if ok {
		t.Fatal("served a call that changes the field outside the variable's word")
	}
	wantWorded(t, diag, "body.data.note", "appId")
	ok, _, diag = matchVar(t, h, db, liveReq("PUT", "/secrets/app", `{"data":{"note":"made by appLive9 at boot"}}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if b, bound := db.table.Lookup("appId"); !bound || b.Value != "appLive9" {
		t.Fatalf("bound %+v, want appLive9", b)
	}
}

// A word is where the rest of its value places it, so its value is
// compared with the recording, its word put back, whatever noise
// keploy.yml gives there: a call whose sid value has two words sends "uid"
// at word 3 of the Cookie header, and is not the mock's. A call that sends
// the rest as recorded binds the word.
func TestVariables_NoiseOverAWordedValueLeavesTheRestCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		header, body map[string][]string
		mock         func() *models.Mock
		live         func(string) *req
		shifted, ok  string
		want, field  string
	}{
		{
			header: map[string][]string{"cookie": {}},
			mock: func() *models.Mock {
				m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8"}`,
					models.MockVariables{"b": bindV("appRec8", "header.Cookie#3", "body.u")})
				m.Spec.HTTPReq.Header["Cookie"] = "sid=appRec7; uid=appRec8"
				return m
			},
			live: func(v string) *req {
				r := liveReq("GET", "/apps", "")
				r.header.Set("Cookie", v)
				return r
			},
			shifted: "sid=app-LiveA; uid=appLiveB", ok: "sid=appRec7; uid=appLiveB", want: "appLiveB", field: "header.Cookie",
		},
		{
			header: map[string][]string{"Cookie": {}},
			mock: func() *models.Mock {
				m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8"}`,
					models.MockVariables{"b": bindV("appRec8", "header.Cookie#3", "body.u")})
				m.Spec.HTTPReq.Header["Cookie"] = "sid=appRec7; uid=appRec8"
				return m
			},
			live: func(v string) *req {
				r := liveReq("GET", "/apps", "")
				r.header.Set("Cookie", v)
				return r
			},
			shifted: "sid=app-LiveA; uid=appLiveB", ok: "sid=appRec7; uid=appLiveB", want: "appLiveB", field: "header.Cookie",
		},
		{
			body: map[string][]string{"note": {}},
			mock: func() *models.Mock {
				return varMock("mock-1", "PUT", "http://store/notes/1", `{"note":"by appRec7 at boot"}`, 200, `{"u":"appRec7"}`,
					models.MockVariables{"b": bindV("appRec7", "body.note#1", "body.u")})
			},
			live:    func(v string) *req { return liveReq("PUT", "/notes/1", `{"note":"`+v+`"}`) },
			shifted: "by app-Live9 at boot", ok: "by appLive9 at boot", want: "appLive9", field: "body.note",
		},
	} {
		for _, live := range []string{c.shifted, c.ok} {
			h := newHTTP()
			db := &varDb{perTest: []*models.Mock{at(c.mock(), 1)}}
			ok, stub, diag, err := h.match(t.Context(), c.live(live), db, c.header, c.body, nil, true, false, false, true, true)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			b, bound := db.table.Lookup("b")
			if live == c.shifted {
				if ok || bound {
					t.Fatalf("noise %v %v, %q: served (%v), b bound as %q (%v); want a miss", c.header, c.body, live, ok, b.Value, bound)
				}
				wantWorded(t, diag, c.field, "b")
				continue
			}
			if !ok {
				t.Fatalf("noise %v %v, %q: not served: %+v", c.header, c.body, live, diag)
			}
			if !bound || b.Value != c.want || stub.Spec.HTTPResp.Body != `{"u":"`+c.want+`"}` {
				t.Fatalf("noise %v %v, %q: b bound as %q (%v), answered %s; want %s", c.header, c.body, live, b.Value, bound, stub.Spec.HTTPResp.Body, c.want)
			}
		}
	}
}

// Whether a recorded request holds a place (variables.InRequest, which reads
// a mock's variable places for its replay and its learn step) is what the
// replay reads there (recordedView): one value at a segment, a query name, a
// header or a body path. A request's recorded URLParams are its query where
// it has any, before its URL's own query.
func TestVariables_TheRecordingHoldsWhatTheReplayReads(t *testing.T) {
	r := &models.HTTPReq{
		Method: "PUT",
		URL:    "http://store/apps/a%2Fb/x?ref=one&two=1&two=2&page%231=p",
		Header: map[string]string{"X-App": "v", "x-other": "w"},
		Body:   `{"a":{"b":[1,"x"]},"a#0":"y","n":null}`,
	}
	params := &models.HTTPReq{
		Method:    "GET",
		URL:       "http://store/apps?none=u&two=1&two=2",
		URLParams: map[string]string{"ref": "one", "two": "2", "page#1": "p"},
	}
	for _, hr := range []*models.HTTPReq{r, params} {
		holds := variables.InRequest(hr)
		rec, _ := recordedView(hr)
		for _, s := range []string{"url.path[0]", "url.path[1]", "url.path[2]", "url.path[3]", "url.query.ref", "url.query.two", `url.query.page\#1`, "url.query.none",
			"header.X-App", "header.x-app", "header.X-Other", "header.None", `body.a.b[1]`, `body.a.b[2]`, "body.a", `body["a#0"]`, "body.n", "body.none"} {
			p, err := variables.Parse(s)
			if err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			_, want := rec.leaf(p)
			if got := holds(p); got != want {
				t.Errorf("%s of %s: InRequest holds it %v, the replay reads it %v", s, hr.URL, got, want)
			}
		}
	}
	if !variables.InRequest(params)(variables.Place{Part: variables.PartQuery, Name: "ref"}) {
		t.Error("a recorded URLParams name is not held")
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

// A use at a word, before any call bound its variable, names the word the
// call sent there, not the segment's whole text or "".
func TestVariables_AnEarlyUseAtAWordNamesTheWordSent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "GET", "http://store/apps/app-appRec7", "", 200, `{}`,
			models.MockVariables{"appId": useV("appRec7", []string{"url.path[1]#1"})}), 1),
	}}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/apps/app-appOther", ""))
	p := wantProblem(t, ok, diag, "before the call that binds it")
	if p.place != "url.path[1]#1" || p.actual != "appOther" {
		t.Fatalf("evidence %+v; want the word appOther the call sent at url.path[1]#1", p)
	}
}

// Of two type changes, the first place's is the evidence, at a word as at a
// whole place; the diff is of the word's whole field, named by its path.
func TestVariables_ATypeChangeAtAWordIsADiffOfItsField(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, word := range []string{"", "#0"} {
		h := newHTTP()
		db := &varDb{perTest: []*models.Mock{
			at(varMock("mock-1", "PUT", "http://store/secrets/app", `{"x":"1730000000","y":"1730000001"}`, 204, `{}`,
				models.MockVariables{"a": bindV("1730000000", "body.x"+word), "b": bindV("1730000001", "body.y"+word)}), 1),
		}}
		body := `{"x":5,"y":6}`
		ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/secrets/app", body))
		p := wantProblem(t, ok, diag, "a value of another JSON type")
		if p.place != "body.x"+word || p.name != "a" {
			t.Fatalf("evidence %+v; want variable a's type change at body.x%s", p, word)
		}
		rep := h.buildHTTPMismatchReport(makeReq("PUT", "/secrets/app"), []byte(body), db, nil, nil, nil, diag)
		want := models.MockFieldDiff{Path: "body.x", Kind: models.DiffKindTypeChanged, Expected: `"1730000000"`, Actual: "5"}
		if rep.MatchPhase != models.MatchPhaseVariable || len(rep.FieldDiffs) != 1 || rep.FieldDiffs[0] != want {
			t.Fatalf("at body.x%s: phase %q, diffs %+v; want %+v", word, rep.MatchPhase, rep.FieldDiffs, want)
		}
	}
}

// Two answers at one word, or at bytes of one value that overlap under
// other steps, are refused as two whole answers at one place are: the
// one written last would drop the other's value. Two words of one value
// are two places.
func TestVariables_TwoAnswersAtOneWordAreRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		a, b, recorded, header string
		want                   string // the answer, or "" for the refusal
	}{
		{a: "body.msg", b: "body.msg", recorded: `{"msg":"appRec7"}`},
		{a: "body.msg#1", b: "body.msg#1", recorded: `{"msg":"hello appRec7"}`},
		{a: "header.Set-Cookie#1", b: "header.Set-Cookie|cookie.sid#0", header: "sid=appRec7; Path=/"},
		{a: "header.Set-Cookie|cookie.sid", b: "header.set-cookie#1", header: "sid=appRec7; Path=/"},
		{a: "body.msg#1", b: "body.msg#2", recorded: `{"msg":"hello appRec7 appRec7"}`, want: `{"msg":"hello appLiveA appLiveB"}`},
		{a: "header.Set-Cookie|cookie.sid#0", b: "header.Set-Cookie|cookie.uid", header: "sid=appRec7; uid=appRec7", want: "sid=appLiveA; uid=appLiveB"},
	} {
		h := newHTTP()
		resp := c.recorded
		if resp == "" {
			resp = `{}`
		}
		m := varMock("mock-1", "POST", "http://store/apps", `{"a":"appRec7","b":"appRec7"}`, 201, resp,
			models.MockVariables{"a": bindV("appRec7", "body.a", c.a), "b": bindV("appRec7", "body.b", c.b)})
		if c.header != "" {
			m.Spec.HTTPResp.Header["Set-Cookie"] = c.header
		}
		db := &varDb{perTest: []*models.Mock{at(m, 1)}}
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"appLiveA","b":"appLiveB"}`))
		if c.want == "" {
			p := wantProblem(t, ok, diag, "two of its answer places overlap")
			if strings.ContainsAny(c.a+c.b, "#|") && (p.place != c.b || !strings.Contains(p.why, c.a+" of variable a")) {
				t.Fatalf("%s and %s: evidence %+v; want b's place refused beside a's", c.a, c.b, p)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s and %s: not served: %+v", c.a, c.b, diag)
		}
		got := stub.Spec.HTTPResp.Body
		if c.header != "" {
			got = stub.Spec.HTTPResp.Header["Set-Cookie"]
		}
		if got != c.want {
			t.Fatalf("%s and %s: answered %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

// A word that holds a pct escape's digits shares no byte with the word the
// escape stands beside, yet writing it rewrites what pct decodes: "2F"
// written as "41" makes "%41%41", whose word 0 is "AA", and b's answer
// would drop a's. The two are refused as overlapping places.
func TestVariables_AWordInAnEscapeOverlapsTheWordsItDecodesTo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/apps", `{"a":"2F","b":"A"}`, 201, `{}`,
		models.MockVariables{"a": bindV("2F", "body.a", "header.X-Tok#0"), "b": bindV("A", "body.b", "header.X-Tok|pct#0")})
	m.Spec.HTTPResp.Header["X-Tok"] = "%2F%41"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"41","b":"B"}`))
	if ok {
		t.Fatalf("served X-Tok %q; want the two answer places refused", stub.Spec.HTTPResp.Header["X-Tok"])
	}
	p := wantProblem(t, ok, diag, "two of its answer places overlap")
	if p.place != "header.X-Tok|pct#0" || !strings.Contains(p.why, "header.X-Tok#0 of variable a") {
		t.Fatalf("evidence %+v; want b's place refused beside a's", p)
	}
}

// Only a header's name is read in any case: two cookies, or two claims,
// whose names differ in case are two places.
func TestVariables_CookieNamesInTwoCasesAreTwoPlaces(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{}`,
		models.MockVariables{"a": bindV("appRec7", "header.Cookie|cookie.SID"), "b": bindV("appRec8", "header.Cookie|cookie.sid")})
	m.Spec.HTTPReq.Header["Cookie"] = "SID=appRec7; sid=appRec8"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	r := liveReq("GET", "/apps", "")
	r.header.Set("Cookie", "SID=appLiveA; sid=appLiveB")
	if ok, _, diag := matchVar(t, h, db, r); !ok {
		t.Fatalf("not served: %+v", diag)
	}
	for name, want := range map[string]string{"a": "appLiveA", "b": "appLiveB"} {
		if b, bound := db.table.Lookup(name); !bound || b.Value != want {
			t.Fatalf("%s bound as %+v (%v), want %s", name, b, bound, want)
		}
	}

	m = varMock("mock-1", "GET", "http://store/apps", "", 200, `{}`,
		models.MockVariables{"a": bindV("appRec7", "header.Cookie|cookie.sid"), "b": useV("appRec7", []string{"header.cookie|cookie.sid"})})
	m.Spec.HTTPReq.Header["Cookie"] = "sid=appRec7"
	db = &varDb{perTest: []*models.Mock{at(m, 1)}}
	r = liveReq("GET", "/apps", "")
	r.header.Set("Cookie", "sid=appLiveA")
	ok, _, diag := matchVar(t, h, db, r)
	wantProblem(t, ok, diag, "the request place header.cookie|cookie.sid is declared twice")
}

// A cookie's value answered whole may be written with another number of
// words ("app-LiveA" is two): a word of the header after it then names
// another word, and served it would rename the uid cookie or the Path
// attribute. The two places are refused as overlapping, whatever the
// values and in either order of the variables' names.
func TestVariables_AWholeCookieAnswerMovesTheWordsAfterIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct{ header, liveA string }{
		{"sid=appRec7; uid=appRec8", "app-LiveA"},
		{"sid=appRec7; Path=/appRec8", "app-LiveA"},
		{"sid=appRec7; uid=appRec8", "appLiveA"},
	} {
		for _, names := range [][2]string{{"a", "b"}, {"z", "b"}} {
			h := newHTTP()
			m := varMock("mock-1", "POST", "http://store/apps", `{"a":"appRec7","b":"appRec8"}`, 201, `{}`,
				models.MockVariables{names[0]: bindV("appRec7", "body.a", "header.Set-Cookie|cookie.sid"), names[1]: bindV("appRec8", "body.b", "header.Set-Cookie#3")})
			m.Spec.HTTPResp.Header["Set-Cookie"] = c.header
			db := &varDb{perTest: []*models.Mock{at(m, 1)}}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"`+c.liveA+`","b":"appLiveB"}`))
			if ok {
				t.Fatalf("%q, a=%q, names %v: served Set-Cookie %q; want the two answer places refused", c.header, c.liveA, names, stub.Spec.HTTPResp.Header["Set-Cookie"])
			}
			p := wantProblem(t, ok, diag, "two of its answer places overlap")
			sid, word := "header.Set-Cookie|cookie.sid", "header.Set-Cookie#3"
			named := p.place == word && strings.Contains(p.why, sid+" of variable "+names[0]) ||
				p.place == sid && strings.Contains(p.why, word+" of variable "+names[1])
			if !named {
				t.Fatalf("%q, names %v: evidence %+v; want one place refused beside the other", c.header, names, p)
			}
		}
	}
}

// A word read through another cookie, by its name, stays where it is
// whatever is written in this cookie's value.
func TestVariables_AWholeCookieAnswerBesideAnotherCookie(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "POST", "http://store/apps", `{"a":"appRec7","b":"appRec8"}`, 201, `{}`,
		models.MockVariables{"a": bindV("appRec7", "body.a", "header.Set-Cookie|cookie.sid"), "b": bindV("appRec8", "body.b", "header.Set-Cookie|cookie.uid#0")})
	m.Spec.HTTPResp.Header["Set-Cookie"] = "sid=appRec7; uid=appRec8.x"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"a":"app-LiveA","b":"appLiveB"}`))
	if !ok {
		t.Fatalf("not served: %+v", diag)
	}
	if got, want := stub.Spec.HTTPResp.Header["Set-Cookie"], "sid=app-LiveA; uid=appLiveB.x"; got != want {
		t.Fatalf("served Set-Cookie %q, want %q", got, want)
	}
}

// The same on the request side: a call whose sid value has two words
// sends "uid" at its word 3, not the value the recording holds there. The
// two bind places are refused as overlapping, never bound as "uid", and so
// they are where the call's sid value is one word, and where the word's
// place writes the header's name in another case.
func TestVariables_AWholeCookieBindMovesTheWordsAfterIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, live := range []string{"sid=app-LiveA; uid=appLiveB", "sid=appLiveA; uid=appLiveB"} {
		for _, word := range []string{"header.Cookie#3", "header.cookie#3"} {
			h := newHTTP()
			m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{}`,
				models.MockVariables{"a": bindV("appRec7", "header.Cookie|cookie.sid"), "b": bindV("appRec8", word)})
			m.Spec.HTTPReq.Header["Cookie"] = "sid=appRec7; uid=appRec8"
			db := &varDb{perTest: []*models.Mock{at(m, 1)}}
			r := liveReq("GET", "/apps", "")
			r.header.Set("Cookie", live)
			ok, _, diag := matchVar(t, h, db, r)
			p := wantProblem(t, ok, diag, "two of its request places overlap")
			if !strings.Contains(p.why, word+" (variable b)") || !strings.Contains(p.why, "header.Cookie|cookie.sid (variable a)") {
				t.Fatalf("%q, %s: evidence %+v; want both places named", live, word, p)
			}
			if _, bound := db.table.Lookup("b"); bound {
				t.Fatalf("%q, %s: b bound", live, word)
			}
		}
	}
}

// A value bound whole and at a word of it is refused as two places that
// overlap: the call may send the value with another number of words (a sid
// value of two words makes word 3 of the Cookie header "uid"), and the
// word would be bound where the call did not send it. So it is where the
// call sends the value with as many words as the recording.
func TestVariables_AValueBoundWholeAndAtAWord(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct{ whole, word, header, live string }{
		{"header.Cookie", "header.Cookie#3", "sid=appRec7; uid=appRec8", "sid=app-LiveA; uid=appLiveB"},
		{"header.Cookie", "header.cookie#3", "sid=appRec7; uid=appRec8", "sid=appLiveA; uid=appLiveB"},
		{"body.note", "body.note#1", "", `{"note":"by app-Live9 at boot"}`},
		{"body.note", "body.note#1", "", `{"note":"by appLive9 at boot"}`},
	} {
		h := newHTTP()
		var m *models.Mock
		r := liveReq("PUT", "/notes/1", c.live)
		if c.header != "" {
			m = varMock("mock-1", "PUT", "http://store/notes/1", "", 200, `{"u":"appRec8"}`,
				models.MockVariables{"a": bindV(c.header, c.whole), "b": bindV("appRec8", c.word, "body.u")})
			m.Spec.HTTPReq.Header["Cookie"] = c.header
			r = liveReq("PUT", "/notes/1", "")
			r.header.Set("Cookie", c.live)
		} else {
			m = varMock("mock-1", "PUT", "http://store/notes/1", `{"note":"by appRec7 at boot"}`, 200, `{"u":"appRec7"}`,
				models.MockVariables{"a": bindV("by appRec7 at boot", c.whole), "b": bindV("appRec7", c.word, "body.u")})
		}
		db := &varDb{perTest: []*models.Mock{at(m, 1)}}
		ok, stub, diag := matchVar(t, h, db, r)
		if ok {
			t.Fatalf("%s and %s, %q: served %s; want the two places refused", c.whole, c.word, c.live, stub.Spec.HTTPResp.Body)
		}
		p := wantProblem(t, ok, diag, "two of its request places overlap")
		if !strings.Contains(p.why, c.word+" (variable b)") || !strings.Contains(p.why, c.whole+" (variable a)") {
			t.Fatalf("%s and %s: evidence %+v; want both places named", c.whole, c.word, p)
		}
		if _, bound := db.table.Lookup("b"); bound {
			t.Fatalf("%s and %s, %q: b bound", c.whole, c.word, c.live)
		}
	}
}

// Of three places of one value, two that are not side by side in the
// order of binding are refused as overlapping too, before any is put back.
func TestVariables_ThreeWordsOfOneValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "PUT", "http://store/notes/1", `{"note":"appRec7 x appRec8"}`, 204, `{}`,
		models.MockVariables{"a": bindV("appRec7", "body.note#0"), "b": bindV("appRec8", "body.note#2"), "c": bindV("appRec7", "body.note|pct#0")})
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/notes/1", `{"note":"appLiveA x appLiveB"}`))
	wantProblem(t, ok, diag, "a word written at body.note|pct#0 (variable c) changes what body.note#0 (variable a) names")
}

// A put-back that changes a word put back before it is refused with both
// places named, whichever was put back first: the call's "%20" makes b's
// word 0 "20b", and appRec8 put back there leaves "%ap", which a's pct
// step cannot read.
func TestVariables_APutBackThatMovesAnEarlierWord(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{}`,
		models.MockVariables{"a": bindV("appRec7", "header.X-Q|pct#1"), "b": bindV("appRec8", "header.X-Q#0")})
	m.Spec.HTTPReq.Header["X-Q"] = "appRec8 appRec7"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	r := liveReq("GET", "/apps", "")
	r.header.Set("X-Q", "%20b appLiveA")
	ok, _, diag := matchVar(t, h, db, r)
	wantProblem(t, ok, diag, "a word put back at header.X-Q#0 (variable b) or at header.X-Q|pct#1 (variable a) moves what the other names")
}

// Two word places the recorded request does not hold as words, or holds
// at a value of another type, are refused for what the recording holds,
// not as overlapping.
func TestVariables_TwoWordsTheRecordingDoesNotHold(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct{ body, a, b, want string }{
		{`{"note":"by appRec7"}`, "body.note#1", "body.note#4", "holds no single value at body.note#4"},
		{`{"note":"by appRec7"}`, "body.note#4", "body.note#1", "holds no single value at body.note#4"},
		{`{"n":12.5}`, "body.n#0", "body.n|pct#0", "which is no string to hold a word of"},
		// Two values whole under a step, at a field the recording does
		// not hold, are left to the variable pass, which says so: they
		// are not refused as overlapping. Read in the empty text, both
		// would overlap there.
		{`{"other":"x"}`, "body.note|pct", "body.note|pct|pct", "holds no single value at body.note|pct"},
	} {
		h := newHTTP()
		db := &varDb{perTest: []*models.Mock{
			at(varMock("mock-1", "PUT", "http://store/notes/1", c.body, 204, `{}`,
				models.MockVariables{"a": bindV("appRec7", c.a), "b": bindV("appRec7", c.b)}), 1),
		}}
		ok, _, diag := matchVar(t, h, db, liveReq("PUT", "/notes/1", c.body))
		wantProblem(t, ok, diag, c.want)
	}
}

// A variables block read with no recorded request, as the learn step reads
// the variables it paired, holds no value to overlap in.
func TestVariables_WordsReadWithNoRequest(t *testing.T) {
	decls, why := readDecls(&models.Mock{Name: "mock-1", Spec: models.MockSpec{Variables: models.MockVariables{
		"a": bindV("appRec7", "header.Cookie|cookie.sid"), "b": bindV("appRec8", "header.Cookie#3"),
	}}})
	if why != "" || len(decls) != 2 {
		t.Fatalf("readDecls = %d decls, %q; want both read", len(decls), why)
	}
}

// wantWorded checks a miss's evidence is the worded check's: the text
// field that places variable name's word is not the recording's, and noise
// is not applied there.
func wantWorded(t *testing.T, diag *matchDiag, field, name string) *varProblem {
	t.Helper()
	p := wantProblem(t, false, diag, "noise is not applied there")
	if p.kind != models.DiffKindValueChanged || p.field != field || p.name != name || p.expected == p.actual {
		t.Fatalf("evidence %+v; want a value change of %s for variable %s", p, field, name)
	}
	return p
}

// A cookie read by name is placed by its own value, which no other cookie
// of the header moves: that value, its words put back, is compared with
// the recording, and the rest of the header keeps the noise keploy.yml or
// the learn step gives it. So another session cookie, one with more words,
// or (under keploy.yml's noise) the cookies in another order still bind
// the word; a shift inside the cookie's own value is a miss that names it.
// The learn step learns the header by its words (the session cookie's
// value), so under its noise the rest of the header is compared word by
// word: cookies in another order are a miss (moved). A word of the header
// itself is placed by the whole header, which is compared whatever the
// noise.
func TestVariables_ACookieReadByNameIsPlacedByItsOwnValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec = "sid=appRec7; uid=appRec8"
	for _, c := range []struct {
		place, rec, live string
		want             string // the value bound, or "" for a miss
		// a miss's evidence: the text that places the word, its recorded
		// and sent values
		field, expected, actual string
		// moved: the header's skeleton moved, a miss under learned noise
		moved bool
	}{
		{"header.Cookie|cookie.uid", rec, "sid=appLiveX; uid=appLiveB", "appLiveB", "", "", "", false},
		{"header.Cookie|cookie.uid", rec, "sid=app-LiveA; uid=appLiveB", "appLiveB", "", "", "", false},
		{"header.Cookie|cookie.uid", rec, "uid=appLiveB; sid=appRec7", "appLiveB", "", "", "", true},
		{"header.Cookie|cookie.uid#1", "sid=appRec7; uid=a.appRec8", "sid=appLiveX; uid=a.appLiveB", "appLiveB", "", "", "", false},
		{"header.Cookie|cookie.uid#1", "sid=appRec7; uid=a.appRec8", "sid=appRec7; uid=a-b.appLive", "", "header.Cookie|cookie.uid", "a.appRec8", "a-b.appLive", false},
		{"header.Cookie#3", rec, "sid=appLiveX; uid=appLiveB", "", "header.Cookie", rec, "sid=appLiveX; uid=appLiveB", false},
	} {
		for _, learned := range []bool{false, true} {
			h := newHTTP()
			m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8"}`,
				models.MockVariables{"b": bindV("appRec8", c.place, "body.u")})
			m.Spec.HTTPReq.Header["Cookie"] = c.rec
			headerNoise := map[string][]string{"cookie": {}}
			if learned {
				// The session cookie's value, word 1, as the learn step
				// learns it.
				m.Spec.ReqBodyNoise = map[string][]string{"header.cookie": {learn.NoiseWordsPattern([]int{1})}}
				headerNoise = nil
			}
			db := &varDb{perTest: []*models.Mock{at(m, 1)}}
			r := liveReq("GET", "/apps", "")
			r.header.Set("Cookie", c.live)
			ok, stub, diag, err := h.match(t.Context(), r, db, headerNoise, nil, nil, true, false, false, true, true)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			b, bound := db.table.Lookup("b")
			if learned && c.moved {
				if ok || bound {
					t.Fatalf("%s, %q (learned noise): served (%v), b bound as %q; want a miss, the header's skeleton moved", c.place, c.live, ok, b.Value)
				}
				continue
			}
			if c.want == "" {
				if ok || bound {
					t.Fatalf("%s, %q (learned noise %v): served (%v), b bound as %q; want a miss", c.place, c.live, learned, ok, b.Value)
				}
				if p := wantWorded(t, diag, c.field, "b"); p.expected != c.expected || p.actual != c.actual {
					t.Fatalf("%s, %q (learned noise %v): evidence %+v; want %q recorded, %q sent", c.place, c.live, learned, p, c.expected, c.actual)
				}
				continue
			}
			if !ok {
				t.Fatalf("%s, %q (learned noise %v): not served: %+v", c.place, c.live, learned, diag)
			}
			if !bound || b.Value != c.want || stub.Spec.HTTPResp.Body != `{"u":"`+c.want+`"}` {
				t.Fatalf("%s, %q (learned noise %v): b bound as %q (%v), answered %s; want %s", c.place, c.live, learned, b.Value, bound, stub.Spec.HTTPResp.Body, c.want)
			}
		}
	}
	// With no noise at the header, its other cookies are compared.
	h := newHTTP()
	m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8"}`,
		models.MockVariables{"b": bindV("appRec8", "header.Cookie|cookie.uid", "body.u")})
	m.Spec.HTTPReq.Header["Cookie"] = rec
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	r := liveReq("GET", "/apps", "")
	r.header.Set("Cookie", "sid=appLiveX; uid=appLiveB")
	if ok, stub, _ := matchVar(t, h, db, r); ok {
		t.Fatalf("served %s with another sid and no noise", stub.Spec.HTTPResp.Body)
	}
}

// Two cookies read by name in one Cookie header are two texts, each
// compared with the recording, its word put back, whatever the header's
// noise: a shift inside either cookie's value is a miss that names that
// cookie, its recorded and sent values, and a call that sends both in the
// recorded shape binds both, and so does one that sends them beside another
// cookie or in another order under keploy.yml's noise. Under the learn
// step's, which reads the header by its words, that is a miss (moved).
func TestVariables_TwoCookiesReadByNameInOneHeader(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		live string
		// a miss's evidence, or "" where the call is served
		name, field, expected, actual string
		// moved: the header's skeleton moved, a miss under learned noise
		moved bool
	}{
		{"sid=x.appLiveA; uid=a-b.appLive", "b", "header.Cookie|cookie.uid", "a.appRec8", "a-b.appLive", false},
		{"sid=x-y.appLiveA; uid=a.appLiveB", "a", "header.Cookie|cookie.sid", "x.appRec7", "x-y.appLiveA", false},
		{"sid=x.appLiveA; uid=a.appLiveB", "", "", "", "", false},
		{"z=1; uid=a.appLiveB; sid=x.appLiveA", "", "", "", "", true},
	} {
		for _, learned := range []bool{false, true} {
			h := newHTTP()
			m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8","v":"appRec7"}`, models.MockVariables{
				"a": bindV("appRec7", "header.Cookie|cookie.sid#1", "body.v"),
				"b": bindV("appRec8", "header.Cookie|cookie.uid#1", "body.u"),
			})
			m.Spec.HTTPReq.Header["Cookie"] = "sid=x.appRec7; uid=a.appRec8"
			headerNoise := map[string][]string{"cookie": {}}
			if learned {
				// The session cookie's value, word 1, as the learn step
				// learns it.
				m.Spec.ReqBodyNoise = map[string][]string{"header.cookie": {learn.NoiseWordsPattern([]int{1})}}
				headerNoise = nil
			}
			db := &varDb{perTest: []*models.Mock{at(m, 1)}}
			r := liveReq("GET", "/apps", "")
			r.header.Set("Cookie", c.live)
			ok, stub, diag, err := h.match(t.Context(), r, db, headerNoise, nil, nil, true, false, false, true, true)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			a, aBound := db.table.Lookup("a")
			b, bBound := db.table.Lookup("b")
			if learned && c.moved {
				if ok || aBound || bBound {
					t.Fatalf("%q (learned noise): served (%v), a bound as %q (%v), b as %q (%v); want a miss, the header's skeleton moved", c.live, ok, a.Value, aBound, b.Value, bBound)
				}
				continue
			}
			if c.name != "" {
				if ok || aBound || bBound {
					t.Fatalf("%q (learned noise %v): served (%v), a bound as %q (%v), b as %q (%v); want a miss", c.live, learned, ok, a.Value, aBound, b.Value, bBound)
				}
				if p := wantWorded(t, diag, c.field, c.name); p.expected != c.expected || p.actual != c.actual {
					t.Fatalf("%q (learned noise %v): evidence %+v; want %q recorded, %q sent", c.live, learned, p, c.expected, c.actual)
				}
				continue
			}
			if !ok || !aBound || !bBound || a.Value != "appLiveA" || b.Value != "appLiveB" || stub.Spec.HTTPResp.Body != `{"u":"appLiveB","v":"appLiveA"}` {
				t.Fatalf("%q (learned noise %v): served %v, a bound as %q (%v), b as %q (%v), diag %+v", c.live, learned, ok, a.Value, aBound, b.Value, bBound, diag)
			}
		}
	}
}

// Every value a word is bound in is compared, not the first only: a call
// that sends the header's word as recorded but the note with more words
// before its word misses on the note, whatever its noise, and names it.
// Where both are sent so, the miss names the first in the order of binding.
func TestVariables_EachWordedValueIsCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct{ xa, note, field, name string }{
		{"id appLive5", "by app-Live9 at boot", "body.note", "c"},
		{"i-d appLive5", "by app-Live9 at boot", "header.X-A", "b"},
		{"id appLive5", "by appLive9 at boot", "", ""},
	} {
		note := c.note
		h := newHTTP()
		m := varMock("mock-1", "PUT", "http://store/notes/1", `{"note":"by appRec7 at boot"}`, 200, `{"u":"appRec7","v":"appRec5"}`,
			models.MockVariables{"b": bindV("appRec5", "header.X-A#1", "body.v"), "c": bindV("appRec7", "body.note#1", "body.u")})
		m.Spec.HTTPReq.Header["X-A"] = "id appRec5"
		db := &varDb{perTest: []*models.Mock{at(m, 1)}}
		r := liveReq("PUT", "/notes/1", `{"note":"`+note+`"}`)
		r.header.Set("X-A", c.xa)
		ok, stub, diag, err := h.match(t.Context(), r, db, nil, map[string][]string{"note": {}}, nil, true, false, false, true, true)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		v, bound := db.table.Lookup("c")
		if c.field != "" {
			if ok || bound {
				t.Fatalf("%q, %q: served (%v), c bound as %q; want a miss", c.xa, note, ok, v.Value)
			}
			wantWorded(t, diag, c.field, c.name)
			continue
		}
		if !ok || !bound || v.Value != "appLive9" || stub.Spec.HTTPResp.Body != `{"u":"appLive9","v":"appLive5"}` {
			t.Fatalf("%q: served %v, c bound as %q (%v), diag %+v", note, ok, v.Value, bound, diag)
		}
	}
}

// A call that changes a bound value's JSON type and sends a worded value in
// another shape misses with the type change as its evidence, as where that
// is the call's one difference.
func TestVariables_ATypeChangeIsTheEvidenceBeforeAWordedMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := varMock("mock-1", "PUT", "http://store/notes/1", `{"x":"1730000000"}`, 200, `{"v":"appRec5"}`,
		models.MockVariables{"a": bindV("1730000000", "body.x"), "b": bindV("appRec5", "header.X-A#1", "body.v")})
	m.Spec.HTTPReq.Header["X-A"] = "id appRec5"
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	r := liveReq("PUT", "/notes/1", `{"x":5}`)
	r.header.Set("X-A", "i-d appLive5")
	ok, _, diag := matchVar(t, h, db, r)
	if p := wantProblem(t, ok, diag, "a value of another JSON type"); p.name != "a" || p.place != "body.x" || p.kind != models.DiffKindTypeChanged {
		t.Fatalf("evidence %+v; want variable a's type change at body.x", p)
	}
	if _, bound := db.table.Lookup("b"); bound {
		t.Fatal("b bound")
	}
}

// A value bound whole is refused beside a word of it whichever comes first
// in the order of binding, with both places named.
func TestVariables_AWordBoundBeforeItsValueWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec = "sid=appRec7; uid=appRec8"
	h := newHTTP()
	m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"appRec8"}`,
		models.MockVariables{"a": bindV("appRec8", "header.Cookie#3", "body.u"), "b": bindV(rec, "header.Cookie")})
	m.Spec.HTTPReq.Header["Cookie"] = rec
	db := &varDb{perTest: []*models.Mock{at(m, 1)}}
	r := liveReq("GET", "/apps", "")
	r.header.Set("Cookie", "sid=app-LiveA; uid=appLiveB")
	ok, _, diag := matchVar(t, h, db, r)
	wantProblem(t, ok, diag, "two of its request places overlap: a word written at header.Cookie (variable b) changes what header.Cookie#3 (variable a) names")
	if _, bound := db.table.Lookup("a"); bound {
		t.Fatal("a bound")
	}
}

// A text bound whole under a step has no word to move, and the field it is
// read in is still compared as the recording spells it, the recorded text
// put back: a call that spells the escape before the variable's text as
// recorded binds it, and one that spells it otherwise misses, naming the
// field.
func TestVariables_AWholeTextUnderAStepIsComparedAsRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for live, want := range map[string]string{"app%2DLive7": "app-Live7", "app-Live7": ""} {
		h := newHTTP()
		m := varMock("mock-1", "GET", "http://store/apps", "", 200, `{"u":"app-Rec7"}`,
			models.MockVariables{"b": bindV("app-Rec7", "header.X-Q|pct", "body.u")})
		m.Spec.HTTPReq.Header["X-Q"] = "app%2DRec7"
		db := &varDb{perTest: []*models.Mock{at(m, 1)}}
		r := liveReq("GET", "/apps", "")
		r.header.Set("X-Q", live)
		ok, stub, diag := matchVar(t, h, db, r)
		b, bound := db.table.Lookup("b")
		if want == "" {
			if ok || bound {
				t.Fatalf("%q: served (%v), b bound as %q; want a miss", live, ok, b.Value)
			}
			wantWorded(t, diag, "header.X-Q", "b")
			continue
		}
		if !ok || !bound || b.Value != want || stub.Spec.HTTPResp.Body != `{"u":"`+want+`"}` {
			t.Fatalf("%q: served %v, b bound as %q (%v), diag %+v", live, ok, b.Value, bound, diag)
		}
	}
}
