package http

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap/zaptest"
)

// learnDb is a mock store with a test's own mocks (consumed when served) and
// a pool.
type learnDb struct {
	mockMemDb
	perTest  []*models.Mock
	consumed []models.Mock
	pooled   []*models.Mock // the pool's mocks as UpdateUnFilteredMock left them
	// otherTest are the moments recorded in another test's window.
	otherTest map[time.Time]bool
	// pool are moments recorded outside every recorded test's window by
	// mocks the fake does not show (a data-plane mock of the pool, which
	// the exact view hides while a test is open).
	pool map[time.Time]bool
	// staged are the mocks the fake stages but does not show
	// (integrations.StagedReader).
	staged []*models.Mock
	// observe, when set, is told of every mock a claim takes, as the mock
	// manager tells its served observer (the learn step's tracker).
	observe func(*models.Mock)
}

func (d *learnDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return d.perTest, nil }

func (d *learnDb) StagedMocks() []*models.Mock { return d.staged }

// The fake's session mocks were recorded outside every recorded test's
// window (the start-up): the pool's; every other moment is inside one, the
// open test's (otherTest names moments of another recorded test).
func (d *learnDb) InTestWindow(time.Time) bool { return false }
func (d *learnDb) InRecordedTestWindow(t time.Time) (bool, bool) {
	if d.pool[t] {
		return false, true
	}
	for _, m := range d.mocks {
		if m.Spec.ReqTimestampMock.Equal(t) {
			return false, true
		}
	}
	return true, true
}
func (d *learnDb) SameRecordedTestWindow(a, b time.Time) (bool, bool) {
	ia, _ := d.InRecordedTestWindow(a)
	ib, _ := d.InRecordedTestWindow(b)
	return ia && ib && d.otherTest[a] == d.otherTest[b], true
}
func (d *learnDb) GetSessionMocks() ([]*models.Mock, error) { return d.mocks, nil }
func (d *learnDb) DeleteFilteredMock(m models.Mock) bool {
	if d.observe != nil {
		c := m
		d.observe(&c)
	}
	for i, p := range d.perTest {
		if p.Name == m.Name {
			d.perTest = append(d.perTest[:i:i], d.perTest[i+1:]...)
			d.consumed = append(d.consumed, m)
			return true
		}
	}
	return false
}
func (d *learnDb) UpdateUnFilteredMock(old, new *models.Mock) bool {
	if d.observe != nil {
		d.observe(new)
	}
	for i, p := range d.mocks {
		if p == old {
			d.mocks[i] = new
			d.pooled = append(d.pooled, new)
			return true
		}
	}
	return false
}

var learnAt = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func learnRecorded(name string, at time.Duration, method, target, body string, header map[string]string, respBody string) *models.Mock {
	h := map[string]string{"Host": "api.test", "Content-Type": "application/json"}
	for k, v := range header {
		h[k] = v
	}
	u, _ := url.Parse(target)
	params := map[string]string{}
	for k, v := range u.Query() {
		params[k] = strings.Join(v, ", ")
	}
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient},
			HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: target, URLParams: params, Header: h, Body: body},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: respBody},
			ReqTimestampMock: learnAt.Add(at),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
	}
}

func live(method, target, body string, header map[string]string) *req {
	u, _ := url.Parse(target)
	h := http.Header{}
	h.Set("Host", "api.test")
	h.Set("Content-Type", "application/json")
	for k, v := range header {
		h.Set(k, v)
	}
	return &req{method: method, url: u, header: h, body: []byte(body)}
}

func learnOn(t *testing.T) (*HTTP, learnNoise, models.OutgoingOptions) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	opts := models.OutgoingOptions{Learn: true}
	return h, h.newLearnNoise(opts, nil, nil), opts
}

func TestLearnMatchLearnsADriftedBodyValueAsNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"request_id":"r-1","item":"book"}`, nil, `{"ok":true}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", `{"request_id":"r-2","item":"book"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if len(db.consumed) != 1 || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, map[string][]string{"body.request_id": {}}) {
		t.Fatalf("the consumed mock carries %v, want body.request_id learned as noise", db.consumed)
	}
}

func TestLearnMatchLearnsEveryPartOfARequest(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "GET", "/v1/users/41?at=100", "", map[string]string{"Authorization": "Bearer one", "X-Trace": "t1"}, `{}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("GET", "/v1/users/42?at=200", "", map[string]string{"Authorization": "Bearer two", "X-Trace": "t2"}), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	// The path segment, the query value, the header, and the credential the
	// static noise list names: it leaves that list in the learn step.
	want := map[string][]string{"url.path.2": {}, "url.query.at": {}, "header.x-trace": {}, "header.authorization": {}}
	if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
		t.Fatalf("learned %v, want %v", got, want)
	}
}

func TestLearnMatchStaticNoiseHeadersAreNotLearned(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "GET", "/a", "", map[string]string{"Traceparent": "00-1"}, `{}`)}}
	served, miss, _ := h.learnMatch(context.Background(), live("GET", "/a", "", map[string]string{"Traceparent": "00-2"}), db, n, opts)
	if served == nil || miss != nil || len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("a header of the static list is not compared, so nothing drifted: served %v miss %+v learned %v", served, miss, db.consumed)
	}
}

// A value the answer echoes is a variable of the call alone: bound at the
// request, answered with this run's value where the recording holds it whole
// (a mention inside a longer text is served as recorded), never noise.
func TestLearnMatchLearnsAnEchoAsAVariableOfItsCall(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","amount":5}`, nil, `{"id":"ch_1","idempotency_key":"k-1","amount":5,"note":"k-1 ok"}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","amount":5}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"ch_1","idempotency_key":"k-2","amount":5,"note":"k-1 ok"}` {
		t.Fatalf("served %s: this run's value is answered where the recorded one was echoed whole", served.Spec.HTTPResp.Body)
	}
	want := models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}}
	if got := db.consumed[0].Spec.Learned(); !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v", got, want)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("an echo is not noise: %v", db.consumed[0].Spec.ReqBodyNoise)
	}

	// Echoed only inside a longer text, it cannot be answered: refused,
	// never learned as noise.
	db = &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1"}`, nil, `{"note":"k-1 ok"}`)}}
	served, miss, _ = h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2"}`, nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "only inside a longer value") {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if len(db.perTest) != 1 {
		t.Fatal("the refused call's mock stays unconsumed")
	}
}

// A value an earlier call of the test sent is a variable: bound there, used
// here, and the call is answered only when it holds the value sent there.
func TestLearnMatchFollowsAVariable(t *testing.T) {
	h, n, opts := learnOn(t)
	setup := func() (context.Context, *learnDb) {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		return ctx, db
	}

	ctx, db := setup()
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"o-9","state":"new"}` {
		t.Fatalf("served %s: the answer holds this run's value", served.Spec.HTTPResp.Body)
	}
	use := db.consumed[1]
	if want := (models.MockVariables{"order": {Recorded: "o-1", Use: []string{"url.path[2]"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(use.Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"order": {Recorded: "o-1", Bind: "body.order"}}}); !reflect.DeepEqual(use.Spec.LearnedBinds(), want) {
		t.Fatalf("binds %+v, want %+v", use.Spec.LearnedBinds(), want)
	}
	if len(use.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("a variable is never noise: %v", use.Spec.ReqBodyNoise)
	}

	// A call that sends another value than the one the test sent is refused.
	ctx, db = setup()
	served, miss, _ = h.learnMatch(ctx, live("GET", "/v1/orders/o-5", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged ||
		!strings.Contains(miss.res.Why, "url.path.2 does not hold the value the test sent at body.order of mock-0") || miss.closest == nil || miss.closest.Name != "mock-1" {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if len(db.perTest) != 1 {
		t.Fatal("the refused call's mock stays unconsumed")
	}
}

// A value sent under an operator's key (an extended-JSON {"$oid": ...}) is a
// variable named after the key above it, the value's own name: "order", not
// "$oid".
func TestLearnMatchNamesAVariableAboveADollarKey(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":{"$oid":"o-1"}}`, nil, `{}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1"}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":{"$oid":"o-9"}}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the first call is paired: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	use := db.consumed[1]
	if want := (models.MockVariables{"order": {Recorded: "o-1", Use: []string{"url.path[2]"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(use.Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"order": {Recorded: "o-1", Bind: "body.order.$oid"}}}); !reflect.DeepEqual(use.Spec.LearnedBinds(), want) {
		t.Fatalf("binds %+v, want %+v", use.Spec.LearnedBinds(), want)
	}
}

// A variable whose value F saw drift only with its clock where it is bound (a
// clock read moved by the lead: learn.ClockOf) is marked so at its bind with
// the read its recording holds, the instant and resolution it says
// (MockVariable.Clock, learn.ClockReads); one that drifted otherwise is not.
func TestLearnMatchMarksAClockReadAtItsBind(t *testing.T) {
	rec := learnAt.Add(-time.Hour).Truncate(time.Second)
	for _, c := range []struct {
		name  string
		live  string
		clock []models.ClockRead
	}{
		{"moved by the lead", rec.Add(learn.FullRoundLead).Format(time.RFC3339), []models.ClockRead{{At: rec.UTC(), Res: time.Second}}},
		{"moved by another amount", rec.Add(5 * time.Minute).Format(time.RFC3339), nil},
	} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		recorded := rec.Format(time.RFC3339)
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/events", `{"at":"`+recorded+`"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/events?since="+url.QueryEscape(recorded), "", nil, `{}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/events", `{"at":"`+c.live+`"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("%s: the first call is paired: %+v", c.name, miss)
		}
		served, miss, err := h.learnMatch(ctx, live("GET", "/v1/events?since="+url.QueryEscape(c.live), "", nil), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", c.name, served, miss, err)
		}
		binds := db.consumed[1].Spec.LearnedBinds()["mock-0"]
		if len(binds) != 1 {
			t.Fatalf("%s: binds %+v, want one variable bound at mock-0", c.name, db.consumed[1].Spec.LearnedBinds())
		}
		for name, v := range binds {
			if v.Bind != "body.at" || !reflect.DeepEqual(v.Clock, c.clock) {
				t.Fatalf("%s: variable %s bound %+v, want at body.at with Clock %v", c.name, name, v, c.clock)
			}
		}
	}
}

func TestLearnMatchMissReasons(t *testing.T) {
	h, n, opts := learnOn(t)
	for _, tc := range []struct {
		name   string
		mock   *models.Mock
		req    *req
		reason string
	}{
		{"an array in another order", learnRecorded("m", 0, "POST", "/a", `{"ids":[1,2,3]}`, nil, `{}`), live("POST", "/a", `{"ids":[3,1,2]}`, nil), models.MissUnstableOrder},
		{"a field added", learnRecorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":1,"x":2}`, nil), models.MissRequestChanged},
		{"a type changed", learnRecorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":"1"}`, nil), models.MissRequestChanged},
		{"another method", learnRecorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("PUT", "/a", `{"id":1}`, nil), models.MissUnmockedCall},
		{"another host", learnRecorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":1}`, map[string]string{"Host": "other.test"}), models.MissUnmockedCall},
		{"another number of path segments", learnRecorded("m", 0, "GET", "/a/b", ``, nil, `{}`), live("GET", "/a/b/c", ``, nil), models.MissUnmockedCall},
		{"a header added", learnRecorded("m", 0, "GET", "/a", ``, nil, `{}`), live("GET", "/a", ``, map[string]string{"If-None-Match": "x"}), models.MissRequestChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &learnDb{perTest: []*models.Mock{tc.mock}}
			served, miss, err := h.learnMatch(context.Background(), tc.req, db, n, opts)
			if err != nil || served != nil || miss == nil || miss.res.Reason != tc.reason {
				t.Fatalf("served %v miss %+v err %v, want reason %s", served, miss, err, tc.reason)
			}
			r := &models.MockMismatchReport{NextSteps: "re-record"}
			miss.explain(r)
			if r.Reason != tc.reason || !strings.HasPrefix(r.NextSteps, "keploy's learn step: ") || !strings.HasSuffix(r.NextSteps, " re-record") {
				t.Fatalf("report %+v", r)
			}
		})
	}
}

func TestLearnMatchPoolOnlyInAFullRecord(t *testing.T) {
	h, n, opts := learnOn(t)
	pool := learnRecorded("pool-0", 0, "GET", "/config/1", "", nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}}
	if served, miss, _ := h.learnMatch(context.Background(), live("GET", "/config/2", "", nil), db, n, opts); served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("a partial record never pairs a pool mock: served %v miss %+v", served, miss)
	}
	opts.LearnPool = true
	served, miss, _ := h.learnMatch(context.Background(), live("GET", "/config/2", "", nil), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("a full record pairs it: miss %+v", miss)
	}
	if len(db.pooled) != 1 || !reflect.DeepEqual(db.pooled[0].Spec.ReqBodyNoise, map[string][]string{"url.path.1": {}}) {
		t.Fatalf("the pool mock carries what was learned: %+v", db.pooled)
	}
	// The pool keeps it: the next call with yet another value matches exactly.
	if served, miss, _ := h.learnMatch(context.Background(), live("GET", "/config/3", "", nil), db, n, models.OutgoingOptions{Learn: true}); served == nil || miss != nil {
		t.Fatalf("learned noise is known to the next call: miss %+v", miss)
	}
}

func TestLearnedMatchHonoursWhatFLearned(t *testing.T) {
	h, n, _ := learnOn(t)
	m := learnRecorded("mock-0", 0, "POST", "/v1/charges/1", `{"key":"k-1","amount":5}`, map[string]string{"X-Trace": "t1"}, `{"amount":5}`)
	m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}, "header.x-trace": {}, "body.key": {}}
	db := &learnDb{perTest: []*models.Mock{m}}
	served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":5}`, map[string]string{"X-Trace": "t9"}), db, n, models.OutgoingOptions{})
	if err != nil || served == nil {
		t.Fatalf("served %v err %v", served, err)
	}
	// What was not learned is still compared: another amount is not served.
	db = &learnDb{perTest: []*models.Mock{m}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":6}`, map[string]string{"X-Trace": "t9"}), db, n, models.OutgoingOptions{}); served != nil {
		t.Fatal("a value nothing learned is compared")
	}
	// A recording that declares variables is the matcher's variable pass's.
	v := learnRecorded("mock-0", 0, "POST", "/v1/charges/1", `{"key":"k-1","amount":5}`, map[string]string{"X-Trace": "t1"}, `{"amount":5}`)
	v.Spec.ReqBodyNoise = m.Spec.ReqBodyNoise
	v.Spec.Variables = models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Echo: true}}
	db = &learnDb{perTest: []*models.Mock{v}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":5}`, map[string]string{"X-Trace": "t9"}), db, n, models.OutgoingOptions{}); served != nil {
		t.Fatal("learnedMatch served a recording that declares variables")
	}
}

// Outside record's full round a request that came with an array in another
// order is still unstable_order (design §7.3: F or C), and names the call it
// was refused against; any other miss keeps the matcher's report.
func TestOrderMissOutsideTheFullRound(t *testing.T) {
	h, n, _ := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("m", 0, "POST", "/a", `{"ids":[1,2,3]}`, nil, `{}`)}}
	miss := h.orderMiss(context.Background(), live("POST", "/a", `{"ids":[3,1,2]}`, nil), db, n)
	if miss == nil || miss.res.Reason != models.MissUnstableOrder || miss.closest == nil || miss.closest.Name != "m" {
		t.Fatalf("miss %+v", miss)
	}
	if len(db.perTest) != 1 {
		t.Fatal("reading a miss consumes nothing")
	}
	if miss := h.orderMiss(context.Background(), live("POST", "/a", `{"ids":[1,2,3],"x":1}`, nil), db, n); miss != nil {
		t.Fatalf("a changed request is not an order miss: %+v", miss.res)
	}
}

// A value that flowed from an earlier call is a variable whatever its type
// there: followed when keploy can bind it there whole, refused otherwise
// (inside a longer value), and never noise.
func TestLearnMatchVariablesAreNeverNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	for _, tc := range []struct {
		name          string
		first, second *models.Mock
		f, s          *req
		followed      bool
	}{
		{
			"a number flowing into a path",
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":12345}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/orders/12345", "", nil, `{}`),
			live("POST", "/v1/orders", `{"order":54321}`, nil),
			live("GET", "/v1/orders/54321", "", nil),
			true,
		},
		{
			"a token flowing into a credential",
			learnRecorded("mock-0", 0, "POST", "/v1/login", `{"token":"tok-A"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-A"}, `{}`),
			live("POST", "/v1/login", `{"token":"tok-B"}`, nil),
			live("GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-B"}),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{tc.first, tc.second}}
			if served, miss, _ := h.learnMatch(ctx, tc.f, db, n, opts); served == nil || miss != nil {
				t.Fatalf("the first call is paired: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, tc.s, db, n, opts)
			if !tc.followed {
				if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
					t.Fatalf("a value that flowed is refused until it can be followed: served %v miss %+v", served, miss)
				}
				return
			}
			if served == nil || miss != nil {
				t.Fatalf("served %v miss %+v", served, miss)
			}
			if got := db.consumed[1].Spec; len(got.Learned()) != 1 || len(got.ReqBodyNoise) != 0 {
				t.Fatalf("variables %+v noise %v", got.Variables, got.ReqBodyNoise)
			}
		})
	}
}

// A value of another kind of mock earlier in the test (an id the app
// inserted into a database) is a variable too.
func TestLearnMatchFindsEarlierPlacesOfAnyKind(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		Metadata: map[string]string{"query": "INSERT INTO users VALUES ($1)", "params": "u-77"}, GenericRequests: []models.Payload{{Message: []models.OutputBinary{{Data: "INSERT u-77"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "GET", "/v1/users/u-77", "", nil, `{}`)}}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/users/u-88", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "(in pg-0)") {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// What drifted in one element of an array is learned for that element alone.
func TestLearnMatchNoiseNamesOneElement(t *testing.T) {
	h, n, opts := learnOn(t)
	pool := learnRecorded("pool-0", 0, "POST", "/v1/put", `{"items":[{"k":"a","at":"t-1"},{"k":"b","at":"fixed"}]}`, nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}}
	opts.LearnPool = true
	if served, miss, _ := h.learnMatch(context.Background(), live("POST", "/v1/put", `{"items":[{"k":"a","at":"t-2"},{"k":"b","at":"fixed"}]}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("paired: %+v", miss)
	}
	if got := db.pooled[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, map[string][]string{"body.items[0].at": {}}) {
		t.Fatalf("learned %v", got)
	}
	// The other element's value never drifted: a change there is not excused.
	served, _, _ := h.learnMatch(context.Background(), live("POST", "/v1/put", `{"items":[{"k":"a","at":"t-3"},{"k":"b","at":"REGRESSED"}]}`, nil), db, n, models.OutgoingOptions{Learn: true})
	if served != nil {
		t.Fatal("noise learned on items[0] excused items[1]")
	}
}

// Outside every test a request belongs to the pool: a test's own mocks are
// never paired then, and a partial record (no pool) pairs nothing.
func TestLearnMatchOutsideATestPairsOnlyThePool(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker() // no test has begun
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"id":"x"}`, nil, `{}`)}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"y"}`, nil), db, n, opts); served != nil || miss == nil {
		t.Fatalf("a test's mock was paired outside every test: served %v", served)
	}
	if served, _, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"x"}`, nil), db, n, opts); served == nil {
		t.Fatal("an exact match is served outside a test, as before")
	}
}

// ownedView is a view that scopes the pool to a test and knows of no open one.
type ownedView struct{ *learnDb }

func (ownedView) OwnedByTest(*models.Mock) (bool, bool) { return false, false }

func TestLearnMatchAScopedViewWithNoTestPairsNothing(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := ownedView{&learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"id":"x"}`, nil, `{}`)}}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"y"}`, nil), db, n, opts); served != nil || miss == nil {
		t.Fatalf("a mock the view does not say is the test's was paired: served %v", served)
	}
}

// A multipart body is read by its own boundary, minted per request.
func TestLearnMatchMultipartBoundary(t *testing.T) {
	h, n, opts := learnOn(t)
	body := func(b, file, note string) string {
		return "--" + b + "\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\n" + note + "\r\n--" + b +
			"\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a.txt\"\r\n\r\n" + file + "\r\n--" + b + "--\r\n"
	}
	rec := learnRecorded("mock-0", 0, "POST", "/up", body("AAA", "data", "n-1"), map[string]string{"Content-Type": "multipart/form-data; boundary=AAA"}, `{}`)
	in := live("POST", "/up", body("BBB", "data", "n-2"), map[string]string{"Content-Type": "multipart/form-data; boundary=BBB"})
	db := &learnDb{perTest: []*models.Mock{rec}}
	served, miss, _ := h.learnMatch(context.Background(), in, db, n, opts)
	if served == nil || miss != nil || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, map[string][]string{"body.note": {}}) {
		t.Fatalf("served %v miss %+v consumed %+v", served, miss, db.consumed)
	}
	db = &learnDb{perTest: []*models.Mock{rec}}
	if served, miss, _ := h.learnMatch(context.Background(), live("POST", "/up", body("CCC", "other", "n-1"), map[string]string{"Content-Type": "multipart/form-data; boundary=CCC"}), db, n, opts); served != nil || miss == nil {
		t.Fatal("a file part is compared whole")
	}
}

// The destination in the operation key is the Host, as the replay matcher
// compares it under the switch: a container key the recording carries is not
// compared (its first-use numbering can swap two containers of one image).
func TestOperationKeyNamesTheHost(t *testing.T) {
	_, n, _ := learnOn(t)
	rec := learnRecorded("m", 0, "GET", "/a", "", nil, `{}`)
	rec.Spec.Metadata[containerkey.MetadataKey] = "docker:api#1"
	rp := recordedParts(rec)
	for _, tc := range []struct {
		host string
		key  bool
	}{
		{"api.test", true},
		{"API.test", true},
		{"other.test", false},
		{"api.test:8080", false},
	} {
		lp := liveParts(context.Background(), live("GET", "/a", "", map[string]string{"Host": tc.host}))
		got := n.shape(rp, destOf(lp, rp)).Key == n.shape(lp, sameDest).Key
		if got != tc.key {
			t.Errorf("host %s: candidate %v, want %v", tc.host, got, tc.key)
		}
	}
}

// What F learns is what every later replay follows: the variables learned
// in F, written onto the two recordings as the replay writes them (the use
// on the later call, the bind its state carries onto the earlier one), are
// served by the variable pass with yet another value, and a later call that
// sends another value than the test bound is a variable miss, not an answer.
func TestWhatFLearnsTheVariablePassFollows(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	f := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`),
	}}
	recordings := map[string]*models.Mock{}
	for _, m := range f.perTest {
		recordings[m.Name] = m
	}
	for _, r := range []*req{live("POST", "/v1/orders", `{"order":"o-9"}`, nil), live("GET", "/v1/orders/o-9", "", nil)} {
		if served, miss, _ := h.learnMatch(ctx, r, f, n, opts); served == nil || miss != nil {
			t.Fatalf("F: %+v", miss)
		}
	}
	// The replay writes each consumed mock's variables onto its recording,
	// and the binds a state carries onto the mock they are bound at.
	written := func() []*models.Mock {
		out := map[string]*models.Mock{}
		for name, m := range recordings {
			c := *m
			c.Spec.Variables = models.MockVariables{}
			out[name] = &c
		}
		for _, st := range f.consumed {
			for vn, v := range st.Spec.Learned() {
				out[st.Name].Spec.Variables[vn] = v
			}
			for target, vs := range st.Spec.LearnedBinds() {
				for vn, v := range vs {
					out[target].Spec.Variables[vn] = v
				}
			}
		}
		return []*models.Mock{out["mock-0"], out["mock-1"]}
	}

	ci := &varDb{perTest: written()}
	if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/orders", `{"order":"o-55"}`, nil)); !ok {
		t.Fatalf("the create in CI: %+v", diag)
	}
	ok, stub, diag := matchVar(t, h, ci, live("GET", "/v1/orders/o-55", "", nil))
	if !ok || stub.Spec.HTTPResp.Body != `{"id":"o-55","state":"new"}` {
		t.Fatalf("the read in CI: ok %v diag %+v", ok, diag)
	}

	ci = &varDb{perTest: written()}
	if ok, _, _ := matchVar(t, h, ci, live("POST", "/v1/orders", `{"order":"o-55"}`, nil)); !ok {
		t.Fatal("the create in CI")
	}
	ok, _, diag = matchVar(t, h, ci, live("GET", "/v1/orders/o-56", "", nil))
	if ok || diag == nil || diag.phase != models.MatchPhaseVariable {
		t.Fatalf("a read of another id is a variable miss: ok %v diag %+v", ok, diag)
	}
}

// Only a test's own calls carry its variables: a value a pool call sent is
// refused where a later call uses it, never learned as noise.
func TestLearnMatchAVariableFromThePoolIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	pool := learnRecorded("pool-0", 0, "POST", "/v1/sessions", `{"sid":"s-1"}`, nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}, perTest: []*models.Mock{
		learnRecorded("mock-1", time.Second, "GET", "/v1/sessions/s-1", "", nil, `{}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/sessions", `{"sid":"s-9"}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the pool call is paired: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/sessions/s-9", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "only a test's own calls carry its variables") {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// Leaves of one call that drifted from one recorded value, echoed by its
// answer, are one variable: bound at the first, used at the others (the
// variable pass refuses one value bound as two variables).
func TestLearnMatchOneValueIsOneEcho(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","again":"k-1"}`, map[string]string{"Idempotency-Key": "k-1"}, `{"key":"k-1"}`)}}
	served, miss, _ := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","again":"k-2"}`, map[string]string{"Idempotency-Key": "k-2"}), db, n, opts)
	if served == nil || miss != nil || served.Spec.HTTPResp.Body != `{"key":"k-2"}` {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	vars := db.consumed[0].Spec.Learned()
	if len(vars) != 1 {
		t.Fatalf("variables %+v: one value, one variable", vars)
	}
	for _, v := range vars {
		if !v.Echo || v.Bind == "" || len(v.Use) != 2 || !reflect.DeepEqual(v.Answer, []string{"body.key"}) {
			t.Fatalf("variable %+v", v)
		}
		if _, problem := readDecls(&models.Mock{Name: "mock-0", Spec: models.MockSpec{Variables: vars}}); problem != "" {
			t.Fatalf("the variable pass cannot read it: %s", problem)
		}
	}
}

// racingDb consumes the producer, as a concurrent connection's claim would,
// the first time the consumer's request reads its scope, and tells the
// tracker as the mock manager does (before the mock leaves the pool).
type racingDb struct {
	*learnDb
	tr       *learn.Tracker
	producer string
	fired    bool
}

func (d *racingDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	if !d.fired {
		d.fired = true
		for _, m := range d.perTest {
			if m.Name == d.producer {
				cp := *m
				d.tr.ServedMock(&cp)
				d.learnDb.DeleteFilteredMock(cp)
				break
			}
		}
	}
	return d.learnDb.GetPerTestMocksInWindow()
}

// A value the app sends to two dependencies at once is a variable when one
// was recorded first: the earlier call consumed by a concurrent connection
// while this request starts is still an earlier place, so the value is
// refused (request_changed) rather than learned as noise (the reviewer's
// round-2 repro).
func TestLearnMatchAProducerClaimedConcurrentlyIsStillEarlier(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	base := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-a", 0, "POST", "/v1/a", `{"id":"x-1111"}`, nil, `{}`),
		learnRecorded("mock-b", time.Millisecond, "POST", "/v1/b", `{"id":"x-1111"}`, nil, `{}`),
	}}
	db := &racingDb{learnDb: base, tr: tr, producer: "mock-a"}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/b", `{"id":"x-2222"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v err %v: a value recorded earlier in mock-a must not be learned as noise on mock-b", served, miss, err)
	}
}

// A place F saw hold its recorded value did not drift, so nothing that
// drifted came from it: a drifting timestamp is noise even when a word of it
// ("00") is in an earlier answer's Date header.
func TestLearnMatchAnEarlierPlaceThatDidNotDriftIsNoSource(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	first := learnRecorded("mock-0", 0, "GET", "/v1/ping", "", nil, `{"ok":true}`)
	first.Spec.HTTPResp.Header["Date"] = "Tue, 14 Oct 2026 09:59:00 GMT"
	second := learnRecorded("mock-1", time.Second, "POST", "/v1/events", `{"at":"2026-10-14T10:00:00Z","kind":"x"}`, nil, `{}`)
	db := &learnDb{perTest: []*models.Mock{first, second}}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/ping", "", nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("first: %+v", miss)
	}
	tr.ServedMock(first) // as the mock manager tells it
	// F runs the test learn.FullRoundLead past its recording.
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/events", `{"at":"2026-10-14T10:01:01Z","kind":"x"}`, nil), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("a drifting timestamp was refused: %+v", miss)
	}
	if !reflect.DeepEqual(db.consumed[1].Spec.ReqBodyNoise, map[string][]string{"body.at": {}}) {
		t.Fatalf("noise %v", db.consumed[1].Spec.ReqBodyNoise)
	}
}

// A closer recording L may not learn onto (the pool's, in a partial record)
// is still the closest: the request is a miss, never paired with a farther
// own mock whose extra drift F never saw.
func TestLearnMatchACloserRecordingItMayNotPairIsAMiss(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	pool := learnRecorded("pool-0", 0, "GET", "/v1/flags?user=alice&ts=1", "", nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	own := learnRecorded("mock-1", time.Second, "GET", "/v1/flags?user=bob&ts=2", "", nil, `{}`)
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}, perTest: []*models.Mock{own}}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/flags?user=alice&ts=9", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || miss.closest == nil || miss.closest.Name != "pool-0" {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// Outside every test a per-test mock is no one's to pair, even in a full
// record (between tests every test's mocks are in the window).
func TestLearnMatchBetweenTestsAPerTestMockIsNotPaired(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.Begin()
	tr.End()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"kind":"order","id":"x"}`, nil, `{}`)}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"kind":"refund","id":"x"}`, nil), db, n, opts); served != nil || miss == nil {
		t.Fatalf("a per-test mock was paired outside every test: served %v", served)
	}
}

// endingDb is a learnDb whose test ends while a call reads its scope: after
// the tracker said the test was open, before the call took the test's own
// mock (learn.Tracker.Answer).
type endingDb struct {
	*learnDb
	end  func()
	once sync.Once
}

func (d *endingDb) InRecordedTestWindow(t time.Time) (bool, bool) {
	d.once.Do(d.end)
	return d.learnDb.InRecordedTestWindow(t)
}

// A call of a test that ended before it took the test's own mock is a call of
// the gap: no test is open, so, as main's rule for the gap says, it answers
// on an exact match only, and never as the ended test's own. One that drifted
// from the recording is refused, and nothing is learned onto it.
func TestLearnMatchALateCallOfAnEndedTestAnswersOnAnExactMatchOnly(t *testing.T) {
	for _, drifted := range []bool{false, true} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &endingDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"kind":"order","id":"x"}`, nil, `{}`)}}, end: tr.End}
		body := `{"kind":"order","id":"x"}`
		if drifted {
			body = `{"kind":"order","id":"y"}`
		}
		served, miss, err := h.learnMatch(ctx, live("POST", "/a", body, nil), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Open() {
			t.Fatal("precondition: the test did not end while the call read its scope")
		}
		if tr.Answered("mock-0") {
			t.Fatalf("drifted %v: a call made after its test ended was answered as that test's own", drifted)
		}
		if drifted {
			if served != nil || miss == nil {
				t.Fatalf("a drifted call of an ended test was paired: served %v", served != nil)
			}
			if len(db.consumed) != 0 {
				t.Fatalf("a refused call of an ended test consumed its recording, noise %v", db.consumed[0].Spec.ReqBodyNoise)
			}
			continue
		}
		if served == nil || miss != nil {
			t.Fatalf("a call of an ended test that matches its recording exactly was not answered: %+v", miss)
		}
	}
}

// Two leaves of one call recorded with one value that F saw drift to two
// values are not one echo: refused, never served.
func TestLearnMatchAnEchoOfTwoValuesIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 0, "POST", "/pay", `{"a":"k-1111","b":"k-1111"}`, nil, `{"key":"k-1111"}`)}}
	served, miss, _ := h.learnMatch(context.Background(), live("POST", "/pay", `{"a":"k-2222","b":"k-3333"}`, nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// learnedOnto is what the replay writes after F (mockdb's mergeLearned, for
// these cases): each consumed mock's noise and variables onto its recording,
// and binds onto the mock they are bound at, an echo turned into a bind.
func learnedOnto(recs []*models.Mock, consumed []models.Mock) []*models.Mock {
	byName := map[string]*models.Mock{}
	var out []*models.Mock
	for _, r := range recs {
		cp := *r
		cp.Spec.Variables, cp.Spec.ReqBodyNoise = nil, nil
		byName[cp.Name] = &cp
		out = append(out, &cp)
	}
	vars := func(m *models.Mock) models.MockVariables {
		if m.Spec.Variables == nil {
			m.Spec.Variables = models.MockVariables{}
		}
		return m.Spec.Variables
	}
	for _, c := range consumed {
		m := byName[c.Name]
		m.Spec.ReqBodyNoise = mergeReqBodyNoise(m.Spec.ReqBodyNoise, c.Spec.ReqBodyNoise)
		for n, v := range c.Spec.Learned() {
			vars(m)[n] = v
		}
	}
	for _, c := range consumed {
		for target, vs := range c.Spec.LearnedBinds() {
			m := byName[target]
			for n, b := range vs {
				cur, ok := vars(m)[n]
				if ok {
					cur.Echo = cur.Echo && b.Echo
					if cur.Bind == "" {
						cur.Bind = b.Bind
					}
					b = cur
				}
				vars(m)[n] = b
				if leaf, ok := learn.LeafPath(b.Bind); ok {
					delete(m.Spec.ReqBodyNoise, leaf)
				}
			}
		}
	}
	return out
}

// fullRound runs calls through the learn step against recs and returns the
// consumed mocks.
func fullRound(t *testing.T, recs []*models.Mock, calls []*req) []models.Mock {
	t.Helper()
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	var per []*models.Mock
	for _, r := range recs {
		cp := *r
		per = append(per, &cp)
	}
	// The mock manager tells the tracker of each mock a claim takes.
	db := &learnDb{perTest: per, observe: tr.ServedMock}
	for i, c := range calls {
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || served == nil {
			t.Fatalf("F call %d not served: %+v %v", i, miss, err)
		}
	}
	return db.consumed
}

// inCI replays calls with the variable pass against mocks; every one must be
// answered.
func inCI(t *testing.T, mocks []*models.Mock, calls []*req) {
	t.Helper()
	h := newHTTP()
	db := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	for i, c := range calls {
		if ok, _, diag := matchVar(t, h, db, c); !ok {
			t.Fatalf("CI call %d is a miss after F learned it: %+v", i, diag)
		}
	}
}

// A call that gets a variable and also noise outside its body scalars (a
// query value, a path segment, one array element) replays: the variable
// pass honours what the learn step learned at each exact place.
func TestWhatFLearnsReplaysWithNoiseBesideAVariable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recs       []*models.Mock
		f, ci      []*req
		wantNoise  string
		wantOnMock string
	}{
		{
			"a query value",
			[]*models.Mock{
				learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
				learnRecorded("mock-2", time.Second, "GET", "/orders/o-1111?ts=100", "", nil, `{"id":"o-1111","state":"new"}`),
			},
			[]*req{live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), live("GET", "/orders/o-9999?ts=200", "", nil)},
			[]*req{live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil), live("GET", "/orders/o-5555?ts=300", "", nil)},
			"url.query.ts", "mock-2",
		},
		{
			"a path segment",
			[]*models.Mock{
				learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
				learnRecorded("mock-2", time.Second, "GET", "/orders/o-1111/v/100", "", nil, `{"id":"o-1111","state":"new"}`),
			},
			[]*req{live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), live("GET", "/orders/o-9999/v/200", "", nil)},
			[]*req{live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil), live("GET", "/orders/o-5555/v/300", "", nil)},
			"url.path.3", "mock-2",
		},
		{
			"one array element",
			[]*models.Mock{learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","items":[{"at":"t-1"}]}`, nil, `{"id":"o-1111"}`)},
			[]*req{live("POST", "/orders", `{"id":"o-9999","items":[{"at":"t-2"}]}`, nil)},
			[]*req{live("POST", "/orders", `{"id":"o-5555","items":[{"at":"t-3"}]}`, nil)},
			"body.items[0].at", "mock-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumed := fullRound(t, tc.recs, tc.f)
			mocks := learnedOnto(tc.recs, consumed)
			for _, m := range mocks {
				if m.Name == tc.wantOnMock {
					if _, ok := m.Spec.ReqBodyNoise[tc.wantNoise]; !ok || len(m.Spec.Variables) == 0 {
						t.Fatalf("%s: noise %v variables %v", m.Name, m.Spec.ReqBodyNoise, m.Spec.Variables)
					}
				}
			}
			inCI(t, mocks, tc.ci)
		})
	}
}

// givenDb is a learnDb whose test case gave values (keploy test's template
// keys, #4789), bound in its variable table.
type givenDb struct {
	*learnDb
	table variables.Table
}

func (d *givenDb) Variables() *variables.Table { return &d.table }

// A value the test case's request gave is its key's variable: the call that
// carries it uses the key, with no bind, and is answered with this run's
// value. A value merely recorded equal to a key's is not the key's, and no
// other variable takes the key's name.
func TestLearnMatchAValueTheTestCaseGaveIsItsKey(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "GET", "/v1/users/u-1", "", nil, `{"id":"u-1","name":"a"}`)}}}
	db.table.Start([]variables.Binding{{Name: "userId", Value: "u-9", Recorded: "u-1", Mock: "test case get-user-1", Place: "url.path[2]", Given: true}})
	served, miss, _ := h.learnMatch(context.Background(), live("GET", "/v1/users/u-9", "", nil), db, n, opts)
	if served == nil || miss != nil || served.Spec.HTTPResp.Body != `{"id":"u-9","name":"a"}` {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if want := (models.MockVariables{"userId": {Recorded: "u-1", Use: []string{"url.path[2]"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(db.consumed[0].Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", db.consumed[0].Spec.Learned(), want)
	}

	// Recorded equal to the key's value, but another value now: an echo of
	// its own, which does not take the key's name.
	db = &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"1"}`, nil, `{"id":"1"}`)}}}
	db.table.Start([]variables.Binding{{Name: "id", Value: "7", Recorded: "1", Mock: "test case create-1", Place: "body.id", Given: true}})
	served, miss, _ = h.learnMatch(context.Background(), live("POST", "/v1/orders", `{"id":"5"}`, nil), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	got := db.consumed[0].Spec.Learned()
	if _, took := got["id"]; took || len(got) != 1 || got["id-2"].Echo != true {
		t.Fatalf("variables %+v: the order's own echo, not the key's name", got)
	}
}

// (Round-4 review repro.) the app mints a session token, stores it with a SQL INSERT (one
// query text, as the MySQL/generic recorders keep it), then sends it as a
// bearer token. In F the token is minted again, so the bearer header drifts.
// Its token flowed from an earlier call of the test: design §7.2 step 6/8
// make it a variable (here: refused, since a raw place cannot bind). Since
// 00b3ca812 the word is searched in another protocol's payload only as a
// WHOLE scalar, so it is not found and the credential is learned as NOISE:
// CI never compares the Authorization header again.
func TestLearnMatchACredentialTokenFromASQLTextIsNotNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "mysql-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8",
			Data: "INSERT INTO sessions(token, user_id) VALUES ('tok-7f3a9c', 42)"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "GET", "/v1/me", "",
		map[string]string{"Authorization": "Bearer tok-7f3a9c"}, `{"ok":true}`)}}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-0b81e2"}), db, n, opts)
	if served != nil {
		t.Fatalf("a bearer token the app stored earlier in the test was learned, not refused: noise %v",
			db.consumed[0].Spec.ReqBodyNoise)
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// (Round-4 review repro.) the previous test inserted a row stamped with the second it ran in;
// this test's own call carries its own clock read, the same second when
// recorded. Nothing flowed, so in F the drifted value is noise; with the
// round-3 `before` raw places it is refused (request_changed), and the test
// is quarantined as flaky.
func TestLearnMatchAPreviousTestsSameSecondIsNoSource(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin() // test 1
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8",
			Data: "INSERT INTO audit(at, what) VALUES (1760436000, 'created')"}}}}}})
	tr.Begin() // test 2
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/ping", `{"at":1760436000}`, nil, `{}`)},
		otherTest: map[time.Time]bool{learnAt: true}} // pg-0 was recorded in test 1
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/ping", `{"at":1760436093}`, nil), db, n, opts)
	if served == nil {
		t.Fatalf("the previous test's same-second row refused this test's own clock read: %+v", miss)
	}
}

// (Round-4 review repro.) an excused header (x-ms-date, sent by every Azure SDK call) is now
// an earlier place (withExcused). A later drifted timestamp's word "00" is a
// token of it, so the timestamp is refused as "inside a longer value": the
// round-2 Date-header false refusal, back through the excused headers.
func TestLearnMatchAnExcusedDateHeaderIsNoSourceOfAClockRead(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "GET", "/blob/a", "", map[string]string{"X-Ms-Date": "Wed, 14 Oct 2026 10:00:00 GMT"}, `{}`),
		learnRecorded("mock-1", time.Second, "POST", "/v1/ping", `{"at":"2026-10-14T10:00:00Z"}`, nil, `{}`),
	}}
	// F runs the test learn.FullRoundLead past its recording: both clock
	// reads moved by it.
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/blob/a", "", map[string]string{"X-Ms-Date": "Wed, 14 Oct 2026 10:01:01 GMT"}), db, n, opts); served == nil {
		t.Fatalf("precondition: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/ping", `{"at":"2026-10-14T10:01:02Z"}`, nil), db, n, opts)
	if served == nil {
		t.Fatalf("a clock read was refused because of an earlier excused date header: %+v", miss.res)
	}
}

// (Round-5 review repro.) the app mints a document id, sends it whole, then
// uploads the document under "<id>.json" (or a cache key "doc:<id>", or
// "doc-<id>"). In F the id is minted again, so both drift together. The
// later call's value flowed from the earlier call, inside a longer value:
// refused (request_changed), never learned as noise.
func TestLearnMatchAFlowedIdInsideALongerValueIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, recID, liveID, prefix, suffix string }{
		{"dot", "7c9e6679-7425-40de-944b-e07fc1f90ae7", "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "", ".json"},
		{"colon", "7c9e6679-7425-40de-944b-e07fc1f90ae7", "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "doc:", ""},
		{"dash", "7c9e6679", "1b4e28ba", "doc-", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/docs", `{"id":"`+tc.recID+`"}`, nil, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "PUT", "/bucket/"+tc.prefix+tc.recID+tc.suffix, `{"v":1}`, nil, `{}`),
			}}
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/docs", `{"id":"`+tc.liveID+`"}`, nil), db, n, opts); served == nil {
				t.Fatalf("precondition: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, live("PUT", "/bucket/"+tc.prefix+tc.liveID+tc.suffix, `{"v":1}`, nil), db, n, opts)
			if served != nil {
				t.Fatalf("the id that flowed from mock-0 was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "inside a longer value") {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-5 review repro.) a trace id the app sends in traceparent (a header
// the comparison excuses) and then in a later call's body: it flowed, part
// of the header, so the body field is refused, not noise. The other way
// round too: a whole id sent first, then inside a longer value with a part
// minted fresh in the later call.
func TestLearnMatchAPartOfAnEarlierValueIsRefused(t *testing.T) {
	rec, now := "4bf92f3577b34da6a3ce929d0e0e4736", "0af7651916cd43dd8448eb211c80319c"
	for _, tc := range []struct {
		name         string
		first        *models.Mock
		firstLive    *req
		second       *models.Mock
		secondLive   *req
		secondAnswer string
	}{
		{
			name:       "a part of an excused header",
			first:      learnRecorded("mock-0", 0, "GET", "/v1/a", "", map[string]string{"Traceparent": "00-" + rec + "-00f067aa0ba902b7-01"}, `{}`),
			firstLive:  live("GET", "/v1/a", "", map[string]string{"Traceparent": "00-" + now + "-b7ad6b7169203331-01"}),
			second:     learnRecorded("mock-1", time.Second, "POST", "/v1/audit", `{"trace_id":"`+rec+`"}`, nil, `{}`),
			secondLive: live("POST", "/v1/audit", `{"trace_id":"`+now+`"}`, nil),
		},
		{
			name:       "a whole value, then a part of a longer one",
			first:      learnRecorded("mock-0", 0, "POST", "/v1/a", `{"trace":"`+rec+`"}`, nil, `{}`),
			firstLive:  live("POST", "/v1/a", `{"trace":"`+now+`"}`, nil),
			second:     learnRecorded("mock-1", time.Second, "POST", "/v1/audit", `{"ctx":"00-`+rec+`-00f067aa0ba902b7-01"}`, nil, `{}`),
			secondLive: live("POST", "/v1/audit", `{"ctx":"00-`+now+`-b7ad6b7169203331-01"}`, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{tc.first, tc.second}}
			if served, miss, _ := h.learnMatch(ctx, tc.firstLive, db, n, opts); served == nil {
				t.Fatalf("precondition: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, tc.secondLive, db, n, opts)
			if served != nil {
				t.Fatalf("the trace id the app sent earlier was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-5 review repro.) keploy.yml's url noise masks the id in the earlier
// call's path for the comparison only: the id is still a value the app sent
// there, so the later call's body field that carries it is the variable
// bound at that path segment, never noise.
func TestLearnMatchAnIdInAURLNoisePlaceIsStillASource(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	opts := models.OutgoingOptions{Learn: true}
	n := h.newLearnNoise(opts, nil, []string{`o-[0-9]+`})
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "PUT", "/v1/orders/o-1", `{"state":"new"}`, nil, `{}`),
		learnRecorded("mock-1", time.Second, "POST", "/v1/ship", `{"order":"o-1"}`, nil, `{}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("PUT", "/v1/orders/o-9", `{"state":"new"}`, nil), db, n, opts); served == nil {
		t.Fatalf("precondition: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/ship", `{"order":"o-9"}`, nil), db, n, opts)
	if served == nil {
		t.Fatalf("miss %+v", miss)
	}
	got := db.consumed[1].Spec
	if _, noise := got.ReqBodyNoise["body.order"]; noise {
		t.Fatalf("the id from mock-0's url-noise path was learned as noise: %v", got.ReqBodyNoise)
	}
	vars := got.Learned()
	if len(vars) != 1 {
		t.Fatalf("variables %+v", vars)
	}
	for name, v := range vars {
		if v.Recorded != "o-1" || len(v.Use) != 1 || v.Use[0] != "body.order" || got.LearnedBinds()["mock-0"][name].Bind != "url.path[2]" {
			t.Fatalf("variable %s %+v, binds %+v", name, v, got.LearnedBinds())
		}
	}
}

// A mock of another recorded test's window is no earlier place of the paired
// mock's test; the pool's (recorded outside every window) and the paired
// test's own are, and with no recorded windows every one is.
func TestOfPairedTest(t *testing.T) {
	pool, own, other := learnAt, learnAt.Add(time.Second), learnAt.Add(2*time.Second)
	at := func(name string, t time.Time) *models.Mock {
		return &models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: t}}
	}
	db := &learnDb{mockMemDb: mockMemDb{}, otherTest: map[time.Time]bool{other: true}}
	db.mocks = []*models.Mock{at("pool", pool)}
	paired := at("paired", own.Add(time.Millisecond))
	got := ofPairedTest([]*models.Mock{at("pool", pool), at("own", own), at("other", other), nil}, paired, db, true)
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "pool,own" {
		t.Fatalf("kept %v", names)
	}
	if len(ofPairedTest([]*models.Mock{at("other", other)}, paired, nil, true)) != 1 {
		t.Fatal("with no recorded windows every mock is kept")
	}
	// A paired mock of the pool's may carry a value of any test's, outside
	// every window or inside one.
	if got := ofPairedTest([]*models.Mock{at("own", own), at("other", other)}, at("pool", pool), db, true); len(got) != 2 {
		t.Fatalf("a mock outside every window kept %d of 2 earlier places", len(got))
	}
	if got := ofPairedTest([]*models.Mock{at("own", own), at("other", other)}, paired, db, false); len(got) != 2 {
		t.Fatalf("a pool mock inside a window kept %d of 2 earlier places", len(got))
	}
}

// poolView is a view that scopes the pool to a test and says each mock it
// knows is the pool's, not the open test's.
type poolView struct{ *recordedBeforeDb }

func (poolView) OwnedByTest(*models.Mock) (bool, bool) { return false, true }

// A pool mock the store places inside a recorded test's window (a view says
// it is the pool's) may carry a value from any recording made before it, as
// one outside every window may: a token another test's login answered, kept
// on disk, is refused, not learned as noise.
func TestLearnMatchAPoolMockInsideAWindowReadsEveryEarlierRecording(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
	refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
	db := poolView{&recordedBeforeDb{learnDb: &learnDb{perTest: []*models.Mock{refresh}, otherTest: map[time.Time]bool{login.Spec.ReqTimestampMock: true}}, onDisk: []*models.Mock{login}}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "holds earlier (in mock-0)") {
		t.Fatalf("served %v miss %+v err %v; want refused, the token another test's login answered", served, miss, err)
	}
}

// At start-up a pool mock recorded between tests can be paired in a full
// record; a value it was recorded with that a test's own mock answered
// before it flowed from there, and is refused, not learned as noise.
func TestLearnMatchAPoolMocksValueFromATestIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	ctx := learn.WithTracker(context.Background(), learn.NewTracker()) // no test has begun
	login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
	refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
	refresh.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{refresh}}, staged: []*models.Mock{login}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "holds earlier (in mock-0)") {
		t.Fatalf("served %v miss %+v err %v pooled %+v; want refused, the token answered earlier by mock-0", served, miss, err, db.pooled)
	}
}

// (Round-6 review repro.) An app traced with Jaeger (uber-trace-id:
// {trace}:{span}:{parent}:{flags}) or Elastic APM (elastic-apm-traceparent:
// 00-{trace}-{span}-01) mints a trace id per test and sends it on every
// outgoing call, each call with its own fresh span id. Neither header is on
// the static noise list, so both are learnable leaves. In F the trace id and
// every span id are minted again. The trace id in call 2's header flowed from
// call 1's header, but in both values it sits next to a fresh span id, so the
// run of changed words is "<trace>:<span2>" here and "<trace>:<span1>"
// there: neither is a token of the other, and the second call's header is
// learned as NOISE. A later build that stops propagating the trace (a lost
// context) passes CI.
func TestLearnMatchATraceIdNextToAFreshSpanIsRefused(t *testing.T) {
	T1, T2 := "4bf92f3577b34da6a3ce929d0e0e4736", "0af7651916cd43dd8448eb211c80319c"
	for _, tc := range []struct{ name, header, rec1, rec2, live1, live2 string }{
		{"jaeger", "Uber-Trace-Id",
			T1 + ":00f067aa0ba902b7:a1b2c3d4e5f60718:1", T1 + ":11f067aa0ba902b8:a1b2c3d4e5f60718:1",
			T2 + ":b7ad6b7169203331:0102030405060708:1", T2 + ":c7ad6b7169203332:0102030405060708:1"},
		{"elastic", "Elastic-Apm-Traceparent",
			"00-" + T1 + "-00f067aa0ba902b7-01", "00-" + T1 + "-11f067aa0ba902b8-01",
			"00-" + T2 + "-b7ad6b7169203331-01", "00-" + T2 + "-c7ad6b7169203332-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "GET", "/v1/users/7", "", map[string]string{tc.header: tc.rec1}, `{"id":7}`),
				learnRecorded("mock-1", time.Second, "GET", "/v1/orders/7", "", map[string]string{tc.header: tc.rec2}, `{"orders":[]}`),
			}}
			if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/users/7", "", map[string]string{tc.header: tc.live1}), db, n, opts); served == nil {
				t.Fatalf("precondition (call 1 mints the trace: noise): %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders/7", "", map[string]string{tc.header: tc.live2}), db, n, opts)
			if served != nil {
				t.Fatalf("the trace id that flowed from call 1 was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-6 review repro.) two clock reads of one second, one with
// milliseconds: an Azure call's x-ms-date (excused, RFC 1123) and then a
// document's createdAt (JS toISOString). In F both move by the shift. They
// share a minute and a second, not a run nor a long word: the later value is
// noise, not refused as a part of the header.
func TestLearnMatchAClockReadWithMillisAfterADateHeaderIsNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "PUT", "/container/blob1", `{"v":1}`, map[string]string{"X-Ms-Date": "Wed, 14 Oct 2026 10:00:41 GMT"}, `{}`),
		learnRecorded("mock-1", 200*time.Millisecond, "POST", "/dbs/app/docs", `{"createdAt":"2026-10-14T10:00:41.456Z"}`, nil, `{}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("PUT", "/container/blob1", `{"v":1}`, map[string]string{"X-Ms-Date": "Wed, 14 Oct 2026 10:01:42 GMT"}), db, n, opts); served == nil {
		t.Fatalf("precondition: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/dbs/app/docs", `{"createdAt":"2026-10-14T10:01:42.956Z"}`, nil), db, n, opts)
	if served == nil {
		t.Fatalf("a clock read with millis was refused after a same-second date header: %+v", miss.res)
	}
}

// (Round-7 review repro.) a value the app mints with no ASCII letter or
// digit (an emoji, a CJK or Cyrillic word) is sent to one service, then
// flows to a second call. Text of a script keploy does not split into words
// is outside the declared readings: it is refused where it drifts, never
// learned as noise, so a regression in it cannot pass.
func TestLearnMatchAValueBeyondASCIIIsRefusedNotNoise(t *testing.T) {
	for _, tc := range []struct{ name, rec, live string }{
		{"emoji", "🦊", "🐼"},
		{"cjk", "ゆき", "はな"},
		{"cyrillic", "лиса", "панда"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/profiles", `{"nick":"`+tc.rec+`"}`, nil, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/notify", `{"to":"`+tc.rec+`"}`, nil, `{}`),
			}}
			for _, call := range []*req{live("POST", "/v1/profiles", `{"nick":"`+tc.live+`"}`, nil), live("POST", "/v1/notify", `{"to":"`+tc.live+`"}`, nil)} {
				served, miss, _ := h.learnMatch(ctx, call, db, n, opts)
				if served != nil {
					t.Fatalf("a value beyond ASCII was learned: noise %v", db.consumed[len(db.consumed)-1].Spec.ReqBodyNoise)
				}
				if miss == nil || miss.res.Reason != models.MissRequestChanged {
					t.Fatalf("miss %+v", miss)
				}
			}
		})
	}
}

// (Round-8 review repro.) the app mints an order ref (uuid4) and registers
// two callback URLs with it, each signed with a fresh standard-base64 HMAC
// ('+', '/', '=' inside). A standard base64 token splits into another number
// of words run to run, so the values' words do not line up; the ref is
// still a word both lost and both gained: refused, never noise.
func TestLearnMatchARefNextToAFreshBase64SignatureIsRefused(t *testing.T) {
	U1, U2 := "3f2b8c1e-9a4d-4e7b-8c21-5d6e7f8a9b0c", "a7c4e2d1-0b3f-4a8e-9d12-6f5e4d3c2b1a"
	S1, S2 := "q3+/Zx9Lk2mN8pQ4rS7tU1vW5xY0zA6bC+dE/fG3hI8=", "Hj2Kl/9Mn4+Op6Qr8St0Uv/Wx1Yz3Ab5Cd7Ef+Gh9Ij="
	S3, S4 := "Zz9/Yy8+Xx7Ww6Vv5Uu4Tt3Ss2Rr1Qq0Pp/Oo+Nn9Mm=", "Kk1Ll2Mm3Nn4Oo5Pp6Qq7Rr8Ss9Tt0Uu/Vv1Ww2Xx3Y="
	cb := func(ref, sig string) string { return "https://app.example/cb?ref=" + ref + "&sig=" + sig }
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/intents", `{"callback":"`+cb(U1, S1)+`"}`, nil, `{"ok":true}`),
		learnRecorded("mock-1", time.Second, "POST", "/v1/hooks", `{"url":"`+cb(U1, S2)+`"}`, nil, `{"ok":true}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/intents", `{"callback":"`+cb(U2, S3)+`"}`, nil), db, n, opts); served == nil {
		t.Fatalf("precondition (call 1 mints the ref: noise): %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/hooks", `{"url":"`+cb(U2, S4)+`"}`, nil), db, n, opts)
	if served != nil {
		t.Fatalf("the ref that flowed from call 1 was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// (Round-9 review repro.) the app mints an object id (hex, as uuid4().hex or
// randomBytes(16).toString('hex')), uploads to GCS with it in the query
// (name=uploads/<id>.png: decoded, the id is a token), then hands the
// transcoder the object's URL, built with encodeURIComponent(name): the id
// sits right after "%2F", so "2F<id>" is one word in every reading. Neither
// value holds the other's piece as a token and no word is lost or gained by
// both: the job's input is learned as NOISE although the id flowed from the
// upload. A later build that hands the transcoder another object passes CI.
func TestLearnMatchAnIdAfterAPercentEscapeIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, id1, id2 string }{
		{"hex32", "3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c", "a7c4e2d10b3f4a8e9d126f5e4d3c2b1a"},
		{"alnum", "A7K29QXM", "Z3M81XPD"},
		{"numeric", "184467201", "551209873"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			job := func(id string) string {
				return `{"input":"https://storage.googleapis.com/storage/v1/b/bkt/o/uploads%2F` + id + `.png?alt=media"}`
			}
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/upload/storage/v1/b/bkt/o?uploadType=media&name=uploads/"+tc.id1+".png", `{}`, nil, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/jobs", job(tc.id1), nil, `{"ok":true}`),
			}}
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/upload/storage/v1/b/bkt/o?uploadType=media&name=uploads/"+tc.id2+".png", `{}`, nil), db, n, opts); served == nil {
				t.Fatalf("precondition (the upload mints the id: noise): %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/jobs", job(tc.id2), nil), db, n, opts)
			if served != nil {
				t.Fatalf("the object id that flowed from the upload was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-9 review repro.) the app mints a session token and stores it in Redis
// (SET sess:<user> <token>, framed in RESP as the generic and Redis
// recorders keep it: "$<len>\r\n<token>\r\n"), then sends it as a bearer
// token. RawOf JSON-marshals the payload, so every RESP argument follows a
// literal "\n" escape: "nTOKEN" is one word and the token is never a token
// of the text. The credential is learned as NOISE.
func TestLearnMatchATokenStoredInRedisIsRefused(t *testing.T) {
	for _, tok := range []string{"7f3a9c1e5b2d4a60", "A7K29QXM", "184467201"} {
		t.Run(tok, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			resp := "*3\r\n$3\r\nSET\r\n$12\r\nsess:user-42\r\n$" + strconv.Itoa(len(tok)) + "\r\n" + tok + "\r\n"
			tr.ServedMock(&models.Mock{Name: "redis-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
				GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: resp}}}}}})
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "GET", "/v1/me", "",
				map[string]string{"Authorization": "Bearer " + tok}, `{"ok":true}`)}}
			served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/me", "", map[string]string{"Authorization": "Bearer Zq81Lm0Xw2"}), db, n, opts)
			if served != nil {
				t.Fatalf("a bearer token the app stored in Redis earlier in the test was learned: noise %v (raw text %s)",
					db.consumed[0].Spec.ReqBodyNoise, learn.RawOf(&models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: resp}}}}}}, true).Text)
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-9 review repro, founder decision.) the app reads its clock once,
// INSERTs the epoch second as an interpolated literal (MySQL text protocol,
// PDO emulated prepares, pymysql) and sends the same value whole to a
// webhook. The value stands whole in the query ("(1760436041,42)": ",42" is
// the next column), so it is a whole repeated value that cannot be bound:
// refused. containsWholeClock reads ",4" as a decimal-comma fraction and the
// webhook's ts is learned as NOISE.
func TestLearnMatchAWholeEpochInsertedNextToAColumnIsRefused(t *testing.T) {
	for _, q := range []string{
		"INSERT INTO events (ts,user_id) VALUES (1760436041,42)",
		"INSERT INTO events (ts, user_id) VALUES (1760436041, 42)",
	} {
		t.Run(q, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			tr.ServedMock(&models.Mock{Name: "mysql-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
				GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: q}}}}}})
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/hooks", `{"ts":1760436041,"user":42}`, nil, `{"ok":true}`)}}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/hooks", `{"ts":1760436102,"user":42}`, nil), db, n, opts)
			if served != nil {
				t.Fatalf("a clock value stored whole and sent whole was learned: noise %v", db.consumed[0].Spec.ReqBodyNoise)
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-10 review repro.) the app mints an upload id, INSERTs the object's URL (GCS
// object URLs carry the object name percent-encoded: uploads%2F<id>.png)
// before calling the API with the id. RawOf's text is not read decoded, so
// "2F<id>" is one word there: the id that flowed is learned as NOISE.
func TestLearnMatchAnIdAfterAPercentEscapeInAPayloadIsRefused(t *testing.T) {
	id1, id2 := "3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c", "a7c4e2d10b3f4a8e9d126f5e4d3c2b1a"
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	q := "INSERT INTO uploads (id, url) VALUES ($1, $2)"
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string",
			Data: q + " -- ['upl-77', 'https://storage.googleapis.com/storage/v1/b/bkt/o/uploads%2F" + id1 + ".png?alt=media']"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/transcode", `{"object":"`+id1+`"}`, nil, `{"ok":true}`)}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/transcode", `{"object":"`+id2+`"}`, nil), db, n, opts)
	if served != nil {
		t.Fatalf("the id that flowed from the INSERT (after %%2F) was learned: noise %v variables %+v", db.consumed[0].Spec.ReqBodyNoise, db.consumed[0].Spec.Learned())
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// (Round-10 review repro.) the app INSERTs an outbox event into Postgres (v3 parser: a
// jsonb bind is a map with its own keys) whose document carries the order
// ref under "metadata" (Stripe-style) and the epoch under "created", then
// POSTs the webhook with both. dropKeys drops "metadata" and "created" at
// any depth, so neither is in the raw text: both are learned as NOISE.
func TestLearnMatchAJsonbDocumentsOwnKeysAreSources(t *testing.T) {
	for _, tc := range []struct {
		name, recBody, liveBody string
		doc                     map[string]interface{}
	}{
		{"metadata", `{"ref":"A7K29QXM"}`, `{"ref":"Zq81Lm0X"}`, map[string]interface{}{"type": "order.created", "metadata": map[string]interface{}{"order_ref": "A7K29QXM"}}},
		{"created whole epoch", `{"created":1760436041}`, `{"created":1760436102}`, map[string]interface{}{"type": "order.created", "created": json.Number("1760436041")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: learnAt,
				PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
					SQLNormalized: "INSERT INTO outbox (payload) VALUES ($1)",
					BindValues:    models.PostgresV3Cells{{Value: tc.doc}},
				}}}})
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/hooks", tc.recBody, nil, `{"ok":true}`)}}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/hooks", tc.liveBody, nil), db, n, opts)
			if served != nil {
				t.Fatalf("a value the outbox INSERT carried was learned: noise %v variables %+v", db.consumed[0].Spec.ReqBodyNoise, db.consumed[0].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-10 review repro.) a magic-link email: the app mints an invite id, creates it
// (POST /v1/invites {"id":<id>}), then sends an email whose link is the
// IdP's authorize URL with redirect_uri=<cb?next=/invites/<id>>: next is
// encoded inside redirect_uri, which is encoded again inside the link, so
// the id follows "%252F". unescaped decodes once: "2F<id>" is one word:
// the email's link is learned as NOISE.
func TestLearnMatchADoublyEncodedIdIsRefused(t *testing.T) {
	id1, id2 := "A7K29QXM", "Zq81Lm0X"
	link := func(id string) string {
		return `{"html":"<a href='https://idp.example/authorize?client_id=app&redirect_uri=https%3A%2F%2Fapp.example%2Fcb%3Fnext%3D%252Finvites%252F` + id + `'>Join</a>"}`
	}
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/invites", `{"id":"`+id1+`"}`, nil, `{"ok":true}`),
		learnRecorded("mock-1", time.Second, "POST", "/v3/mail/send", link(id1), nil, `{"ok":true}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/invites", `{"id":"`+id2+`"}`, nil), db, n, opts); served == nil {
		t.Fatalf("precondition: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v3/mail/send", link(id2), nil), db, n, opts)
	if served != nil {
		t.Fatalf("the invite id that flowed into the link was learned: noise %v variables %+v", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Learned())
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// (Round-11 review repro.) the app mints an order ref, INSERTs it into
// Postgres, then searches it through Hasura/PostgREST with an _ilike
// pattern "%<ref>%", or a Windows path whose separator precedes an id that
// starts with b/f/n/r/t. A '%' or '\' may be literal: a value with escapes
// is read raw as well as decoded, so a decode that mangles it does not hide
// the ref that flowed.
func TestLearnMatchAnIdAfterALiteralPercentOrBackslashIsRefused(t *testing.T) {
	hexA, hexB := "b3f2b8c1e9a4d4e7b8c215d6e7f8a9b0", "f1e2d3c4b5a6978877665544332211ff"
	for _, tc := range []struct {
		name, id1, rec, liveBody string
	}{
		{"hasura _ilike alnum ref", "A7K29QXM", `{"query":"query($p:String!){orders(where:{ref:{_ilike:$p}}){id}}","variables":{"p":"%A7K29QXM%"}}`, `{"query":"query($p:String!){orders(where:{ref:{_ilike:$p}}){id}}","variables":{"p":"%Zq81Lm0X%"}}`},
		{"hasura _ilike hex id", hexA, `{"query":"q","variables":{"p":"%` + hexA + `%"}}`, `{"query":"q","variables":{"p":"%` + hexB + `%"}}`},
		{"windows path", hexA, `{"input":"C:\\jobs\\` + hexA + `.docx"}`, `{"input":"C:\\jobs\\` + hexB + `.docx"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
				GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string",
					Data: "INSERT INTO orders (ref, total) VALUES ('" + tc.id1 + "', 42)"}}}}}})
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/graphql", tc.rec, nil, `{"data":{"orders":[]}}`)}}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/graphql", tc.liveBody, nil), db, n, opts)
			if served != nil {
				t.Fatalf("the id that flowed from the INSERT was learned: noise %v variables %+v", db.consumed[0].Spec.ReqBodyNoise, db.consumed[0].Spec.Learned())
			}
			if miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("miss %+v", miss)
			}
		})
	}
}

// (Round-12 review repros.)
func b64Of(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// (Round-12 review repro.) the app inserts a user it names
// (Cyrillic) into Postgres, then calls an OAuth authorize endpoint with a
// base64url JSON state holding that name. The name that flowed is learned as
// noise on the authorize call.
func TestLearnMatchANameInABase64StateFromSQLIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string",
			Data: "INSERT INTO users (name) VALUES ('Иван Петров')"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	rec := b64Of(`{"name":"Иван Петров","n":1}`)
	lv := b64Of(`{"name":"Пётр Сидоров","n":1}`)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "GET", "/v1/authorize?state="+rec, "", nil, `{"ok":true}`)}}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/authorize?state="+lv, "", nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served != nil {
		t.Fatalf("the name that flowed from the INSERT was learned: noise %v", db.consumed[0].Spec.ReqBodyNoise)
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// Same, the name's earlier place an HTTP call whose answer echoed it.
func TestLearnMatchANameInABase64StateFromAnEchoIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	rec := b64Of(`{"name":"Иван Петров"}`)
	lv := b64Of(`{"name":"Пётр Сидоров"}`)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/users", `{"name":"Иван Петров"}`, nil, `{"name":"Иван Петров","id":"u1"}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/authorize?state="+rec, "", nil, `{"ok":true}`),
	}}
	// The name is text of a script keploy does not split into words: even
	// echoed, it is refused where it drifts, never learned.
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/users", `{"name":"Пётр Сидоров"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("first call: served %v miss %+v err %v", served, miss, err)
	}
	served, miss, err = h.learnMatch(ctx, live("GET", "/v1/authorize?state="+lv, "", nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served != nil {
		t.Fatalf("the echoed name that flowed into the state was learned: noise %v", db.consumed[1].Spec.ReqBodyNoise)
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// An app-minted ASCII ref, sent plainly (learned as noise: no source), then
// inside a base64url return URL that percent-encodes its slashes.
func TestLearnMatchAnIdAfterAPercentEscapeInsideBase64IsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	rec := b64Of("next=%2Forders%2FA7K29QXM&lang=en")
	lv := b64Of("next=%2Forders%2FZq81Lm0X&lang=en")
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"ref":"A7K29QXM"}`, nil, `{"ok":true}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/login?state="+rec, "", nil, `{"ok":true}`),
	}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/orders", `{"ref":"Zq81Lm0X"}`, nil), db, n, opts)
	if err != nil || served == nil {
		t.Fatalf("first call: served %v miss %+v err %v", served, miss, err)
	}
	served, miss, err = h.learnMatch(ctx, live("GET", "/v1/login?state="+lv, "", nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served != nil {
		t.Fatalf("the ref that flowed into the base64 return URL was learned: noise %v", db.consumed[1].Spec.ReqBodyNoise)
	}
	if miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("miss %+v", miss)
	}
}

// The app stores base64url("next=%2Forders%2F<ref>") through a parser of
// another protocol, then posts the ref plainly: the ref flowed, and is
// refused, not learned.
func TestLearnMatchRefusesARefThatFlowedThroughAPayloadsBase64(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string",
			Data: "INSERT INTO oauth_states (state) VALUES ('" + b64Of("next=%2Forders%2FA7K29QXM&lang=en") + "')"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/orders/confirm", `{"ref":"A7K29QXM"}`, nil, `{"ok":true}`)}, observe: tr.ServedMock}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/orders/confirm", `{"ref":"Zq81Lm0X"}`, nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// An earlier call's answer lists two ids, which the app joins in map or set
// order: in F the order differs, and the joined value is refused, every word
// of it having come from the answer.
func TestLearnMatchRefusesIdsOfAnEarlierAnswerInAnotherOrder(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "GET", "/v1/orders", "", nil, `{"items":[{"id":"ord-7Q2K"},{"id":"ord-9MX4"}]}`),
		learnRecorded("mock-1", time.Second, "POST", "/v1/ship", `{"ids":"ord-7Q2K,ord-9MX4"}`, nil, `{"ok":true}`),
	}, observe: tr.ServedMock}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders", "", nil), db, n, opts); served == nil {
		t.Fatalf("first call: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/ship", `{"ids":"ord-9MX4,ord-7Q2K"}`, nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// Two calls each carry a token minted for it (jti random, iat now): the
// header and the claims' names they share did not change, and each token is
// noise.
func TestLearnMatchLearnsTwoFreshTokensThatShareAHeader(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	tok := func(jti, iat, sig string) string {
		return "Bearer " + b64Of(`{"alg":"HS256","typ":"JWT"}`) + "." + b64Of(`{"jti":"`+jti+`","iat":`+iat+`}`) + "." + b64Of("\x01\x02"+sig+"\xfe\xff")
	}
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "GET", "/v1/a", "", map[string]string{"X-Svc-Token": tok("k3J9xQ2mW8", "1760436000", "aaa")}, `{}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/b", "", map[string]string{"X-Svc-Token": tok("Hq6Ys0Bc5D", "1760436001", "bbb")}, `{}`),
	}, observe: tr.ServedMock}
	for i, in := range []*req{
		live("GET", "/v1/a", "", map[string]string{"X-Svc-Token": tok("Lp4Zt7Vn1R", "1760436061", "ccc")}),
		live("GET", "/v1/b", "", map[string]string{"X-Svc-Token": tok("Mw2Xr8Ue3F", "1760436062", "ddd")}),
	} {
		if served, miss, _ := h.learnMatch(ctx, in, db, n, opts); served == nil {
			t.Fatalf("call %d refused: %+v", i, miss)
		}
	}
	for _, m := range db.consumed {
		if _, ok := m.Spec.ReqBodyNoise["header.x-svc-token"]; !ok {
			t.Fatalf("%s: noise %v", m.Name, m.Spec.ReqBodyNoise)
		}
	}
}

// An earlier call sent two ids in a part compared whole, which no leaf
// names (a text or XML body, a repeated query parameter, a file part); the
// later call picks one by set order. The pick is refused, as from a JSON
// body.
func TestLearnMatchAccountsForAnEarlierCallsWholeParts(t *testing.T) {
	multipart := func(file string) string {
		return "--AAA\r\nContent-Disposition: form-data; name=\"f\"; filename=\"ids.csv\"\r\n\r\n" + file + "\r\n--AAA--\r\n"
	}
	for _, tc := range []struct{ name, target, ct, body string }{
		{"an XML body", "/v1/batch", "application/xml", `<ship><id>ord-7Q2K</id><id>ord-9MX4</id></ship>`},
		{"a text body", "/v1/batch", "text/plain", "ord-7Q2K\nord-9MX4"},
		{"a repeated query parameter", "/v1/batch?id=ord-7Q2K&id=ord-9MX4", "application/json", ""},
		{"a file part", "/v1/batch", "multipart/form-data; boundary=AAA", multipart("id\nord-7Q2K\nord-9MX4\n")},
		{"a JSON body", "/v1/batch", "application/json", `{"ids":["ord-7Q2K","ord-9MX4"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			ct := map[string]string{"Content-Type": tc.ct}
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", tc.target, tc.body, ct, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/label", `{"ref":"ord-7Q2K"}`, nil, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live("POST", tc.target, tc.body, ct), db, n, opts); served == nil {
				t.Fatalf("first call: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/label", `{"ref":"ord-9MX4"}`, nil), db, n, opts)
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("served %v (noise %v) miss %+v", served, db.consumed, miss)
			}
		})
	}
}

// An id that is a key of an earlier call's JSON (an answer keyed by id, a
// request body's map) is an earlier place: picked from there in another
// order, it is refused. The same ids as values are the control.
func TestLearnMatchRefusesAnIdPickedFromAnEarlierCallsKeys(t *testing.T) {
	for _, tc := range []struct{ name, body, answer string }{
		{"an answer keyed by id", "", `{"orders":{"ord-7Q2K":{"qty":1},"ord-9MX4":{"qty":2}}}`},
		{"an answer listing ids", "", `{"orders":[{"id":"ord-7Q2K","qty":1},{"id":"ord-9MX4","qty":2}]}`},
		{"a request keyed by id", `{"qty":{"ord-7Q2K":1}}`, `{"ok":true}`},
		{"a request listing ids", `{"ids":["ord-7Q2K"]}`, `{"ok":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/orders", tc.body, nil, tc.answer),
				learnRecorded("mock-1", time.Second, "POST", "/v1/ship", `{"id":"ord-7Q2K"}`, nil, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", tc.body, nil), db, n, opts); served == nil {
				t.Fatalf("first call: %+v", miss)
			}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/ship", `{"id":"ord-9MX4"}`, nil), db, n, opts)
			if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("served %v miss %+v err %v", served, miss, err)
			}
		})
	}
}

// An id an earlier call sent, inside a Rails-signed blob ("<base64>--<digest>")
// in a header, or inside Ruby-wrapped base64 in a body, is refused.
func TestLearnMatchRefusesAnIdInsideASignedOrWrappedBlob(t *testing.T) {
	signed := func(id string) (string, string) {
		data := base64.StdEncoding.EncodeToString([]byte(`{"_rails":{"message":"` + id + `","exp":null,"pur":"x"}}`))
		sum := sha1.Sum([]byte(data))
		return "", data + "--" + hex.EncodeToString(sum[:])
	}
	wrapped := func(id string) (string, string) {
		e := base64.StdEncoding.EncodeToString([]byte(`{"note":"xxxxxxxxxxxxxxxxxxxx","ref":"` + id + `","v":1}`))
		var sb strings.Builder
		for len(e) > 60 {
			sb.WriteString(e[:60] + "\n")
			e = e[60:]
		}
		sb.WriteString(e + "\n")
		b, _ := json.Marshal(map[string]string{"payload": sb.String()})
		return string(b), ""
	}
	for _, tc := range []struct {
		name string
		mk   func(id string) (body, header string)
	}{{"a signed header", signed}, {"a wrapped body", wrapped}} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			call := func(id string) (string, map[string]string) {
				body, header := tc.mk(id)
				if header == "" {
					return body, nil
				}
				return body, map[string]string{"X-Signed": header}
			}
			recBody, recHeader := call("rAHcEraS")
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/users", `{"user":"rAHcEraS"}`, nil, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/jobs", recBody, recHeader, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/users", `{"user":"Zq81Lm0X"}`, nil), db, n, opts); served == nil {
				t.Fatalf("first call: %+v", miss)
			}
			liveBody, liveHeader := call("Zq81Lm0X")
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/jobs", liveBody, liveHeader), db, n, opts)
			if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("served %v miss %+v err %v", served, miss, err)
			}
		})
	}
}

// A request value its answer echoes at a key ({"<id>":{...}}) is an echo
// keploy cannot answer: refused, never noise.
func TestLearnMatchRefusesAnEchoAtAnAnswerKey(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/lookup", `{"id":"A7K29QXM"}`, nil, `{"A7K29QXM":{"ok":true}}`),
	}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/lookup", `{"id":"Zq81Lm0X"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
}

// An id that is a parameter name of an earlier call (a form field, a query
// parameter, a multipart field: "qty[<id>]=1" of a Rails or PHP form) is an
// earlier place: picked from there in another order, it is refused.
func TestLearnMatchRefusesAnIdPickedFromAnEarlierCallsParameterNames(t *testing.T) {
	var mp bytes.Buffer
	w := multipart.NewWriter(&mp)
	_ = w.SetBoundary("learnboundary")
	_ = w.WriteField("qty[ord-7Q2K]", "1")
	_ = w.WriteField("qty[ord-9MX4]", "2")
	_ = w.Close()
	for _, tc := range []struct{ name, method, target, body, ctype string }{
		{"form field names", "POST", "/v1/cart", "qty%5Bord-7Q2K%5D=1&qty%5Bord-9MX4%5D=2", "application/x-www-form-urlencoded"},
		{"query parameter names", "GET", "/v1/stock?qty%5Bord-7Q2K%5D=1&qty%5Bord-9MX4%5D=2", "", "application/json"},
		{"plain query names", "GET", "/v1/stock?ord-7Q2K=1&ord-9MX4=2", "", "application/json"},
		{"multipart field names", "POST", "/v1/cart", mp.String(), w.FormDataContentType()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			hdr := map[string]string{"Content-Type": tc.ctype}
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, tc.method, tc.target, tc.body, hdr, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/ship", `{"id":"ord-7Q2K"}`, nil, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live(tc.method, tc.target, tc.body, hdr), db, n, opts); served == nil {
				t.Fatalf("first call: %+v", miss)
			}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/ship", `{"id":"ord-9MX4"}`, nil), db, n, opts)
			if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("served %v miss %+v err %v", served, miss, err)
			}
		})
	}
}

// A value the answer echoes only in another form (inside a base64 cursor,
// percent-encoded, in another case) cannot be answered with this run's
// value: refused, never noise.
func TestLearnMatchRefusesAnEchoInAnotherForm(t *testing.T) {
	for _, answer := range []string{
		`{"next":"` + base64.StdEncoding.EncodeToString([]byte(`{"after":"A7K29QXMtq","n":20}`)) + `"}`,
		`{"next":"https://api.test/v1/next?u=%2Fusers%2FA7K29QXMtq"}`,
		`{"label":"USER-A7K29QXMTQ"}`,
	} {
		h, n, opts := learnOn(t)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/users", `{"id":"A7K29QXMtq"}`, nil, answer)}}
		served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/users", `{"id":"Zq81Lm0Xwe"}`, nil), db, n, opts)
		if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
			t.Fatalf("%s: served %v miss %+v err %v", answer, served, miss, err)
		}
	}
}

// An order id that flowed into a quoted-printable mail ("order=3D<id>"),
// sent base64-encoded through SES's raw message, is refused.
func TestLearnMatchRefusesAnIdInAQuotedPrintableMail(t *testing.T) {
	mime := func(id, boundary, date string) string {
		return "From: shop@test\r\nSubject: Your order\r\nDate: " + date + "\r\n" +
			"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n" +
			"--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
			"<p>Thanks!</p><a href=3D\"https://shop.test/track?order=3D" + id + "\">Track your order</a>\r\n--" + boundary + "--\r\n"
	}
	ses := func(id, boundary, date string) string {
		return `{"Content":{"Raw":{"Data":"` + base64.StdEncoding.EncodeToString([]byte(mime(id, boundary, date))) + `"}}}`
	}
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/payments", `{"order":"A7K29QXMtq","amount":20}`, nil, `{"ok":true}`),
		learnRecorded("mock-1", time.Second, "POST", "/v2/email/outbound-emails", ses("A7K29QXMtq", "b1a2c3d4e5f60718", "Wed, 14 Oct 2026 10:00:41 +0000"), nil, `{"MessageId":"m-1"}`),
	}, observe: tr.ServedMock}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/payments", `{"order":"Zq81Lm0Xwe","amount":20}`, nil), db, n, opts); served == nil {
		t.Fatalf("first call: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v2/email/outbound-emails", ses("Zq81Lm0Xwe", "9f8e7d6c5b4a3921", "Wed, 14 Oct 2026 10:01:42 +0000"), nil), db, n, opts)
	if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
}

// An id that names a header of an earlier answer (S3's x-amz-meta-<id>) is
// an earlier place: picked from there in another order, it is refused.
func TestLearnMatchRefusesAnIdPickedFromAnEarlierAnswersHeaderNames(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	head := learnRecorded("mock-0", 0, "GET", "/bucket/cart.json", "", nil, `{}`)
	head.Spec.HTTPResp.Header = map[string]string{"X-Amz-Meta-Ord-7q2k": "1", "X-Amz-Meta-Ord-9mx4": "2"}
	db := &learnDb{perTest: []*models.Mock{
		head,
		learnRecorded("mock-1", time.Second, "POST", "/v1/ship", `{"id":"ord-7q2k"}`, nil, `{"ok":true}`),
	}, observe: tr.ServedMock}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/bucket/cart.json", "", nil), db, n, opts); served == nil {
		t.Fatalf("first call: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/ship", `{"id":"ord-9mx4"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
}

// Noise learned on a SigV4 Authorization (its signature changes every run)
// excuses nothing the strict matcher compares: a later replay leaves a call
// signed in another region, for another service or over another set of
// signed headers to it, which compares those parts (founder decision 25);
// another access key, as a CI's, is not compared (SF-3) and is served.
func TestLearnedMatchLeavesASigV4CredentialToTheStrictMatcher(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := `{"id":"a-1"}`
	for _, tc := range []struct {
		name       string
		learned    bool
		auth       string
		wantServed bool
	}{
		{"learned, signed again in the same scope", true, sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), true},
		{"not learned, another access key", false, sigV4("OTHER", "20261015", "us-east-1", "s3", signed, sig(2)), true},
		{"learned, another access key", true, sigV4("OTHER", "20261015", "us-east-1", "s3", signed, sig(2)), true},
		{"learned, another region", true, sigV4("AKID", "20261015", "eu-west-1", "s3", signed, sig(2)), false},
		{"learned, another service", true, sigV4("AKID", "20261015", "us-east-1", "sqs", signed, sig(2)), false},
		{"learned, other signed headers", true, sigV4("AKID", "20261015", "us-east-1", "s3", "host;x-amz-date", sig(2)), false},
	} {
		recH := awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body))
		recH["Content-Type"] = "application/json"
		rec := recorded("put", "/bucket/key", recH, body)
		rec.Spec.HTTPReq.ProtoMajor, rec.Spec.HTTPReq.ProtoMinor = 1, 1
		if tc.learned {
			rec.Spec.ReqBodyNoise = map[string][]string{"header.authorization": {}, "header.x-amz-date": {}}
		}
		head := fmt.Sprintf("POST /bucket/key HTTP/1.1\r\nHost: api.example\r\nAuthorization: %s\r\nX-Amz-Date: 20261015T101533Z\r\nX-Amz-Content-Sha256: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
			tc.auth, hashOf(body), len(body))
		app, proxy := tcpPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		ctx = learn.WithTracker(ctx, learn.NewTracker())
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		go func() {
			_ = h.decodeHTTP(ctx, append([]byte(head), body...), proxy, nil, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true, deleteFilteredReturn: true}, models.OutgoingOptions{})
		}()
		got := readUntil(app, "\r\n\r\n", 20*time.Second)
		cancel()
		_ = app.Close()
		if served := strings.HasPrefix(got, "HTTP/1.1 200"); served != tc.wantServed {
			t.Errorf("%s: served %v (%q), want %v", tc.name, served, strings.SplitN(got, "\r\n", 2)[0], tc.wantServed)
		}
	}
}

// Noise F learned on a SigV4 Authorization rides a mock that also declares
// a variable; the variable pass puts the recorded value back there, so it
// first leaves a call whose credential parts differ to the strict matcher
// (founder decision 25).
func TestTheVariablePassLeavesASigV4CredentialToTheStrictMatcher(t *testing.T) {
	hdr := func(auth string) map[string]string {
		return map[string]string{"Authorization": auth, "X-Amz-Content-Sha256": "UNSIGNED-PAYLOAD"}
	}
	rec := func() []*models.Mock {
		return []*models.Mock{learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111aa7Kq9","item":"pen"}`,
			hdr(sigV4("AKID", "20261014", "us-east-1", "dynamodb", "host;x-amz-content-sha256", sig(1))), `{"id":"o-1111aa7Kq9"}`)}
	}
	f := []*req{live("POST", "/orders", `{"id":"o-9999Zx3Lm2","item":"pen"}`, hdr(sigV4("AKID", "20261014", "us-east-1", "dynamodb", "host;x-amz-content-sha256", sig(2))))}
	consumed := fullRound(t, rec(), f)
	mocks := learnedOnto(rec(), consumed)
	if _, ok := mocks[0].Spec.ReqBodyNoise["header.authorization"]; !ok || len(mocks[0].Spec.Variables) == 0 {
		t.Fatalf("F learned noise %v variables %v, want the Authorization's noise and the echo", mocks[0].Spec.ReqBodyNoise, mocks[0].Spec.Variables)
	}
	for _, tc := range []struct {
		name   string
		auth   string
		served bool
	}{
		{"the same scope, a new signature", sigV4("AKID", "20261014", "us-east-1", "dynamodb", "host;x-amz-content-sha256", sig(3)), true},
		{"another access key (not compared)", sigV4("OTHER", "20261014", "us-east-1", "dynamodb", "host;x-amz-content-sha256", sig(3)), true},
		{"another region", sigV4("AKID", "20261014", "eu-west-1", "dynamodb", "host;x-amz-content-sha256", sig(3)), false},
		{"another service", sigV4("AKID", "20261014", "us-east-1", "sqs", "host;x-amz-content-sha256", sig(3)), false},
		{"other signed headers", sigV4("AKID", "20261014", "us-east-1", "dynamodb", "host", sig(3)), false},
		{"no SigV4 at all", "Bearer abc", false},
	} {
		db := &varDb{perTest: learnedOnto(rec(), consumed)}
		if ok, _, _ := matchVar(t, newHTTP(), db, live("POST", "/orders", `{"id":"o-5555Pq8Rt4","item":"pen"}`, hdr(tc.auth))); ok != tc.served {
			t.Errorf("%s: served %v, want %v", tc.name, ok, tc.served)
		}
	}
}

// Noise learned on a presigned URL's signing parameters excuses nothing the
// strict matcher compares: a later replay leaves a call whose
// X-Amz-Credential names another region to it; another key is not compared
// (SF-3).
func TestLearnedMatchLeavesAPresignedCredentialToTheStrictMatcher(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	q := func(cred, date, sig string) string {
		return "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + cred + "&X-Amz-Date=" + date + "&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=" + sig
	}
	for _, tc := range []struct {
		name    string
		learned bool
		cred    string
		served  bool
	}{
		{"learned, the same scope", true, "AKID%2F20261015%2Fus-east-1%2Fs3%2Faws4_request", true},
		{"not learned, another region", false, "AKID%2F20261015%2Feu-west-1%2Fs3%2Faws4_request", false},
		{"learned, another region", true, "AKID%2F20261015%2Feu-west-1%2Fs3%2Faws4_request", false},
		{"learned, another access key", true, "OTHER%2F20261015%2Fus-east-1%2Fs3%2Faws4_request", true},
	} {
		rec := recorded("get", q("AKID%2F20261015%2Fus-east-1%2Fs3%2Faws4_request", "20261015T101010Z", sig(1)), map[string]string{}, "")
		rec.Spec.HTTPReq.ProtoMajor, rec.Spec.HTTPReq.ProtoMinor = 1, 1
		if tc.learned {
			rec.Spec.ReqBodyNoise = map[string][]string{"url.query.X-Amz-Credential": {}, "url.query.X-Amz-Date": {}, "url.query.X-Amz-Signature": {}}
		}
		head := "POST " + q(tc.cred, "20261015T101533Z", sig(2)) + " HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"
		app, proxy := tcpPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		ctx = learn.WithTracker(ctx, learn.NewTracker())
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		go func() {
			_ = h.decodeHTTP(ctx, []byte(head), proxy, nil, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true, deleteFilteredReturn: true}, models.OutgoingOptions{})
		}()
		got := readUntil(app, "\r\n\r\n", 20*time.Second)
		cancel()
		_ = app.Close()
		if served := strings.HasPrefix(got, "HTTP/1.1 200"); served != tc.served {
			t.Errorf("%s: served %v (%q), want %v", tc.name, served, strings.SplitN(got, "\r\n", 2)[0], tc.served)
		}
	}
}

// An id the app sends inside a longer value (a GraphQL argument, an OData
// filter) that the answer holds is an echo keploy cannot answer: refused,
// never noise. The id sent whole is an echo variable (control).
func TestLearnMatchRefusesAnIdItsAnswerHoldsFromInsideALongerValue(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, ans string
		refused              bool
	}{
		{"a GraphQL inline argument", `{"query":"query { order(id: \"A7K29QXMtq\") { id status } }"}`, `{"query":"query { order(id: \"Zq81Lm0Xwe\") { id status } }"}`, `{"data":{"order":{"id":"A7K29QXMtq","status":"NEW"}}}`, true},
		{"an OData filter", `{"filter":"id eq 'A7K29QXMtq'"}`, `{"filter":"id eq 'Zq81Lm0Xwe'"}`, `{"id":"A7K29QXMtq","state":"new"}`, true},
		{"the id whole", `{"id":"A7K29QXMtq"}`, `{"id":"Zq81Lm0Xwe"}`, `{"id":"A7K29QXMtq","state":"new"}`, false},
		// A word of the drift the answer holds by coincidence ("1" of
		// "x-1" and of "n-1") is refused all the same: keploy cannot tell.
		{"a drift word the answer holds", `{"nonce":"n-1","ts":"x-1"}`, `{"nonce":"n-2","ts":"x-2"}`, `{"nonce":"n-1"}`, true},
	} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/graphql", tc.rec, nil, tc.ans)}, observe: tr.ServedMock}
		served, miss, err := h.learnMatch(ctx, live("POST", "/graphql", tc.live, nil), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		if refused := served == nil; refused != tc.refused {
			t.Fatalf("%s: refused %v (%+v), want %v", tc.name, refused, miss, tc.refused)
		}
		if !tc.refused && !strings.Contains(served.Spec.HTTPResp.Body, "Zq81Lm0Xwe") {
			t.Fatalf("%s: answered %s, want this run's id", tc.name, served.Spec.HTTPResp.Body)
		}
	}
}

// parkedOnDisk is m as the agent keeps a mock whose answer it parked on disk
// (diskmocks.go: an answer of responseSpillMinBytes or more): no answer on
// the mock, read back on demand. With fail set, reading it back fails.
func parkedOnDisk(m *models.Mock, fail bool) *models.Mock {
	saved := m.Spec.HTTPResp
	m.Spec.HTTPResp = nil
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		if fail {
			return nil, nil, errors.New("disk mocks: store closed")
		}
		return saved, nil, nil
	})
	return m
}

// bigAnswer is a JSON answer of more than 8 KiB holding fields: one the agent
// parks on disk.
func bigAnswer(fields string) string {
	return `{` + fields + `,"padding":"` + strings.Repeat("x", 9000) + `"}`
}

// An answer the agent parked on disk is read back before the learn step
// classifies what drifted: a value it echoes is an echo (a variable of the
// call), never noise. Read without it, the echo was learned as noise and
// never compared again.
func TestLearnMatchFindsAnEchoInAnAnswerParkedOnDisk(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{parkedOnDisk(learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","amount":5}`, nil, bigAnswer(`"id":"ch_1","idempotency_key":"k-1"`)), false)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","amount":5}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if !strings.Contains(served.Spec.HTTPResp.Body, `"idempotency_key":"k-2"`) {
		t.Fatalf("served %.80s…: this run's value is answered where the recorded one was echoed", served.Spec.HTTPResp.Body)
	}
	if got := db.consumed[0].Spec; len(got.ReqBodyNoise) != 0 || len(got.Learned()) != 1 {
		t.Fatalf("learned noise %v, variables %+v; want the echo as a variable, no noise", got.ReqBodyNoise, got.Learned())
	}
}

// A value an earlier answer held, which the agent parked on disk, flowed
// from there: refused (keploy cannot follow a value from an answer yet),
// never noise, whether that answer was served by the matcher or by the learn
// step.
func TestLearnMatchFindsAValueAnEarlierAnswerParkedOnDiskHeld(t *testing.T) {
	earlier := func() *models.Mock {
		return parkedOnDisk(learnRecorded("mock-0", 0, "POST", "/v1/sessions", `{"user":"u-1"}`, nil, bigAnswer(`"token":"tok-7f3a9c"`)), false)
	}
	later := learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"token":"tok-7f3a9c","item":"book"}`, nil, `{}`)
	for _, tc := range []struct {
		what  string
		setup func(tr *learn.Tracker, h *HTTP, n learnNoise, opts models.OutgoingOptions, ctx context.Context) *learnDb
	}{
		{"served by the matcher", func(tr *learn.Tracker, _ *HTTP, _ learnNoise, _ models.OutgoingOptions, _ context.Context) *learnDb {
			tr.ServedMock(earlier())
			return &learnDb{perTest: []*models.Mock{later.DeepCopy()}}
		}},
		{"served by the learn step", func(tr *learn.Tracker, h *HTTP, n learnNoise, opts models.OutgoingOptions, ctx context.Context) *learnDb {
			db := &learnDb{perTest: []*models.Mock{earlier(), later.DeepCopy()}, observe: tr.ServedMock}
			if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions", `{"user":"u-1"}`, nil), db, n, opts); served == nil || miss != nil || err != nil {
				t.Fatalf("the earlier call is not answered: %+v %v", miss, err)
			}
			return db
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := tc.setup(tr, h, n, opts, ctx)
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"token":"tok-0c41b2","item":"book"}`, nil), db, n, opts)
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "mock-0") {
				t.Fatalf("served %v miss %+v; want the value refused as one mock-0 answered", served, miss)
			}
		})
	}
}

// An answer that cannot be read back leaves nothing learned: of the paired
// mock (what its answer echoes cannot be told), or of an earlier one (what
// flowed from it cannot be told). The call is a miss that says so.
func TestLearnMatchLearnsNothingFromAnAnswerItCannotReadBack(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{parkedOnDisk(learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1"}`, nil, bigAnswer(`"id":"ch_1"`)), true)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2"}`, nil), db, n, opts)
	// keploy's own store failed, not the request: no reason of the learn
	// step's (request_changed would blame the request).
	if err != nil || served != nil || miss == nil || miss.res.Reason != "" || !strings.Contains(miss.res.Why, "cannot read") || len(db.consumed) != 0 {
		t.Fatalf("served %v miss %+v err %v consumed %d; want a miss that learned nothing", served, miss, err, len(db.consumed))
	}

	// An earlier one, named or not (what tells it is the read's error).
	for _, name := range []string{"mock-0", ""} {
		tr := learn.NewTracker()
		tr.Begin()
		tr.ServedMock(parkedOnDisk(learnRecorded(name, 0, "POST", "/v1/sessions", `{"user":"u-1"}`, nil, bigAnswer(`"token":"tok-7f3a9c"`)), true))
		ctx := learn.WithTracker(context.Background(), tr)
		db = &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"note":"n-1"}`, nil, `{}`)}}
		served, miss, err = h.learnMatch(ctx, live("POST", "/v1/orders", `{"note":"n-2"}`, nil), db, n, opts)
		if err != nil || served != nil || miss == nil || miss.res.Reason != "" || !strings.Contains(miss.res.Why, "cannot read") || len(db.consumed) != 0 {
			t.Fatalf("earlier mock %q: served %v miss %+v err %v; want a miss that learned nothing", name, served, miss, err)
		}
	}
}

// A variable's answer places are read from the answer read back from disk:
// the call is answered with this run's value where the parked answer holds
// the recorded one.
func TestLearnMatchAnswersAVariableInAnAnswerParkedOnDisk(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		parkedOnDisk(learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, bigAnswer(`"id":"o-1","state":"new"`)), false),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the first call is paired: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if !strings.HasPrefix(served.Spec.HTTPResp.Body, `{"id":"o-9","state":"new"`) {
		t.Fatalf("served %.40s…: the answer holds this run's value", served.Spec.HTTPResp.Body)
	}
}

// With no drift cache yet (a tracker before its first test: the app's
// start-up calls in F), an earlier answer parked on disk is read back too,
// and the learn step keeps working for the tests after it. The start-up's
// drifted exchange is the run's, an earlier place of every test of the set
// (learn.Earlier.Run): the later test's value shares no word with it, which
// would refuse that call for another reason (a word of it appears earlier).
func TestLearnMatchReadsAParkedAnswerBeforeTheFirstTest(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	pool := func(name string, at time.Duration, method, target, body, resp string) *models.Mock {
		m := learnRecorded(name, at, method, target, body, nil, resp)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		return m
	}
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{
		parkedOnDisk(pool("pool-0", 0, "GET", "/v1/config", "", bigAnswer(`"region":"eu"`)), false),
		pool("pool-1", time.Second, "POST", "/v1/register", `{"node":"n-a"}`, `{}`),
	}}}
	tr := learn.NewTracker() // not begun: no drift cache
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"node":"n-b"}`, nil), db, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("start-up call: served %v miss %+v err %v", served, miss, err)
	}
	tr.Begin()
	done := make(chan struct{})
	go func() {
		defer close(done)
		db := &learnDb{perTest: []*models.Mock{
			parkedOnDisk(learnRecorded("mock-0", 0, "GET", "/v1/config", "", nil, bigAnswer(`"region":"eu"`)), false),
			learnRecorded("mock-1", time.Second, "POST", "/v1/charges", `{"key":"k-1"}`, nil, `{}`),
		}}
		if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/charges", `{"key":"k-3"}`, nil), db, n, opts); err != nil || served == nil || miss != nil {
			t.Errorf("a later test's call: served %v miss %+v err %v", served, miss, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a later test's call never returned")
	}
}

// A drifted start-up exchange is the run's, an earlier place of every test of
// the set (learn.Earlier.Run): a later test's drifted value that holds a word
// the start-up's held is refused, as one an earlier exchange of the test held
// is, since keploy cannot tell whether it came from there; it is never
// learned as noise. The reason names the set's start-up, not the test.
func TestLearnMatchRefusesAWordTheSetsStartUpHeld(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	startUp := learnRecorded("pool-1", time.Second, "POST", "/v1/register", `{"node":"n-1"}`, nil, `{}`)
	startUp.TestModeInfo.Lifetime = models.LifetimeSession
	tr := learn.NewTracker()
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"node":"n-2"}`, nil), &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{startUp}}}, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("start-up call: served %v miss %+v err %v", served, miss, err)
	}
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 2*time.Second, "POST", "/v1/charges", `{"key":"k-1"}`, nil, `{}`)}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/charges", `{"key":"k-3"}`, nil), db, n, opts)
	const why = "a word of it appears in the test set's start-up, in pool-1 at body.node"
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, why) {
		t.Fatalf("served %v miss %+v err %v; want the call refused: %s", served, miss, err, why)
	}
}

// An exchange the learn step served whose parked answer read back for the
// claim and then cannot be read back again is not kept as an exchange with
// no answer: a later value from it is refused, never learned as noise.
func TestLearnMatchKeepsNoExchangeWhoseAnswerItCannotReadAgain(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	var reads atomic.Int32
	m0 := learnRecorded("mock-0", 0, "POST", "/v1/sessions", `{"user":"u-1"}`, nil, bigAnswer(`"token":"tok-7f3a9c"`))
	saved := m0.Spec.HTTPResp
	m0.Spec.HTTPResp = nil
	m0.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		if reads.Add(1) == 1 {
			return saved, nil, nil
		}
		return nil, nil, errors.New("disk mocks: store closed")
	})
	later := learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"token":"tok-7f3a9c","item":"book"}`, nil, `{}`)
	db := &learnDb{perTest: []*models.Mock{m0, later}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions", `{"user":"u-1"}`, nil), db, n, opts); served == nil || miss != nil || err != nil {
		t.Fatalf("the earlier call is not answered: %+v %v", miss, err)
	}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"token":"tok-0c41b2","item":"book"}`, nil), db, n, opts); served != nil || miss == nil || miss.res.Reason != "" || !strings.Contains(miss.res.Why, "cannot read") {
		t.Fatalf("served %v miss %+v; want refused, learning nothing", served, miss)
	}
}

// A Mongo mock's answer, which the agent always parks on disk, is an earlier
// place of the test too: a value an HTTP call sends that came from it is
// refused, never noise.
func TestLearnMatchFindsAValueAParkedMongoAnswerHeld(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	mg := &models.Mock{Name: "mock-0", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: learnAt}}
	saved := []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"oid-55aa77"}]}} }`}}}}
	mg.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, saved, nil })
	tr.ServedMock(mg)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"ref":"oid-55aa77"}`, nil, `{}`)}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"ref":"oid-99bb00"}`, nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "mock-0") {
		t.Fatalf("served %v miss %+v; want the value refused as one mock-0 answered", served, miss)
	}
}

// recordedBeforeDb is learnDb whose store reads every recording before an
// instant (integrations.RecordedBeforeReader): those it keeps on disk
// (onDisk), or err.
type recordedBeforeDb struct {
	*learnDb
	onDisk []*models.Mock
	err    error
}

func (d *recordedBeforeDb) RangeRecordedBefore(t time.Time, visit func(*models.Mock) bool) error {
	if d.err != nil {
		return d.err
	}
	for _, m := range d.onDisk {
		if m.Spec.ReqTimestampMock.Before(t) && !visit(m) {
			return nil
		}
	}
	return nil
}

// A pool mock's earlier places are read from every recording the store keeps
// before it, the ones it keeps on disk too (a test's own, until its test
// opens): a token a test's login answered there is refused. A store that
// cannot read them refuses.
func TestLearnMatchAPoolMocksEarlierPlacesAreEveryRecordingBeforeIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		why  string
	}{
		{"on disk", nil, "holds earlier (in mock-0)"},
		{"on a disk that cannot be read", errors.New("disk mocks: store closed"), "cannot read the recordings of the test set made before mock-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			ctx := learn.WithTracker(context.Background(), learn.NewTracker())
			login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
			refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
			refresh.TestModeInfo.Lifetime = models.LifetimeSession
			db := &recordedBeforeDb{learnDb: &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{refresh}}}, onDisk: []*models.Mock{login}, err: tc.err}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
			if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, tc.why) {
				t.Fatalf("served %v miss %+v err %v pooled %+v; want refused: %s", served, miss, err, db.pooled, tc.why)
			}
		})
	}
}

// inFlightDb is recordedBeforeDb whose store serves some mocks as the open
// test's, in flight when it began (integrations.InFlightOwner).
type inFlightDb struct {
	*recordedBeforeDb
	inFlight map[string]bool
}

func (d *inFlightDb) InFlightIntoOpenTest(m *models.Mock) bool { return d.inFlight[m.Name] }

// A test's own call in flight when its test began (sent in the gap before
// its window, answered inside it) is in no recorded window: its earlier
// places are every recording made before it, so a token test 1's login
// answered (kept on disk) is refused, not learned as noise.
func TestLearnMatchAnOwnCallInFlightReadsEveryEarlierRecording(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.Begin() // test 2
	ctx := learn.WithTracker(context.Background(), tr)
	login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
	refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
	refresh.TestModeInfo.Lifetime = models.LifetimeSession
	db := &inFlightDb{recordedBeforeDb: &recordedBeforeDb{learnDb: &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{refresh}}}, onDisk: []*models.Mock{login}}, inFlight: map[string]bool{"mock-1": true}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-0") {
		t.Fatalf("served %v miss %+v err %v; want refused, the token answered earlier by mock-0", served, miss, err)
	}
}

// At start-up F's clock runs the lead past the earliest recording from when
// keploy froze it, before the app booted: a start-up call's clock read moved
// by the lead and the boot's difference, which the lead alone does not read
// (the minute and second that changed, words of the receive's Date header,
// then read as a value that flowed from the receive, refused). Read in the
// call's window (the receive served before it, its arrival, on the app's
// clock) it is a clock read, learned as noise, whether the boot took 2 s or
// 30 s longer. A test's own call is read by the lead alone, as before: F
// sets each test's clock.
func TestLearnMatchAStartUpClockReadIsReadInItsCallsWindow(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	recRead := learnAt.Add(3 * time.Millisecond)
	for _, tc := range []struct {
		name  string
		boot  time.Duration
		clock bool
		own   bool
		noise bool
	}{
		{"a 2 s longer boot", 2 * time.Second, true, false, true},
		{"a 30 s longer boot", 30 * time.Second, true, false, true},
		{"no app clock", 2 * time.Second, false, false, false},
		{"a test's own call", 2 * time.Second, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
			receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
			event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(recRead)+`"}`, nil, `{}`)
			event.TestModeInfo.Lifetime = models.LifetimeSession
			db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{event}}, staged: []*models.Mock{receive}, pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true}}
			tr := learn.NewTracker() // no test has begun: the start-up
			if tc.own {
				// The event is the open test's own, in its window.
				event.TestModeInfo.Lifetime = models.LifetimePerTest
				db = &learnDb{perTest: []*models.Mock{event}, staged: []*models.Mock{receive}, pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true}}
				tr.Begin()
			}
			offset := learn.FullRoundLead + tc.boot - 40*time.Second
			read := 0
			if tc.clock {
				tr.SetAppClock(func(real time.Time) (time.Time, bool) { read++; return real.Add(offset), true })
			}
			served := learnAt.Add(40 * time.Second) // when F's replay served the receive
			tr.ServedMockAt(receive, served)
			in := live("POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(served.Add(offset+time.Millisecond))+`"}`, nil)
			in.arrived = served.Add(2 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.own && read != 0 {
				t.Fatalf("a test's own call read its window (the app's clock %d times)", read)
			}
			if !tc.noise {
				if got != nil || miss == nil || !strings.Contains(miss.res.Why, "earlier") || !strings.Contains(miss.res.Why, "mock-0") {
					t.Fatalf("served %v miss %+v; want refused, a value from mock-0 as the lead reads it", got, miss)
				}
				return
			}
			if got == nil || len(db.pooled) != 1 || !reflect.DeepEqual(db.pooled[0].Spec.ReqBodyNoise, map[string][]string{"body.processed_at": {}}) {
				t.Fatalf("served %v miss %+v pooled %+v; want body.processed_at learned as noise", got, miss, db.pooled)
			}
		})
	}
}

// A window is one run of F's clock: across a re-write of the app's clock
// between the previous serve and the arrival (a test's begin), there is
// none, and an instant a served recorded answer carries (another receive's
// Date header, echoed into the event) is refused as with one offset, not
// read as a clock read in a span minutes wide.
func TestLearnMatchAWindowIsOneRunOfTheAppsClock(t *testing.T) {
	iso := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	for _, jump := range []time.Duration{0, 300 * time.Second} {
		t.Run(fmt.Sprintf("the clock moved %v", jump), func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
			receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
			later := learnRecorded("mock-2", 120*time.Second, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
			later.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Add(120 * time.Second).Format(http.TimeFormat)}
			later.TestModeInfo.Lifetime = models.LifetimeSession
			event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","seen_at":"`+iso(learnAt)+`"}`, nil, `{}`)
			event.TestModeInfo.Lifetime = models.LifetimeSession
			db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{event}}, staged: []*models.Mock{receive, later},
				pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true, later.Spec.ReqTimestampMock: true}}
			tr := learn.NewTracker()
			served := learnAt.Add(40 * time.Second)
			rewrite := served.Add(time.Millisecond)
			o1 := learn.FullRoundLead + 2*time.Second - 40*time.Second
			tr.SetAppClock(func(real time.Time) (time.Time, bool) {
				if real.Before(rewrite) {
					return real.Add(o1), true
				}
				return real.Add(o1 + jump), true
			})
			tr.ServedMockAt(later, served)
			in := live("POST", "/v1/events", `{"event":"m-1","seen_at":"`+iso(learnAt.Add(120*time.Second))+`"}`, nil)
			in.arrived = served.Add(3 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil || miss == nil {
				t.Fatalf("served %v miss %+v pooled %+v; want refused: the instant came from a served answer", got, miss, db.pooled)
			}
		})
	}
}

// The window's previous recorded call is found among every recording of
// the set: those the store stages, those it keeps on disk, and those the
// replay served (a per-test recording a serve took out of the store): a read
// made before that previous call, though after the latest one found
// elsewhere, is no clock read of the call, and is refused.
func TestLearnMatchTheWindowsPreviousCallMayBeOnDisk(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	for _, tc := range []struct {
		name               string
		disk, staged, gone bool
		noise              bool
	}{
		{"the previous call in memory", false, false, false, true},
		{"a later previous call on disk", true, false, false, false},
		{"a later previous call staged", false, true, false, false},
		{"the previous call served, no longer staged", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
			receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
			ack := learnRecorded("mock-2", 2*time.Millisecond, "POST", "/queue/ack", `{"id":"135dcb85"}`, nil, `{}`)
			event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(learnAt.Add(time.Millisecond))+`"}`, nil, `{}`)
			event.TestModeInfo.Lifetime = models.LifetimeSession
			var onDisk []*models.Mock
			if tc.disk {
				onDisk = []*models.Mock{ack}
			}
			staged := []*models.Mock{receive}
			switch {
			case tc.staged:
				staged = append(staged, ack)
			case tc.gone:
				staged = nil
			}
			db := &recordedBeforeDb{learnDb: &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{event}}, staged: staged, pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true, ack.Spec.ReqTimestampMock: true}}, onDisk: onDisk}
			tr := learn.NewTracker()
			offset := learn.FullRoundLead + 2*time.Second - 40*time.Second
			tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
			served := learnAt.Add(40 * time.Second)
			tr.ServedMockAt(receive, served)
			in := live("POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(served.Add(offset+time.Millisecond))+`"}`, nil)
			in.arrived = served.Add(2 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if learned := got != nil && len(db.pooled) == 1; learned != tc.noise {
				t.Fatalf("learned %v (miss %+v); want %v", learned, miss, tc.noise)
			}
		})
	}
}
