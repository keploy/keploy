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
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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

// clockedTracker is a tracker that knows the application reads the real
// clock (Tracker.SetAppClock), as the agent's own hooks tell it: with no app
// clock a test's own clock read, in a call with no window, is refused at
// learn (TestLearnMatchRefusesAClockReadWhereKeployCannotTellTheAppsClock).
func clockedTracker() *learn.Tracker {
	tr := learn.NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	return tr
}

// A variable whose value F saw drift only with its clock where it is bound (a
// clock read moved by the lead: learn.ClockOf) is marked so at its bind with
// the read its recording holds, the instant and resolution it says
// (MockVariable.Clock, learn.ClockReads); one that drifted otherwise is not.
func TestLearnMatchMarksAClockReadAtItsBind(t *testing.T) {
	// A whole second, no whole minute: a writer of seconds whose two values
	// both fall on a whole minute shows a cut, which the learn step would
	// learn at the minute.
	rec := learnAt.Add(-time.Hour + 17*time.Second)
	for _, c := range []struct {
		name  string
		live  string
		clock []models.ClockRead
	}{
		{"moved by the lead", rec.Add(learn.FullRoundLead).Format(time.RFC3339), []models.ClockRead{{At: rec.UTC(), Res: time.Second}}},
		{"moved by another amount", rec.Add(5 * time.Minute).Format(time.RFC3339), nil},
	} {
		h, n, opts := learnOn(t)
		tr := clockedTracker()
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
		// within: the parts the followed variable holds its value at
		// inside a longer value.
		within []models.VariablePart
	}{
		{
			"a number flowing into a path",
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":12345}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/orders/12345", "", nil, `{}`),
			live("POST", "/v1/orders", `{"order":54321}`, nil),
			live("GET", "/v1/orders/54321", "", nil),
			true,
			nil,
		},
		{
			"a token flowing into a credential",
			learnRecorded("mock-0", 0, "POST", "/v1/login", `{"token":"tok-A"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-A"}, `{}`),
			live("POST", "/v1/login", `{"token":"tok-B"}`, nil),
			live("GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-B"}),
			true,
			[]models.VariablePart{{Place: "header.authorization", Before: "Bearer "}},
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
			got := db.consumed[1].Spec
			if len(got.Learned()) != 1 || len(got.ReqBodyNoise) != 0 {
				t.Fatalf("variables %+v noise %v", got.Variables, got.ReqBodyNoise)
			}
			for _, v := range got.Learned() {
				if !reflect.DeepEqual(v.Within, tc.within) {
					t.Fatalf("parts %+v, want %+v", v.Within, tc.within)
				}
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

// umDb is a store as keploy mock replay's is under the proven switch: no
// recorded test windows (it implements no integrations.TestWindows), and its
// scope table's pool slices name the mocks recorded while no test was open
// (integrations.PoolMembership; known is false before the replay sent the
// table).
type umDb struct {
	mockMemDb
	pool   map[string]bool
	known  bool
	pooled []*models.Mock
	// inFlight are the mocks the store serves as its open test's, in flight
	// when it began (integrations.InFlightOwner).
	inFlight map[string]bool
}

func (d *umDb) InFlightIntoOpenTest(m *models.Mock) bool { return d.inFlight[m.Name] }

func (d *umDb) GetSessionMocks() ([]*models.Mock, error) { return d.mocks, nil }
func (d *umDb) InPool(m *models.Mock) (bool, bool)       { return d.pool[m.Name], d.known }
func (d *umDb) UpdateUnFilteredMock(old, new *models.Mock) bool {
	for i, p := range d.mocks {
		if p == old {
			d.mocks[i] = new
			d.pooled = append(d.pooled, new)
			return true
		}
	}
	return false
}

// With no recorded windows (keploy mock replay), the scope table says which
// mocks are the pool's: a start-up call in a full record is paired with the
// start-up recording a pool slice names, and its changed field learned onto
// it; one no pool slice names, or any before the replay sent the table, is
// no one's to pair with no test open.
func TestLearnMatchWithNoWindowsTheScopeTableSaysWhatIsThePools(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	ctx := learn.WithTracker(context.Background(), clockedTracker()) // the start-up: no test begun
	for _, tc := range []struct {
		what     string
		pool     map[string]bool
		known    bool
		inFlight bool
		paired   bool
	}{
		{"in a pool slice", map[string]bool{"boot-0": true}, true, false, true},
		{"in no pool slice", map[string]bool{}, true, false, false},
		{"before the table", map[string]bool{"boot-0": true}, false, false, false},
		// The store's next test opened and the tracker has not begun it
		// (the gap): one in flight into it is that test's, not the pool's.
		{"in a pool slice, in flight into the store's open test", map[string]bool{"boot-0": true}, true, true, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			boot := learnRecorded("boot-0", 0, "POST", "/v1/events", `{"kind":"boot","at":"2026-10-19T10:00:00Z"}`, nil, `{"ok":true}`)
			db := &umDb{mockMemDb: mockMemDb{mocks: []*models.Mock{boot}}, pool: tc.pool, known: tc.known, inFlight: map[string]bool{"boot-0": tc.inFlight}}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/events", `{"kind":"boot","at":"2026-10-20T11:30:01Z"}`, nil), db, n, opts)
			if (served != nil) != tc.paired {
				t.Fatalf("served %v miss %+v; want paired=%v", served, miss, tc.paired)
			}
			// With no window, the stamp, one instant whole, is a read of the
			// clock a replay checks in its call's own span, never noise.
			if tc.paired && (len(db.pooled) != 1 || !learnedClockRead(db.pooled[0].Spec, "body.at")) {
				t.Fatalf("the start-up recording carries %+v; want body.at learned as a read of the clock", db.pooled)
			}
		})
	}
}

// windowsAndTable is learnDb that also answers from a scope table, which
// names every mock the pool's: where the store knows the recorded windows,
// they, not the table, say whose a mock is.
type windowsAndTable struct{ *learnDb }

func (windowsAndTable) InPool(*models.Mock) (bool, bool) { return true, true }

func TestLearnMatchRecordedWindowsOutrankTheScopeTable(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	ctx := learn.WithTracker(context.Background(), learn.NewTracker())
	// A test's mock, recorded inside a test's window (the fake places only
	// its session mocks outside every window).
	db := windowsAndTable{&learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/events", `{"at":"a1"}`, nil, `{}`)}}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/events", `{"at":"a2"}`, nil), db, n, opts); served != nil || miss == nil {
		t.Fatalf("a mock the windows place inside a test was paired with no test open: served %v", served)
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

// A value one test's call made, which a later test of the set's call sends
// again, is followed as the run's variable: F binds it at test A's call and
// uses it at test B's, by the run's name, never as noise. The replay holds
// B's call to what A's call sent in the same run: B's call that sends
// another value is a variable miss, and B replayed without A's call before
// it is one that says the test set's call that binds it was not made.
func TestLearnMatchAValueATestMadeIsFollowedInALaterTest(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	create := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`)
	read := learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`)
	tr.Begin()
	a := &learnDb{perTest: []*models.Mock{create}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), a, n, opts); served == nil || miss != nil {
		t.Fatalf("test A's create: %+v", miss)
	}
	tr.End()
	tr.Begin()
	b := &learnDb{perTest: []*models.Mock{read}}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), b, n, opts); served == nil || miss != nil {
		t.Fatalf("test B's read of test A's order: %+v", miss)
	}
	name := models.RunVariablePrefix + "order"
	use, bind := b.consumed[0].Spec.Learned(), b.consumed[0].Spec.LearnedBinds()["mock-0"]
	if len(use) != 1 || len(use[name].Use) != 1 || len(bind) != 1 || bind[name].Bind != "body.order" {
		t.Fatalf("test B uses %+v and binds %+v at test A's call, want the run's %s", use, bind, name)
	}
	if len(b.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("test B's read learned test A's order as noise too: %v", b.consumed[0].Spec.ReqBodyNoise)
	}
	// The replay writes the use onto test B's recording and the bind onto
	// test A's, as it writes every consumed state.
	recA, recB := *create, *read
	recA.Spec.Variables, recB.Spec.Variables = models.MockVariables{name: bind[name]}, models.MockVariables{name: use[name]}
	replay := func(tests ...[]*models.Mock) *varDb {
		ci := &varDb{}
		ci.table.KeepRun()
		for _, mocks := range tests {
			ci.table.Start(nil)
			ci.perTest = mocks
			if len(mocks) == 1 && mocks[0].Name == "mock-0" {
				if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/orders", `{"order":"o-55"}`, nil)); !ok {
					t.Fatalf("test A's create in CI: %+v", diag)
				}
			}
		}
		return ci
	}
	ci := replay([]*models.Mock{&recA}, []*models.Mock{&recB})
	if ok, stub, diag := matchVar(t, h, ci, live("GET", "/v1/orders/o-55", "", nil)); !ok || stub.Spec.HTTPResp.Body != `{"id":"o-55","state":"new"}` {
		t.Fatalf("test B's read of test A's order in CI: ok %v diag %+v", ok, diag)
	}
	ci = replay([]*models.Mock{&recA}, []*models.Mock{&recB})
	ok, _, diag := matchVar(t, h, ci, live("GET", "/v1/orders/o-56", "", nil))
	if ok || diag == nil || diag.phase != models.MatchPhaseVariable {
		t.Fatalf("test B's read of another order than test A made is answered: ok %v diag %+v", ok, diag)
	}
	ci = replay([]*models.Mock{&recB})
	ok, _, diag = matchVar(t, h, ci, live("GET", "/v1/orders/o-55", "", nil))
	wantProblem(t, ok, diag, "made by the test set's start-up or an earlier test, was not made before it")
}

// A value test B both binds for itself (it makes an order and reads it
// back) and a later test C reads again is one variable at one place: the
// run's, bound at B's create, which B's own read uses too
// (learn.RunTakesPlaces). A full replay of B then C, with the application
// behaving as recorded, serves all three calls, and C reading another order
// misses.
func TestLearnMatchATestsOwnVariableALaterTestReusesIsTheRunsAtOnePlace(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	create := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`)
	read := learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`)
	read2 := learnRecorded("mock-2", 2*time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`)
	tr.Begin()
	b := &learnDb{perTest: []*models.Mock{create, read}}
	for _, r := range []*req{live("POST", "/v1/orders", `{"order":"o-9"}`, nil), live("GET", "/v1/orders/o-9", "", nil)} {
		if served, miss, _ := h.learnMatch(ctx, r, b, n, opts); served == nil || miss != nil {
			t.Fatalf("test B's %s: %+v", r.method, miss)
		}
	}
	tr.End()
	tr.Begin()
	c := &learnDb{perTest: []*models.Mock{read2}}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), c, n, opts); served == nil || miss != nil {
		t.Fatalf("test C's read of test B's order: %+v", miss)
	}
	tr.End()
	owner := map[string]string{"mock-0": "TestB", "mock-1": "TestB", "mock-2": "TestC"}
	recs := learnedOntoTaking([]*models.Mock{create, read, read2}, append(append([]models.Mock{}, b.consumed...), c.consumed...), owner)
	name := models.RunVariablePrefix + "order"
	if vs := recs[0].Spec.Variables; len(vs) != 1 || vs[name].Bind != "body.order" {
		t.Fatalf("test B's create carries %+v; want only the run's %s, bound at body.order", vs, name)
	}
	for _, m := range recs[1:] {
		if vs := m.Spec.Variables; len(vs) != 1 || len(vs[name].Use) != 1 || vs[name].Bind != "" {
			t.Fatalf("%s carries %+v; want it to use the run's %s only", m.Name, vs, name)
		}
	}
	replay := func(cRead string) (bool, *matchDiag) {
		ci := &varDb{}
		ci.table.KeepRun()
		ci.table.Start(nil)
		ci.perTest = []*models.Mock{recs[0], recs[1]}
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/orders", `{"order":"o-55"}`, nil)); !ok {
			t.Fatalf("a full replay: test B's create is a miss: %+v", diag)
		}
		if ok, _, diag := matchVar(t, h, ci, live("GET", "/v1/orders/o-55", "", nil)); !ok {
			t.Fatalf("a full replay: test B's read is a miss: %+v", diag)
		}
		ci.table.Start(nil)
		ci.perTest = []*models.Mock{recs[2]}
		ok, _, diag := matchVar(t, h, ci, live("GET", cRead, "", nil))
		return ok, diag
	}
	if ok, diag := replay("/v1/orders/o-55"); !ok {
		t.Fatalf("a full replay: test C's read of the order B made is a miss: %+v", diag)
	}
	if ok, _ := replay("/v1/orders/o-56"); ok {
		t.Fatal("a full replay: test C read another order than the one B made and was served")
	}
}

// A later test's second call that sends again the value an earlier test
// made uses the run's variable, as its first call does: the first call is
// where the run's variable is used, not where a variable of the test's own
// is bound. A replay of A then B answers both of B's calls with the value A
// sent in it, and B's second call that sends another value is a variable
// miss.
func TestLearnMatchALaterTestsSecondCallOfAnEarlierTestsValueUsesTheRunsVariable(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	create := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`)
	read := learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`)
	del := learnRecorded("mock-2", 2*time.Second, "DELETE", "/v1/orders/o-1", "", nil, `{}`)
	tr.Begin()
	a := &learnDb{perTest: []*models.Mock{create}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), a, n, opts); served == nil || miss != nil {
		t.Fatalf("test A's create: %+v", miss)
	}
	tr.End()
	tr.Begin()
	b := &learnDb{perTest: []*models.Mock{read, del}}
	for _, r := range []*req{live("GET", "/v1/orders/o-9", "", nil), live("DELETE", "/v1/orders/o-9", "", nil)} {
		if served, miss, _ := h.learnMatch(ctx, r, b, n, opts); served == nil || miss != nil {
			t.Fatalf("test B's %s of test A's order: %+v", r.method, miss)
		}
	}
	tr.End()
	name := models.RunVariablePrefix + "order"
	for _, c := range b.consumed {
		use, binds := c.Spec.Learned(), c.Spec.LearnedBinds()
		if len(use) != 1 || len(use[name].Use) != 1 || len(binds) != 1 || len(binds["mock-0"]) != 1 || binds["mock-0"][name].Bind != "body.order" {
			t.Fatalf("test B's %s uses %+v and binds %+v, want only the run's %s, bound at test A's call", c.Name, use, binds, name)
		}
	}
	recs := learnedOnto([]*models.Mock{create, read, del}, append(append([]models.Mock{}, a.consumed...), b.consumed...))
	replay := func(deleted string) (bool, *matchDiag) {
		ci := &varDb{}
		ci.table.KeepRun()
		ci.table.Start(nil)
		ci.perTest = recs[:1]
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/orders", `{"order":"o-55"}`, nil)); !ok {
			t.Fatalf("test A's create in CI: %+v", diag)
		}
		ci.table.Start(nil)
		ci.perTest = recs[1:]
		if ok, stub, diag := matchVar(t, h, ci, live("GET", "/v1/orders/o-55", "", nil)); !ok || stub.Spec.HTTPResp.Body != `{"id":"o-55","state":"new"}` {
			t.Fatalf("test B's read of test A's order in CI: ok %v diag %+v", ok, diag)
		}
		ok, _, diag := matchVar(t, h, ci, live("DELETE", "/v1/orders/"+deleted, "", nil))
		return ok, diag
	}
	if ok, diag := replay("o-55"); !ok {
		t.Fatalf("test B's delete of test A's order in CI: %+v", diag)
	}
	if ok, diag := replay("o-56"); ok || diag == nil || diag.phase != models.MatchPhaseVariable {
		t.Fatalf("test B's delete of another order than test A made is answered: ok %v diag %+v", ok, diag)
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
	if !learnedClockRead(db.consumed[1].Spec, "body.at") {
		t.Fatalf("noise %v clocks %v; want body.at learned as a read of the clock", db.consumed[1].Spec.ReqBodyNoise, db.consumed[1].Spec.Clocks)
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
		cp.Spec.Variables, cp.Spec.ReqBodyNoise, cp.Spec.Clocks = nil, nil, nil
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
		m.Spec.Clocks = models.MergeClocks(m.Spec.Clocks, c.Spec.Clocks)
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

// learnedOntoTaking is learnedOnto after what the replay does first
// (learn.RunTakesPlaces), each consumed mock read as learned in its test
// (owner).
func learnedOntoTaking(recs []*models.Mock, consumed []models.Mock, owner map[string]string) []*models.Mock {
	states := map[string]models.MockState{}
	for _, c := range consumed {
		states[c.Name] = models.MockState{Name: c.Name, Variables: c.Spec.Learned(), Binds: c.Spec.LearnedBinds()}
	}
	states, _ = learn.RunTakesPlaces(states, func(m string) string { return owner[m] })
	out := make([]models.Mock, len(consumed))
	for i, c := range consumed {
		c.Spec.SetLearned(states[c.Name].Variables, states[c.Name].Binds)
		out[i] = c
	}
	return learnedOnto(recs, out)
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
	inCIIn(t, context.Background(), mocks, calls)
}

// inCIIn is inCI on a connection whose context is ctx.
func inCIIn(t *testing.T, ctx context.Context, mocks []*models.Mock, calls []*req) {
	t.Helper()
	h := newHTTP()
	db := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	for i, c := range calls {
		if ok, _, diag := matchVarIn(t, ctx, h, db, c); !ok {
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

// A value the test case's request gave, carried in a claim of the token a
// call sends, uses the key at the claim's own place: the token is compared by
// its claims, the given one checked to hold this run's value.
func TestLearnMatchAValueTheTestCaseGaveIsFollowedIntoAJWTClaim(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	tok := func(at, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sub":"u-1","auth_time":`+at+`}`, sig)}
	}
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, tok("1793373215", "QUFBQUFB"), `{"ok":true}`)}}}
	db.table.Start([]variables.Binding{{Name: "authTime", Value: "1793387031", Recorded: "1793373215", Mock: "test case get-1", Place: "header.Authorization{jwt}.auth_time", Given: true}})
	served, miss, _ := h.learnMatch(context.Background(), live("POST", "/run/r-1", `{}`, tok("1793387031", "QkJCQkJC")), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if want := (models.MockVariables{"authTime": {Recorded: "1793373215", Use: []string{"header.authorization{jwt}.auth_time"}}}); !reflect.DeepEqual(db.consumed[0].Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", db.consumed[0].Spec.Learned(), want)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("noise %v: every other claim is still compared", db.consumed[0].Spec.ReqBodyNoise)
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

// Through the HTTP integration, a test's own call (no window) whose epoch
// second F read 93 s after its recording (no lead moves it) is learned as a
// read of the clock, checked at replay, where keploy knows which clock the
// application reads; where it cannot tell (no app clock), no replay could
// check that read, and the call is refused at learn, its reason saying so,
// never served with the value noise.
func TestLearnMatchRefusesAClockReadWhereKeployCannotTellTheAppsClock(t *testing.T) {
	for _, clocked := range []bool{true, false} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		if clocked {
			tr = clockedTracker()
		}
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/ping", `{"at":1760436000}`, nil, `{}`)}}
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/ping", `{"at":1760436093}`, nil), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		if clocked && (served == nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, "body.at")) {
			t.Fatalf("an app clock: served %v miss %+v; want body.at learned as a read of the clock", served != nil, miss)
		}
		if !clocked && (served != nil || miss == nil || !strings.Contains(miss.res.Why, "body.at changed, and nothing shows it is a read of the application's clock: keploy cannot tell which clock the application reads")) {
			t.Fatalf("no app clock: served %v miss %+v; want refused, the reason naming body.at and the clock keploy cannot tell", served != nil, miss)
		}
	}
}

// (Round-4 review repro.) the previous test inserted a row stamped with the second it ran in;
// this test's own call carries its own clock read, the same second when
// recorded. Nothing flowed, so in F the drifted value is noise; with the
// round-3 `before` raw places it is refused (request_changed), and the test
// is quarantined as flaky.
func TestLearnMatchAPreviousTestsSameSecondIsNoSource(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := clockedTracker()
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
// never learned as noise. It is the variable, used inside the longer value
// with the text around it compared (refused before the variable could be
// followed there; where it cannot, TestLearnMatchFollowsAVariableInside-
// ALongerValue keeps it refused).
func TestLearnMatchAFlowedIdInsideALongerValueIsAVariable(t *testing.T) {
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
			if served == nil || miss != nil {
				t.Fatalf("the id that flowed from mock-0 was not followed: %+v", miss)
			}
			got := db.consumed[1].Spec
			want := models.MockVariables{"id": {Recorded: tc.recID, Use: []string{"url.path[1]"}, Within: []models.VariablePart{{Place: "url.path[1]", Before: tc.prefix, After: tc.suffix}}}}
			if len(got.ReqBodyNoise) != 0 || !reflect.DeepEqual(got.Learned(), want) {
				t.Fatalf("noise %v variables %+v, want %+v", got.ReqBodyNoise, got.Learned(), want)
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
// noise, not refused as a part of the header. Each call is recorded after the
// read it carries: a read of the clock is no later than its call.
func TestLearnMatchAClockReadWithMillisAfterADateHeaderIsNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 42*time.Second, "PUT", "/container/blob1", `{"v":1}`, map[string]string{"X-Ms-Date": "Wed, 14 Oct 2026 10:00:41 GMT"}, `{}`),
		learnRecorded("mock-1", 42*time.Second+200*time.Millisecond, "POST", "/dbs/app/docs", `{"createdAt":"2026-10-14T10:00:41.456Z"}`, nil, `{}`),
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
					db.consumed[0].Spec.ReqBodyNoise, learn.RawOf(&models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: resp}}}}}}, true).Text())
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
// header and the claims' names they share did not change, and each token's
// moved claims are learned at their own places (the jti noise, the iat a
// read of the clock), never the token whole: every other claim, its JOSE
// header and its signature's form are still compared (jwtDiffs).
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
		_, whole := m.Spec.ReqBodyNoise["header.x-svc-token"]
		_, jti := m.Spec.ReqBodyNoise["header.x-svc-token{jwt}.jti"]
		_, iatNoise := m.Spec.ReqBodyNoise["header.x-svc-token{jwt}.iat"]
		_, iatClock := m.Spec.Clocks["header.x-svc-token{jwt}.iat"]
		if whole || !jti || !iatNoise && !iatClock {
			t.Fatalf("%s: noise %v clocks %v", m.Name, m.Spec.ReqBodyNoise, m.Spec.Clocks)
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
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		stop := startDecodeUntilStopped(t, func(ctx context.Context) error {
			return h.decodeHTTP(learn.WithTracker(ctx, learn.NewTracker()), append([]byte(head), body...), proxy, nil, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true, deleteFilteredReturn: true}, models.OutgoingOptions{})
		})
		got := readUntil(app, "\r\n\r\n", 20*time.Second)
		stop()
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
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		stop := startDecodeUntilStopped(t, func(ctx context.Context) error {
			return h.decodeHTTP(learn.WithTracker(ctx, learn.NewTracker()), []byte(head), proxy, nil, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true, deleteFilteredReturn: true}, models.OutgoingOptions{})
		})
		got := readUntil(app, "\r\n\r\n", 20*time.Second)
		stop()
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
// mock (what its answer echoes cannot be told: the call is a miss of
// keploy's, which says so), or of an earlier one (what flowed from it
// cannot be told: the changed value is refused, naming the recording and the
// read's error, where a rule reads it).
func TestLearnMatchLearnsNothingFromAnAnswerItCannotReadBack(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{parkedOnDisk(learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1"}`, nil, bigAnswer(`"id":"ch_1"`)), true)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2"}`, nil), db, n, opts)
	// keploy's own store failed, not the request: no reason of the learn
	// step's (request_changed would blame the request).
	if err != nil || served != nil || miss == nil || miss.res.Reason != "" || !strings.Contains(miss.res.Why, "cannot read") || len(db.consumed) != 0 {
		t.Fatalf("served %v miss %+v err %v consumed %d; want a miss that learned nothing", served, miss, err, len(db.consumed))
	}

	// An earlier one, named or not.
	for _, name := range []string{"mock-0", ""} {
		tr := learn.NewTracker()
		tr.Begin()
		tr.ServedMock(parkedOnDisk(learnRecorded(name, 0, "POST", "/v1/sessions", `{"user":"u-1"}`, nil, bigAnswer(`"token":"tok-7f3a9c"`)), true))
		ctx := learn.WithTracker(context.Background(), tr)
		db = &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"note":"n-1"}`, nil, `{}`)}}
		served, miss, err = h.learnMatch(ctx, live("POST", "/v1/orders", `{"note":"n-2"}`, nil), db, n, opts)
		named := name
		if named == "" {
			named = "a recording of the test"
		}
		why := "keploy could not read back " + named + ", an earlier place of the call, with its recorded answer (http: load the recorded response of mock \"" + name + "\": disk mocks: store closed)"
		if err != nil || served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, why) || len(db.consumed) != 0 {
			t.Fatalf("earlier mock %q: served %v miss %+v err %v; want the value refused: %s", name, served, miss, err, why)
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
	const why = "appears in the test set's start-up, in pool-1 at body.node"
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, why) {
		t.Fatalf("served %v miss %+v err %v; want the call refused: %s", served, miss, err, why)
	}
}

// An earlier test's own call that sent a value the run made is the run's
// too (learn.Earlier.Run, not the start-up's): a later test's drifted value
// that holds a word it held is refused as one the start-up held is, and the
// reason names the earlier test, not the start-up.
func TestLearnMatchRefusesAWordAnEarlierTestHeld(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	tr.Begin()
	a := &learnDb{perTest: []*models.Mock{learnRecorded("mock-a", time.Second, "POST", "/v1/register", `{"node":"n-1"}`, nil, `{}`)}}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"node":"n-2"}`, nil), a, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("test A's call: served %v miss %+v err %v", served, miss, err)
	}
	tr.End()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 2*time.Second, "POST", "/v1/charges", `{"key":"k-1"}`, nil, `{}`)}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/charges", `{"key":"k-3"}`, nil), db, n, opts)
	const why = "appears in an earlier test of the set, in mock-a at body.node"
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, why) {
		t.Fatalf("served %v miss %+v err %v; want the call refused: %s", served, miss, err, why)
	}
}

// A value an earlier test's call sent where its learn step learned a plain
// read of the clock (api-server's vault PUT's lastUsedAt), which a later
// test's call carries where its window does not hold it (a login code's
// authTime nine seconds later, the session token's auth time the test's
// client minted in the PUT's second), is refused, naming the earlier call
// and its place: keploy cannot tell a copy of the read from another read of
// the clock it does not see. Followed, the PUT's place would carry the run's
// variable and a clock read at once, and the PUT would be refused for every
// test. In a call keploy knows no window of (keploy record's calls under a
// moved clock), the later call's value is its own read of the clock, as the
// leaf learns one there with the switch off too, which every replay checks
// in its call's span: an application that writes the second it read for
// each call sends the earlier call's text again in that second.
func TestLearnMatchRefusesAnEarlierTestsClockReadNoWindowOfTheCallHolds(t *testing.T) {
	lead := learn.FullRoundLead
	for _, window := range []bool{true, false} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		if window {
			// keploy mock's client tests: each test's first call has a
			// window from its begin, in the recording and in F.
			tr.SetClientTests(true)
			tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		}
		tr.NewSet()
		ctx := learn.WithTracker(context.Background(), tr)
		if window {
			boot := learnRecorded("mock-0", 0, "GET", "/v1/health", "", nil, `{}`)
			tr.ServedMockAt(boot, learnAt.Add(lead))
			tr.ServedMockAsRecorded(boot)
			tr.RecordedBegin(learnAt.Add(time.Second))
			tr.BeginAt(learnAt.Add(lead + time.Second))
		} else {
			tr.Begin()
		}
		recA := learnRecorded("mock-a", 1100*time.Millisecond, "PUT", "/v1/secret/tokens", `{"data":{"lastUsedAt":1791972001}}`, nil, `{}`)
		a := &learnDb{perTest: []*models.Mock{recA}}
		in := live("PUT", "/v1/secret/tokens", `{"data":{"lastUsedAt":1791972062}}`, nil)
		in.arrived = learnAt.Add(lead + 1100*time.Millisecond)
		if served, miss, err := h.learnMatch(ctx, in, a, n, opts); err != nil || served == nil || miss != nil || !learnedClockRead(a.consumed[0].Spec, "body.data.lastUsedAt") {
			t.Fatalf("window %v: test A's call: served %v miss %+v err %v; want lastUsedAt learned as a read of the clock", window, served, miss, err)
		}
		tr.End()
		at := 10 * time.Second
		if window {
			tr.RecordedBegin(learnAt.Add(at))
			tr.BeginAt(learnAt.Add(lead + at))
		} else {
			// The application read its clock for this call in the PUT's second.
			at = 1400 * time.Millisecond
			tr.Begin()
		}
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", at+100*time.Millisecond, "POST", "/v1/codes", `{"authTime":1791972001}`, nil, `{}`)}}
		in = live("POST", "/v1/codes", `{"authTime":1791972062}`, nil)
		in.arrived = learnAt.Add(lead + at + 100*time.Millisecond)
		served, miss, err := h.learnMatch(ctx, in, db, n, opts)
		const why = "body.authTime changed, and its recorded value was sent in mock-a at body.data.lastUsedAt, an earlier test's call, where keploy learned it as a read of the application's clock"
		switch {
		case err != nil:
			t.Fatalf("window %v: %v", window, err)
		case window && (served != nil || miss == nil || !strings.Contains(miss.res.Why, why) || strings.Contains(miss.res.Why, "179197")):
			t.Fatalf("window %v: served %v miss %+v; want the call refused: %s, never the value", window, served, miss, why)
		case !window && (served == nil || miss != nil || !learnedClockRead(db.consumed[0].Spec, "body.authTime")):
			t.Fatalf("window %v: served %v miss %+v; want authTime learned as the call's own read of the clock", window, served, miss)
		}
		if v := a.consumed[0].Spec.Variables; len(v) != 0 {
			t.Fatalf("window %v: test A's PUT binds %+v; want nothing bound at its clock read", window, v)
		}
	}
}

// A word of an earlier test's leaf that did not drift is no source of a
// later test's value: the run keeps of test A's call only the request
// leaves that drifted (learn's ranCalls). A's call drifts at node and sends
// qty as recorded ("7"); test B's key, which drifted and holds a 7 too, but
// came from nowhere in A, is learned as noise, as it is within one test,
// never refused for A's qty.
func TestLearnMatchAWordOfAnEarlierTestsLeafThatDidNotDriftIsNoSource(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	tr.Begin()
	a := &learnDb{perTest: []*models.Mock{learnRecorded("mock-a", time.Second, "POST", "/v1/register", `{"node":"n-1","qty":"7"}`, nil, `{}`)}}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"node":"n-2","qty":"7"}`, nil), a, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("test A's call: served %v miss %+v err %v", served, miss, err)
	}
	tr.End()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 2*time.Second, "POST", "/v1/charges", `{"key":"k-7"}`, nil, `{}`)}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/charges", `{"key":"k-8"}`, nil), db, n, opts)
	if miss != nil {
		t.Fatalf("test B's call was refused: %s", miss.res.Why)
	}
	if err != nil || served == nil || len(db.consumed) != 1 || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, map[string][]string{"body.key": {}}) {
		t.Fatalf("served %v err %v consumed %d; want test B's key learned as noise", served, err, len(db.consumed))
	}
}

// An exchange the learn step served whose parked answer read back for the
// claim and then cannot be read back again is not kept as an exchange with
// no answer: a later value from it is refused, naming the recording it
// cannot read, never learned as noise.
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
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"token":"tok-0c41b2","item":"book"}`, nil), db, n, opts); served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "could not read back mock-0") {
		t.Fatalf("served %v miss %+v; want refused, naming mock-0 it cannot read back", served, miss)
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
		if !m.Spec.ReqTimestampMock.After(t) && !visit(m) {
			return nil
		}
	}
	return nil
}

// A pool mock's earlier places are read from every recording the store keeps
// before it, the ones it keeps on disk too (a test's own, until its test
// opens): a token a test's login answered there is refused, one recorded at
// the call's own instant too (a fixture's whole second may have made it
// before the call). A store that cannot read them refuses.
func TestLearnMatchAPoolMocksEarlierPlacesAreEveryRecordingBeforeIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
		err  error
		why  string
	}{
		{"on disk", 0, nil, "holds earlier (in mock-0)"},
		{"on disk, recorded at the call's own instant", 2 * time.Second, nil, "holds earlier (in mock-0)"},
		{"on a disk that cannot be read", 0, errors.New("disk mocks: store closed"), "cannot read the recordings of the test set made before mock-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			ctx := learn.WithTracker(context.Background(), learn.NewTracker())
			login := learnRecorded("mock-0", tc.at, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
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

// The learn step reads the order F served the test's mocks in (the tracker's
// served mocks) into the offset rule. Two receives of one queue, both
// recorded before the insert and both served before it in F, A recorded
// first: the insert's visible_after is the first message's SentTimestamp and
// a minute, to the minute. F serving B first, the recording's value is built
// on A and F's on B: refused, naming both. F serving A first, as recorded, F
// gave the application nothing in the other's place: the move is the lead's,
// learned as noise. Through the payloads any parser served, and through the
// exchanges the learn step served leaf by leaf (learn.InServeOrder).
func TestLearnMatchReadsTheOrderFServedTheTestsMocksIn(t *testing.T) {
	sentA := learnAt.Add(-4*time.Minute + 473*time.Millisecond)
	sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
	receive := func(name string, at time.Duration, sent time.Time) *models.Mock {
		m := learnRecorded(name, at, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"`+name+`","Attributes":{"SentTimestamp":"`+strconv.FormatInt(sent.UnixMilli(), 10)+`"}}]}`)
		m.Spec.HTTPResp.Header["Date"] = learnAt.Add(-time.Hour).Format(http.TimeFormat)
		return m
	}
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	for _, tc := range []struct {
		name    string
		bFirst  bool
		leafA   bool
		refused bool
	}{
		{"F served B first", true, false, true},
		{"F served A first, as recorded", false, false, false},
		{"F served B first, A leaf by leaf", true, true, true},
		{"F served A first, as recorded, A leaf by leaf", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			a, b := receive("mock-0", 0, sentA), receive("mock-1", time.Millisecond, sentB)
			insert := learnRecorded("mock-2", 4*time.Millisecond, "POST", "/v1/messages", `{"visible_after":"`+minute(sentA.Add(time.Minute))+`"}`, nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{insert}}
			tr := learn.NewTracker()
			tr.Begin()
			first, second := a, b
			if tc.bFirst {
				first, second = b, a
			}
			served := learnAt.Add(40 * time.Second)
			tr.ServedMockAt(first, served)
			tr.ServedMockAt(second, served.Add(time.Millisecond))
			if tc.leafA {
				// The learn step served A leaf by leaf: its exchange holds
				// its payload, and raw does not.
				test, _, _, _ := tr.Test()
				tr.Served(test, learn.Earlier{Mock: a.Name, At: a.Spec.ReqTimestampMock, Own: true, Payload: learn.RawOf(a, true)})
			}
			in := live("POST", "/v1/messages", `{"visible_after":"`+minute(sentB.Add(time.Minute))+`"}`, nil)
			in.arrived = served.Add(2 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refused {
				if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-0 at spec.resp.body holds as the recording gave it") || !strings.Contains(miss.res.Why, "mock-1 at spec.resp.body holds, which record's full round gave the application in its place") {
					t.Fatalf("served %v miss %+v; want refused, built on mock-0 and mock-1 by an offset", got, miss)
				}
				return
			}
			if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, "body.visible_after") {
				t.Fatalf("served %v miss %+v; want body.visible_after learned as a read of the clock, still compared", got, miss)
			}
		})
	}
}

// F served A and B in the recorded order, but each in a request the
// application had in flight on its own connection while the other was too
// (learn.Flight): which answer the application held first is not told by
// the order F claimed them in, so a value built by an offset on one of them
// is refused, never learned as noise; the same requests one after the other
// keep the recorded order's verdict. The verdict is the one whichever F
// claimed first, and whether B's request reached keploy before A's answer
// went out with B claimed after it.
func TestLearnMatchRefusesAnOffsetOnServesInParallelRequests(t *testing.T) {
	sentA := learnAt.Add(-4*time.Minute + 473*time.Millisecond)
	sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
	receive := func(name string, at time.Duration, sent time.Time) *models.Mock {
		m := learnRecorded(name, at, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"`+name+`","Attributes":{"SentTimestamp":"`+strconv.FormatInt(sent.UnixMilli(), 10)+`"}}]}`)
		m.Spec.HTTPResp.Header["Date"] = learnAt.Add(-time.Hour).Format(http.TimeFormat)
		return m
	}
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	for _, tc := range []struct {
		name    string
		order   string // per step: 1/2 lift A's/B's request, a/b serve A/B, x/y land A's/B's
		refused bool
	}{
		{"one after the other", "1ax2by", false},
		{"both aloft, A claimed first", "12abxy", true},
		{"both aloft, B claimed first, A's answer out first", "12baxy", true},
		{"B in before A's answer went out, claimed after", "1a2xby", true},
		{"B's answer still to go out at the read", "1ax2b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			a, b := receive("mock-0", 0, sentA), receive("mock-1", time.Millisecond, sentB)
			insert := learnRecorded("mock-2", 4*time.Millisecond, "POST", "/v1/messages", `{"visible_after":"`+minute(sentA.Add(time.Minute))+`"}`, nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{insert}}
			tr := learn.NewTracker()
			tr.Begin()
			served := learnAt.Add(40 * time.Second)
			var fa, fb *learn.Flight
			for _, s := range tc.order {
				switch s {
				case '1':
					fa = tr.Lift()
				case '2':
					fb = tr.Lift()
				case 'a':
					tr.ServedMockAt(a, served)
				case 'b':
					tr.ServedMockAt(b, served.Add(time.Millisecond))
				case 'x':
					fa.Land()
				case 'y':
					fb.Land()
				}
			}
			// The call that carries the value is the application's next
			// request, on a connection of its own.
			call := tr.Lift()
			defer call.Land()
			in := live("POST", "/v1/messages", `{"visible_after":"`+minute(sentB.Add(time.Minute))+`"}`, nil)
			in.arrived = served.Add(2 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refused {
				if got != nil || miss == nil || !strings.Contains(miss.res.Why, "gave the application mock-0 and mock-1 in an order keploy cannot tell: in requests the application had in flight at once") {
					t.Fatalf("served %v miss %+v; want refused, naming mock-0 and mock-1", got, miss)
				}
				t.Logf("refused: %s", miss.res.Why)
				return
			}
			if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, "body.visible_after") {
				t.Fatalf("served %v miss %+v; want body.visible_after learned as a read of the clock, still compared", got, miss)
			}
		})
	}
}

// F gave the application every mock it served before the test as much as one
// it served in the test: an application that long-polls a queue at start-up
// (or consumed a message in an earlier test) builds a later write on what it
// holds. The learn step reads each as given, numbered in the one order F
// first served the set's mocks in (learn.Order), so F serving B
// first, wherever before the call, is refused naming both receives, and F
// serving them in the recorded order is the lead's move, learned as noise,
// as it is inside the test. Receives recorded in another recorded test's
// window are places of the offset rule alone, and refused as well.
func TestLearnMatchReadsWhatFServedBeforeTheTestAsGiven(t *testing.T) {
	sentA := learnAt.Add(-4*time.Minute + 473*time.Millisecond)
	sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
	receive := func(name string, at time.Duration, sent time.Time) *models.Mock {
		m := learnRecorded(name, at, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"`+name+`","Attributes":{"SentTimestamp":"`+strconv.FormatInt(sent.UnixMilli(), 10)+`"}}]}`)
		m.Spec.HTTPResp.Header["Date"] = learnAt.Add(-time.Hour).Format(http.TimeFormat)
		return m
	}
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	for _, where := range []string{
		"at the start-up",
		"in the previous test",
		"two tests before",
		"the first before the test, the second in it",
		"at the start-up, recorded outside every window",
		"in the previous test, recorded in its window",
		"the first at the start-up and again after the second, in the test",
	} {
		for _, bFirst := range []bool{true, false} {
			t.Run(where+map[bool]string{true: ", B first", false: ", in the recorded order"}[bFirst], func(t *testing.T) {
				h, n, opts := learnOn(t)
				a, b := receive("mock-0", 0, sentA), receive("mock-1", time.Millisecond, sentB)
				insert := learnRecorded("mock-2", 4*time.Millisecond, "POST", "/v1/messages", `{"visible_after":"`+minute(sentA.Add(time.Minute))+`"}`, nil, `{}`)
				db := &learnDb{perTest: []*models.Mock{insert}}
				tr := learn.NewTracker()
				first, second := a, b
				if bFirst {
					first, second = b, a
				}
				served := learnAt.Add(40 * time.Second)
				both := func() {
					tr.ServedMockAt(first, served)
					tr.ServedMockAt(second, served.Add(time.Millisecond))
				}
				switch where {
				case "at the start-up":
					both()
					tr.Begin()
				case "in the previous test":
					tr.Begin()
					both()
					tr.Begin()
				case "two tests before":
					tr.Begin()
					both()
					tr.Begin()
					tr.Begin()
				case "the first before the test, the second in it":
					tr.ServedMockAt(first, served)
					tr.Begin()
					tr.ServedMockAt(second, served.Add(time.Millisecond))
				case "at the start-up, recorded outside every window":
					db.pool = map[time.Time]bool{a.Spec.ReqTimestampMock: true, b.Spec.ReqTimestampMock: true}
					both()
					tr.Begin()
				case "in the previous test, recorded in its window":
					db.otherTest = map[time.Time]bool{a.Spec.ReqTimestampMock: true, b.Spec.ReqTimestampMock: true}
					tr.Begin()
					both()
					tr.Begin()
				case "the first at the start-up and again after the second, in the test":
					// A pool's reusable recording: F gave the application
					// each of the two before the other, in either order.
					tr.ServedMockAt(first, served)
					tr.Begin()
					tr.ServedMockAt(second, served.Add(time.Millisecond))
					tr.ServedMockAt(first, served.Add(2*time.Millisecond))
				}
				interleaved := strings.HasPrefix(where, "the first at the start-up and again")
				in := live("POST", "/v1/messages", `{"visible_after":"`+minute(sentB.Add(time.Minute))+`"}`, nil)
				in.arrived = served.Add(3 * time.Millisecond)
				got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if interleaved {
					if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-0 at spec.resp.body") || !strings.Contains(miss.res.Why, "mock-1 at spec.resp.body") {
						t.Fatalf("served %v miss %+v; want refused, built on mock-0 and mock-1 by an offset", got, miss)
					}
					return
				}
				if bFirst {
					if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-0 at spec.resp.body holds as the recording gave it") || !strings.Contains(miss.res.Why, "mock-1 at spec.resp.body holds, which record's full round gave the application in its place") {
						t.Fatalf("served %v miss %+v; want refused, built on mock-0 and mock-1 by an offset", got, miss)
					}
					return
				}
				if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, "body.visible_after") {
					t.Fatalf("served %v miss %+v; want body.visible_after learned as a read of the clock, still compared", got, miss)
				}
			})
		}
	}
}

// A recording F served the application before the call (at the start-up,
// or earlier in the test: a poll's reusable recording) is a place F gave it,
// even where the call pairs with that very recording: a poll whose request
// carries a value built by an offset on the message the previous poll got,
// which F gave the application from the recording the later poll pairs
// with, is refused naming both receives, never learned as noise. Where F
// never served it, the move is the lead's, as before.
func TestLearnMatchReadsThePairedRecordingFServedBeforeTheCall(t *testing.T) {
	sentA := learnAt.Add(-4*time.Minute + 473*time.Millisecond)
	sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	receive := func(name string, at time.Duration, body string, sent time.Time) *models.Mock {
		m := learnRecorded(name, at, "POST", "/queue/receive", body, nil, `{"Messages":[{"MessageId":"`+name+`","Attributes":{"SentTimestamp":"`+strconv.FormatInt(sent.UnixMilli(), 10)+`"}}]}`)
		m.Spec.HTTPResp.Header["Date"] = learnAt.Add(-time.Hour).Format(http.TimeFormat)
		return m
	}
	for _, where := range []string{"at the start-up", "earlier in the test", "never"} {
		t.Run(where, func(t *testing.T) {
			h, n, opts := learnOn(t)
			e1 := receive("mock-0", 0, `{"MaxNumberOfMessages":10}`, sentA)
			e2 := receive("mock-1", time.Millisecond, `{"visible_after":"`+minute(sentA.Add(time.Minute))+`"}`, sentB)
			db := &learnDb{perTest: []*models.Mock{e1, e2}}
			tr := learn.NewTracker()
			served := learnAt.Add(40 * time.Second)
			switch where {
			case "at the start-up":
				tr.ServedMockAt(e2, served)
				tr.Begin()
			case "earlier in the test":
				tr.Begin()
				tr.ServedMockAt(e2, served)
			default:
				tr.Begin()
			}
			in := live("POST", "/queue/receive", `{"visible_after":"`+minute(sentB.Add(time.Minute))+`"}`, nil)
			in.arrived = served.Add(3 * time.Millisecond)
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if where != "never" {
				if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-0 at spec.resp.body holds as the recording gave it") || !strings.Contains(miss.res.Why, "mock-1 at spec.resp.body holds, which record's full round gave the application in its place") {
					t.Fatalf("served %v miss %+v; want refused, built on mock-0 and mock-1 by an offset", got, miss)
				}
				return
			}
			if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, "body.visible_after") {
				t.Fatalf("served %v miss %+v; want body.visible_after learned as a read of the clock, still compared", got, miss)
			}
		})
	}
}

// A mock F served in the previous test but recorded after a pool call is no
// earlier place of the call's for any rule but the offset rule and the word
// rule: a value of the call that changed to a word of that mock's answer is
// bound to nothing, and is refused naming it (keploy follows no value of
// another test's), never learned as noise.
func TestLearnMatchAPoolCallReadsNoMockRecordedAfterItThatFServedBefore(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	beat := learnRecorded("pool-0", 4*time.Millisecond, "POST", "/v1/beat", `{"counter":"17"}`, nil, `{}`)
	beat.TestModeInfo.Lifetime = models.LifetimeSession
	stats := learnRecorded("mock-1", 10*time.Millisecond, "GET", "/stats", "", nil, `{"count":"42"}`)
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{beat}}}
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(stats)
	tr.Begin()
	got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/beat", `{"counter":"42"}`, nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || len(db.pooled) != 0 ||
		!strings.Contains(miss.res.Why, "a word of what record's full round saw in it (its characters 1 to 2) appears in mock-1, a recording record's full round gave the application before the call that was recorded after the call") {
		t.Fatalf("served %v miss %+v; want body.counter refused naming mock-1, never noise nor bound", got, miss)
	}
}

// A mock F served before the call (at the start-up, in the previous test, or
// earlier in the call's own test) but recorded after the call is no earlier
// place of the call's own test for any rule but the offset rule, the word
// rule and the echo of what F gave the application there: the recording
// made the call before it. A
// value of the test's own call that changed to a word of that mock's answer
// (an id, a whole id, an instant, copied word for word) is bound to nothing
// and refused, naming the mock and that it was recorded after the call,
// never calling it another test's (it is the call's own test's); never
// learned as noise. So is a time copied from it that moved as the lead moves
// a read of the clock (a read before the call, and the mock's answer that
// read a lead later), which the word rule leaves to the clock: the echo reads
// the moment F gave the application there.
func TestLearnMatchAnOwnCallRefusesAValueCopiedFromAMockRecordedAfterItThatFServedBefore(t *testing.T) {
	inst := learnAt.Add(5 * time.Minute).Format(time.RFC3339)
	read, copied := learnAt.Add(-30*time.Second).Format(time.RFC3339), learnAt.Add(31*time.Second).Format(time.RFC3339)
	for _, tc := range []struct{ name, rec, live, answer, found string }{
		{"a word", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"42"}`, "appears in mock-2"},
		{"an id", `{"ref":"ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323"}`, `{"ref":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`, `{"id":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`, "appears in mock-2"},
		{"an instant", `{"ref":"` + learnAt.Add(-3*time.Hour).Format(time.RFC3339) + `"}`, `{"ref":"` + inst + `"}`, `{"at":"` + inst + `"}`, "appears in mock-2"},
		{"a time the lead moved", `{"ref":"` + read + `"}`, `{"ref":"` + copied + `"}`, `{"at":"` + copied + `"}`, "the moment record's full round saw in it stands in mock-2 at spec.resp.body"},
	} {
		for _, where := range []string{"at the start-up", "in the previous test", "earlier in the test"} {
			t.Run(tc.name+", "+where, func(t *testing.T) {
				h, n, opts := learnOn(t)
				call := learnRecorded("mock-1", time.Millisecond, "POST", "/v1/messages", tc.rec, nil, `{}`)
				late := learnRecorded("mock-2", 5*time.Millisecond, "GET", "/stats", "", nil, tc.answer)
				db := &learnDb{perTest: []*models.Mock{call}}
				tr := learn.NewTracker()
				if where != "at the start-up" {
					tr.Begin()
				}
				tr.ServedMock(late)
				if where != "earlier in the test" {
					tr.Begin()
				}
				got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", tc.live, nil), db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if got != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || len(db.consumed) != 0 ||
					!strings.Contains(miss.res.Why, "body.ref changed") ||
					!strings.Contains(miss.res.Why, tc.found) ||
					!strings.Contains(miss.res.Why, tc.found+", a recording record's full round gave the application before the call that was recorded after the call") ||
					strings.Contains(miss.res.Why, "another test's") {
					t.Fatalf("served %v miss %+v consumed %d; want body.ref refused naming mock-2 as recorded after the call, never noise nor bound", got, miss, len(db.consumed))
				}
			})
		}
	}
}

// A word the call's value was recorded with that only a mock recorded after
// the call holds is no earlier place of the call, wherever F served that
// mock before the call (as recorded: its adapter said so), earlier in the
// test included: the recording made the call before it, so its answer is
// never read as "the test's recording holds earlier". The control, the same
// mock recorded before the call and served earlier in the test, is an
// earlier place the value was copied from.
func TestLearnMatchAWordOnlyAMockRecordedAfterTheCallHoldsIsNoEarlierPlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after bool
	}{{"recorded after the call", true}, {"recorded before the call", false}} {
		for _, where := range []string{"at the start-up", "earlier in the test"} {
			t.Run(tc.name+", "+where, func(t *testing.T) {
				h, n, opts := learnOn(t)
				callAt, lateAt := 5*time.Millisecond, time.Millisecond
				if tc.after {
					callAt, lateAt = time.Millisecond, 5*time.Millisecond
				}
				call := learnRecorded("mock-1", callAt, "POST", "/v1/messages", `{"ref":"7"}`, nil, `{}`)
				late := learnRecorded("mock-2", lateAt, "GET", "/stats", "", nil, `{"count":"7"}`)
				db := &learnDb{perTest: []*models.Mock{call}}
				tr := learn.NewTracker()
				if where == "earlier in the test" {
					tr.Begin()
				}
				tr.ServedMock(late)
				tr.ServedMockAsRecorded(late)
				if where != "earlier in the test" {
					tr.Begin()
				}
				got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", `{"ref":"42"}`, nil), db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if tc.after {
					if got == nil || miss != nil || len(db.consumed) != 1 {
						t.Fatalf("served %v miss %+v consumed %d; want the call served, body.ref learned as noise: mock-2, recorded after the call, holds no earlier place of it", got, miss, len(db.consumed))
					}
					return
				}
				if got != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || len(db.consumed) != 0 ||
					!strings.Contains(miss.res.Why, "body.ref carries a value the test's recording holds earlier (in mock-2)") {
					t.Fatalf("served %v miss %+v consumed %d; want body.ref refused as a value of mock-2, recorded before the call", got, miss, len(db.consumed))
				}
			})
		}
	}
}

// The test's own recording with no recorded time: which recordings came
// before it cannot be told, so whether what changed came from one cannot
// either; the learn step learns nothing from it and says why.
func TestLearnMatchAnOwnRecordingWithNoRecordedTimeLearnsNothing(t *testing.T) {
	h, n, opts := learnOn(t)
	call := learnRecorded("mock-1", time.Millisecond, "POST", "/v1/messages", `{"ref":"7"}`, nil, `{}`)
	call.Spec.ReqTimestampMock, call.Spec.ResTimestampMock = time.Time{}, time.Time{}
	db := &learnDb{perTest: []*models.Mock{call}}
	tr := learn.NewTracker()
	tr.Begin()
	got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", `{"ref":"42"}`, nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || len(db.consumed) != 0 ||
		!strings.Contains(miss.res.Why, "keploy cannot tell which recordings of the test set came before mock-1, which has no recorded time") {
		t.Fatalf("served %v miss %+v consumed %d; want the call refused, naming mock-1's missing recorded time", got, miss, len(db.consumed))
	}
}

// A mock F served before the test is a place of every rule where it is an
// earlier place of the call's own test (the previous test's, the start-up's,
// between tests': ofPairedTest) recorded before it, read as F served it, at
// the first and the last of its serves; every other mock F served in the set
// (another recorded test's, one recorded after the call, an earlier test's)
// is read only for the offset rule, as F served it, and only when that rule
// asks.
func TestEarlierRawReadsTheOwnTestsPlacesAndLeavesTheSetToTheOffsetRule(t *testing.T) {
	own := learnRecorded("mock-0", 0, "GET", "/a", "", nil, `{}`)
	pool := learnRecorded("mock-1", time.Millisecond, "GET", "/b", "", nil, `{}`)
	other := learnRecorded("mock-2", 2*time.Millisecond, "GET", "/c", "", nil, `{}`)
	paired := learnRecorded("mock-3", 3*time.Millisecond, "GET", "/d", "", nil, `{}`)
	late := learnRecorded("mock-4", 4*time.Millisecond, "GET", "/e", "", nil, `{}`)
	earliest := learnRecorded("mock-5", -time.Millisecond, "GET", "/f", "", nil, `{}`)
	db := &learnDb{pool: map[time.Time]bool{pool.Spec.ReqTimestampMock: true}, otherTest: map[time.Time]bool{other.Spec.ReqTimestampMock: true}}
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(earliest) // 1: two tests before
	tr.Begin()
	for _, m := range []*models.Mock{own, pool, other, late, pool, paired} { // 2..7: the previous test
		tr.ServedMock(m)
	}
	tr.Begin()
	p := tr.Placed()
	given := ofPairedTest(p.Before, paired, db, true)
	if !reflect.DeepEqual(given, []*models.Mock{own, pool, late, paired}) {
		t.Fatalf("given %v", given)
	}
	raw, err := earlierRaw(learn.NewDriftCache(), paired, p.Served, given, nil, p.Order, nil, nil, learn.NewPreviousCall(paired))
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw %v err %v; want mock-0 and mock-1 alone", raw, err)
	}
	for i, want := range [][2]int{{2, 2}, {3, 6}} {
		r := raw[i]
		if f, l := r.Span(); !r.Served() || f != want[0] || l != want[1] || r.AnotherTests() {
			t.Fatalf("raw[%d] %s: served %v in %d..%d, offset-only %v; want served in %v", i, r.Mock(), r.Served(), f, l, r.AnotherTests(), want)
		}
	}
	more, err := givenEarlier(paired, p.Set, p.Served, given, nil, p.Order)()
	if err != nil || len(more) != 4 {
		t.Fatalf("given earlier %v err %v; want mock-5, mock-2, mock-4 and the paired mock-3", more, err)
	}
	for i, want := range []struct {
		name string
		span [2]int
	}{{"mock-5", [2]int{1, 1}}, {"mock-2", [2]int{4, 4}}, {"mock-4", [2]int{5, 5}}, {"mock-3", [2]int{7, 7}}} {
		r := more[i]
		if f, l := r.Span(); r.Mock() != want.name || f != want.span[0] || l != want.span[1] || !r.Read() {
			t.Fatalf("given earlier[%d] %s: served in %d..%d, read %v; want %s served in %v", i, r.Mock(), f, l, r.Read(), want.name, want.span)
		}
	}
	// A paired mock F served in the test itself, before the call, is read
	// by the offset rule alone as well; one F never served is not.
	tr.ServedMock(paired) // 8
	p = tr.Placed()
	more, err = givenEarlier(paired, p.Set, p.Served, given, nil, p.Order)()
	if err != nil || len(more) != 4 || more[3].Mock() != "mock-3" {
		t.Fatalf("given earlier %v err %v; want the paired mock-3 among them", more, err)
	}
	if f, l := more[3].Span(); f != 7 || l != 8 {
		t.Fatalf("the paired mock-3 served in %d..%d; want 7..8", f, l)
	}
	unserved := learnRecorded("mock-6", 5*time.Millisecond, "GET", "/g", "", nil, `{}`)
	more, err = givenEarlier(unserved, p.Set, p.Served, given, nil, p.Order)()
	for _, r := range more {
		if r.Mock() == "mock-6" || err != nil {
			t.Fatalf("given earlier %v err %v; want no place of a paired mock F never served", more, err)
		}
	}
}

// learn.InServeOrder reads each leafed exchange's payload as F served it in the
// set (the read's Order: its first and last serve), leaves one the served
// mocks do not hold, or F's order does not number, in an order keploy cannot
// tell, and leaves the round's slice as it was.
func TestInServeOrderNumbersTheLeafedPayloadsByTheServedMocks(t *testing.T) {
	a, b, c := learnRecorded("mock-0", 0, "GET", "/a", "", nil, `{}`), learnRecorded("mock-1", time.Millisecond, "GET", "/b", "", nil, `{}`), learnRecorded("mock-2", 2*time.Millisecond, "GET", "/c", "", nil, `{}`)
	earlier := []learn.Earlier{
		{Mock: a.Name, At: a.Spec.ReqTimestampMock, Payload: learn.RawOf(a, true)},
		{Mock: b.Name, At: b.Spec.ReqTimestampMock, Payload: learn.RawOf(b, true)},
		{Mock: c.Name, At: c.Spec.ReqTimestampMock, Payload: learn.RawOf(c, true)},
		{Given: true},
	}
	tr := learn.NewTracker()
	tr.ServedMock(b)
	tr.ServedMock(a)
	tr.ServedMock(b)
	got := learn.InServeOrder(earlier, []*models.Mock{nil, b, a, b, c}, tr.Placed().Order)
	var spans [][2]int
	for _, e := range got {
		f, l := e.Payload.Span()
		spans = append(spans, [2]int{f, l})
	}
	if want := [][2]int{{2, 2}, {1, 3}, {0, 0}, {0, 0}}; !reflect.DeepEqual(spans, want) {
		t.Fatalf("payloads served in %v; want %v", spans, want)
	}
	for i, e := range earlier {
		if e.Payload.Nth() != 0 {
			t.Fatalf("the round's earlier[%d] was changed: %d", i, e.Payload.Nth())
		}
	}
}

// At start-up F's clock runs the lead past the earliest recording from when
// keploy froze it, before the app booted: a start-up call's clock read moved
// by the lead and the boot's difference, which the lead alone does not read.
// Its words are read one by one, the value whole or inside a longer one
// ("at <instant>") alike, and the window, its provenance, decides: read in
// the call's window (the receive served before it, its arrival, on the app's
// clock) it is a clock read, learned as noise, whether the boot took 2 s or
// 30 s longer; read by the lead alone (no app clock, or a test's own call,
// which is read so: F sets each test's clock), nothing shows it is one, and
// the minute and second that changed are words of the receive's Date header,
// a value that may have flowed from the receive: refused, naming it.
func TestLearnMatchAStartUpClockReadIsReadInItsCallsWindow(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	recRead := learnAt.Add(3 * time.Millisecond)
	for _, form := range []struct {
		name string
		of   func(time.Time) string
	}{
		{"inside a longer value", func(t time.Time) string { return "at " + iso(t) }},
		{"one instant whole", iso},
	} {
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
			t.Run(form.name+", "+tc.name, func(t *testing.T) {
				h, n, opts := learnOn(t)
				opts.LearnPool = true
				receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
				receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
				event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","processed_at":"`+form.of(recRead)+`"}`, nil, `{}`)
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
				in := live("POST", "/v1/events", `{"event":"m-1","processed_at":"`+form.of(served.Add(offset+time.Millisecond))+`"}`, nil)
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
				var learned []models.Mock
				for _, m := range db.pooled {
					learned = append(learned, *m)
				}
				if tc.own {
					learned = db.consumed
				}
				// A read of the clock inside a longer value stays noise; one
				// that is an instant whole is still compared, in its call's
				// window at every replay (MockSpec.Clocks).
				ok := len(learned) == 1
				if ok && form.name == "one instant whole" {
					ok = learnedClockRead(learned[0].Spec, "body.processed_at")
				} else if ok {
					ok = reflect.DeepEqual(learned[0].Spec.ReqBodyNoise, map[string][]string{"body.processed_at": {}}) && len(learned[0].Spec.Clocks) == 0
				}
				if got == nil || miss != nil || !ok {
					t.Fatalf("served %v miss %+v learned %+v; want body.processed_at learned (%s)", got, miss, learned, form.name)
				}
			})
		}
	}
}

// keploy mock's client tests (a Playwright or CLI test drives the app; no
// test case's request reaches keploy): the app stamps its clock into the
// first call of a test, which the test's begin moved the app's clock for
// (its recorded begin plus the lead). The call's window runs from the begin
// on F's clock, and from the recording's previous call, and holds the stamp
// in both runs: noise, and the replay after it answers the call with a
// third stamp. A slot the harness gave the app that moved a minute on, by
// about the lead, lies in neither span: refused, saying no clock read by the
// app matched it; in keploy test, where a test's own call has no window, it
// is one instant whole, learned as a read of the clock, which the replay
// after it (record's confirming round) refuses, naming the field: it is no
// read of the clock, and record filters it there. With no app clock (no clock hooks) nothing shows the stamp is
// a read either: refused, saying keploy saw no read of the app's clock for
// the call. Where the recording has no call before the test's (its first
// data-plane call), the call's window in the recording runs from the test's
// recorded begin (learn.Tracker.RecordedBegin), and the stamp is noise; with
// no recorded begin given it has none, and is refused so.
func TestLearnMatchAClientTestLearnsTheAppsStampInItsWindowOnly(t *testing.T) {
	iso := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	recBegin := learnAt.Add(time.Second)
	recRead := learnAt.Add(1995 * time.Millisecond) // 5 ms before the recorded call
	base := learnAt.Add(10 * time.Minute)           // F's boot call, on the real clock
	moved := base.Add(time.Second)                  // the begin moved the app's clock
	began := moved.Add(time.Millisecond)
	before := learn.FullRoundLead - 10*time.Minute
	after := recBegin.Add(learn.FullRoundLead).Sub(began)
	appClock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r.Add(before), true
		}
		return r.Add(after), true
	}
	read := began.Add(994 * time.Millisecond) // the app's read in F, on the real clock
	arrival := began.Add(time.Second)
	for _, tc := range []struct {
		name         string
		client, hook bool
		rec, f       string // the field and its value, recorded and in F
		noise        bool
		// first: the recording has no call before the test's (no boot
		// staged), and the test's recorded begin is given or not (begun).
		first, begun bool
		windowed     bool // the refusal's call had a window
	}{
		{"the app's stamp", true, true, `"at":"` + iso(recRead) + `"`, `"at":"` + iso(read.Add(after)) + `"`, true, false, false, true},
		{"a slot the harness gave, a minute on", true, true, `"slot":"2026-10-14T10:05Z"`, `"slot":"2026-10-14T10:06Z"`, false, false, false, true},
		{"the slot in keploy test", false, true, `"slot":"2026-10-14T10:05Z"`, `"slot":"2026-10-14T10:06Z"`, true, false, false, true},
		{"the app's stamp, no clock hooks", true, false, `"at":"` + iso(recRead) + `"`, `"at":"` + iso(read.Add(after)) + `"`, false, false, false, false},
		{"the app's stamp in the recording's first call", true, true, `"at":"` + iso(recRead) + `"`, `"at":"` + iso(read.Add(after)) + `"`, true, true, true, true},
		{"the app's stamp in the recording's first call, no recorded begin", true, true, `"at":"` + iso(recRead) + `"`, `"at":"` + iso(read.Add(after)) + `"`, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			boot := learnRecorded("mock-0", 0, "POST", "/v1/flags", `{"app":"web"}`, nil, `{"on":true}`)
			signup := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/events", `{"event":"signup",`+tc.rec+`}`, nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{signup}, staged: []*models.Mock{boot}, pool: map[time.Time]bool{boot.Spec.ReqTimestampMock: true}}
			if tc.first {
				db = &learnDb{perTest: []*models.Mock{signup}}
			}
			tr := learn.NewTracker()
			tr.SetClientTests(tc.client)
			if tc.hook {
				tr.SetAppClock(appClock)
			}
			tr.ServedMockAt(boot, base)
			// Served as recorded, as its adapter says.
			tr.ServedMockAsRecorded(boot)
			if tc.begun {
				tr.RecordedBegin(recBegin)
			}
			tr.BeginAt(began)
			in := live("POST", "/v1/events", `{"event":"signup",`+tc.f+`}`, nil)
			in.arrived = arrival
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			field := "body.at"
			if strings.HasPrefix(tc.rec, `"slot"`) {
				field = "body.slot"
			}
			if !tc.noise {
				head := field + " changed, and no clock read by the app matched it (an instant, its characters 1 to "
				if !tc.windowed {
					head = field + " changed, and keploy saw no read of the app's clock for its call (an instant, its characters 1 to "
				}
				if got != nil || miss == nil || !strings.Contains(miss.res.Why, head) || !strings.Contains(miss.res.Why, "If your test supplies this value, have it supply the same value in every run") {
					t.Fatalf("served %v miss %+v; want %s refused, saying keploy saw no read of the app's clock that matched it", got, miss, field)
				}
				return
			}
			// The app's stamp, an instant whole, is a read of the clock, still
			// compared; so is the slot in keploy test, one instant whole in a
			// test's own call, which has no window: never noise.
			if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, field) {
				t.Fatalf("served %v miss %+v consumed %+v; want %s learned as a read of the clock", got, miss, db.consumed, field)
			}
			// The replay after record's full round: a third value is
			// answered, a stamp read in its own call's window.
			at := read.Add(after + 90*time.Second)
			third := `"at":"` + iso(at) + `"`
			if field == "body.slot" {
				third = `"slot":"2026-10-14T10:09Z"`
			}
			call := live("POST", "/v1/events", `{"event":"signup",`+third+`}`, nil)
			call.arrived = at.Add(time.Millisecond)
			if field == "body.slot" {
				// The slot is no read of the clock: the next replay, record's
				// confirming round's first, refuses it, naming the field and
				// why, so record filters the test there.
				db := &varDb{perTest: learnedOnto([]*models.Mock{signup}, db.consumed)}
				ok, _, diag := matchVarIn(t, clockConn(true, at.Add(-time.Second)), newHTTP(), db, call)
				why := ""
				for _, f := range diag.differs {
					if f.shape == field {
						why = f.why
					}
				}
				if ok || !strings.HasPrefix(why, field+" was learned as ") || !strings.Contains(why, "it changed") {
					t.Fatalf("the replay of the slot: answered %v, differs %v (%q); want it refused, naming %s and why", ok, differs(diag), why, field)
				}
				return
			}
			inCIIn(t, clockConn(true, at.Add(-time.Second)), learnedOnto([]*models.Mock{signup}, db.consumed), []*req{call})
		})
	}
}

// A window is one run of F's clock: across a re-write of the app's clock
// between the previous serve and the arrival (a test's begin), there is
// none, and an instant a served recorded answer carries (another receive's
// Date header, echoed into the event) is refused as with one offset, not
// read as a clock read in a span minutes wide.
// A clock read the learn step learns onto a recording that already carries
// one at another place (an earlier round's, kept on the recording) is added
// to it: the consumed state carries both, so neither place goes back to
// being compared as recorded, or unchecked, in the confirm round.
func TestLearnMatchAClockReadIsAddedToTheRecordingsOwn(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
	receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
	event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(learnAt.Add(3*time.Millisecond))+`"}`, nil, `{}`)
	event.TestModeInfo.Lifetime = models.LifetimeSession
	own := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond}
	event.Spec.Clocks = map[string]models.ClockOffset{"body.seen_at": own}
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{event}}, staged: []*models.Mock{receive}, pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true}}
	tr := learn.NewTracker()
	offset := learn.FullRoundLead + 2*time.Second - 40*time.Second
	tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
	served := learnAt.Add(40 * time.Second)
	tr.ServedMockAt(receive, served)
	in := live("POST", "/v1/events", `{"event":"m-1","processed_at":"`+iso(served.Add(offset+time.Millisecond))+`"}`, nil)
	in.arrived = served.Add(2 * time.Millisecond)
	got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
	if err != nil || got == nil || miss != nil || len(db.pooled) != 1 {
		t.Fatalf("served %v miss %+v err %v pooled %d; want served, body.processed_at learned", got, miss, err, len(db.pooled))
	}
	for _, m := range db.pooled {
		if !learnedClockRead(m.Spec, "body.processed_at") || m.Spec.Clocks["body.seen_at"] != own {
			t.Fatalf("the consumed copy carries clock reads %+v; want body.processed_at learned and the recording's body.seen_at (%s) kept", m.Spec.Clocks, own)
		}
	}
	if event.Spec.Clocks["body.processed_at"] != (models.ClockOffset{}) {
		t.Fatal("the learn step wrote onto the pooled recording itself")
	}
}

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
// elsewhere, is no clock read of the call, and is refused, the value whole or
// inside a longer one alike: its words, read one by one, are words of the
// receive's Date header.
func TestLearnMatchTheWindowsPreviousCallMayBeOnDisk(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	for _, whole := range []bool{false, true} {
		form := func(t time.Time) string { return "at " + iso(t) }
		if whole {
			form = iso
		}
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
			t.Run(fmt.Sprintf("%s, one instant whole %v", tc.name, whole), func(t *testing.T) {
				h, n, opts := learnOn(t)
				opts.LearnPool = true
				receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
				receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
				ack := learnRecorded("mock-2", 2*time.Millisecond, "POST", "/queue/ack", `{"id":"135dcb85"}`, nil, `{}`)
				event := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/events", `{"event":"m-1","processed_at":"`+form(learnAt.Add(time.Millisecond))+`"}`, nil, `{}`)
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
				in := live("POST", "/v1/events", `{"event":"m-1","processed_at":"`+form(served.Add(offset+time.Millisecond))+`"}`, nil)
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
}

// A pool mock of no recorded time follows no recording the store can name:
// keploy cannot tell where its values came from, and refuses it.
func TestLearnMatchAPoolMockOfNoRecordedTimeIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	ctx := learn.WithTracker(context.Background(), learn.NewTracker())
	boot := learnRecorded("boot-0", 0, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
	boot.Spec.ReqTimestampMock = time.Time{}
	db := &umDb{mockMemDb: mockMemDb{mocks: []*models.Mock{boot}}, pool: map[string]bool{"boot-0": true}, known: true}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
	if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "which has no recorded time") {
		t.Fatalf("served %v miss %+v err %v; want refused, no recording is known to precede it", served, miss, err)
	}
}

// A mock the store shows and will not hand out (a session-lifetime mock in
// the per-test tier: its claim updates the session tier, which does not hold
// it) ends the call as a miss: looking again finds it, as it was, and its
// claim would fail forever. Exactly matched or paired, the call returns. The
// harness bounds the wait and cancels the call's context, which the learn
// step reads on each pass, so a call that spins is stopped.
func TestLearnMatchAMockItsStoreWillNotHandOutIsAMissNotAHang(t *testing.T) {
	for _, tc := range []struct{ name, live string }{
		{"matched exactly", `{"id":"o-1"}`},
		{"paired", `{"id":"o-2"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			own := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"o-1"}`, nil, `{"ok":true}`)
			own.TestModeInfo.Lifetime = models.LifetimeSession
			db := &learnDb{perTest: []*models.Mock{own}}
			ctx, cancel := context.WithCancel(learn.WithTracker(context.Background(), tr))
			defer cancel()
			type result struct {
				served *models.Mock
				miss   *learnMiss
				err    error
			}
			done := make(chan result, 1)
			go func() {
				served, miss, err := h.learnMatch(ctx, live("POST", "/v1/orders", tc.live, nil), db, n, opts)
				done <- result{served, miss, err}
			}()
			select {
			case r := <-done:
				if r.err != nil || r.served != nil || r.miss == nil || !strings.Contains(r.miss.res.Why, "would not hand out mock-0") {
					t.Fatalf("served %v miss %+v err %v; want a miss: the store would not hand out mock-0", r.served, r.miss, r.err)
				}
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("the learn step kept looking again at a mock its store will not hand out")
			}
		})
	}
}

// flakyClaimDb is learnDb whose first consume is lost to a change of the
// store (another call took the mock and gave it back, or a test's begin
// closed the view the scope was read from): the store's revision moves.
type flakyClaimDb struct {
	*learnDb
	lost bool
	rev  uint64
}

func (d *flakyClaimDb) Revision() uint64 { return d.rev }
func (d *flakyClaimDb) DeleteFilteredMock(m models.Mock) bool {
	if !d.lost {
		d.lost = true
		d.rev++
		return false
	}
	return d.learnDb.DeleteFilteredMock(m)
}

// A claim lost while the store changed is looked at again, and the call
// answered from what the store shows then, matched exactly or paired.
func TestLearnMatchAClaimLostToAStoreChangeIsLookedAtAgain(t *testing.T) {
	for _, tc := range []struct{ name, live string }{
		{"matched exactly", `{"id":"o-1"}`},
		{"paired", `{"id":"o-2"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			own := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"o-1"}`, nil, `{"ok":true}`)
			db := &flakyClaimDb{learnDb: &learnDb{perTest: []*models.Mock{own}}}
			served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/orders", tc.live, nil), db, n, opts)
			if err != nil || served == nil || !db.lost {
				t.Fatalf("served %v miss %+v err %v lost %v; want answered on the second look", served, miss, err, db.lost)
			}
		})
	}
}

// swapDuringScopeDb is learnDb whose view is swapped while the learn step's
// first pass reads its scope (a test's begin landing between them): that
// pass's claim, against the view it read, fails, and the next view shows the
// same mock again, which a claim then takes.
type swapDuringScopeDb struct {
	*learnDb
	reads int
	rev   uint64
	lost  bool
}

func (d *swapDuringScopeDb) Revision() uint64 { return d.rev }
func (d *swapDuringScopeDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.reads++
	if d.reads == 1 {
		d.rev++
	}
	return d.learnDb.GetPerTestMocksInWindow()
}
func (d *swapDuringScopeDb) DeleteFilteredMock(m models.Mock) bool {
	if !d.lost {
		d.lost = true
		return false
	}
	return d.learnDb.DeleteFilteredMock(m)
}

// The store's revision a failed claim is judged by is read before the pass
// reads its scope: a view swapped while the scope was read is a change of the
// store since that revision, so the call looks again, and is answered, rather
// than missing at a test's boundary.
func TestLearnMatchAViewSwappedWhileTheScopeWasReadIsLookedAtAgain(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	own := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"o-1"}`, nil, `{"ok":true}`)
	db := &swapDuringScopeDb{learnDb: &learnDb{perTest: []*models.Mock{own}}}
	served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/orders", `{"id":"o-2"}`, nil), db, n, opts)
	if err != nil || served == nil || !db.lost {
		t.Fatalf("served %v miss %+v err %v lost %v; want answered on the second look", served, miss, err, db.lost)
	}
}

// A departure the app picks from a timetable the learn step served, by its
// clock, stays checked end to end: the timetable is an exchange the test was
// served leaf by leaf (an Earlier, no Raw), and the moment F's booking names
// is one of its departures, so the booking is refused as a value an answer
// gave, never served with its departure learned as noise. So it is with a
// zoned booking from a timetable of local times of day. A booking the
// timetable did not list at record is refused too.
func TestLearnMatchKeepsAPickFromAServedTimetableChecked(t *testing.T) {
	for _, tc := range []struct {
		name, table, rec, live, why string
	}{
		{"a time of day", `{"date":"2026-10-14","departures":["10:00","10:01","10:02","10:03"]}`, `{"departure":"2026-10-14 10:01"}`, `{"departure":"2026-10-14 10:02"}`, "answered earlier in the test"},
		{"an instant", `{"departures":["2026-10-14T10:00:00Z","2026-10-14T10:01:00Z","2026-10-14T10:02:00Z"]}`, `{"departure":"2026-10-14T10:01:00Z"}`, `{"departure":"2026-10-14T10:02:00Z"}`, "answered earlier in the test"},
		{"a zoned instant", `{"date":"2026-10-14","departures":["15:30","15:31","15:32","15:33"]}`, `{"departure":"2026-10-14T15:31+05:30"}`, `{"departure":"2026-10-14T15:32+05:30"}`, "answered earlier in the test"},
		{"one unlisted at record", `{"date":"2026-10-14","departures":["10:00","10:02","10:03"]}`, `{"departure":"2026-10-14 10:01"}`, `{"departure":"2026-10-14 10:02"}`, "in mock-0 at body.departures[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "GET", "/v1/timetable", "", nil, tc.table),
				learnRecorded("mock-1", time.Second, "POST", "/v1/book", tc.rec, nil, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/timetable", "", nil), db, n, opts); served == nil {
				t.Fatalf("the timetable: %+v", miss)
			}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/book", tc.live, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil {
				t.Fatalf("FALSE GREEN: the pick from the served timetable was served, learned %v", db.consumed[len(db.consumed)-1].Spec.ReqBodyNoise)
			}
			if miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "body.departure") || !strings.Contains(miss.res.Why, tc.why) {
				t.Fatalf("refused as %s: %q; want request_changed naming body.departure and %q", miss.res.Reason, miss.res.Why, tc.why)
			}
		})
	}
}

// An exchange's Date header, within the span the exchange was recorded in,
// is its server's clock read as it answered: a later value whose changed
// word it holds by coincidence is no value it gave, and is learned as noise
// end to end. A Date header outside that span (the server's clock ahead) is
// searched as any leaf is, and refuses it.
func TestLearnMatchReadsAServedDateHeaderAsItsAnswersStamp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answered time.Duration
		served   bool
	}{
		{"within its span", 300 * time.Millisecond, true},
		{"after its answer", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			status := learnRecorded("mock-0", 0, "GET", "/v1/status", "", nil, `{"ok":true}`)
			status.Spec.HTTPResp.Header["Date"] = learnAt.Format(http.TimeFormat)
			status.Spec.ReqTimestampMock = learnAt.Add(-200 * time.Millisecond)
			status.Spec.ResTimestampMock = learnAt.Add(tc.answered)
			db := &learnDb{perTest: []*models.Mock{
				status,
				learnRecorded("mock-1", time.Second, "POST", "/v1/log", `{"batch":"batch-00"}`, nil, `{"ok":true}`),
			}, observe: tr.ServedMock}
			if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/status", "", nil), db, n, opts); served == nil {
				t.Fatalf("the status: %+v", miss)
			}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/log", `{"batch":"batch-07"}`, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.served && (served == nil || !reflect.DeepEqual(db.consumed[len(db.consumed)-1].Spec.ReqBodyNoise, map[string][]string{"body.batch": {}})) {
				t.Fatalf("served %v, miss %+v; want body.batch learned as noise", served != nil, miss)
			}
			if !tc.served && (served != nil || !strings.Contains(miss.res.Why, "mock-0 at header.date")) {
				t.Fatalf("served %v, miss %+v; want it refused, naming mock-0's header.date", served, miss)
			}
		})
	}
}

// The call's own answer's Date header, within the span the exchange was
// recorded in, is its server's clock read as it answered the call: a word
// of the call's drifted value that it holds by coincidence (the second of a
// sent_at that no lead or window shows a clock read) is no echo, and the
// value, one instant whole in a call with no window, is learned as a read of
// the clock, which a replay checks in its call's own span. A Date header outside that span is read as any
// leaf of the answer, and refuses it.
func TestLearnMatchReadsItsOwnAnswersDateAsItsStamp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answered time.Duration
		served   bool
	}{
		{"within its span", 1500 * time.Millisecond, true},
		{"before its Date", 900 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			send := learnRecorded("mock-0", time.Second, "POST", "/v1/send", `{"sent_at":"2026-10-14T10:00:01Z"}`, nil, `{"ok":true}`)
			send.Spec.HTTPResp.Header["Date"] = "Wed, 14 Oct 2026 10:00:01 GMT"
			send.Spec.ResTimestampMock = learnAt.Add(tc.answered)
			db := &learnDb{perTest: []*models.Mock{send}, observe: tr.ServedMock}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/send", `{"sent_at":"2026-10-14T10:01:04Z"}`, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.served && (served == nil || !learnedClockRead(db.consumed[len(db.consumed)-1].Spec, "body.sent_at")) {
				t.Fatalf("served %v, miss %+v; want body.sent_at learned as a read of the clock", served != nil, miss)
			}
			if !tc.served && (served != nil || !strings.Contains(miss.res.Why, "comes back in the answer at header.date")) {
				t.Fatalf("served %v, miss %+v; want it refused, the answer's header.date holding a word of it", served != nil, miss)
			}
		})
	}
}

// In keploy mock's client tests a variable keploy binds carries no clock
// evidence (models.MockVariable.Clock): the lead shows no read of the app's
// clock there, so a value the test harness gave the application that the
// lead moved, followed into a later call, could otherwise share its live
// value with another variable in a replay (models.ReadInOneSecond). The
// app's stamp, noise in its window at the first call and sent again in a
// later one, is that call's variable bound at the first, with no clock
// evidence; in keploy test the same bind carries the read the lead moved.
func TestLearnMatchAClientTestsBindCarriesNoClockEvidence(t *testing.T) {
	iso := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	recBegin := learnAt.Add(time.Second)
	recRead := learnAt.Add(1995 * time.Millisecond) // 5 ms before the recorded call
	base := learnAt.Add(10 * time.Minute)           // F's boot call, on the real clock
	moved := base.Add(time.Second)                  // the begin moved the app's clock
	began := moved.Add(time.Millisecond)
	before := learn.FullRoundLead - 10*time.Minute
	after := recBegin.Add(learn.FullRoundLead).Sub(began)
	appClock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r.Add(before), true
		}
		return r.Add(after), true
	}
	read := began.Add(994 * time.Millisecond) // the app's read in F, on the real clock
	arrival := began.Add(time.Second)
	for _, client := range []bool{true, false} {
		t.Run(map[bool]string{true: "a client test", false: "keploy test"}[client], func(t *testing.T) {
			h, n, opts := learnOn(t)
			boot := learnRecorded("mock-0", 0, "POST", "/v1/flags", `{"app":"web"}`, nil, `{"on":true}`)
			signup := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/events", `{"event":"signup","at":"`+iso(recRead)+`"}`, nil, `{}`)
			list := learnRecorded("mock-2", 3*time.Second, "GET", "/v1/events?since="+url.QueryEscape(iso(recRead)), "", nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{signup, list}, staged: []*models.Mock{boot}, pool: map[time.Time]bool{boot.Spec.ReqTimestampMock: true}}
			tr := learn.NewTracker()
			tr.SetClientTests(client)
			tr.SetAppClock(appClock)
			tr.ServedMockAt(boot, base)
			tr.BeginAt(began)
			ctx := learn.WithTracker(context.Background(), tr)
			fAt := iso(read.Add(after))
			in := live("POST", "/v1/events", `{"event":"signup","at":"`+fAt+`"}`, nil)
			in.arrived = arrival
			if got, miss, err := h.learnMatch(ctx, in, db, n, opts); err != nil || got == nil || miss != nil {
				t.Fatalf("the stamp's call: served %v miss %+v err %v; want it served, the stamp noise", got, miss, err)
			}
			in = live("GET", "/v1/events?since="+url.QueryEscape(fAt), "", nil)
			in.arrived = arrival.Add(time.Second)
			if got, miss, err := h.learnMatch(ctx, in, db, n, opts); err != nil || got == nil || miss != nil {
				t.Fatalf("the later call: served %v miss %+v err %v", got, miss, err)
			}
			binds := db.consumed[1].Spec.LearnedBinds()["mock-1"]
			if len(binds) != 1 {
				t.Fatalf("binds %+v; want one variable bound at mock-1", db.consumed[1].Spec.LearnedBinds())
			}
			for name, v := range binds {
				if v.Bind != "body.at" || (v.Clock == nil) != client {
					t.Fatalf("variable %s bound %+v; want at body.at, with clock evidence only in keploy test", name, v)
				}
			}
		})
	}
}

// A call that reaches the learn step after keploy test opened its test's
// window and before the filter that opens the test brought its test case's
// request waits for the request, and is learned from as a call made after it:
// its nonce that drifted is noise either way, and one the request holds a
// word of is refused, naming the request, either way. Each order is forced
// through the tracker's OnAwait, through no clock, 200 times each, from a
// store read one read after another and from one read in one snapshot of the
// round (integrations.RoundReader), as the agent's MockManager is under exact
// serving: there the wait is the snapshot's (RoundSnapshot.Learn.Awaiting),
// and a pass that dropped it decided the call at once, against a request it
// could not read.
func TestLearnMatchACallInTheGapIsLearnedAsOneAfterTheRequest(t *testing.T) {
	h, n, opts := learnOn(t)
	request := func(q string) *models.TestRequest {
		return &models.TestRequest{Test: "test-2", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"q":"` + q + `"}`, Sent: `{"q":"` + q + `"}`}}}
	}
	call := live("POST", "/v1/nonce", `{"nonce":"n-77c0d3"}`, nil)
	outcome := func(served *models.Mock, miss *learnMiss, err error) string {
		switch {
		case err != nil:
			return "error " + err.Error()
		case miss != nil:
			return "miss " + miss.res.Why
		}
		return "served " + served.Name
	}
	stores := []struct {
		name string
		of   func(*learnDb) integrations.MockMemDb
	}{
		{"read one after another", func(ld *learnDb) integrations.MockMemDb { return ld }},
		{"read in one snapshot of the round", func(ld *learnDb) integrations.MockMemDb {
			return &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ld}, test: "test-2"}
		}},
	}
	for _, store := range stores {
		db := func() integrations.MockMemDb {
			return store.of(&learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/nonce", `{"nonce":"n-5f2a91"}`, nil, `{"ok":true}`)}})
		}
		for _, q := range []string{"books", "n-5f2a91"} {
			for i := 0; i < 200; i++ {
				// The request first.
				tr := learn.NewTracker()
				tr.BeginAwaiting()
				tr.Give(learn.GivenRequest(request(q)))
				after := outcome(h.learnMatch(learn.WithTracker(context.Background(), tr), call, db(), n, opts))
				if q == "books" && after != "served mock-0" || q != "books" && !strings.Contains(after, "test case test-2 at body.q") {
					t.Fatalf("%s, q %s, after the request: %s", store.name, q, after)
				}
				// The call first, in the gap.
				tr = learn.NewTracker()
				waiting := make(chan struct{}, 1)
				tr.OnAwait(func() {
					select {
					case waiting <- struct{}{}:
					default:
					}
				})
				tr.BeginAwaiting()
				gapDb := db()
				got := make(chan string, 1)
				go func() {
					got <- outcome(h.learnMatch(learn.WithTracker(context.Background(), tr), call, gapDb, n, opts))
				}()
				var before string
				select {
				case <-waiting:
					tr.Give(learn.GivenRequest(request(q)))
					before = <-got
				case before = <-got:
					tr.Give(learn.GivenRequest(request(q)))
				}
				if before != after {
					t.Fatalf("%s, q %s: in the gap %s, after the request %s: two outcomes by where the call fell", store.name, q, before, after)
				}
			}
		}
	}
}

// A call that waited for its test case's request is decided in the round it
// was made in, or in none: once the test's window closed without the request
// (End, the next test begun, awaiting its own request or not, the next set
// begun), the call misses, saying where its first pass read it and that the
// replay ended that test, and is never learned from against the next test's
// places, nor left to wait for, and be decided by, the next test's request.
// One whose own request came is learned from as before. Each close is forced
// through the tracker's OnAwait, through no clock, 200 times, from a store
// read one read after another and from one read in one snapshot of the
// round (integrations.RoundReader), showing the test's view or its pool
// view, the tracker's test not installed: there the call is the pool's
// (integrations.RoundSnapshot.PoolsCall), and still waits for its request,
// then is decided in the round between tests, where mock-0, a test's own
// recording, is no recording it may learn onto.
func TestLearnMatchACallThatWaitedIsDecidedInItsOwnTestsRound(t *testing.T) {
	h, n, opts := learnOn(t)
	request := func(test string) *models.TestRequest {
		return &models.TestRequest{Test: test, Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"q":"books"}`, Sent: `{"q":"books"}`}}}
	}
	call := live("POST", "/v1/nonce", `{"nonce":"n-77c0d3"}`, nil)
	type decided struct {
		out  string
		miss *learnMiss
	}
	outcome := func(served *models.Mock, miss *learnMiss, err error) decided {
		switch {
		case err != nil:
			return decided{out: "error " + err.Error()}
		case miss != nil:
			return decided{out: "miss " + miss.res.Why, miss: miss}
		}
		return decided{out: "served " + served.Name}
	}
	// The miss names the test the call was read in: by its view's name
	// where the store reads its round in one snapshot, as the test then
	// running where it does not.
	movedWhy := func(where, moved string) string {
		if moved == "" {
			moved = "ended that test"
		}
		return "keploy record's full round read this request " + where + ", and the replay " + moved + " before keploy could answer it"
	}
	// came is what a call whose own request came gets.
	const served = "served mock-0"
	stores := []struct {
		name, where, came, moved string
		of                       func(*learnDb) integrations.MockMemDb
	}{
		{"read one after another", "in the test then running", served, "", func(ld *learnDb) integrations.MockMemDb { return ld }},
		{"read in one snapshot of the round", "in test-2", served, "", func(ld *learnDb) integrations.MockMemDb {
			return &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ld}, test: "test-2"}
		}},
		// The store shows its pool view while the tracker awaits the
		// test's request: the pool's call
		// (integrations.RoundSnapshot.PoolsCall), which never waits for
		// the store (this one tells no move: no integrations.RoundWaiter),
		// only for its request, then is decided in the round between
		// tests, where mock-0 is no recording it may learn onto. Any close
		// of the window is the round between tests moving on.
		{"read in one snapshot of the round, the store on its pool view", "in the round between tests, as keploy's learn tracker began the next test",
			"miss its closest recorded call is not one the learn step may learn onto (another test's, or the pool's outside a full record): body.nonce changed",
			"moved on to another test or test set",
			func(ld *learnDb) integrations.MockMemDb {
				return &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ld}}
			}},
	}
	closes := []struct {
		name  string
		close func(*learn.Tracker)
		moved bool
	}{
		{"its own request came", func(tr *learn.Tracker) { tr.Give(learn.GivenRequest(request("test-2"))) }, false},
		{"its window ended unread", (*learn.Tracker).End, true},
		{"the next test began", (*learn.Tracker).Begin, true},
		{"the next test began awaiting its own request", (*learn.Tracker).BeginAwaiting, true},
		{"the next set began", (*learn.Tracker).NewSet, true},
	}
	for _, store := range stores {
		for _, c := range closes {
			want := store.came
			if c.moved {
				want = "miss " + movedWhy(store.where, store.moved)
			}
			for i := 0; i < 200; i++ {
				db := store.of(&learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/nonce", `{"nonce":"n-5f2a91"}`, nil, `{"ok":true}`)}})
				tr := learn.NewTracker()
				waiting := make(chan struct{}, 2)
				tr.OnAwait(func() {
					select {
					case waiting <- struct{}{}:
					default:
					}
				})
				tr.BeginAwaiting()
				got := make(chan decided, 1)
				go func() {
					got <- outcome(h.learnMatch(learn.WithTracker(context.Background(), tr), call, db, n, opts))
				}()
				select {
				case <-waiting:
				case d := <-got:
					t.Fatalf("%s, %s: decided before its request came or its window closed: %s", store.name, c.name, d.out)
				}
				c.close(tr)
				var d decided
				select {
				case d = <-got:
				case <-waiting:
					// It waits again: for the next test's request, which
					// ends the wait here, not at the test's timeout.
					tr.Give(learn.GivenRequest(request("test-3")))
					d = <-got
					d.out = "waited for the next test's request, then " + d.out
				}
				if d.out != want {
					t.Fatalf("%s, %s: %s; want %s", store.name, c.name, d.out, want)
				}
				if !c.moved {
					continue
				}
				// The miss says where the call was read and what moved, and
				// gives no reason: nothing of the request was compared.
				r := &models.MockMismatchReport{}
				d.miss.explain(r)
				if r.Reason != "" || !strings.HasPrefix(r.NextSteps, movedWhy(store.where, store.moved)+": keploy learns nothing of a call in a round it was not made in") {
					t.Fatalf("%s, %s: the miss reads reason %q, next steps %q; want no reason, and the round that moved", store.name, c.name, r.Reason, r.NextSteps)
				}
			}
		}
	}
}

// A pass after a wait is no retake: a first pass that read test-1's view on
// the store with no test open on the learn tracker, followed by a pass, not
// read again for a refused take, on which the tracker began the test the
// view is bound to, is the round moving (the tracker began a test), not the
// tracker catching up. The same two reads on a retake are the catch-up.
// learnMatchIn never makes the first: it waits only for an open test's
// request. movedFrom's rule says so all the same.
func TestMovedFromAPassAfterAWaitIsNoCatchUp(t *testing.T) {
	first := learnRound{read: true, snap: true, epoch: 1, viewGen: 4, view: "test-1", test: 1}
	now := learnRound{read: true, snap: true, epoch: 1, viewGen: 4, view: "test-1", test: 2, open: true, agrees: true}
	if moved, caught := first.movedFrom(now, false); moved != "began a test" || caught {
		t.Fatalf("a pass after a wait read %q, caught up %v; want the move %q, no catch-up", moved, caught, "began a test")
	}
	if moved, caught := first.movedFrom(now, true); moved != "" || !caught {
		t.Fatalf("a retake read %q, caught up %v; want the tracker's catch-up", moved, caught)
	}
}

// A catch-up lands only on the test the first pass read
// (learnRound.movedFrom). The tracker catches up with the store's view only
// where it had no test open at the first pass: one that had another test's
// view open moved. The store catches up only with the tracker's same test,
// and only where the first pass found the tracker ahead of the view of the
// test before (learnRound.waits): its install of the view of the tracker's
// next test, as the tracker moved on to it, is a move. A first pass that
// found the tracker ahead of the store's pool view read the pool's call
// (learnRound.pools), of the round between tests: the store opening the
// tracker's test, another pool view, or the tracker moving on, is a move of
// that round, never a catch-up. movedFrom's rule does not rest on its
// caller.
func TestMovedFromCatchesUpOnlyWithTheFirstPassesTest(t *testing.T) {
	pool := learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 2, open: true, ahead: true}
	behind := learnRound{read: true, snap: true, epoch: 1, viewGen: 4, view: "test-1", test: 2, open: true, ahead: true}
	for _, tc := range []struct {
		name       string
		first, now learnRound
		retake     bool
		moved      string
		caught     bool
	}{
		{"a retake, the tracker that had test-1 open beginning test-2, whose view the store showed",
			learnRound{read: true, snap: true, epoch: 1, viewGen: 4, view: "test-2", test: 1, open: true},
			learnRound{read: true, snap: true, epoch: 1, viewGen: 4, view: "test-2", test: 2, open: true, agrees: true},
			true, "ended that test", false},
		// The pool's call (integrations.RoundSnapshot.PoolsCall).
		{"the pool's call, a retake, the tracker ending test-2, which it began before the store opened it",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 2, agrees: true},
			true, "moved on to another test or test set", false},
		{"the pool's call, after a wait, the store opening test-3 as the tracker moved on to it from test-2",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 6, view: "test-3", test: 3, open: true, agrees: true},
			false, "began a test", false},
		{"the pool's call, after a wait, the store opening test-2, which the tracker has open",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 6, view: "test-2", test: 2, open: true, agrees: true},
			false, "began a test", false},
		{"the pool's call, a retake, the store opening test-2, which the tracker has open",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 6, view: "test-2", test: 2, open: true, agrees: true},
			true, "began a test", false},
		{"the pool's call, the store installing a pool view after its pool view, the tracker keeping test-2",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 6, test: 2, open: true, ahead: true},
			false, "served another view of its recordings between tests", false},
		{"the pool's call, the tracker beginning test-3 before the store opened test-2",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 3, open: true, ahead: true},
			false, "moved on to another test or test set", false},
		{"the pool's call, the store's next pool view as the tracker ended test-2",
			pool, learnRound{read: true, snap: true, epoch: 1, viewGen: 6, test: 2, agrees: true},
			false, "served another view of its recordings between tests", false},
		{"the pool's call, its round unmoved",
			pool, pool, false, "", false},
		// The tracker ahead of the view of the test before
		// (learnRound.waits): the call waits for the store to open the
		// tracker's test, through any view not that test's.
		{"behind, the store installing its pool view after test-1's, the tracker keeping test-2",
			behind, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 2, open: true, ahead: true},
			false, "", false},
		{"behind, the store opening test-2, which the tracker has open",
			behind, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, view: "test-2", test: 2, open: true, agrees: true},
			false, "", true},
		{"behind, the store opening test-3 as the tracker moved on to it from test-2",
			behind, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, view: "test-3", test: 3, open: true, agrees: true},
			false, "ended that test", false},
		// Ahead at both passes, but the tracker moved on: the call's test
		// ended before the store opened it.
		{"behind, the tracker beginning test-3 before the store opened test-2",
			behind, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 3, open: true, ahead: true},
			false, "ended that test", false},
		{"behind, the tracker ending test-2 on the store's pool view",
			behind, learnRound{read: true, snap: true, epoch: 1, viewGen: 5, test: 2, agrees: true},
			false, "ended that test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if moved, caught := tc.first.movedFrom(tc.now, tc.retake); moved != tc.moved || caught != tc.caught {
				t.Fatalf("movedFrom read %q, caught up %v; want %q, caught up %v", moved, caught, tc.moved, tc.caught)
			}
		})
	}
}

// A call that waited for its test case's request in test-2, read with the
// store showing test-2's view and the tracker test-2 open, is held to that
// view: a store that then shows another test's view while the tracker keeps
// test-2 open moved the round under the call, and the call misses once its
// request came. Only where the first pass found the two apart is the
// store's install a catch-up (learnRound.movedFrom); a store that binds no
// view to the tracker's test, as this one, agrees with the tracker on
// whichever test's view it shows.
func TestLearnMatchACallIsHeldToTheViewItsFirstPassReadAgreed(t *testing.T) {
	h, n, opts := learnOn(t)
	call := live("POST", "/v1/nonce", `{"nonce":"n-77c0d3"}`, nil)
	db := &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/nonce", `{"nonce":"n-5f2a91"}`, nil, `{"ok":true}`)}}}, test: "test-2"}
	tr := learn.NewTracker()
	waiting := make(chan struct{}, 1)
	tr.OnAwait(func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	})
	tr.BeginAwaiting()
	type decided struct {
		served *models.Mock
		miss   *learnMiss
		err    error
	}
	got := make(chan decided, 1)
	go func() {
		served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), call, db, n, opts)
		got <- decided{served, miss, err}
	}()
	select {
	case <-waiting:
	case d := <-got:
		t.Fatalf("decided before its request came: %+v", d)
	}
	// The store shows test-3's view; the call's wait is the only reader,
	// and it reads after the Give below.
	db.test = "test-3"
	tr.Give(learn.GivenRequest(&models.TestRequest{Test: "test-2", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"q":"books"}`, Sent: `{"q":"books"}`}}}))
	d := <-got
	const why = "keploy record's full round read this request in test-2, and the replay ended that test before keploy could answer it"
	if d.err != nil || d.served != nil || d.miss == nil || !d.miss.moved || d.miss.res.Why != why {
		t.Fatalf("a call read in test-2's view, decided once the store showed test-3's: %+v; want the moved miss %q", d, why)
	}
}

// On the HTTP learn step, a test case's request whose body declares no media
// type, read as text by its bytes, refuses a call's value that changed and
// that it holds glued to the bytes around it (a protobuf's printable tag and
// length before it, "BA"), naming the body as one that declares no media
// type: keploy cannot tell such framing from words. The same bytes declared
// text are read by their words, which hold no word of the value: noise.
func TestLearnMatchRefusesAWordGluedInARequestBodyOfNoDeclaredType(t *testing.T) {
	h, n, opts := learnOn(t)
	call := live("POST", "/v1/nonce", `{"nonce":"n-77c0d3"}`, nil)
	body := "BAn-5f2a91" + strings.Repeat("x", 55)
	for _, row := range []struct {
		name, ct, want string
	}{
		{"declaring no media type", "", "in test case test-2 at body, glued to the bytes around it, in a body that declares no media type (no Content-Type)"},
		{"declared text", "text/plain", "served mock-0"},
	} {
		parts := []models.TestRequestPart{{Name: "body", Recorded: body, Sent: body}}
		if row.ct != "" {
			parts = append(parts, models.TestRequestPart{Name: "header.Content-Type", Recorded: row.ct, Sent: row.ct})
		}
		tr := learn.NewTracker()
		tr.BeginAwaiting()
		tr.Give(learn.GivenRequest(&models.TestRequest{Test: "test-2", Parts: parts}))
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/nonce", `{"nonce":"n-5f2a91"}`, nil, `{"ok":true}`)}}
		served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), call, db, n, opts)
		got := ""
		switch {
		case err != nil:
			got = "error " + err.Error()
		case miss != nil:
			got = "miss " + miss.res.Why
		case served != nil:
			got = "served " + served.Name
		}
		if !strings.Contains(got, row.want) {
			t.Errorf("%s: %s; want %q", row.name, got, row.want)
		}
	}
}

// An exchange served leaf by leaf carries its recording's payload
// (learn.Earlier.Payload), its answer read, for the learn step's offset rule:
// read back from the recording as the store keeps it (its answer parked on
// disk) only where a rule first reads it, and kept in the set's bounded
// store, so the exchanges a test keeps do not hold every answer's text.
func TestAnExchangeServedLeafByLeafCarriesItsPayload(t *testing.T) {
	m := learnRecorded("mock-4", 0, "POST", "/v1/receive", `{"max":1}`, nil, `{"Messages":[{"SentTimestamp":"1792667656473"}]}`)
	recorded := *m
	parked := parkedOnDisk(m, false)
	dc := learn.NewTracker().Drifts()
	e := earlierOf(dc, parked, &recorded, learn.Shape{}, learn.Shape{}, &recorded, false)
	if n := dc.Loads(); n != 0 {
		t.Fatalf("keeping the exchange loaded %d payloads; want none until a rule reads it", n)
	}
	if !e.Payload.Read() || e.Payload.Mock() != "mock-4" || !strings.Contains(e.Payload.Text(), "1792667656473") {
		t.Fatalf("payload %+v; want mock-4's, its answer read", e.Payload)
	}
	if n := dc.Loads(); n != 1 {
		t.Fatalf("reading the payload loaded %d payloads; want it read back once, into the set's store", n)
	}
}

// jsonType is a test case's request's Content-Type part declaring JSON.
var jsonType = models.TestRequestPart{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"}

// A recording made at the call's own instant (a fixture's whole second)
// that F did not serve (the scope shows it, or the store stages it) may have
// been made before the call, so it is an earlier place, read as one recorded
// a moment before the call is: the same verdict and reason, the reason
// saying it was recorded at the call's own instant. Read by no rule, a value
// copied from it would be learned as noise. One F served is read so too
// (TestLearnMatchAServedRecordingAtTheCallsOwnInstantGetsTheVerdictOfOneAMomentBefore).
func TestLearnMatchAMockRecordedAtTheCallsOwnInstantIsReadByOneRule(t *testing.T) {
	// Not served: shown by the scope, or staged by the store. Recorded at
	// the call's own instant, it is read as one recorded a moment before it.
	for _, tc := range []struct {
		name, rec, live, answer string
	}{
		{"a word copied", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"42"}`},
		{"an id copied", `{"ref":"ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323"}`, `{"ref":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`, `{"id":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`},
		{"holding the recorded word", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"7"}`},
	} {
		for _, staged := range []bool{false, true} {
			verdict := func(at time.Duration) (string, string) {
				h, n, opts := learnOn(t)
				call := learnRecorded("mock-1", time.Millisecond, "POST", "/v1/messages", tc.rec, nil, `{}`)
				other := learnRecorded("mock-2", at, "GET", "/stats", "", nil, tc.answer)
				db := &learnDb{perTest: []*models.Mock{call, other}}
				if staged {
					db = &learnDb{perTest: []*models.Mock{call}, staged: []*models.Mock{other}}
				}
				tr := learn.NewTracker()
				tr.Begin()
				got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", tc.live, nil), db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if miss == nil {
					return fmt.Sprintf("served %v", got != nil), ""
				}
				return "refused", miss.res.Why
			}
			before, beforeWhy := verdict(0)
			equal, equalWhy := verdict(time.Millisecond)
			if before != "refused" || !strings.Contains(beforeWhy, "mock-2") {
				t.Fatalf("%s (staged %v), recorded a moment before the call: %s (%s); want it refused naming mock-2", tc.name, staged, before, beforeWhy)
			}
			want := strings.Replace(beforeWhy, "mock-2 at spec.resp.body", "mock-2 at spec.resp.body (recorded at the call's own instant, so keploy reads it as given to the application before the call)", 1)
			if equal != before || want == beforeWhy || equalWhy != want {
				t.Errorf("FALSE GREEN? %s (staged %v), recorded at the call's own instant: %s (%s); want what one recorded a moment before gets: %s (%s)", tc.name, staged, equal, equalWhy, before, want)
			}
		}
	}
}

// A recording F served before the call that was made at the call's own
// instant (a fixture's whole second) is read as one made a moment before the
// call is: keploy reads the equal instant as possibly before the call, the
// fail-closed side, everywhere. So a value it holds whole, or a word or an id
// copied from it, is refused naming it wherever F served it (in the test, at
// the start-up, in the previous test, two tests back), whether the store
// still stages it or not, and the reason says it was recorded at the call's
// own instant. A value it does not hold stays noise in both. Read as an
// earlier place, a value it holds whole is followed from there; read as a
// recording made after the call too, the word rule refuses it, and the
// reason says so.
func TestLearnMatchAServedRecordingAtTheCallsOwnInstantGetsTheVerdictOfOneAMomentBefore(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, answer string
		refused                 bool
	}{
		{"holding the recorded value whole", `{"msg_id":"k1b2e4d70"}`, `{"msg_id":"k5d0e8a33"}`, `{"Messages":[{"MessageId":"k1b2e4d70"}]}`, true},
		{"holding the recorded word", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"7"}`, true},
		{"a word copied", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"42"}`, true},
		{"an id copied", `{"ref":"ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323"}`, `{"ref":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`, `{"id":"0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"}`, true},
		{"holding neither (control)", `{"ref":"7"}`, `{"ref":"42"}`, `{"count":"9"}`, false},
	} {
		for _, where := range []string{"in the test", "at the start-up", "in the previous test", "two tests back"} {
			for _, staged := range []bool{false, true} {
				name := fmt.Sprintf("%s, served %s, staged %v", tc.name, where, staged)
				verdict := func(at time.Duration) (string, string) {
					h, n, opts := learnOn(t)
					call := learnRecorded("mock-1", time.Millisecond, "POST", "/v1/messages", tc.rec, nil, `{}`)
					other := learnRecorded("mock-2", at, "POST", "/v1/receive", `{"max":10}`, nil, tc.answer)
					db := &learnDb{perTest: []*models.Mock{call}}
					if staged {
						db.staged = []*models.Mock{other}
					}
					tr := learn.NewTracker()
					switch where {
					case "in the test":
						tr.Begin()
						tr.ServedMock(other)
						tr.ServedMockAsRecorded(other)
					case "at the start-up":
						tr.ServedMock(other)
						tr.ServedMockAsRecorded(other)
						tr.Begin()
					case "in the previous test":
						tr.Begin()
						tr.ServedMock(other)
						tr.ServedMockAsRecorded(other)
						tr.End()
						tr.Begin()
					case "two tests back":
						tr.Begin()
						tr.ServedMock(other)
						tr.ServedMockAsRecorded(other)
						tr.End()
						tr.Begin()
						tr.End()
						tr.Begin()
					}
					got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", tc.live, nil), db, n, opts)
					if err != nil {
						t.Fatal(err)
					}
					if miss == nil {
						return fmt.Sprintf("served %v", got != nil), ""
					}
					return "refused", miss.res.Why
				}
				before, beforeWhy := verdict(0)
				equal, equalWhy := verdict(time.Millisecond)
				t.Logf("VERDICT %s: a moment before: %s; at the call's own instant: %s (%q)", name, before, equal, equalWhy)
				if !tc.refused {
					if before != "served true" || equal != "served true" {
						t.Errorf("%s: a fresh value refused: a moment before %s (%s); at the call's own instant %s (%s)", name, before, beforeWhy, equal, equalWhy)
					}
					continue
				}
				if before != "refused" || !strings.Contains(beforeWhy, "mock-2") {
					t.Errorf("%s, recorded a moment before the call: %s (%s); want it refused naming mock-2", name, before, beforeWhy)
				}
				if equal != "refused" || !strings.Contains(equalWhy, "mock-2") || !strings.Contains(equalWhy, "recorded at the call's own instant") {
					t.Errorf("FALSE GREEN? %s, recorded at the call's own instant: %s (%s); want it refused naming mock-2 and the instant", name, equal, equalWhy)
					continue
				}
				// The reason is the one a moment before gets, saying the
				// recording was made at the call's own instant: read as
				// the same place, by the same rule.
				const note = " (recorded at the call's own instant, so keploy reads it as given to the application before the call)"
				want := strings.Replace(beforeWhy, "mock-2 at spec.resp.body", "mock-2 at spec.resp.body"+note, 1)
				if want == beforeWhy {
					want = strings.Replace(beforeWhy, "is another test's (another recorded test's, or one of an earlier test of the set): keploy follows no value of another test's", "was"+strings.TrimSuffix(strings.Replace(note, " (", " ", 1), ")")+", yet as no earlier place of the call's own test: keploy follows no value of it", 1)
				}
				// Where the earlier place's reading binds the value (a
				// variable this protocol cannot bind yet), the reading as a
				// recording made after the call refuses it on its own, and
				// its reason says the recording is read both ways
				// (TestLearnMatchAServedRecordingAtTheCallsOwnInstantIsReadBothWays),
				// and where the first reading would have bound it: either
				// reading refusing refuses the value (fail closed).
				late := strings.Contains(beforeWhy, "keploy follows it as a value from there") && strings.Contains(equalWhy, " changed, and a word of its recorded value") && strings.Contains(equalWhy, "appears in mock-2, a recording record's full round gave the application before the call that was recorded at the call's own instant, so keploy reads it as given to the application before the call, yet may as well have been made after the call, so keploy reads it both ways") &&
					strings.Contains(equalWhy, "; read as a recording made before the call, the value would be a variable bound at mock-2 ") && strings.HasSuffix(equalWhy, ", yet keploy refuses a value either reading refuses")
				if want == beforeWhy || equalWhy != want && !late {
					t.Errorf("%s, recorded at the call's own instant: %q; want what one recorded a moment before gets, saying the instant: %q, or, read as made after the call too, its refusal", name, equalWhy, want)
				}
			}
		}
	}
}

// A recording F served before the call (in the test, or at the start-up)
// that was made at the call's own instant cannot be shown, from the record,
// to have been given to the recorded application before the call or after
// it, so keploy reads it both ways and refuses where either reading refuses:
// as an earlier place of the call's own test, and as a recording made at or
// after the call, which only what F served there counts for. Its answer's
// 10:00:40 rounds to F's minute (10:01) and cuts down to the recorded one
// (10:00): an earlier place both runs were given shows no copy of it, so one
// recorded a second before the call leaves the minute noise, while what F
// alone was served there shows F's minute copied from it, so at the call's
// own instant the value is refused naming the recording and its instant,
// never noise. A recording of a previous test is read once, as another
// test's, wherever it was made. The recorded minute alone (10:00:10) names
// no minute of F's, and stays noise at either instant.
func TestLearnMatchAServedRecordingAtTheCallsOwnInstantIsReadBothWays(t *testing.T) {
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	call := 2 * time.Second
	for _, tc := range []struct {
		name    string
		sent    time.Duration
		refused bool // at the call's own instant
	}{
		{"rounds to F's minute, cuts down to the recorded one", 40 * time.Second, true},
		{"the recorded minute alone (control)", 10 * time.Second, false},
	} {
		for _, where := range []string{"in the test", "at the start-up", "in the previous test", "two tests back"} {
			verdict := func(nat time.Duration) (string, string) {
				h, n, opts := learnOn(t)
				own := learnRecorded("mock-1", call, "POST", "/v1/messages", `{"sent_at":"`+minute(learnAt.Add(call))+`"}`, nil, `{}`)
				other := learnRecorded("mock-2", nat, "POST", "/v1/receive", `{"max":10}`, nil, `{"Messages":[{"SentTimestamp":"`+learnAt.Add(tc.sent).Format(time.RFC3339)+`"}]}`)
				db := &learnDb{perTest: []*models.Mock{own}}
				tr := learn.NewTracker()
				served := learnAt.Add(61 * time.Second)
				switch where {
				case "in the test":
					tr.Begin()
					tr.ServedMockAt(other, served)
				case "at the start-up":
					tr.ServedMockAt(other, served)
					tr.Begin()
				case "in the previous test":
					tr.Begin()
					tr.ServedMockAt(other, served)
					tr.End()
					tr.Begin()
				case "two tests back":
					tr.Begin()
					tr.ServedMockAt(other, served)
					tr.End()
					tr.Begin()
					tr.End()
					tr.Begin()
				}
				in := live("POST", "/v1/messages", `{"sent_at":"`+minute(learnAt.Add(call+time.Minute))+`"}`, nil)
				in.arrived = learnAt.Add(call + time.Minute)
				got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if miss == nil {
					return fmt.Sprintf("served %v", got != nil), ""
				}
				return "refused", miss.res.Why
			}
			before, beforeWhy := verdict(call - time.Second)
			equal, equalWhy := verdict(call)
			if before != "served true" {
				t.Errorf("%s, served %s, recorded a second before the call: %s (%s); want noise, an earlier place both runs were given showing no copy", tc.name, where, before, beforeWhy)
			}
			if !tc.refused {
				if equal != "served true" {
					t.Errorf("%s, served %s, recorded at the call's own instant: %s (%s); want noise, no minute of F's named", tc.name, where, equal, equalWhy)
				}
				continue
			}
			// Another test's recording (two tests back) is read once, as
			// another test's; one the call's own test reads as an earlier
			// place is read both ways, and the reason says so.
			whose := "yet may as well have been made after the call, so keploy reads it both ways: as one made after the call, keploy follows no value of it but what record's full round served there"
			if where == "two tests back" {
				whose = "yet as no earlier place of the call's own test: keploy follows no value of it"
			}
			if equal != "refused" || !strings.Contains(equalWhy, "stands in mock-2 at spec.resp.body, a recording record's full round gave the application before the call that was recorded at the call's own instant, so keploy reads it as given to the application before the call, "+whose) {
				t.Errorf("FALSE GREEN? %s, served %s, recorded at the call's own instant: %s (%s); want it refused naming mock-2, read both ways: %q", tc.name, where, equal, equalWhy, whose)
			}
		}
	}
}

// An exchange keploy's HTTP learn step served leaf by leaf in the test
// before the call, whose request was recorded after the call's (later in the
// call's own test's window), gave the recorded application nothing before
// the call: only what F served there counts, so a minute F's run copied from
// its answer (10:00:40 rounded to F's minute, 10:01:05 cut to it) is refused
// naming the exchange, never noise because its leaves were read as an
// earlier place both runs were given. One recorded at the call's own instant
// is read both ways and refused so too. Recorded a second before the call
// (control), 10:00:40 names both runs' minutes at a place both runs were
// given, and stays noise; the recorded minute alone (10:00:10) names no
// minute of F's, and stays noise wherever the exchange was recorded.
func TestLearnMatchAnExchangeServedLeafByLeafIsReadByWhenItWasRecorded(t *testing.T) {
	minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
	call := 2 * time.Second
	for _, tc := range []struct {
		name string
		sent time.Duration
		copy bool
	}{
		{"rounds to F's minute, cuts down to the recorded one", 40 * time.Second, true},
		{"F's minute and more", 65 * time.Second, true},
		{"the recorded minute alone", 10 * time.Second, false},
	} {
		verdict := func(nat time.Duration) (string, string) {
			h, n, opts := learnOn(t)
			nb := learnRecorded("mock-0", nat, "POST", "/v1/receive", `{"max":10}`, nil, `{"Messages":[{"SentTimestamp":"`+learnAt.Add(tc.sent).Format(time.RFC3339)+`"}]}`)
			own := learnRecorded("mock-1", call, "POST", "/v1/messages", `{"sent_at":"`+minute(learnAt.Add(call))+`"}`, nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{nb, own}}
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			first := live("POST", "/v1/receive", `{"max":10}`, nil)
			first.arrived = learnAt.Add(61 * time.Second)
			if got, miss, err := h.learnMatch(ctx, first, db, n, opts); err != nil || miss != nil || got == nil || got.Name != "mock-0" {
				t.Fatalf("%s: the receive is paired with mock-0 and served leaf by leaf: %v %+v %v", tc.name, got, miss, err)
			}
			in := live("POST", "/v1/messages", `{"sent_at":"`+minute(learnAt.Add(call+time.Minute))+`"}`, nil)
			in.arrived = learnAt.Add(call + time.Minute)
			got, miss, err := h.learnMatch(ctx, in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if miss == nil {
				return fmt.Sprintf("served %v", got != nil), ""
			}
			return "refused", miss.res.Why
		}
		for _, rc := range []struct {
			name string
			at   time.Duration
		}{
			{"recorded later in the call's own test's window", call + 30*time.Second},
			{"recorded at the call's own instant", call},
		} {
			got, why := verdict(rc.at)
			if !tc.copy {
				if got != "served true" {
					t.Errorf("%s, %s: %s (%s); want noise, no minute of F's named", tc.name, rc.name, got, why)
				}
				continue
			}
			if got != "refused" || !strings.Contains(why, "mock-0 at spec.resp.body") {
				t.Errorf("FALSE GREEN? %s, %s: %s (%s); want it refused naming mock-0, the exchange F served the minute's source in", tc.name, rc.name, got, why)
			}
			// Where only the second reading refuses, the reason says the
			// exchange is read both ways.
			if rc.at == call && tc.sent == 40*time.Second && !strings.Contains(why, "so keploy reads it both ways") {
				t.Errorf("%s, %s: %q; want it to say the exchange is read both ways", tc.name, rc.name, why)
			}
		}
		if tc.sent == 65*time.Second {
			continue
		}
		if got, why := verdict(call - time.Second); got != "served true" {
			t.Errorf("%s, recorded a second before the call: %s (%s); want noise, an earlier place both runs were given showing no copy", tc.name, got, why)
		}
	}
}

// learnedClockRead reports whether spec carries path as a read of the
// application's clock the learn step learned (MockSpec.Clocks), still
// compared, and no noise.
func learnedClockRead(spec models.MockSpec, path string) bool {
	_, clock := spec.Clocks[path]
	return clock && len(spec.ReqBodyNoise) == 0
}

// A read of the clock the learn step hands back (learn.KindClockRead) is
// carried as a read a replay checks, never as noise; a kind the plan does
// not carry (a read with no read beside it, an offset with no offset, one
// it does not know) refuses the call with the learn step's reason, never
// leaves the field out of the noise to be compared by luck.
func TestALearnedReadOfTheClockIsCarriedAndAKindThePlanCannotCarryIsRefused(t *testing.T) {
	m := &models.Mock{Name: "mock-1"}
	leaf := learn.Leaf{Path: "body.at", Value: "2026-10-27T10:00:00Z", Type: learn.String}
	read := models.ClockOffset{Res: time.Second, Zone: models.ZoneUTC}
	p, why := learnPlan(learn.NewTracker(), 1, m, learn.Shape{}, true, []learn.Learned{{Kind: learn.KindClockRead, Recorded: leaf, Live: leaf, Read: &read, Why: "unchecked"}}, nil, nil)
	if why != "" {
		t.Fatalf("a read of the clock refused: %s", why)
	}
	if got, ok := p.clocks["body.at"]; !ok || got != read {
		t.Fatalf("clocks = %v, want body.at carried as %v", p.clocks, read)
	}
	if _, ok := p.noise["body.at"]; ok {
		t.Fatalf("a read of the clock learned as noise too: %v", p.noise)
	}
	for _, k := range []learn.Kind{learn.KindClockRead, learn.KindClockOffset, "a kind of tomorrow"} {
		p, why := learnPlan(learn.NewTracker(), 1, m, learn.Shape{}, true, []learn.Learned{{Kind: k, Recorded: leaf, Live: leaf, Why: "it is what it is"}}, nil, nil)
		if want := "body.at changed, and it is what it is"; why != want {
			t.Errorf("%s with nothing beside it: why %q, want %q", k, why, want)
		}
		if len(p.noise) != 0 || len(p.clocks) != 0 {
			t.Errorf("%s with nothing beside it: noise %v, clocks %v, want neither", k, p.noise, p.clocks)
		}
	}
	if _, why := learnPlan(learn.NewTracker(), 1, m, learn.Shape{}, true, []learn.Learned{{Kind: learn.KindClockRead, Recorded: leaf, Live: leaf}}, nil, nil); why != "body.at changed, and keploy learned it as clock-read, which this replay cannot check" {
		t.Errorf("a read with no read beside it and no reason: why %q, want the kind named", why)
	}
}

// A token the application read in another recorded test's answer, and sends
// again in a later test's call, is a value F may have given it there: the
// replay served that answer (mock-2) in the older test, and only its adapter
// can say what the application got from that serve (a variable's value, a
// template, a view). Told nothing, keploy cannot tell, and the call's fresh
// token is refused, saying so; told with a view whose answer holds the token,
// it is refused, naming mock-2 as the full round gave it. Told as recorded,
// the recording's own words are what the application got, none is the
// token's, and the token is learned as noise (the control). keploy read
// another test's recording by its recorded words alone, so the token was
// learned as noise whatever the serve gave the application (FALSE GREEN).
func TestLearnMatchATokenAnotherTestsServeMayHaveGivenIsRefusedUnlessItsAdapterSaidWhatItGave(t *testing.T) {
	const (
		unstated = "where its adapter did not say what it gave the application there"
		gave     = "appears in mock-2 as record's full round gave it"
	)
	for _, tc := range []struct {
		name string
		tell func(tr *learn.Tracker, m *models.Mock)
		why  string // "" learns noise
	}{
		{"told nothing", func(*learn.Tracker, *models.Mock) {}, unstated},
		{"told with a view whose answer holds the token", func(tr *learn.Tracker, m *models.Mock) {
			tr.ServedMockLive(m, "v1", func() (*models.Mock, error) {
				v := m.ShallowCopy()
				resp := *m.Spec.HTTPResp
				resp.Body = `{"token":"tok-9c4d"}`
				v.Spec.HTTPResp = &resp
				return v, nil
			})
		}, gave},
		{"told as recorded", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockAsRecorded(m) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			other := learnRecorded("mock-2", time.Millisecond, "GET", "/session", "", nil, `{"token":"tok-0b7e"}`)
			call := learnRecorded("mock-1", 5*time.Millisecond, "POST", "/v1/messages", `{"ref":"tok-5a21"}`, nil, `{}`)
			db := &learnDb{perTest: []*models.Mock{call}, otherTest: map[time.Time]bool{other.Spec.ReqTimestampMock: true}}
			tr := learn.NewTracker()
			tr.Begin() // the older test, which the replay served mock-2 in
			tr.ServedMock(other)
			tc.tell(tr, other)
			tr.Begin() // the call's test
			got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/messages", `{"ref":"tok-9c4d"}`, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			served := "nothing"
			if got != nil {
				served = got.Name
			}
			if tc.why == "" {
				if got == nil || miss != nil || len(db.consumed) != 1 {
					t.Fatalf("FALSE RED: served %s miss %+v consumed %d; want the call served, body.ref learned as noise: mock-2 gave the application its recorded answer, which holds no word of the token", served, miss, len(db.consumed))
				}
				return
			}
			if got != nil || miss == nil || len(db.consumed) != 0 {
				t.Fatalf("FALSE GREEN: served %s miss %+v consumed %d; want body.ref refused: F may have given the application the token in mock-2", served, miss, len(db.consumed))
			}
			if miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.why) {
				t.Fatalf("FALSE REASON: refused %s (%q); want request_changed saying %q", miss.res.Reason, miss.res.Why, tc.why)
			}
		})
	}
}

// A value the test sent whole that a later call sends as one part of a
// longer value ("APP:" before the app id the test minted) is the variable,
// bound where it was sent and used inside the longer value: only its words
// move, the text around it is still compared, and the answer that holds the
// longer value is served with this run's value inside it. Two places, a
// part of a word, or the text around it changed stay refused, with the
// reason named.
func TestLearnMatchFollowsAVariableInsideALongerValue(t *testing.T) {
	h, n, opts := learnOn(t)
	type round struct {
		ctx context.Context
		db  *learnDb
	}
	setup := func(recKey, recAnswer string) round {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/apps", `{"app":"a-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/bindings/find", `{"key":"`+recKey+`"}`, nil, recAnswer),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/apps", `{"app":"a-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		return round{ctx, db}
	}

	r := setup("APP:a-1", `{"key":"APP:a-1","role":"admin"}`)
	served, miss, err := h.learnMatch(r.ctx, live("POST", "/v1/bindings/find", `{"key":"APP:a-9"}`, nil), r.db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"key":"APP:a-9","role":"admin"}` {
		t.Fatalf("served %s: the answer holds this run's value inside the recorded text", served.Spec.HTTPResp.Body)
	}
	use := r.db.consumed[1]
	want := models.MockVariables{"app": {Recorded: "a-1", Use: []string{"body.key"}, Answer: []string{"body.key"}, Within: []models.VariablePart{
		{Place: "body.key", Before: "APP:"}, {Place: "body.key", Answer: true, Before: "APP:"},
	}}}
	if !reflect.DeepEqual(use.Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"app": {Recorded: "a-1", Bind: "body.app"}}}); !reflect.DeepEqual(use.Spec.LearnedBinds(), want) {
		t.Fatalf("binds %+v, want %+v", use.Spec.LearnedBinds(), want)
	}

	// The replay reads the value out of the longer one and compares the
	// text around it; the answer holds this run's value inside its text.
	written := func(within bool) []*models.Mock {
		m0 := learnRecorded("mock-0", 0, "POST", "/v1/apps", `{"app":"a-1"}`, nil, `{}`)
		m1 := learnRecorded("mock-1", time.Second, "POST", "/v1/bindings/find", `{"key":"APP:a-1"}`, nil, `{"key":"APP:a-1","role":"admin"}`)
		m0.Spec.Variables = models.MockVariables{"app": {Recorded: "a-1", Bind: "body.app"}}
		v := use.Spec.Learned()["app"]
		if !within {
			v.Within = nil
		}
		m1.Spec.Variables = models.MockVariables{"app": v}
		return []*models.Mock{m0, m1}
	}
	ci := &varDb{perTest: written(true)}
	if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/apps", `{"app":"a-55"}`, nil)); !ok {
		t.Fatalf("the create in CI: %+v", diag)
	}
	ok, stub, diag := matchVar(t, h, ci, live("POST", "/v1/bindings/find", `{"key":"APP:a-55"}`, nil))
	if !ok || stub.Spec.HTTPResp.Body != `{"key":"APP:a-55","role":"admin"}` {
		t.Fatalf("the find in CI: ok %v diag %+v", ok, diag)
	}
	for _, key := range []string{"APX:a-55", "APP:a-56", "APP:a-55:a-55"} {
		ci = &varDb{perTest: written(true)}
		if ok, _, _ := matchVar(t, h, ci, live("POST", "/v1/apps", `{"app":"a-55"}`, nil)); !ok {
			t.Fatal("the create in CI")
		}
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/bindings/find", `{"key":"`+key+`"}`, nil)); ok || diag == nil || diag.phase != models.MatchPhaseVariable {
			t.Fatalf("a find of %s is a variable miss: ok %v diag %+v", key, ok, diag)
		}
	}
	// A replay that reads no part compares the place whole, and refuses.
	ci = &varDb{perTest: written(false)}
	if ok, _, _ := matchVar(t, h, ci, live("POST", "/v1/apps", `{"app":"a-55"}`, nil)); !ok {
		t.Fatal("the create in CI")
	}
	if ok, _, _ := matchVar(t, h, ci, live("POST", "/v1/bindings/find", `{"key":"APP:a-55"}`, nil)); ok {
		t.Fatal("without its parts the variable's use place is compared whole, and the find is refused")
	}

	// The value at the end, the text after it.
	r = setup("a-1/settings", `{}`)
	if served, miss, _ := h.learnMatch(r.ctx, live("POST", "/v1/bindings/find", `{"key":"a-9/settings"}`, nil), r.db, n, opts); served == nil || miss != nil {
		t.Fatalf("a value before constant text: %+v", miss)
	}
	if got := r.db.consumed[1].Spec.Learned()["app"].Within; !reflect.DeepEqual(got, []models.VariablePart{{Place: "body.key", After: "/settings"}}) {
		t.Fatalf("parts %+v", got)
	}

	for _, c := range []struct{ name, rec, live, why string }{
		{"two places", "a-1:a-1", "a-9:a-9", "stands in it at 2 places"},
		{"a part of a word", "APPa-1", "APPa-9", "as a part of a word"},
		{"the text beside it changed", "APP:a-1", "APX:a-9", "the text beside the value changed"},
	} {
		r = setup(c.rec, `{}`)
		served, miss, _ := h.learnMatch(r.ctx, live("POST", "/v1/bindings/find", `{"key":"`+c.live+`"}`, nil), r.db, n, opts)
		if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, c.why) {
			t.Fatalf("%s: served %v miss %+v", c.name, served, miss)
		}
	}
}

// A call whose bearer JWT moved learns the claims that moved, each at its
// own place, never the token whole: the replay compares every other claim,
// the scheme, the JOSE header and the signature's form (jwtDiffs).
func TestLearnMatchLearnsAJWTByItsClaims(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	tok := func(jti, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sub":"u-1","role":"admin","cid":"c-1","jti":"`+jti+`"}`, sig)}
	}
	sigA, sigB := "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI"
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, tok("6f1c2a9e-4b7d-4e21-9a3f-1d2c3b4a5e6f", sigA), `{"ok":true}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	served, miss, err := h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, tok("0b9d8c7a-1e2f-4a3b-8c5d-6e7f8091a2b3", sigB)), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	got := db.consumed[0].Spec.ReqBodyNoise
	if _, ok := got["header.authorization{jwt}.jti"]; !ok || len(got) != 1 {
		t.Fatalf("learned %v, want the jti claim alone", got)
	}
}

// A claim that carries a value the test sent whole at an earlier call is
// followed at its own place (header.<name>{jwt}.<claim>): a replay checks it
// holds this run's value and puts the recorded one back in the token's
// claims, and every other claim, the JOSE header and the signature's form are
// still compared. A claim holding the value as one part of a longer value, or
// a claim that must not move (role) that changes, is refused.
func TestLearnMatchFollowsAVariableIntoAJWTClaim(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	tok := func(sub, role, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sub":"`+sub+`","role":"`+role+`","n":7}`, sig)}
	}
	const sigA, sigB = "QUFBQUFB", "QkJCQkJC"
	setup := func(recSub string) (context.Context, *learnDb) {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/users", `{"id":"u-71b2c3"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/run/r-1", `{}`, tok(recSub, "admin", sigA), `{"ok":true}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/users", `{"id":"u-94e5f6"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		return ctx, db
	}
	ctx, db := setup("u-71b2c3")
	served, miss, err := h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, tok("u-94e5f6", "admin", sigB)), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	use := db.consumed[1]
	want := models.MockVariables{"id": {Recorded: "u-71b2c3", Use: []string{"header.authorization{jwt}.sub"}}}
	if !reflect.DeepEqual(use.Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Learned(), want)
	}
	if len(use.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("noise %v: the token is compared by its claims", use.Spec.ReqBodyNoise)
	}

	// The replay: the claim holds this run's value; another value, a role
	// that changed, or the claim gone is a miss.
	written := func() []*models.Mock {
		m0 := learnRecorded("mock-0", 0, "POST", "/v1/users", `{"id":"u-71b2c3"}`, nil, `{}`)
		m1 := learnRecorded("mock-1", time.Second, "POST", "/run/r-1", `{}`, tok("u-71b2c3", "admin", sigA), `{"ok":true}`)
		m0.Spec.Variables = models.MockVariables{"id": {Recorded: "u-71b2c3", Bind: "body.id"}}
		m1.Spec.Variables = use.Spec.Learned()
		return []*models.Mock{m0, m1}
	}
	for _, c := range []struct {
		name string
		hdr  map[string]string
		pass bool
	}{
		{"this run's value", tok("u-55", "admin", sigB), true},
		{"another value", tok("u-56", "admin", sigB), false},
		{"a role that changed", tok("u-55", "owner", sigB), false},
		{"another signature form", tok("u-55", "admin", sigA+"QQ"), false},
		{"no claim", map[string]string{"Authorization": bearerJWT(hs, `{"role":"admin","n":7}`, sigB)}, false},
		{"no token", map[string]string{"Authorization": "Bearer u-55"}, false},
	} {
		ci := &varDb{perTest: written()}
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/v1/users", `{"id":"u-55"}`, nil)); !ok {
			t.Fatalf("%s: the first call in CI: %+v", c.name, diag)
		}
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/run/r-1", `{}`, c.hdr)); ok != c.pass {
			t.Fatalf("%s: answered %v, want %v (%+v)", c.name, ok, c.pass, diag)
		}
	}

	// A claim that holds the value as one part of a longer value.
	ctx, db = setup("user:u-71b2c3")
	served, miss, err = h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, tok("user:u-94e5f6", "admin", sigB)), db, n, opts)
	if err != nil || served != nil || miss == nil {
		t.Fatalf("served %v miss %+v err %v: refused", served, miss, err)
	}
	if why := fmt.Sprintf("%+v", miss); !strings.Contains(why, "header.authorization{jwt}.sub is a claim of the token header.authorization holds") || !strings.Contains(why, "mock-0 sent at body.id as one part of a longer value") {
		t.Fatalf("refused with %s", why)
	}
}

// Two calls of a client test carry the value the harness gave the
// application (its token's auth_time) into the token the application mints
// for each: both use the one variable read in the test's incoming requests.
// The second is not followed from the first call's claim, a use of that
// variable (learn.Seen.Incoming), so no second variable is bound there.
func TestLearnMatchTwoCallsCarryingWhatTheHarnessGaveUseOneVariable(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	session := func(at string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sid":"e2e-go-session","role":"admin","auth_time":`+at+`}`, "U0lHTg")}
	}
	agent := func(at, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sub":"u-1","role":"agent","auth_time":`+at+`}`, sig)}
	}
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.GiveIncoming("TestRunReportOnClient", []learn.IncomingRequest{learn.IncomingHTTP("test-1", learnAt.Add(-time.Second), "http://app/v1/report", session("1793373215"))})
	tr.Begin()
	tr.Incoming(learn.IncomingHTTP("", time.Now(), "http://app/v1/report", session("1793387031")))
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, agent("1793373215", "QUFBQUFB"), `{"ok":true}`),
		learnRecorded("mock-1", time.Second, "POST", "/run/r-2", `{}`, agent("1793373215", "Q0NDQ0ND"), `{"ok":true}`),
	}}
	for i, path := range []string{"/run/r-1", "/run/r-2"} {
		served, miss, _ := h.learnMatch(ctx, live("POST", path, `{}`, agent("1793387031", "QkJCQkJC")), db, n, opts)
		if served == nil || miss != nil {
			t.Fatalf("call %d: served %v miss %+v", i, served, miss)
		}
	}
	want := models.MockVariables{"auth_time": {Recorded: "1793373215", Use: []string{"header.authorization{jwt}.auth_time"}, Incoming: "header.Authorization{jwt}.auth_time"}}
	for i, m := range db.consumed {
		if got := m.Spec.Learned(); !reflect.DeepEqual(got, want) {
			t.Errorf("call %d: variables %+v, want %+v", i, got, want)
		}
		if binds := m.Spec.LearnedBinds(); len(binds) != 0 {
			t.Errorf("call %d: binds %+v: no call binds what the harness gave", i, binds)
		}
	}
}

// A value a client test's harness gave the application (the auth_time claim
// of the bearer token it signed for its session), carried into a claim of
// the token the application mints from it, is a variable read in the test's
// incoming requests (models.MockVariable.Incoming): used at the claim's own
// place, bound at no call, every other claim still compared.
func TestLearnMatchAValueTheHarnessGaveIsFollowedFromTheTestsIncomingRequests(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	session := func(at string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sid":"e2e-go-session","role":"admin","auth_time":`+at+`}`, "U0lHTg")}
	}
	agent := func(at, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sub":"u-1","role":"agent","auth_time":`+at+`}`, sig)}
	}
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.GiveIncoming("TestGenerateOnClient", []learn.IncomingRequest{learn.IncomingHTTP("test-1", learnAt.Add(-time.Second), "http://app/v1/generate", session("1793373215"))})
	tr.Begin()
	tr.Incoming(learn.IncomingHTTP("", time.Now(), "http://app/v1/generate", session("1793387031")))
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, agent("1793373215", "QUFBQUFB"), `{"ok":true}`)}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, agent("1793387031", "QkJCQkJC")), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	got := db.consumed[0].Spec.Learned()
	want := models.MockVariables{"auth_time": {Recorded: "1793373215", Use: []string{"header.authorization{jwt}.auth_time"}, Incoming: "header.Authorization{jwt}.auth_time"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v", got, want)
	}
	if binds := db.consumed[0].Spec.LearnedBinds(); len(binds) != 0 {
		t.Fatalf("binds %+v: no call binds what the harness gave", binds)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("noise %v: every other claim is still compared", db.consumed[0].Spec.ReqBodyNoise)
	}
}

// A token the harness gave the application, which the application forwards
// whole, is followed whole as one variable of the incoming requests' header:
// a replay reads the whole token there, so its signature's bytes are
// compared too, never its claims and form alone.
func TestLearnMatchATokenTheHarnessGaveForwardedWholeIsFollowedWhole(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	session := func(at, sig string) map[string]string {
		return map[string]string{"Authorization": bearerJWT(hs, `{"sid":"e2e-go-session","role":"admin","auth_time":`+at+`}`, sig)}
	}
	recorded := session("1793373215", "QUFBQUFB")
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.GiveIncoming("TestForwardOnClient", []learn.IncomingRequest{learn.IncomingHTTP("test-1", learnAt.Add(-time.Second), "http://app/v1/generate", recorded)})
	tr.Begin()
	tr.Incoming(learn.IncomingHTTP("", time.Now(), "http://app/v1/generate", session("1793387031", "QkJCQkJC")))
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, recorded, `{"ok":true}`)}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, session("1793387031", "QkJCQkJC")), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	got := db.consumed[0].Spec.Learned()
	want := models.MockVariables{"Authorization": {Recorded: recorded["Authorization"], Use: []string{"header.authorization"}, Incoming: "header.Authorization"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v: the forwarded token followed whole", got, want)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("noise %v", db.consumed[0].Spec.ReqBodyNoise)
	}
}

// A value the harness gave the application whole, which a call holds as one
// part of a longer value ("tenant:<id>"), is read in the incoming requests
// and used at that part, the text around it still compared.
func TestLearnMatchAValueTheHarnessGaveIsFollowedInsideALongerValue(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.GiveIncoming("TestTenantOnClient", []learn.IncomingRequest{learn.IncomingHTTP("test-1", learnAt.Add(-time.Second), "http://app/v1/x", map[string]string{"X-Tenant": "tn-4f1a9c"})})
	tr.Begin()
	tr.Incoming(learn.IncomingHTTP("", time.Now(), "http://app/v1/x", map[string]string{"X-Tenant": "tn-77d0e2"}))
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/keys", `{"key":"tenant:tn-4f1a9c"}`, nil, `{"ok":true}`)}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/keys", `{"key":"tenant:tn-77d0e2"}`, nil), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	got := db.consumed[0].Spec.Learned()
	want := models.MockVariables{"X-Tenant": {Recorded: "tn-4f1a9c", Use: []string{"body.key"}, Incoming: "header.X-Tenant",
		Within: []models.VariablePart{{Place: "body.key", Before: "tenant:"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v", got, want)
	}
}

// A value the harness gave the application, carried by a call that is not
// the test's own (a pool call), is refused there: only a test's own calls
// carry its variables, the incoming requests' as any other.
func TestLearnMatchAValueTheHarnessGaveOnACallNotTheTestsOwnIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.GiveIncoming("TestTenantOnClient", []learn.IncomingRequest{learn.IncomingHTTP("test-1", learnAt.Add(-time.Second), "http://app/v1/x", map[string]string{"X-Tenant": "tn-4f1a9c"})})
	tr.Begin()
	tr.Incoming(learn.IncomingHTTP("", time.Now(), "http://app/v1/x", map[string]string{"X-Tenant": "tn-77d0e2"}))
	ctx := learn.WithTracker(context.Background(), tr)
	pool := learnRecorded("pool-0", 0, "POST", "/v1/keys", `{"tenant":"tn-4f1a9c"}`, nil, `{"ok":true}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/keys", `{"tenant":"tn-77d0e2"}`, nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "only a test's own calls carry its variables") {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// What a token's form holds (its scheme, its JOSE header, its signature's
// form) is never learned: a call whose token moved a claim and changed its
// form there is refused, the claim that moved notwithstanding.
func TestLearnMatchATokenWhoseFormChangedIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	const hs = `{"alg":"HS256","typ":"JWT"}`
	sig := "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"
	claims := func(jti string) string { return `{"sub":"u-1","role":"admin","jti":"` + jti + `"}` }
	rec := bearerJWT(hs, claims("j-1"), sig)
	for _, c := range []struct{ name, auth string }{
		{"the recording's form", bearerJWT(hs, claims("j-2"), "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI")},
		{"another scheme", "Token " + strings.TrimPrefix(bearerJWT(hs, claims("j-2"), "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI"), "Bearer ")},
		{"another algorithm", bearerJWT(`{"alg":"HS512","typ":"JWT"}`, claims("j-2"), "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI")},
		{"a signature of another length", bearerJWT(hs, claims("j-2"), "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJC")},
	} {
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/run/r-1", `{}`, map[string]string{"Authorization": rec}, `{"ok":true}`)}}
		served, miss, _ := h.learnMatch(ctx, live("POST", "/run/r-1", `{}`, map[string]string{"Authorization": c.auth}), db, n, opts)
		if want := c.name == "the recording's form"; (served != nil) != want || (miss == nil) != want {
			t.Fatalf("%s: served %v miss %+v, want served %v", c.name, served != nil, miss, want)
		}
	}
}
