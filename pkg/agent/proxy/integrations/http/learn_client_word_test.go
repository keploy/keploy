package http

import (
	"cmp"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A key record the application names after a value a client test sent
// inside a longer one: api-server's CI trust names a service account's key
// "CI identity: github <repository>", where the test sent the repository
// "octo-org/repo-<UnixNano>" in its GraphQL request, and writes the account's
// id, which the request sent whole, into the key's email
// ("sa-<id>@keploy.io.svc"). The record's PUT (the run's request and body)
// is answered: the learn step binds the nanosecond word of the request's
// field (body.variables.input.conditions[0].value#3) and uses it at the
// name's word (body.data.name#6), and binds the account's id at the field
// that held it whole, used at the email's word (body.data.email#1). The
// key's createdAt and updatedAt are declared clock reads (api-server's
// keploy.yml rules pat-created-at and pat-updated-at), and expiresAt one a
// year on (a rule the suite does not declare; the PUT under the suite's
// two alone is TestLearnMatchKeysPutUnderTheSuitesRulesAlone): each falls
// in the second the repository's stamp names, in both runs, which learning
// alone reads as a copy of the request's stamp. Under both time settings.
func TestLearnMatchBindsTheWordAClientTestSentInsideALongerValue(t *testing.T) {
	served, miss, db := ciKeyPut(t, ciKeyCase{}, true)
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("the key record's PUT: served %v, miss %+v; want it answered", served != nil, miss)
	}
	binds := db.consumed[0].Spec.LearnedBinds()["test case post-query-83"]
	vars := db.consumed[0].Spec.Learned()
	want := map[string][2]string{ // bind -> recorded, use
		"body.variables.input.conditions[0].value#3": {strconv.FormatInt(ciRecBegin().Add(100*time.Millisecond).UnixNano(), 10), "body.data.name#6"},
		"body.variables.sa":                          {ciRecBy, "body.data.email#1"},
	}
	for at, w := range want {
		found := false
		for name, b := range binds {
			if b.Bind != at {
				continue
			}
			found = true
			if v := vars[name]; b.Recorded != w[0] || v.Recorded != w[0] || !containsString(v.Use, w[1]) {
				t.Errorf("variable %s bound at %s: bind %+v, use %+v; want %q used at %s", name, at, b, v, w[0], w[1])
			}
		}
		if !found {
			t.Errorf("binds %+v; want one at the test case's %s", binds, at)
		}
	}
	if _, _, db := ciKeyPut(t, ciKeyCase{}, false); len(db.consumed) != 1 {
		t.Fatalf("the key record's PUT under times strict: not answered")
	}
}

// What keploy cannot read as one word of a client test's request carried
// into a longer value is refused, as before: a name that changed by two
// words, a request field that changed by two or by a byte between words as
// well, a name whose word as F wrote it is not the request's, and a word of
// a JSON number, which a replay reads no word of. A name that holds the word
// whole as a number is used whole, and a name whose stamp as F wrote it is
// not the request's binds nothing at the request.
func TestLearnMatchRefusesAClientTestsWordItCannotBind(t *testing.T) {
	const inside = "body.data.name carries a value test case post-query-83 sent earlier in the test at body.variables.input.conditions[0].value, inside a longer value or at a place keploy cannot bind it"
	for _, tc := range []struct {
		what string
		c    ciKeyCase
		why  string
	}{
		{"the name changed by two words", ciKeyCase{nameTail: func(live bool) string {
			if live {
				return " by 7c2e"
			}
			return " by 4b1d"
		}}, inside},
		{"the request's field changed by two words", ciKeyCase{repoTail: func(live bool) string {
			if live {
				return "-9f3a"
			}
			return "-1e8b"
		}}, inside},
		{"the request's field changed by a byte between words too", ciKeyCase{liveSep: "_"}, inside},
		{"the name's word as F wrote it is not the request's", ciKeyCase{hex: true, nameLive: "f0e1d2c3b4"}, "body.data.name changed, and a word of its recorded value (its characters 35 to 44) appears in what the test case gave the application, in test case post-query-83 at body.variables.input.conditions[0].value, which did not drift with it"},
		{"a word of a number", ciKeyCase{seq: true}, "body.data.seq carries a word test case post-query-83 sent earlier in the test at body.variables.input.conditions[1].value#3, inside a value that is no string, which a replay reads no word of"},
	} {
		for _, timeForm := range []bool{true, false} {
			served, miss, db := ciKeyPut(t, tc.c, timeForm)
			if served != nil || miss == nil || len(db.consumed) != 0 || !strings.Contains(miss.res.Why, tc.why) {
				t.Errorf("%s (time default %v): served %v, miss %+v; want it refused: %s", tc.what, timeForm, served != nil, miss, tc.why)
			}
		}
	}
	served, miss, db := ciKeyPut(t, ciKeyCase{seq: true, seqWhole: true}, true)
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("a number that is the word whole: served %v, miss %+v; want it answered", served != nil, miss)
	}
	var uses []string
	for _, v := range db.consumed[0].Spec.Learned() {
		uses = append(uses, v.Use...)
	}
	if !containsString(uses, "body.data.seq") {
		t.Fatalf("uses %v; want the number used whole at body.data.seq", uses)
	}
	// The name's stamp as F wrote it, a moment in F's call, is not the one
	// the request sent: the request's field binds nothing there, and the
	// stamp is learned as a read of the clock in each run.
	for _, timeForm := range []bool{true, false} {
		other := strconv.FormatInt(learnAt.Add(10*time.Minute+107*time.Millisecond).UnixNano(), 10)
		served, miss, db := ciKeyPut(t, ciKeyCase{nameLive: other}, timeForm)
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("the name's stamp as F wrote it is not the request's (time default %v): served %v, miss %+v; want it answered by the clock", timeForm, served != nil, miss)
		}
		if bindsAt(db.consumed[0], "body.variables.input.conditions[0].value#3") {
			t.Errorf("time default %v: binds %+v; want none at the request's word, which F's name does not hold", timeForm, db.consumed[0].Spec.LearnedBinds())
		}
		if _, ok := db.consumed[0].Spec.Clocks["body.data.name"]; !ok {
			t.Errorf("time default %v: clocks %+v; want the name's stamp learned as a read of the clock", timeForm, db.consumed[0].Spec.Clocks)
		}
	}
}

// An earlier call of the test, the application's audit write, sent the
// repository inside a longer value before the PUT ("trusted
// octo-org/repo-<word>"): such a call binds nothing, and it never hides the
// client test's request field that holds the word the name changed by,
// ranked before it, whether the word is a UnixNano (an instant, which the
// echo finds) or a random one. The PUT is answered, the word bound at the
// request's field. Where the request's field changed by two words (no word
// of it to bind), the call is where the name's value came from, and the PUT
// is refused naming the call. Under both time settings.
func TestLearnMatchBindsAClientTestsWordBeforeACallThatSentItInsideALongerValue(t *testing.T) {
	twoWords := func(live bool) string {
		if live {
			return "-9f3a"
		}
		return "-1e8b"
	}
	const atCall = "body.data.name carries a value mock-1 sent earlier in the test at body.event, inside a longer value or at a place keploy cannot bind it"
	for _, hex := range []bool{false, true} {
		for _, timeForm := range []bool{true, false} {
			served, miss, db := ciKeyPut(t, ciKeyCase{audit: true, hex: hex}, timeForm)
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("an earlier call sent the repository inside a longer value (random word %v, time default %v): served %v, miss %+v; want the PUT answered", hex, timeForm, served != nil, miss)
			}
			if !bindsAt(db.consumed[0], "body.variables.input.conditions[0].value#3") {
				t.Fatalf("random word %v: binds %+v; want the name's word bound at the test case's body.variables.input.conditions[0].value#3", hex, db.consumed[0].Spec.LearnedBinds())
			}
			served, miss, db = ciKeyPut(t, ciKeyCase{audit: true, hex: hex, repoTail: twoWords}, timeForm)
			if served != nil || miss == nil || len(db.consumed) != 0 || !strings.Contains(miss.res.Why, atCall) {
				t.Fatalf("the request's field changed by two words, an earlier call sent the repository inside a longer value (random word %v, time default %v): served %v, miss %+v; want it refused: %s", hex, timeForm, served != nil, miss, atCall)
			}
		}
	}
	// A field whose key ends in #<digits> binds at its escaped name.
	served, miss, db := ciKeyPut(t, ciKeyCase{key: "value#1"}, true)
	if served == nil || miss != nil || len(db.consumed) != 1 || !bindsAt(db.consumed[0], `body.variables.input.conditions[0]["value#1"]#3`) {
		t.Fatalf("the request's field named value#1: served %v, miss %+v; want the PUT answered, the word bound at the field's escaped name", served != nil, miss)
	}
}

// bindsAt reports whether m binds a variable at the test case
// post-query-83's place at.
func bindsAt(m models.Mock, at string) bool {
	for _, b := range m.Spec.LearnedBinds()["test case post-query-83"] {
		if b.Bind == at {
			return true
		}
	}
	return false
}

// ciKeyCase varies the key record ciKeyPut learns on: a tail after the
// name's repository (nameTail), a tail after the request's repository
// (repoTail), and a number in the record (seq) holding the run's id that
// the request sent as a word of its ref condition, after a fraction's
// point (not seqWhole) or whole. audit is an earlier call of the test, the
// application's audit write (mock-1), that sent the repository inside a
// longer value before the PUT, as recorded and as F sent it. key names the
// request's repository field ("value" where ""). hex makes the repository's
// word a random one in place of the UnixNano, a word that is no instant, and
// leaves the record's stamps as recorded. liveSep is the byte before the
// request's word as F sent it ("-" where ""), and nameLive the name's word
// as F wrote it (the request's word where ""). label adds a request field
// that holds the record's name whole (body.variables.label, after the
// repository's field in the request's order), with the word labelLive in
// place of the request's as F sent it, and respWord and respName
// make the PUT's answer hold the repository's word whole (body.data.ref)
// or the name whole (body.data.name), as recorded. suiteRules leaves out
// the expiresAt rule, which api-server's keploy.yml does not declare.
type ciKeyCase struct {
	nameTail, repoTail func(live bool) string
	seq, seqWhole      bool
	audit, hex         bool
	key                string
	liveSep, nameLive  string
	label              bool
	labelLive          string
	respWord, respName bool
	suiteRules         bool
}

const (
	ciRecID, ciLiveID = "5b39677d-8406-4d49-bfd1-853a0bf33fb8", "9e0c2f41-77aa-4c1b-9d2e-3f6b8a5c1d07"
	ciRecBy, ciLiveBy = "6af4e7999b2628f958a1d240", "7bf5e8aa0c3739a069b2e351"
	ciRecSeq          = "40213"
	ciLiveSeq         = "51324"
)

func ciRecBegin() time.Time { return learnAt.Add(time.Second) }

// ciKeyPut learns on the key record's PUT of api-server's CI trust (mock
// 3272 of a recorded run: its request and body), the run's account id and
// repository stamp new, as recorded otherwise, with keploy.yml's clock
// rules at its createdAt, expiresAt and updatedAt, under the time default
// (timeForm) or times strict.
func ciKeyPut(t *testing.T, c ciKeyCase, timeForm bool) (*models.Mock, *learnMiss, *learnDb) {
	t.Helper()
	const ttl = 365 * 24 * time.Hour
	tail := func(f func(bool) string, live bool) string {
		if f == nil {
			return ""
		}
		return f(live)
	}
	word := func(read time.Time, live bool) string {
		switch {
		case !c.hex:
			return strconv.FormatInt(read.UnixNano(), 10)
		case live:
			return "e5f6a7b8c9"
		}
		return "a1b2c3d4e5"
	}
	nameWord := func(read time.Time, live bool) string {
		if live && c.nameLive != "" {
			return c.nameLive
		}
		return word(read, live)
	}
	repoSep := func(live bool) string {
		if live && c.liveSep != "" {
			return c.liveSep
		}
		return "-"
	}
	path := func(id string) string { return "/v1/secret/data/keploy/personal-access-tokens/keploy.io/" + id }
	body := func(id, by, seq string, at, nameRead time.Time, live bool) string {
		extra := ""
		if c.seq {
			n := "1." + seq
			if c.seqWhole {
				n = seq
			}
			extra = `,"seq":` + n
		}
		return `{"data":{"_id":"` + id + `","cid":"keploy.io","createdAt":` + strconv.FormatInt(at.Unix(), 10) +
			`,"createdBy":"` + by + `","email":"sa-` + by + `@keploy.io.svc","expiresAt":` + strconv.FormatInt(at.Add(ttl).Unix(), 10) +
			`,"isServiceAccountKey":true,"isServiceToken":false,"isWebSession":false,"isWorkloadIdentity":true,"keyType":"pat","lastUsedAt":0,"mintedForJti":"","name":"CI identity: github octo-org/repo-` +
			nameWord(nameRead, live) + tail(c.nameTail, live) + `","resourceId":"","resourceType":"","scopes":["read","write","admin"]` + extra +
			`,"sessionPerms":"","status":"active","token":"","updatedAt":"` + at.UTC().Format(time.RFC3339) + `"}}`
	}
	key := cmp.Or(c.key, "value")
	name := func(read time.Time, live bool) string {
		return "CI identity: github octo-org/repo-" + nameWord(read, live) + tail(c.nameTail, live)
	}
	req := func(read time.Time, sa, seq string, live bool) string {
		label := ""
		if c.label {
			v := name(read, live)
			if live && c.labelLive != "" {
				v = "CI identity: github octo-org/repo-" + c.labelLive + tail(c.nameTail, live)
			}
			label = `,"label":"` + v + `"`
		}
		return `{"query":"mutation($sa: String!, $input: CreateWorkloadIdentityInput!) {\n  createServiceAccountWorkloadIdentity(serviceAccountId: $sa, input: $input) { id audience exchangeAudience issuer pinned { claim value } expiresAt }\n}","variables":{"input":{"conditions":[{"claim":"repository","` + key + `":"octo-org/repo` +
			repoSep(live) + word(read, live) + tail(c.repoTail, live) + `"},{"claim":"ref","value":"refs/heads/run-` + seq + `"}],"issuer":"http://127.0.0.1:42833","provider":"github"}` + label + `,"sa":"` + sa + `"}}`
	}
	recBegin := ciRecBegin()
	began := learnAt.Add(10 * time.Minute)
	h, n, opts := learnOn(t)
	n.sources = func(mock string) []models.DeclaredSource {
		if mock != "mock-0" {
			return nil
		}
		rules := []models.DeclaredSource{
			{Rule: "pat-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
			{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
		}
		if c.suiteRules {
			return rules
		}
		return append(rules, models.DeclaredSource{Rule: "key-expires-at", Kind: models.DeclaredSourceClock, Field: "body.data.expiresAt", Clock: &models.ClockOffset{Min: ttl, Max: ttl + time.Second, Res: time.Second}})
	}
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	sent := began.Add(100 * time.Millisecond)
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: recBegin.Add(100 * time.Millisecond),
		Header: map[string]string{"Content-Type": "application/json"}, Body: req(recBegin.Add(100*time.Millisecond), ciRecBy, ciRecSeq, false)}
	tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
	liveReq := *recReq
	liveReq.Body, liveReq.Timestamp = req(sent, ciLiveBy, ciLiveSeq, true), sent
	if !tr.Incoming(&liveReq, "", sent) {
		t.Fatal("the run's request paired with none of the test's recorded requests")
	}
	answer := `{"data":{"version":1}}`
	switch {
	case c.respWord:
		answer = `{"data":{"ref":"` + word(recBegin.Add(100*time.Millisecond), false) + `","version":1}}`
	case c.respName:
		answer = `{"data":{"name":"` + name(recBegin.Add(100*time.Millisecond), false) + `","version":1}}`
	}
	put := learnRecorded("mock-0", 1130*time.Millisecond, "PUT", path(ciRecID),
		body(ciRecID, ciRecBy, ciRecSeq, recBegin.Add(120*time.Millisecond), recBegin.Add(100*time.Millisecond), false), nil, answer)
	db := &learnDb{perTest: []*models.Mock{put}}
	if c.audit {
		event := func(read time.Time, live bool) string {
			return "trusted octo-org/repo-" + word(read, live)
		}
		// Its learn step bound its word, and told the test's tracker what
		// it sent, as recorded and as F sent it, at the variable place of
		// its body's field (earlierOf).
		audit := learnRecorded("mock-1", 1110*time.Millisecond, "POST", "/v1/audit", `{"event":"`+event(recBegin.Add(100*time.Millisecond), false)+`"}`, nil, `{}`)
		test, _, _, _ := tr.Test()
		tr.Served(test, learn.Earlier{Mock: audit.Name, At: audit.Spec.ReqTimestampMock, Own: true, Payload: learn.RawOf(audit, true),
			Seen: []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "body.event", Value: event(recBegin.Add(100*time.Millisecond), false), Type: learn.String}, Live: event(sent, true), Place: "body.event"}}})
	}
	stamped := began.Add(120 * time.Millisecond)
	if c.hex {
		stamped = recBegin.Add(120 * time.Millisecond)
	}
	in := live("PUT", path(ciLiveID), body(ciLiveID, ciLiveBy, ciLiveSeq, stamped, sent, true), nil)
	in.arrived = began.Add(130 * time.Millisecond)
	var store integrations.MockMemDb = db
	if timeForm {
		store = timeFormDb{db}
	}
	served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
	return served, miss, db
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
