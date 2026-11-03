package http

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Rows for word binds that share a live value: a value the learn step binds
// word by word (a fresh UUID, each of its five groups its own variable) holds
// words a second fresh value can hold by chance (a v4 UUID's version group is
// 12 random bits). Two such binds are two variables recorded with two values
// sharing one live value only when the runs of words they are bound in (the
// words their calls bind at consecutive words of one text) are one live run
// and were two recorded ones: the application sent one value where the
// recording had two. One equal group of two runs that differ is chance.

const (
	recEvent = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	recSuite = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
	// Two fresh UUIDs whose version groups are equal, as the api-server's
	// audit event and test-suite event ids were in one replay.
	liveEvent = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
	liveSuite = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
)

// uuidWordBinds binds each word of a UUID the recording holds at field from
// word first on, as variables prefix0..prefix4.
func uuidWordBinds(prefix, recorded, field string, first int) models.MockVariables {
	out := models.MockVariables{}
	for i, w := range splitWords(recorded) {
		out[prefix+strconv.Itoa(i)] = bindV(w, field+"#"+strconv.Itoa(first+i))
	}
	return out
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			cur += string(c)
			continue
		}
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// eventAndSuite: an audit event's id, then a test-suite event's id, each a
// fresh UUID bound word by word; the suite's is "APP:"+id when prefixed.
func eventAndSuite(prefixed bool) []*models.Mock {
	suiteBody, first := `{"id":"`+recSuite+`","kind":"update"}`, 0
	if prefixed {
		suiteBody, first = `{"id":"APP:`+recSuite+`","kind":"update"}`, 1
	}
	return []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/events", `{"id":"`+recEvent+`","kind":"audit"}`, 201, `{}`,
			uuidWordBinds("ev", recEvent, "body.id", 0)), 1),
		at(varMock("mock-2", "POST", "http://store/suites/events", suiteBody, 201, `{}`,
			uuidWordBinds("su", recSuite, "body.id", first)), 2),
	}
}

func TestVariables_TwoFreshUUIDsWithOneEqualGroupBothBind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, prefixed := range []bool{false, true} {
		h := newHTTP()
		db := &varDb{perTest: eventAndSuite(prefixed)}
		if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", `{"id":"`+liveEvent+`","kind":"audit"}`)); !ok || stub.Name != "mock-1" {
			t.Fatalf("prefixed %v: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", prefixed, ok, stub, diag)
		}
		id := liveSuite
		if prefixed {
			id = "APP:" + liveSuite
		}
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", `{"id":"`+id+`","kind":"update"}`))
		if !ok || stub.Name != "mock-2" {
			why := ""
			if diag != nil && diag.variable != nil {
				why = diag.variable.why
			}
			t.Fatalf("prefixed %v: a second fresh UUID whose version group equals the first's by chance should be answered by mock-2: ok=%v stub=%v why=%q", prefixed, ok, stub, why)
		}
		if b, bound := db.table.Lookup("su2"); !bound || b.Value != "41a3" || b.Recorded != "4c89" {
			t.Fatalf("prefixed %v: su2 bound as %+v (%v)", prefixed, b, bound)
		}
	}
}

// The application sent the event's id where the recording sent the suite's,
// another value: refused, whether the suite's field holds the id bare or
// after "APP:".
func TestVariables_OneUUIDWhereTwoWereRecordedIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, prefixed := range []bool{false, true} {
		h := newHTTP()
		db := &varDb{perTest: eventAndSuite(prefixed)}
		if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", `{"id":"`+liveEvent+`","kind":"audit"}`)); !ok {
			t.Fatalf("prefixed %v: the event was not answered: %+v", prefixed, diag)
		}
		id := liveEvent
		if prefixed {
			id = "APP:" + liveEvent
		}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", `{"id":"`+id+`","kind":"update"}`))
		p := wantProblem(t, ok, diag, "they were two values when the test was recorded")
		if p.name != "su0" {
			t.Fatalf("prefixed %v: refused at %s, want su0 (the first word the call binds): %q", prefixed, p.name, p.why)
		}
		if _, bound := db.table.Lookup("su0"); bound {
			t.Fatalf("prefixed %v: a refused call bound su0", prefixed)
		}
	}
}

// Two groups of one fresh UUID may be equal by chance: they are one run of
// one call, recorded and live, and both bind.
func TestVariables_TwoEqualGroupsOfOneUUIDBothBind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: eventAndSuite(false)[:1]}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", `{"id":"7d444840-41a3-41a3-a15b-5b4c7a3f9e21","kind":"audit"}`))
	if !ok || stub.Name != "mock-1" {
		why := ""
		if diag != nil && diag.variable != nil {
			why = diag.variable.why
		}
		t.Fatalf("a UUID with two equal groups should be answered by mock-1: ok=%v stub=%v why=%q", ok, stub, why)
	}
	for name, want := range map[string]string{"ev1": "41a3", "ev2": "41a3"} {
		if b, bound := db.table.Lookup(name); !bound || b.Value != want {
			t.Fatalf("%s bound as %+v (%v)", name, b, bound)
		}
	}
}

// Two values one text holds apart, words that are no run (a word between
// them is not bound), stay two: the application sending one for both is
// refused, as is a word bind whose value a whole bind holds.
func TestVariables_WordsApartAndAWholeBindStayTwoValues(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/links", `{"what":"from appRec7 to ordRec3"}`, 201, `{}`,
			models.MockVariables{"appId": bindV("appRec7", "body.what#1"), "ordId": bindV("ordRec3", "body.what#3")}), 1),
	}}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/links", `{"what":"from appLive9 to appLive9"}`))
	wantProblem(t, ok, diag, "they were two values when the test was recorded")

	h = newHTTP()
	db = &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/codes", `{"code":"c0de"}`, 201, `{}`,
			models.MockVariables{"code": bindV("c0de", "body.code")}), 1),
		at(varMock("mock-2", "POST", "http://store/notes", `{"note":"made by n0te at boot"}`, 201, `{}`,
			models.MockVariables{"noteId": bindV("n0te", "body.note#2")}), 2),
	}}
	if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/codes", `{"code":"L5"}`)); !ok {
		t.Fatalf("the code was not answered: %+v", diag)
	}
	ok, _, diag = matchVar(t, h, db, liveReq("POST", "/notes", `{"note":"made by L5 at boot"}`))
	if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "noteId" {
		t.Fatalf("refused at %s, want noteId", p.name)
	}
}

// An echo's word is the call's own, never bound: a bound word beside it is a
// run of its own, and the application sending its value where another
// variable was recorded with another value is refused.
func TestVariables_AnEchoWordIsNoPartOfARun(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/charges", `{"key":"k1-ord7"}`, 201, `{"key":"k1"}`,
			models.MockVariables{
				"chargeKey": {Recorded: "k1", Bind: "body.key#0", Answer: []string{"body.key"}, Echo: true},
				"orderId":   bindV("ord7", "body.key#1"),
			}), 1),
		at(varMock("mock-2", "POST", "http://store/refunds", `{"ref":"ref3"}`, 201, `{}`,
			models.MockVariables{"refundId": bindV("ref3", "body.ref#0")}), 2),
	}}
	if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/charges", `{"key":"k9-z9"}`)); !ok || stub.Name != "mock-1" {
		t.Fatalf("the charge should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if b, bound := db.table.Lookup("orderId"); !bound || b.WordRun != "z9" || b.RecordedWordRun != "ord7" {
		t.Fatalf("orderId bound as %+v (%v), want its run alone", b, bound)
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/refunds", `{"ref":"z9"}`))
	if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "refundId" {
		t.Fatalf("refused at %s, want refundId", p.name)
	}
}

// Two cookies of one header are two texts: a UUID bound word by word in each
// is a run of its own cookie, never one run with the other's words. Two fresh
// UUIDs whose version groups are equal by chance bind; the application
// sending one UUID in both cookies, where the recording had two, is refused.
func TestVariables_TwoCookiesOfOneHeaderAreTwoRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := func() *models.Mock {
		vars := uuidWordBinds("sd", recEvent, "header.Cookie|cookie.sid", 0)
		for k, v := range uuidWordBinds("cs", recSuite, "header.Cookie|cookie.csrf", 0) {
			vars[k] = v
		}
		m := varMock("mock-1", "POST", "http://store/sessions", `{"kind":"open"}`, 201, `{}`, vars)
		m.Spec.HTTPReq.Header["Cookie"] = "sid=" + recEvent + "; csrf=" + recSuite
		return at(m, 1)
	}
	call := func(sid, csrf string) *req {
		r := liveReq("POST", "/sessions", `{"kind":"open"}`)
		r.header.Set("Cookie", "sid="+sid+"; csrf="+csrf)
		return r
	}
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{rec()}}
	if ok, stub, diag := matchVar(t, h, db, call(liveEvent, liveSuite)); !ok || stub.Name != "mock-1" {
		t.Fatalf("two fresh UUIDs whose version groups are equal by chance were not answered: ok=%v diag=%+v", ok, diag)
	}
	h = newHTTP()
	db = &varDb{perTest: []*models.Mock{rec()}}
	ok, _, diag := matchVar(t, h, db, call(liveEvent, liveEvent))
	wantProblem(t, ok, diag, "they were two values when the test was recorded")
}
