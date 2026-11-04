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
// 12 random bits). Two such binds share a live value by chance only when the
// runs of words they are bound in (the words their calls bind at consecutive
// words of one text), set side by side at the shared word, differ live at a
// word both hold. Otherwise the application sent one value where the
// recording had two, alone or beside another bound value, and it is refused.
// The words read are the value's own: those the recording also sends whole at
// another place. Where it sends no part of the text whole elsewhere, each
// word is its own value, checked as main checks it.

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

// Two fresh UUIDs whose version groups are equal by chance, each sent whole
// in a field of its own too, both bind, bare or after "APP:". Where the
// recording sends no part of either id whole elsewhere, each word is its own
// value and the equal group is refused, as main refuses it.
func TestVariables_TwoFreshUUIDsWithOneEqualGroupBothBind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, prefixed := range []bool{false, true} {
		h := newHTTP()
		db := &varDb{perTest: eventAndSuite(prefixed, false)}
		if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, false))); !ok || stub.Name != "mock-1" {
			t.Fatalf("prefixed %v, nothing sent whole: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", prefixed, ok, stub, diag)
		}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveSuite, prefixed, false)))
		if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "su2" {
			t.Fatalf("prefixed %v, nothing sent whole: refused at %s, want su2 (the equal group, as main): %q", prefixed, p.name, p.why)
		}
	}
	for _, prefixed := range []bool{false, true} {
		h := newHTTP()
		db := &varDb{perTest: eventAndSuite(prefixed, true)}
		if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, true))); !ok || stub.Name != "mock-1" {
			t.Fatalf("prefixed %v: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", prefixed, ok, stub, diag)
		}
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveSuite, prefixed, true)))
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

// Two groups of one fresh UUID may be equal by chance: where the recording
// also sends the UUID whole in a field of its own, they are one value of one
// call, recorded and live, and both bind. Where it sends no part of it whole
// elsewhere, each word is its own value, refused as main refuses it.
func TestVariables_TwoEqualGroupsOfOneUUIDBothBind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const live = "7d444840-41a3-41a3-a15b-5b4c7a3f9e21"
	h := newHTTP()
	db := &varDb{perTest: eventAndSuite(false, false)[:1]}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(live, false)))
	if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "ev2" {
		t.Fatalf("nothing sent whole: refused at %s, want ev2 (the second equal group, as main): %q", p.name, p.why)
	}
	h = newHTTP()
	db = &varDb{perTest: eventAndSuite(false, true)[:1]}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(live, true)))
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
// UUIDs whose version groups are equal by chance bind where the body also
// sends each whole, and are refused, as main refuses them, where nothing
// sends them whole; the application sending one UUID in both cookies, where
// the recording had two, is refused either way.
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
		t.Fatalf("two fresh UUIDs whose version groups are equal by chance were not answered: ok=%v diag=%+v", ok, diag)
	}
	for _, sentWhole := range []bool{true, false} {
		h = newHTTP()
		db = &varDb{perTest: []*models.Mock{rec(sentWhole)}}
		ok, _, diag := matchVar(t, h, db, call(liveEvent, liveEvent, sentWhole))
		wantProblem(t, ok, diag, "they were two values when the test was recorded")
	}
	// Sent nowhere whole, each cookie's word is its own value: the group
	// equal by chance is refused, as main refuses it.
	h = newHTTP()
	db = &varDb{perTest: []*models.Mock{rec(false)}}
	ok, _, diag := matchVar(t, h, db, call(liveEvent, liveSuite, false))
	wantProblem(t, ok, diag, "they were two values when the test was recorded")
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
		{"two fresh UUIDs, a group equal by chance", liveSuite, liveY, true, ""},
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
// groups are equal by chance, with no part of either marker sent whole
// elsewhere, are refused, as main refuses them (sent whole, they bind:
// TestVariables_AWordBesideAUUIDTheMockSendsWholeIsItsOwnValue). With the
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
// bind; one appId beside two cids, where two were recorded, is refused.
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
		{"two fresh appIds beside their cids, a group equal by chance", "1", recA, recC, liveX, liveW, ""},
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
// appIds there whose version groups are equal by chance bind: the path's cid
// is the evidence that the appId's words are a value of their own (with
// nothing sent whole they would be checked word by word, and refused). One
// appId sent for two is refused.
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
		{"two fresh appIds, a group equal by chance", recA, recC, liveX, liveW, ""},
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
