package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// aboutRecs are two calls of one test: the POST's id comes back in its
// answer and the GET sends it in its path, and the GET's query drifts.
func aboutRecs() []*models.Mock {
	return []*models.Mock{
		learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
		learnRecorded("mock-2", time.Second, "GET", "/orders/o-1111?ts=100", "", nil, `{"id":"o-1111","state":"new"}`),
	}
}

var aboutFull = []*req{live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), live("GET", "/orders/o-9999?ts=200", "", nil)}

// fullRoundStaged is fullRound over a store that stages every recording,
// served or not, as the agent's does under the proven switch.
func fullRoundStaged(t *testing.T, recs []*models.Mock, calls []*req) []models.Mock {
	t.Helper()
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	var per, staged []*models.Mock
	for _, r := range recs {
		cp, st := *r, *r
		per, staged = append(per, &cp), append(staged, &st)
	}
	db := &learnDb{perTest: per, staged: staged, observe: tr.ServedMock}
	for i, c := range calls {
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || served == nil {
			t.Fatalf("F call %d not served: %+v %v", i, miss, err)
		}
	}
	return db.consumed
}

// wantAbout fails unless a carries what the protocol's learn step owes a
// decision (why, seen, call): a why in words that name no mock (a carry
// renames mocks), both rounds' values, and the call's key.
func wantAbout(t *testing.T, what string, a *models.About, call string) {
	t.Helper()
	switch {
	case a == nil:
		t.Errorf("%s: no about", what)
	case a.Why == "" || strings.Contains(a.Why, "mock-"):
		t.Errorf("%s: why %q, want plain words naming no mock", what, a.Why)
	case a.Seen == nil || a.Seen.Record == "" || a.Seen.Full == "" || a.Seen.Record == a.Seen.Full:
		t.Errorf("%s: seen %+v, want what the recording and the full round saw", what, a.Seen)
	case a.Call != call:
		t.Errorf("%s: call %q, want %q", what, a.Call, call)
	}
}

// decided is vs with each variable's about taken off, after failing the test
// unless every variable carries one with a why: a test that asserts what was
// learned compares the decisions, and still owes each its about.
func decided(t *testing.T, vs models.MockVariables) models.MockVariables {
	t.Helper()
	out := vs.Clone()
	for n, v := range out {
		if v.About == nil || v.About.Why == "" {
			t.Errorf("variable %s: about %+v, want one with a why", n, v.About)
		}
		v.About = nil
		out[n] = v
	}
	return out
}

// decidedBinds is decided over each mock's binds.
func decidedBinds(t *testing.T, binds map[string]models.MockVariables) map[string]models.MockVariables {
	t.Helper()
	out := make(map[string]models.MockVariables, len(binds))
	for m, vs := range binds {
		out[m] = decided(t, vs)
	}
	return out
}

// Each decision the HTTP learn step writes (an ignored field, a variable it
// uses, the bind at the earlier call, an echo) carries why, seen and its
// call's key, and names the other call by its key, never by mock name; the
// consumed states the agent hands back carry them (MockState.NoiseAbout and
// each variable's About). Row 2, the protocol half.
func TestEachDecisionTheHTTPLearnStepWritesSaysWhereItCameFrom(t *testing.T) {
	post, get := "http api.test POST /orders #1", "http api.test GET /orders/o-1111 #1"
	consumed := fullRoundStaged(t, aboutRecs(), aboutFull)
	byName := map[string]models.Mock{}
	for _, c := range consumed {
		byName[c.Name] = c
	}
	m2 := byName["mock-2"]
	if _, ok := m2.Spec.ReqBodyNoise["url.query.ts"]; !ok {
		t.Fatalf("mock-2 learned %v, want url.query.ts as noise", m2.Spec.ReqBodyNoise)
	}
	for path := range m2.Spec.ReqBodyNoise {
		a, ok := m2.Spec.NoiseAbout[path]
		if !ok {
			t.Errorf("noise %s of mock-2 has no about", path)
			continue
		}
		wantAbout(t, "noise "+path, &a, get)
	}
	if a := m2.Spec.NoiseAbout["url.query.ts"]; a.Seen == nil || a.Seen.Record != "100" || a.Seen.Full != "200" {
		t.Errorf("url.query.ts seen %+v, want record 100 and full 200", a.Seen)
	}
	uses := 0
	for name, v := range m2.Spec.Learned() {
		uses++
		wantAbout(t, "variable "+name+" of mock-2", v.About, get)
		if v.About != nil && !strings.Contains(v.About.Why, "("+post+")") {
			t.Errorf("variable %s of mock-2: why %q does not name the call it came from by its key", name, v.About.Why)
		}
	}
	binds := 0
	for target, vs := range m2.Spec.LearnedBinds() {
		for name, v := range vs {
			binds++
			wantAbout(t, "bind "+name+" at "+target, v.About, post)
			if v.About != nil && !strings.Contains(v.About.Why, "("+get+")") {
				t.Errorf("bind %s: why %q does not name the call that sends it by its key", name, v.About.Why)
			}
		}
	}
	m1 := byName["mock-1"]
	for name, v := range m1.Spec.Learned() {
		binds++
		wantAbout(t, "variable "+name+" of mock-1", v.About, post)
	}
	if uses == 0 || binds == 0 {
		t.Fatalf("mock-2 learned %d uses and %d binds, want the order id followed as a variable: %+v", uses, binds, byName)
	}
}

// A value the test case gave, a read of the app's clock and an ignored
// credential header carry their about too; the credential's value is never
// written, only its length. Row 2 (given, clock) and row 9 at the protocol.
func TestAGivenValueAClockReadAndACredentialHeaderSayWhereTheyCameFrom(t *testing.T) {
	m := learnRecorded("mock-4", 0, "POST", "/v1/events?x=1", `{"tenant":"t-rec","at":"2026-10-27T10:00:00Z"}`, map[string]string{"X-Api-Key": "k-recorded-0123"}, `{}`)
	calls := httpCallKeys([]*models.Mock{learnRecorded("mock-3", -time.Second, "POST", "/v1/events", "", nil, ""), m}, m, nil, true)
	if got, want := calls("mock-4"), "http api.test POST /v1/events #2"; got != want {
		t.Fatalf("call key %q, want %q (the second POST /v1/events of the test, the query left out)", got, want)
	}
	if got := calls("mock-9"); got != "" {
		t.Fatalf("a mock the test does not hold is named %q", got)
	}
	read := models.ClockOffset{Res: time.Second, Zone: models.ZoneUTC}
	at := learn.Leaf{Path: "body.at", Value: "2026-10-27T10:00:00Z", Type: learn.String}
	learned := []learn.Learned{
		{Kind: learn.KindVariable, Recorded: learn.Leaf{Path: "body.tenant", Value: "t-rec", Type: learn.String}, Live: learn.Leaf{Path: "body.tenant", Value: "t-full", Type: learn.String}},
		{Kind: learn.KindClockRead, Recorded: at, Live: learn.Leaf{Path: "body.at", Value: "2026-10-27T10:00:07Z", Type: learn.String}, Read: &read},
		{Kind: learn.KindNoise, Recorded: learn.Leaf{Path: "header.x-api-key", Value: "k-recorded-0123", Type: learn.String}, Live: learn.Leaf{Path: "header.x-api-key", Value: "k-live-456", Type: learn.String}, Why: "differs from mock-3"},
	}
	given := []variables.Binding{{Name: "tenant", Value: "t-full", Recorded: "t-rec"}}
	p, why := learnPlan(learn.NewTracker(), 1, m, learn.Shape{Fixed: map[string]string{"body": "json"}}, true, learned, nil, given, calls)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	call := "http api.test POST /v1/events #2"
	wantAbout(t, "given tenant", p.vars["tenant"].About, call)
	ca := p.clockAbout["body.at"]
	wantAbout(t, "clock body.at", &ca, call)
	na := p.noiseAbout["header.x-api-key"]
	wantAbout(t, "noise header.x-api-key", &na, call)
	if na.Seen == nil || na.Seen.Record != "<not written: a credential header, 15 chars>" || na.Seen.Full != "<not written: a credential header, 10 chars>" {
		t.Errorf("a credential header's seen %+v, want its values not written", na.Seen)
	}
	// No call key known: the about has none, and the why names no call.
	p, _ = learnPlan(learn.NewTracker(), 1, m, learn.Shape{}, true, learned[1:], nil, nil, nil)
	if a := p.clockAbout["body.at"]; a.Call != "" || a.Why == "" {
		t.Errorf("with no call keys: about %+v, want a why and no call", a)
	}
}

// stripAbouts is mocks with every about taken off: the same set as an older
// keploy, or a person who deleted them, leaves it.
func stripAbouts(mocks []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		cp := m.DeepCopy()
		cp.Spec.NoiseAbout, cp.Spec.ClockAbout = nil, nil
		vs := cp.Spec.Variables.Clone()
		for n, v := range vs {
			v.About = nil
			vs[n] = v
		}
		cp.Spec.Variables = vs
		out = append(out, cp)
	}
	return out
}

// Replay never reads about: a set that differs only in its abouts answers
// every call the same, a call it answers and one it does not. Row 6 at the
// agent.
func TestTwoSetsThatDifferOnlyInAboutAnswerTheSame(t *testing.T) {
	recs := aboutRecs()
	learnedSet := learnedOntoWithAbout(recs, fullRoundStaged(t, recs, aboutFull))
	abouts := 0
	for _, m := range learnedSet {
		abouts += len(m.Spec.NoiseAbout)
		for _, v := range m.Spec.Variables {
			if v.About != nil {
				abouts++
			}
		}
	}
	if abouts == 0 {
		t.Fatal("the learned set carries no about; the case proves nothing")
	}
	bare := stripAbouts(learnedSet)
	calls := []*req{
		live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil),
		live("GET", "/orders/o-5555?ts=300", "", nil),
		live("GET", "/orders/o-7777?ts=300", "", nil), // not the value the POST sent: a miss
	}
	verdicts := func(mocks []*models.Mock) []string {
		h := newHTTP()
		db := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
		var out []string
		for _, c := range calls {
			ok, stub, _ := matchVarIn(t, context.Background(), h, db, c)
			v := "miss"
			if ok && stub != nil {
				v = "answered by " + stub.Name + ": " + stub.Spec.HTTPResp.Body
			}
			out = append(out, v)
		}
		return out
	}
	with, without := verdicts(learnedSet), verdicts(bare)
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("verdicts with abouts %q, without %q: want the same", with, without)
	}
	if with[0] == "miss" || with[1] == "miss" || with[2] != "miss" {
		t.Fatalf("verdicts %q, want the first two answered and the last a miss", with)
	}
}

// learnedOntoWithAbout is learnedOnto keeping the abouts the consumed mocks
// carry: each noise's and each variable's.
func learnedOntoWithAbout(recs []*models.Mock, consumed []models.Mock) []*models.Mock {
	out := learnedOnto(recs, consumed)
	byName := map[string]*models.Mock{}
	for _, m := range out {
		byName[m.Name] = m
	}
	for _, c := range consumed {
		m := byName[c.Name]
		m.Spec.NoiseAbout = models.MergeAbouts(m.Spec.NoiseAbout, c.Spec.NoiseAbout)
		m.Spec.ClockAbout = models.MergeAbouts(m.Spec.ClockAbout, c.Spec.ClockAbout)
	}
	return out
}

// A call is numbered among its own test's calls with its key, in recorded
// order: the open test's own among its window's recordings (another test's
// and the pool's left out), a pool call among the recordings outside every
// window, each recording once however many tiers stage it. A recording with
// no Host is named by its URL's authority.
func TestACallIsNumberedAmongItsOwnTestsCallsWithItsKey(t *testing.T) {
	rec := func(name string, at time.Duration) *models.Mock {
		return learnRecorded(name, at, "POST", "/v1/events", "", nil, "")
	}
	other, pool1, own1, own2, pool2 := rec("other", 0), rec("pool-1", time.Second), rec("own-1", 2*time.Second), rec("own-2", 3*time.Second), rec("pool-2", 4*time.Second)
	db := &learnDb{otherTest: map[time.Time]bool{other.Spec.ReqTimestampMock: true},
		pool: map[time.Time]bool{pool1.Spec.ReqTimestampMock: true, pool2.Spec.ReqTimestampMock: true}}
	staged := []*models.Mock{pool2, own2, other, own1, pool1, own2}
	ownCalls := httpCallKeys(staged, own2, db, true)
	if got := ownCalls("own-2"); got != "http api.test POST /v1/events #2" {
		t.Errorf("the open test's second call is %q, want #2 among its own", got)
	}
	if got := ownCalls("other"); got != "" {
		t.Errorf("another test's call is named %q in this test", got)
	}
	poolCalls := httpCallKeys(staged, pool2, db, false)
	if got := poolCalls("pool-2"); got != "http api.test POST /v1/events #2" {
		t.Errorf("the second pool call is %q, want #2 among the calls outside every window", got)
	}
	if got := httpCallKeys(staged, pool2, db, true)("pool-2"); got != "http api.test POST /v1/events #2" {
		t.Errorf("the second pool call, paired as the open test's, is %q, want #2 among the calls outside every window", got)
	}
	// Another test's recording paired here: that test's other calls need
	// not be staged, so its place among them is not known and it is named
	// no call rather than a wrong #n.
	if got := httpCallKeys(staged, other, db, false)("other"); got != "" {
		t.Errorf("another test's recording is named %q, want no call", got)
	}
	upper := learnRecorded("upper", 0, "GET", "/v1/x", "", map[string]string{"Host": "API.Test:8080"}, "")
	if got := httpCallKeys([]*models.Mock{upper}, upper, nil, true)("upper"); got != "http api.test:8080 GET /v1/x #1" {
		t.Errorf("a call recorded with Host API.Test:8080 is %q, want the host lowercased", got)
	}
	bare := learnRecorded("bare", 0, "GET", "http://Audit:8080/v1/x?q=1", "", nil, "")
	delete(bare.Spec.HTTPReq.Header, "Host")
	if got := httpCallKeys([]*models.Mock{bare}, bare, nil, true)("bare"); got != "http audit:8080 GET /v1/x #1" {
		t.Errorf("a call with no Host is %q, want its URL's authority", got)
	}
}

// A variable's seen is left out when any place of its value is a
// credential's, not only the first it was learned at.
func TestAVariablesSeenIsLeftOutAtAnyCredentialPlace(t *testing.T) {
	m := learnRecorded("mock-4", 0, "POST", "/v1/events", `{"tenant":"t-rec"}`, map[string]string{"X-Api-Key": "t-rec"}, `{}`)
	leaf := func(path, v string) learn.Leaf { return learn.Leaf{Path: path, Value: v, Type: learn.String} }
	learned := []learn.Learned{
		{Kind: learn.KindVariable, Recorded: leaf("body.tenant", "t-rec"), Live: leaf("body.tenant", "t-full")},
		{Kind: learn.KindVariable, Recorded: leaf("header.x-api-key", "t-rec"), Live: leaf("header.x-api-key", "t-full")},
	}
	p, why := learnPlan(learn.NewTracker(), 1, m, learn.Shape{Fixed: map[string]string{"body": "json"}}, true, learned, nil, []variables.Binding{{Name: "tenant", Value: "t-full", Recorded: "t-rec"}}, nil)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if s := p.vars["tenant"].About.Seen; s == nil || s.Record != "<not written: a credential header, 5 chars>" || s.Full != "<not written: a credential header, 6 chars>" {
		t.Errorf("seen %+v, want the value left out: it is sent at a credential header too", s)
	}
	// The credential place learned first: a later plain place does not
	// write the value back.
	learned[0], learned[1] = learned[1], learned[0]
	p, why = learnPlan(learn.NewTracker(), 1, m, learn.Shape{Fixed: map[string]string{"body": "json"}}, true, learned, nil, []variables.Binding{{Name: "tenant", Value: "t-full", Recorded: "t-rec"}}, nil)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if s := p.vars["tenant"].About.Seen; s == nil || s.Record != "<not written: a credential header, 5 chars>" || s.Full != "<not written: a credential header, 6 chars>" {
		t.Errorf("seen %+v, want the value left out: it was sent at a credential header first", s)
	}
}

// A value the call sends at two places and its answer echoes is one
// variable; its why is the first place's (the echo), kept when the second
// place joins it.
func TestAnEchoSentAtTwoPlacesKeepsItsWhy(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"ref":"r-1","copy":"r-1","amount":5}`, nil, `{"id":"ch_1","ref":"r-1"}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"ref":"r-2","copy":"r-2","amount":5}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	vs := db.consumed[0].Spec.Learned()
	if len(vs) != 1 {
		t.Fatalf("learned %+v, want one variable sent at both places", vs)
	}
	for name, v := range vs {
		if len(v.Use) != 1 {
			t.Fatalf("variable %s: %+v, want it bound at one place and used at the other", name, v)
		}
		if v.About == nil || v.About.Why != echoWhy(v.Answer) {
			t.Errorf("variable %s: about %+v, want the echo's why %q", name, v.About, echoWhy(v.Answer))
		}
		if v.About != nil && (v.About.Seen == nil || v.About.Seen.Record != "r-1" || v.About.Seen.Full != "r-2") {
			t.Errorf("variable %s: seen %+v, want r-1 and r-2", name, v.About.Seen)
		}
	}
}
