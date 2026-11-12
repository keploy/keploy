package http

import (
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
// key's createdAt, expiresAt and updatedAt are declared clock reads
// (keploy.yml's pat-created-at, pat-expires-at and pat-updated-at): here
// each falls in the second the repository's stamp names, in both runs,
// which learning alone reads as a copy of the request's stamp. Under both
// time settings.
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
// words, a request field that changed by two, and a word of a JSON number,
// which a replay reads no word of. A name that holds the word whole as a
// number is used whole.
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
}

// ciKeyCase varies the key record ciKeyPut learns on: a tail after the
// name's repository (nameTail), a tail after the request's repository
// (repoTail), and a number in the record (seq) holding the run's id that
// the request sent as a word of its ref condition, after a fraction's
// point (not seqWhole) or whole.
type ciKeyCase struct {
	nameTail, repoTail func(live bool) string
	seq, seqWhole      bool
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
			strconv.FormatInt(nameRead.UnixNano(), 10) + tail(c.nameTail, live) + `","resourceId":"","resourceType":"","scopes":["read","write","admin"]` + extra +
			`,"sessionPerms":"","status":"active","token":"","updatedAt":"` + at.UTC().Format(time.RFC3339) + `"}}`
	}
	req := func(read time.Time, sa, seq string, live bool) string {
		return `{"query":"mutation($sa: String!, $input: CreateWorkloadIdentityInput!) {\n  createServiceAccountWorkloadIdentity(serviceAccountId: $sa, input: $input) { id audience exchangeAudience issuer pinned { claim value } expiresAt }\n}","variables":{"input":{"conditions":[{"claim":"repository","value":"octo-org/repo-` +
			strconv.FormatInt(read.UnixNano(), 10) + tail(c.repoTail, live) + `"},{"claim":"ref","value":"refs/heads/run-` + seq + `"}],"issuer":"http://127.0.0.1:42833","provider":"github"},"sa":"` + sa + `"}}`
	}
	recBegin := ciRecBegin()
	began := learnAt.Add(10 * time.Minute)
	h, n, opts := learnOn(t)
	n.sources = func(mock string) []models.DeclaredSource {
		if mock != "mock-0" {
			return nil
		}
		return []models.DeclaredSource{
			{Rule: "pat-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
			{Rule: "pat-expires-at", Kind: models.DeclaredSourceClock, Field: "body.data.expiresAt", Clock: &models.ClockOffset{Min: ttl, Max: ttl + time.Second, Res: time.Second}},
			{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
		}
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
	put := learnRecorded("mock-0", 1130*time.Millisecond, "PUT", path(ciRecID),
		body(ciRecID, ciRecBy, ciRecSeq, recBegin.Add(120*time.Millisecond), recBegin.Add(100*time.Millisecond), false), nil, `{"data":{"version":1}}`)
	db := &learnDb{perTest: []*models.Mock{put}}
	in := live("PUT", path(ciLiveID), body(ciLiveID, ciLiveBy, ciLiveSeq, began.Add(120*time.Millisecond), sent, true), nil)
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
