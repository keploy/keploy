package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
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
	tr.Begin()
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
	if miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "url.path.2 carries a value an earlier call of the test (mock-0) recorded") || miss.closest == nil || miss.closest.Name != "mock-1" {
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
	tr := learn.NewTracker(vars)
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
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
	served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":5}`, map[string]string{"X-Trace": "t9"}), db, n, models.OutgoingOptions{})
	if err != nil || served == nil {
		t.Fatalf("served %v err %v", served, err)
	}
	if served.Spec.HTTPResp.Body != `{"idempotency_key":"k-7","amount":5}` {
		t.Fatalf("served %s", served.Spec.HTTPResp.Body)
	}
	// What was not learned is still compared: another amount is not served.
	db = &learnDb{perTest: []*models.Mock{m}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/charges/2", `{"key":"k-7","amount":6}`, map[string]string{"X-Trace": "t9"}), db, n, models.OutgoingOptions{}); served != nil {
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

// Outside record's full round a request that came with an array in another
// order is still unstable_order (design §7.3: F or C), and names the call it
// was refused against; any other miss keeps the matcher's report.
func TestOrderMissOutsideTheFullRound(t *testing.T) {
	h, n, _ := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{recorded("m", 0, "POST", "/a", `{"ids":[1,2,3]}`, nil, `{}`)}}
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
// there, and inside a longer value: never noise (the reviewer's repros).
func TestLearnMatchVariablesAreNeverNoise(t *testing.T) {
	h, n, opts := learnOn(t)
	for _, tc := range []struct {
		name          string
		first, second *models.Mock
		f, s          *req
	}{
		{
			"a number flowing into a path",
			recorded("mock-0", 0, "POST", "/v1/orders", `{"order":12345}`, nil, `{}`),
			recorded("mock-1", time.Second, "GET", "/v1/orders/12345", "", nil, `{}`),
			live("POST", "/v1/orders", `{"order":54321}`, nil),
			live("GET", "/v1/orders/54321", "", nil),
		},
		{
			"a token flowing into a credential",
			recorded("mock-0", 0, "POST", "/v1/login", `{"token":"tok-A"}`, nil, `{}`),
			recorded("mock-1", time.Second, "GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-A"}, `{}`),
			live("POST", "/v1/login", `{"token":"tok-B"}`, nil),
			live("GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-B"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := learn.NewTracker(nil)
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{tc.first, tc.second}}
			if served, miss, _ := h.learnMatch(ctx, tc.f, db, n, opts); served == nil || miss != nil {
				t.Fatalf("the first call is paired: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, tc.s, db, n, opts)
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("a value that flowed is refused until it can be followed: served %v miss %+v", served, miss)
			}
		})
	}
}

// A value of another kind of mock earlier in the test (an id the app
// inserted into a database) is a variable too.
func TestLearnMatchFindsEarlierPlacesOfAnyKind(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker(nil)
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		Metadata: map[string]string{"query": "INSERT INTO users VALUES ($1)", "params": "u-77"}, GenericRequests: []models.Payload{{Message: []models.OutputBinary{{Data: "INSERT u-77"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{recorded("mock-1", time.Second, "GET", "/v1/users/u-77", "", nil, `{}`)}}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/users/u-88", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "(pg-0)") {
		t.Fatalf("served %v miss %+v", served, miss)
	}
}

// What drifted in one element of an array is learned for that element alone.
func TestLearnMatchNoiseNamesOneElement(t *testing.T) {
	h, n, opts := learnOn(t)
	pool := recorded("pool-0", 0, "POST", "/v1/put", `{"items":[{"k":"a","at":"t-1"},{"k":"b","at":"fixed"}]}`, nil, `{}`)
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
	tr := learn.NewTracker(nil) // no test has begun
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/a", `{"id":"x"}`, nil, `{}`)}}
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
	tr := learn.NewTracker(nil)
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := ownedView{&learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/a", `{"id":"x"}`, nil, `{}`)}}}
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
	rec := recorded("mock-0", 0, "POST", "/up", body("AAA", "data", "n-1"), map[string]string{"Content-Type": "multipart/form-data; boundary=AAA"}, `{}`)
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

// The destination in the operation key is the container key when both the
// live connection and the recording name one (row 4k), the Host otherwise:
// a call to the same Host in another container is not a candidate, and one
// to the same container under another Host is.
func TestOperationKeyNamesTheContainer(t *testing.T) {
	_, n, _ := learnOn(t)
	rec := recorded("m", 0, "GET", "/a", "", nil, `{}`)
	rec.Spec.Metadata[containerkey.MetadataKey] = "docker:api"
	rp := recordedParts(rec)
	if rp.container != "docker:api" {
		t.Fatalf("the recorded side reads the mock's container key: %q", rp.container)
	}
	live := func(host, container string) learn.Shape {
		p := liveParts(context.Background(), live("GET", "/a", "", map[string]string{"Host": host}))
		p.container = container
		return n.shape(p, sameDest)
	}
	for _, tc := range []struct {
		host, container string
		key             bool
	}{
		{"api.test", "docker:api", true},
		{"other.test", "docker:api", true}, // the same container under another name
		{"api.test", "docker:web", false},  // the same Host, another container
		{"api.test", "", true},             // no live key: the Host decides
	} {
		l := live(tc.host, tc.container)
		lp := httpParts{host: strings.ToLower(tc.host), container: tc.container}
		got := n.shape(rp, destOf(lp, rp)).Key == l.Key
		if got != tc.key {
			t.Errorf("host %s container %q: candidate %v, want %v", tc.host, tc.container, got, tc.key)
		}
	}
}
