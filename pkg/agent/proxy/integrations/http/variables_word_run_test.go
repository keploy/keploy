package http

import (
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// Rows for word binds that share a live value: a value the learn step binds
// word by word (a fresh UUID, each of its five groups its own variable) holds
// words a second fresh value can hold by chance (a v4 UUID's version group is
// 12 random bits). Two such binds share a live value by chance only when the
// runs of words they are bound in (the words their calls bind at consecutive
// words of one text), set side by side at the shared word, differ live at a
// word both hold. Otherwise the application sent one value where the
// recording had two, alone or beside another bound value, and it is refused.
// The words read are the value's own: those named one value
// (variables.WordBind.Of). The HTTP adapter names only a text that is one
// UUID the call binds at every word (variables.NameUUIDs), so such a UUID's
// words are one value by its form and a group of it equal by chance to
// another's is answered under the proven switch. Every other word is its own
// value, checked as main checks it: a group equal by chance is refused, as
// main refuses it, never bound where main refuses it.

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
// When sentWhole, each body also sends its UUID whole in a field of its own
// (ref, bound whole): the evidence that the UUID's words are one value.
func eventAndSuite(prefixed, sentWhole bool) []*models.Mock {
	first := 0
	if prefixed {
		first = 1
	}
	ev, su := uuidWordBinds("ev", recEvent, "body.id", 0), uuidWordBinds("su", recSuite, "body.id", first)
	if sentWhole {
		ev["evref"], su["suref"] = bindV(recEvent, "body.ref"), bindV(recSuite, "body.ref")
	}
	return []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/events", eventBody(recEvent, sentWhole), 201, `{}`, ev), 1),
		at(varMock("mock-2", "POST", "http://store/suites/events", suiteBody(recSuite, prefixed, sentWhole), 201, `{}`, su), 2),
	}
}

// eventBody and suiteBody are eventAndSuite's request bodies for an id.
func eventBody(id string, sentWhole bool) string {
	if sentWhole {
		return `{"id":"` + id + `","kind":"audit","ref":"` + id + `"}`
	}
	return `{"id":"` + id + `","kind":"audit"}`
}

func suiteBody(id string, prefixed, sentWhole bool) string {
	field := id
	if prefixed {
		field = "APP:" + id
	}
	if sentWhole {
		return `{"id":"` + field + `","kind":"update","ref":"` + id + `"}`
	}
	return `{"id":"` + field + `","kind":"update"}`
}

// Two fresh UUIDs whose version groups are equal by chance, whether or not
// each is also sent whole in a field of its own: bare, each is bound at every
// word and named its own value (variables.NameUUIDs), so under the proven
// switch the shared group is chance and the suite's call is answered. After
// "APP:" the text is no UUID and names nothing (an id sent whole elsewhere is
// no proof that its words are one value): refused at the equal group, as
// main refuses it.
func TestVariables_TwoFreshUUIDsWithOneEqualGroup(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, sentWhole := range []bool{false, true} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuite(prefixed, sentWhole)}
			if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, sentWhole))); !ok || stub.Name != "mock-1" {
				t.Fatalf("prefixed %v, sent whole %v: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", prefixed, sentWhole, ok, stub, diag)
			}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveSuite, prefixed, sentWhole)))
			if !prefixed {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("sent whole %v: the suite's call, a group equal by chance, should be answered by mock-2: ok=%v stub=%v diag=%+v", sentWhole, ok, stub, diag)
				}
				continue
			}
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "su2" {
				t.Fatalf("prefixed %v, sent whole %v: refused at %s, want su2 (the equal group, as main): %q", prefixed, sentWhole, p.name, p.why)
			}
			if _, bound := db.table.Lookup("su2"); bound {
				t.Fatalf("prefixed %v, sent whole %v: a refused call bound su2", prefixed, sentWhole)
			}
		}
	}
}

// The application sent the event's id where the recording sent the suite's,
// another value: refused, whether the suite's field holds the id bare or
// after "APP:", and whether each id is also sent whole in a field of its own
// or not.
func TestVariables_OneUUIDWhereTwoWereRecordedIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, sentWhole := range []bool{false, true} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuite(prefixed, sentWhole)}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, sentWhole))); !ok {
				t.Fatalf("prefixed %v, sent whole %v: the event was not answered: %+v", prefixed, sentWhole, diag)
			}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveEvent, prefixed, sentWhole)))
			p := wantProblem(t, ok, diag, "they were two values when the test was recorded")
			if p.name != "su0" {
				t.Fatalf("prefixed %v, sent whole %v: refused at %s, want su0 (the first word the call binds): %q", prefixed, sentWhole, p.name, p.why)
			}
			if _, bound := db.table.Lookup("su0"); bound {
				t.Fatalf("prefixed %v, sent whole %v: a refused call bound su0", prefixed, sentWhole)
			}
		}
	}
}

// Two groups of one fresh UUID equal by chance, whether or not the recording
// also sends the UUID whole in a field of its own: bound at every word, its
// words are named that UUID (variables.NameUUIDs), the groups of one value:
// answered under the proven switch. After "APP:" its words name nothing and
// the second is refused, as main refuses it.
func TestVariables_TwoEqualGroupsOfOneUUID(t *testing.T) {
	const live = "7d444840-41a3-41a3-a15b-5b4c7a3f9e21"
	t.Setenv(provenswitch.Env, "1")
	for _, sentWhole := range []bool{false, true} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuite(prefixed, sentWhole)[1:]}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(live, prefixed, sentWhole)))
			if !prefixed {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("sent whole %v: one UUID with two equal groups should be answered by mock-2: ok=%v stub=%v diag=%+v", sentWhole, ok, stub, diag)
				}
				continue
			}
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "su2" {
				t.Fatalf("prefixed %v, sent whole %v: refused at %s, want su2 (the second equal group, as main): %q", prefixed, sentWhole, p.name, p.why)
			}
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
	if b, bound := db.table.Lookup("orderId"); !bound || b.WordRun != "z9" || b.WordRunAt != 0 {
		t.Fatalf("orderId bound as %+v (%v), want its run alone", b, bound)
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/refunds", `{"ref":"z9"}`))
	if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "refundId" {
		t.Fatalf("refused at %s, want refundId", p.name)
	}
}

// Two cookies of one header are two texts: a UUID bound word by word in each
// is a run of its own cookie, never one run with the other's words. Two fresh
// UUIDs whose version groups are equal by chance are answered: each cookie's
// UUID is bound at every word and its words are one value by its form
// (variables.NameUUIDs), so the shared group is chance. The application
// sending one UUID in both cookies, where the recording had two, is refused,
// whether or not the body also sends each whole.
func TestVariables_TwoCookiesOfOneHeaderAreTwoRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := func(sid, csrf string, sentWhole bool) string {
		if sentWhole {
			return `{"csrf":"` + csrf + `","kind":"open","sid":"` + sid + `"}`
		}
		return `{"kind":"open"}`
	}
	// sentWhole: the body also sends each cookie's UUID whole, bound whole.
	rec := func(sentWhole bool) *models.Mock {
		vars := uuidWordBinds("sd", recEvent, "header.Cookie|cookie.sid", 0)
		for k, v := range uuidWordBinds("cs", recSuite, "header.Cookie|cookie.csrf", 0) {
			vars[k] = v
		}
		if sentWhole {
			vars["sdw"], vars["csw"] = bindV(recEvent, "body.sid"), bindV(recSuite, "body.csrf")
		}
		m := varMock("mock-1", "POST", "http://store/sessions", body(recEvent, recSuite, sentWhole), 201, `{}`, vars)
		m.Spec.HTTPReq.Header["Cookie"] = "sid=" + recEvent + "; csrf=" + recSuite
		return at(m, 1)
	}
	call := func(sid, csrf string, sentWhole bool) *req {
		r := liveReq("POST", "/sessions", body(sid, csrf, sentWhole))
		r.header.Set("Cookie", "sid="+sid+"; csrf="+csrf)
		return r
	}
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{rec(true)}}
	if ok, stub, diag := matchVar(t, h, db, call(liveEvent, liveSuite, true)); !ok || stub.Name != "mock-1" {
		t.Fatalf("two fresh UUIDs, a group equal by chance, should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	for _, sentWhole := range []bool{true, false} {
		h = newHTTP()
		db = &varDb{perTest: []*models.Mock{rec(sentWhole)}}
		ok, _, diag := matchVar(t, h, db, call(liveEvent, liveEvent, sentWhole))
		wantProblem(t, ok, diag, "they were two values when the test was recorded")
	}
	// Sent nowhere whole, each cookie's UUID is still one value by its form:
	// the group equal by chance is answered.
	h = newHTTP()
	db = &varDb{perTest: []*models.Mock{rec(false)}}
	if ok, stub, diag := matchVar(t, h, db, call(liveEvent, liveSuite, false)); !ok || stub.Name != "mock-1" {
		t.Fatalf("sent nowhere whole: two fresh UUIDs, a group equal by chance, should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
}

// A suite event whose id field holds two fresh UUIDs, each bound word by word
// and set apart only by "/" ("<C>/<B>": one run of ten words). The event's
// UUID sent as the first of them, or one UUID sent as both, is one value
// where two were recorded: refused. Two fresh UUIDs there, one of whose
// groups equals the event's by chance, are answered where each body also
// sends its UUIDs whole, and refused, as main refuses them, where nothing
// sends them whole.
func TestVariables_OneUUIDWhereTwoWereRecordedBesideAnAdjacentBoundUUIDIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		liveY = "0a0b0c0d-1e1f-4a2b-8c3d-4e5f6a7b8c9d"
	)
	// sentWhole: each body also sends each of its UUIDs whole in a field of
	// its own (bound whole), the evidence that their words are values.
	suites := func(first, second string, sentWhole bool) string {
		if sentWhole {
			return `{"c":"` + first + `","id":"` + first + `/` + second + `","kind":"update","s":"` + second + `"}`
		}
		return `{"id":"` + first + `/` + second + `","kind":"update"}`
	}
	mocks := func(sentWhole bool) []*models.Mock {
		vars := uuidWordBinds("su", recC, "body.id", 0)
		for i, w := range splitWords(recSuite) {
			vars["sb"+strconv.Itoa(i)] = bindV(w, "body.id#"+strconv.Itoa(5+i))
		}
		ev := uuidWordBinds("ev", recEvent, "body.id", 0)
		if sentWhole {
			vars["suc"], vars["sus"], ev["evref"] = bindV(recC, "body.c"), bindV(recSuite, "body.s"), bindV(recEvent, "body.ref")
		}
		return []*models.Mock{
			at(varMock("mock-1", "POST", "http://store/events", eventBody(recEvent, sentWhole), 201, `{}`, ev), 1),
			at(varMock("mock-2", "POST", "http://store/suites/events", suites(recC, recSuite, sentWhole), 201, `{}`, vars), 2),
		}
	}
	for _, c := range []struct {
		name, first, second string
		sentWhole           bool
		refusedAt           string
	}{
		{"the event's UUID first", liveEvent, liveY, true, "su0"},
		{"one UUID twice", liveY, liveY, true, "su0"},
		{"two fresh UUIDs, a group equal by chance", liveSuite, liveY, true, "su2"},
		{"nothing sent whole: the event's UUID first", liveEvent, liveY, false, "su0"},
		{"nothing sent whole: one UUID twice", liveY, liveY, false, "su0"},
		// Each word its own value: the equal group is refused, as main.
		{"nothing sent whole: two fresh UUIDs, a group equal by chance", liveSuite, liveY, false, "su2"},
	} {
		h := newHTTP()
		db := &varDb{perTest: mocks(c.sentWhole)}
		if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, c.sentWhole))); !ok {
			t.Fatalf("%s: the event was not answered: %+v", c.name, diag)
		}
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suites(c.first, c.second, c.sentWhole)))
		if c.refusedAt == "" {
			if !ok || stub.Name != "mock-2" {
				t.Fatalf("%s: %s/%s should be answered by mock-2: ok=%v stub=%v diag=%+v", c.name, c.first, c.second, ok, stub, diag)
			}
			continue
		}
		if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != c.refusedAt {
			t.Fatalf("%s: refused at %s, want %s: %q", c.name, p.name, c.refusedAt, p.why)
		}
	}
}

// api-server's "<cid>/<appId>" shape: two POSTs whose id holds two UUIDs,
// each bound word by word, recorded with two appIds. Replay sends one appId
// in both, beside two different cids: the rest of the appId is copied, so it
// is refused, though the cids differ live. Two fresh appIds whose version
// groups are equal by chance are refused, as main refuses them, with or
// without each UUID sent whole elsewhere
// (TestVariables_AWordBesideAUUIDTheMockSendsWholeIsItsOwnValue). With the
// switch off HTTP checks no variable, as main.
func TestVariables_OneAppIdBesideTwoDifferentCidsIsRefused(t *testing.T) {
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveW = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	two := func(pc, rc, pa, ra string) models.MockVariables {
		v := uuidWordBinds(pc, rc, "body.id", 0)
		for i, w := range splitWords(ra) {
			v[pa+strconv.Itoa(i)] = bindV(w, "body.id#"+strconv.Itoa(5+i))
		}
		return v
	}
	for _, c := range []struct {
		name, proven, secondApp, refusedAt string
	}{
		{"one appId for two", "1", liveX, "ta0"},
		// No part of either marker sent whole elsewhere: each word is its own
		// value, and the equal group is refused, as main refuses it.
		{"two fresh appIds, a group equal by chance, nothing sent whole", "1", liveW, "ta2"},
		{"switch off: no variable is checked, as main", "", liveX, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.proven)
			mocks := []*models.Mock{
				at(varMock("mock-1", "POST", "http://store/markers", `{"id":"`+recB+`/`+recA+`","kind":"one"}`, 201, `{}`, two("oc", recB, "oa", recA)), 1),
				at(varMock("mock-2", "POST", "http://store/markers", `{"id":"`+recD+`/`+recC+`","kind":"two"}`, 201, `{}`, two("tc", recD, "ta", recC)), 2),
			}
			h := newHTTP()
			db := &varDb{perTest: mocks}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", `{"id":"`+liveP+`/`+liveX+`","kind":"one"}`)); !ok {
				t.Fatalf("the first marker was not answered: %+v", diag)
			}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/markers", `{"id":"`+liveQ+`/`+c.secondApp+`","kind":"two"}`))
			if c.refusedAt == "" {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("%s/%s should be answered by mock-2: ok=%v stub=%v diag=%+v", liveQ, c.secondApp, ok, stub, diag)
				}
				return
			}
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != c.refusedAt {
				t.Fatalf("refused at %s, want %s: %q", p.name, c.refusedAt, p.why)
			}
		})
	}
}

// The marker's shape with each UUID also sent whole (a cid or appId field):
// that stretch of the marker is a value of its own. A one-word value beside
// a different cid at each mock, where two values were recorded ("<P>/7" and
// "<Q>/7" where "<B>/3" and "<D>/5" were), is its own value and refused, as
// main refuses it; two appIds beside their cids, a group equal by chance,
// are refused too, as main refuses them (no learned value names an appId's
// words one value); one appId beside two cids, where two were recorded, is
// refused.
func TestVariables_AWordBesideAUUIDTheMockSendsWholeIsItsOwnValue(t *testing.T) {
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveW = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	// marker is a mock whose body sends cid whole and "<cid>/<tail>" bound
	// word by word, tail whole too when it is a UUID.
	marker := func(name, p, kind, rc, tail string) *models.Mock {
		v := uuidWordBinds(p+"c", rc, "body.id", 0)
		v[p+"cid"] = bindV(rc, "body.cid")
		body := `{"cid":"` + rc + `","id":"` + rc + `/` + tail + `","kind":"` + kind + `"}`
		tw := splitWords(tail)
		for i, w := range tw {
			v[p+"a"+strconv.Itoa(i)] = bindV(w, "body.id#"+strconv.Itoa(5+i))
		}
		if len(tw) > 1 {
			v[p+"app"] = bindV(tail, "body.app")
			body = `{"app":"` + tail + `","cid":"` + rc + `","id":"` + rc + `/` + tail + `","kind":"` + kind + `"}`
		}
		return varMock(name, "POST", "http://store/markers", body, 201, `{}`, v)
	}
	live := func(cid, tail, kind string) string {
		if len(splitWords(tail)) > 1 {
			return `{"app":"` + tail + `","cid":"` + cid + `","id":"` + cid + `/` + tail + `","kind":"` + kind + `"}`
		}
		return `{"cid":"` + cid + `","id":"` + cid + `/` + tail + `","kind":"` + kind + `"}`
	}
	for _, c := range []struct {
		name, proven, rec1, rec2, live1, live2, refusedAt string
	}{
		{"one word beside a different cid, two recorded", "1", "3", "5", "7", "7", "ta0"},
		{"switch off: no variable is checked, as main", "", "3", "5", "7", "7", ""},
		{"two fresh appIds beside their cids, a group equal by chance", "1", recA, recC, liveX, liveW, "ta2"},
		{"one appId beside two cids, two recorded", "1", recA, recC, liveX, liveX, "ta0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.proven)
			h := newHTTP()
			db := &varDb{perTest: []*models.Mock{at(marker("mock-1", "o", "one", recB, c.rec1), 1), at(marker("mock-2", "t", "two", recD, c.rec2), 2)}}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", live(liveP, c.live1, "one"))); !ok {
				t.Fatalf("the first marker was not answered: %+v", diag)
			}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/markers", live(liveQ, c.live2, "two")))
			if c.refusedAt == "" {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("%s/%s should be answered by mock-2: ok=%v stub=%v diag=%+v", liveQ, c.live2, ok, stub, diag)
				}
				return
			}
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != c.refusedAt {
				t.Fatalf("refused at %s, want %s: %q", p.name, c.refusedAt, p.why)
			}
		})
	}
}

// The same with the cid sent whole in the URL's path: refused. Two fresh
// appIds there whose version groups are equal by chance are refused too, as
// main refuses them: the path's cid shows the cid is a value of its own, but
// no learned value names the appId's words one value. One appId sent for two
// is refused.
func TestVariables_AWordBesideAUUIDThePathSendsWholeIsItsOwnValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveW = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
	)
	mock := func(name, p, rc, tail string) *models.Mock {
		v := uuidWordBinds(p+"c", rc, "body.id", 0)
		v[p+"cid"] = bindV(rc, "url.path[1]")
		for k, b := range uuidWordBinds(p+"a", tail, "body.id", 5) {
			v[k] = b
		}
		return varMock(name, "POST", "http://store/markers/"+rc, `{"id":"`+rc+`/`+tail+`"}`, 201, `{}`, v)
	}
	for _, c := range []struct {
		name, rec1, rec2, live1, live2, refusedAt string
	}{
		{"one word beside the cid, two recorded", "3", "5", "7", "7", "ta0"},
		{"two fresh appIds, a group equal by chance", recA, recC, liveX, liveW, "ta2"},
		{"one appId for two", recA, recC, liveX, liveX, "ta0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			db := &varDb{perTest: []*models.Mock{at(mock("mock-1", "o", recB, c.rec1), 1), at(mock("mock-2", "t", recD, c.rec2), 2)}}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers/"+liveP, `{"id":"`+liveP+`/`+c.live1+`"}`)); !ok {
				t.Fatalf("the first marker was not answered: %+v", diag)
			}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/markers/"+liveQ, `{"id":"`+liveQ+`/`+c.live2+`"}`))
			if c.refusedAt == "" {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("%s/%s should be answered by mock-2: ok=%v stub=%v diag=%+v", liveQ, c.live2, ok, stub, diag)
				}
				return
			}
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != c.refusedAt {
				t.Fatalf("refused at %s, want %s: %q", p.name, c.refusedAt, p.why)
			}
		})
	}
}

// One "7" sent at two POSTs where "3" and "5" were recorded, beside a
// different cid each time: refused, as main refuses it. With the cid sent
// whole in a field of its own and the appId not, "7" is not read with the
// appId's words; with only the whole id sent whole elsewhere ("<cid>/<n>" in
// a marker field too), the recording does not say whether it is one value
// or a cid beside a count.
func TestVariables_OneWordWhereTwoWereRecordedBesideOtherValuesIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveY = "e3b0c442-98fc-4b11-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	// "<cid>/<appId>/<n>", the cid sent whole, the appId not.
	appBody := func(cid, app, n string) string {
		return `{"cid":"` + cid + `","id":"` + cid + `/` + app + `/` + n + `"}`
	}
	appMock := func(name, p, cid, app, n string) *models.Mock {
		v := uuidWordBinds(p+"c", cid, "body.id", 0)
		v[p+"cid"] = bindV(cid, "body.cid")
		for k, b := range uuidWordBinds(p+"a", app, "body.id", 5) {
			v[k] = b
		}
		v[p+"n"] = bindV(n, "body.id#10")
		return varMock(name, "POST", "http://store/markers", appBody(cid, app, n), 201, `{}`, v)
	}
	// "<cid>/<n>", sent whole in a marker field too.
	markerBody := func(cid, _, n string) string {
		return `{"id":"` + cid + `/` + n + `","marker":"` + cid + `/` + n + `"}`
	}
	markerMock := func(name, p, cid, _, n string) *models.Mock {
		v := uuidWordBinds(p+"c", cid, "body.id", 0)
		v[p+"n"] = bindV(n, "body.id#5")
		v[p+"m"] = bindV(cid+"/"+n, "body.marker")
		return varMock(name, "POST", "http://store/markers", markerBody(cid, "", n), 201, `{}`, v)
	}
	for _, c := range []struct {
		name string
		mk   func(name, p, cid, app, n string) *models.Mock
		body func(cid, app, n string) string
	}{
		{"the cid sent whole, the appId not", appMock, appBody},
		{"only the whole id sent whole elsewhere", markerMock, markerBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			db := &varDb{perTest: []*models.Mock{at(c.mk("mock-1", "o", recB, recA, "3"), 1), at(c.mk("mock-2", "t", recD, recC, "5"), 2)}}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", c.body(liveP, liveX, "7"))); !ok {
				t.Fatalf("the first marker was not answered: %+v", diag)
			}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", c.body(liveQ, liveY, "7")))
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "tn" {
				t.Fatalf("refused at %s, want tn (the one 7 where 3 and 5 were recorded): %q", p.name, p.why)
			}
		})
	}
}

// The evidence that cuts a word run into held stretches
// (variables.WordBind.Elsewhere): a value the recorded request holds whole
// at a place other than the run's own text, a body scalar, a path segment, a
// query parameter's value or a header's value, a header named in any case.
// RecordedText is the run's text as recorded. Named a learned value each
// (variables.WordBind.Of), a cid sent whole in a query parameter and an
// appId in a header cut "<cid>/<appId>" into two spans; no learned value
// named, each word is its own.
func TestVariables_TheEvidenceAWordRunIsCutBy(t *testing.T) {
	const (
		recA = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD = "aabbccdd-eeff-4a11-9b22-334455667788"
		recE = "0a0b0c0d-1e1f-4a2b-8c3d-4e5f6a7b8c9d"
	)
	v := uuidWordBinds("c", recB, "body.id", 0)
	for k, b := range uuidWordBinds("a", recA, "body.id", 5) {
		v[k] = b
	}
	for k, b := range uuidWordBinds("h", recE, "header.x-key", 0) {
		v[k] = b
	}
	m := varMock("mock-1", "POST", "http://store/markers/"+recD+"?cid="+recB, `{"id":"`+recB+`/`+recA+`","kind":"`+recC+`"}`, 201, `{}`, v)
	m.Spec.HTTPReq.Header["X-App"] = recA
	m.Spec.HTTPReq.Header["X-Key"] = recE
	decls, bad := readDecls(m)
	if bad != "" {
		t.Fatalf("the mock's variables do not read: %s", bad)
	}
	names, ws := wordBindsOf(m, decls, map[string]held{})
	by := map[string]int{}
	for i, n := range names {
		by[n] = i
	}
	id, hdr := ws[by["c0"]], ws[by["h0"]]
	if id.RecordedText != recB+"/"+recA || hdr.RecordedText != recE {
		t.Fatalf("recorded texts %q and %q, want %q and %q", id.RecordedText, hdr.RecordedText, recB+"/"+recA, recE)
	}
	for _, c := range []struct {
		name, value string
		w           variables.WordBind
		want        bool
	}{
		{"a body scalar", recC, id, true},
		{"a path segment", recD, id, true},
		{"a query parameter's value", recB, id, true},
		{"a header's value", recA, id, true},
		{"the run's own text", recB + "/" + recA, id, false},
		{"held nowhere", "3", id, false},
		{"a header run's own value, its name in lower case", recE, hdr, false},
		{"another header's value, from a header run", recA, hdr, true},
	} {
		if c.w.Elsewhere == nil {
			t.Fatalf("%s: no evidence", c.name)
		}
		if got := c.w.Elsewhere(c.value); got != c.want {
			t.Errorf("%s (%s): held elsewhere %v, want %v", c.name, c.value, got, c.want)
		}
	}
	span := func(r variables.Run) string { return r.Recorded + "@" + strconv.Itoa(r.At) }
	runs := variables.WordRuns(ws)
	for _, n := range []string{"c2", "a2"} {
		if got := span(runs[by[n]]); strings.Contains(got, "-") {
			t.Errorf("%s, named no learned value, spans %s, want its own word", n, got)
		}
	}
	for i, n := range names {
		if strings.HasPrefix(n, "c") {
			ws[i].Of = recB
		} else if strings.HasPrefix(n, "a") {
			ws[i].Of = recA
		}
	}
	runs = variables.WordRuns(ws)
	if got := span(runs[by["c2"]]); got != recB+"@2" {
		t.Errorf("c2, the cid named one learned value and sent whole in the query, spans %s, want %s@2", got, recB)
	}
	if got := span(runs[by["a2"]]); got != recA+"@2" {
		t.Errorf("a2, the appId named one learned value and sent whole in a header, spans %s, want %s@2", got, recA)
	}
}
