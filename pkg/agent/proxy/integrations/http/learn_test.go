package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// learnDb is a mock store with a test's own mocks (consumed when served) and
// a pool.
type learnDb struct {
	mockMemDb
	perTest  []*models.Mock
	consumed []models.Mock
	pooled   []*models.Mock // the pool's mocks as UpdateUnFilteredMock left them
}

func (d *learnDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return d.perTest, nil }
func (d *learnDb) GetSessionMocks() ([]*models.Mock, error)         { return d.mocks, nil }
func (d *learnDb) DeleteFilteredMock(m models.Mock) bool {
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

func recorded(name string, at time.Duration, method, target, body string, header map[string]string, respBody string) *models.Mock {
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/v1/orders", `{"request_id":"r-1","item":"book"}`, nil, `{"ok":true}`)}}
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "GET", "/v1/users/41?at=100", "", map[string]string{"Authorization": "Bearer one", "X-Trace": "t1"}, `{}`)}}
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "GET", "/a", "", map[string]string{"Traceparent": "00-1"}, `{}`)}}
	served, miss, _ := h.learnMatch(context.Background(), live("GET", "/a", "", map[string]string{"Traceparent": "00-2"}), db, n, opts)
	if served == nil || miss != nil || len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("a header of the static list is not compared, so nothing drifted: served %v miss %+v learned %v", served, miss, db.consumed)
	}
}

func TestLearnMatchLearnsAnEchoAsACorrelationAndRendersIt(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","amount":5}`, nil, `{"id":"ch_1","idempotency_key":"k-1","amount":5,"note":"k-1 ok"}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","amount":5}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"ch_1","idempotency_key":"k-2","amount":5,"note":"k-2 ok"}` {
		t.Fatalf("served %s: the live value is rendered where the recorded one was echoed", served.Spec.HTTPResp.Body)
	}
	c := db.consumed[0].Spec.Correlations
	if len(c) != 1 || c[0].RequestPath != "body.key" || !reflect.DeepEqual(c[0].ResponsePaths, []string{"body.idempotency_key", "body.note"}) || c[0].RecordedValue != "k-1" {
		t.Fatalf("correlations %+v", c)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("a correlation is not noise: %v", db.consumed[0].Spec.ReqBodyNoise)
	}
}

func TestLearnMatchRefusesAVariableNothingFollows(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker(nil)
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{
		recorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		recorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{}`),
	}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the first call is paired: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if err != nil || served != nil || miss == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "url.path.2 carries the value of body.order") || miss.closest == nil || miss.closest.Name != "mock-1" {
		t.Fatalf("miss %+v", miss.res)
	}
	if len(db.perTest) != 1 {
		t.Fatal("the refused call's mock stays unconsumed")
	}
}

// following is a variable runtime that follows every variable.
type following struct{ got []learn.Place }

func (f *following) Follow(_ string, _ learn.Leaf, from learn.Place) bool {
	f.got = append(f.got, from)
	return true
}

func TestLearnMatchHandsAVariableToItsRuntime(t *testing.T) {
	h, n, opts := learnOn(t)
	vars := &following{}
	ctx := learn.WithTracker(context.Background(), learn.NewTracker(vars))
	db := &learnDb{perTest: []*models.Mock{
		recorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		recorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{}`),
	}}
	_, _, _ = h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts)
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if served == nil || miss != nil {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if len(vars.got) != 1 || vars.got[0] != (learn.Place{Mock: "mock-0", Side: learn.Request, Path: "body.order", Noise: "body.order"}) {
		t.Fatalf("followed %+v", vars.got)
	}
	if len(db.consumed[1].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("a variable is never noise: %v", db.consumed[1].Spec.ReqBodyNoise)
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
		{"an array in another order", recorded("m", 0, "POST", "/a", `{"ids":[1,2,3]}`, nil, `{}`), live("POST", "/a", `{"ids":[3,1,2]}`, nil), models.MissUnstableOrder},
		{"a field added", recorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":1,"x":2}`, nil), models.MissRequestChanged},
		{"a type changed", recorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":"1"}`, nil), models.MissRequestChanged},
		{"another method", recorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("PUT", "/a", `{"id":1}`, nil), models.MissUnmockedCall},
		{"another host", recorded("m", 0, "POST", "/a", `{"id":1}`, nil, `{}`), live("POST", "/a", `{"id":1}`, map[string]string{"Host": "other.test"}), models.MissUnmockedCall},
		{"another number of path segments", recorded("m", 0, "GET", "/a/b", ``, nil, `{}`), live("GET", "/a/b/c", ``, nil), models.MissUnmockedCall},
		{"a header added", recorded("m", 0, "GET", "/a", ``, nil, `{}`), live("GET", "/a", ``, map[string]string{"If-None-Match": "x"}), models.MissRequestChanged},
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
	pool := recorded("pool-0", 0, "GET", "/config/1", "", nil, `{}`)
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
	m := recorded("mock-0", 0, "POST", "/v1/charges/1", `{"key":"k-1","amount":5}`, map[string]string{"X-Trace": "t1"}, `{"idempotency_key":"k-1","amount":5}`)
	m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}, "header.x-trace": {}}
	m.Spec.Correlations = []models.FieldCorrelation{{RequestPath: "body.key", ResponsePaths: []string{"body.idempotency_key"}, RecordedValue: "k-1"}}
	db := &learnDb{perTest: []*models.Mock{m}}
	served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":5}`, map[string]string{"X-Trace": "t9"}), db, n)
	if err != nil || served == nil {
		t.Fatalf("served %v err %v", served, err)
	}
	if served.Spec.HTTPResp.Body != `{"idempotency_key":"k-7","amount":5}` {
		t.Fatalf("served %s", served.Spec.HTTPResp.Body)
	}
	// What was not learned is still compared: another amount is not served.
	db = &learnDb{perTest: []*models.Mock{m}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":6}`, map[string]string{"X-Trace": "t9"}), db, n); served != nil {
		t.Fatal("a value nothing learned is compared")
	}
}

func TestRenderLearnedOnlyWhereTheEchoWas(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := recorded("m", 0, "POST", "/a", `{"n":7}`, nil, `{"echo":7,"count":7,"loc":"/x/7"}`)
	m.Spec.HTTPResp.Header["Location"] = "/x/7"
	m.Spec.Correlations = []models.FieldCorrelation{{RequestPath: "body.n", ResponsePaths: []string{"body.echo", "header.location"}, RecordedValue: "7"}}
	out := renderCorrelations(m, map[string]string{"body.n": "8"})
	if out.Spec.HTTPResp.Body != `{"echo":8,"count":7,"loc":"/x/7"}` || out.Spec.HTTPResp.Header["Location"] != "/x/8" {
		t.Fatalf("rendered %s %v", out.Spec.HTTPResp.Body, out.Spec.HTTPResp.Header)
	}
	if m.Spec.HTTPResp.Body != `{"echo":7,"count":7,"loc":"/x/7"}` || m.Spec.HTTPResp.Header["Location"] != "/x/7" {
		t.Fatal("the recorded mock is never touched")
	}
}
