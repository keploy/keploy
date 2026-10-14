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

// A value the answer echoes is a variable of the call alone: bound at the
// request, answered with this run's value where the recording holds it whole
// (a mention inside a longer text is served as recorded), never noise.
func TestLearnMatchLearnsAnEchoAsAVariableOfItsCall(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","amount":5}`, nil, `{"id":"ch_1","idempotency_key":"k-1","amount":5,"note":"k-1 ok"}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","amount":5}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"ch_1","idempotency_key":"k-2","amount":5,"note":"k-1 ok"}` {
		t.Fatalf("served %s: this run's value is answered where the recorded one was echoed whole", served.Spec.HTTPResp.Body)
	}
	want := models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}}
	if got := db.consumed[0].Spec.Variables; !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v", got, want)
	}
	if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("an echo is not noise: %v", db.consumed[0].Spec.ReqBodyNoise)
	}

	// Echoed only inside a longer text, it cannot be answered: refused,
	// never learned as noise.
	db = &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1"}`, nil, `{"note":"k-1 ok"}`)}}
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
			recorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			recorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`),
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
	if want := (models.MockVariables{"order": {Recorded: "o-1", Use: []string{"url.path[2]"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(use.Spec.Variables, want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Variables, want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"order": {Recorded: "o-1", Bind: "body.order"}}}); !reflect.DeepEqual(use.Spec.Binds, want) {
		t.Fatalf("binds %+v, want %+v", use.Spec.Binds, want)
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
	m := recorded("mock-0", 0, "POST", "/v1/charges/1", `{"key":"k-1","amount":5}`, map[string]string{"X-Trace": "t1"}, `{"amount":5}`)
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
	v := recorded("mock-0", 0, "POST", "/v1/charges/1", `{"key":"k-1","amount":5}`, map[string]string{"X-Trace": "t1"}, `{"amount":5}`)
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
			recorded("mock-0", 0, "POST", "/v1/orders", `{"order":12345}`, nil, `{}`),
			recorded("mock-1", time.Second, "GET", "/v1/orders/12345", "", nil, `{}`),
			live("POST", "/v1/orders", `{"order":54321}`, nil),
			live("GET", "/v1/orders/54321", "", nil),
			true,
		},
		{
			"a token flowing into a credential",
			recorded("mock-0", 0, "POST", "/v1/login", `{"token":"tok-A"}`, nil, `{}`),
			recorded("mock-1", time.Second, "GET", "/v1/me", "", map[string]string{"Authorization": "Bearer tok-A"}, `{}`),
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
			if got := db.consumed[1].Spec; len(got.Variables) != 1 || len(got.ReqBodyNoise) != 0 {
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-1", time.Second, "GET", "/v1/users/u-77", "", nil, `{}`)}}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/users/u-88", "", nil), db, n, opts)
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "(in pg-0)") {
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
	tr := learn.NewTracker() // no test has begun
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
	tr := learn.NewTracker()
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
		recorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		recorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1","state":"new"}`),
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
			for vn, v := range st.Spec.Variables {
				out[st.Name].Spec.Variables[vn] = v
			}
			for target, vs := range st.Spec.Binds {
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
	pool := recorded("pool-0", 0, "POST", "/v1/sessions", `{"sid":"s-1"}`, nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}, perTest: []*models.Mock{
		recorded("mock-1", time.Second, "GET", "/v1/sessions/s-1", "", nil, `{}`),
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/v1/charges", `{"key":"k-1","again":"k-1"}`, map[string]string{"Idempotency-Key": "k-1"}, `{"key":"k-1"}`)}}
	served, miss, _ := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"key":"k-2","again":"k-2"}`, map[string]string{"Idempotency-Key": "k-2"}), db, n, opts)
	if served == nil || miss != nil || served.Spec.HTTPResp.Body != `{"key":"k-2"}` {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	vars := db.consumed[0].Spec.Variables
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
		recorded("mock-a", 0, "POST", "/v1/a", `{"id":"x-1111"}`, nil, `{}`),
		recorded("mock-b", time.Millisecond, "POST", "/v1/b", `{"id":"x-1111"}`, nil, `{}`),
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
	first := recorded("mock-0", 0, "GET", "/v1/ping", "", nil, `{"ok":true}`)
	first.Spec.HTTPResp.Header["Date"] = "Tue, 14 Oct 2026 09:59:00 GMT"
	second := recorded("mock-1", time.Second, "POST", "/v1/events", `{"at":"2026-10-14T10:00:00Z","kind":"x"}`, nil, `{}`)
	db := &learnDb{perTest: []*models.Mock{first, second}}
	if served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/ping", "", nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("first: %+v", miss)
	}
	tr.ServedMock(first) // as the mock manager tells it
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/events", `{"at":"2026-10-14T10:01:07Z","kind":"x"}`, nil), db, n, opts)
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
	pool := recorded("pool-0", 0, "GET", "/v1/flags?user=alice&ts=1", "", nil, `{}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	own := recorded("mock-1", time.Second, "GET", "/v1/flags?user=bob&ts=2", "", nil, `{}`)
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
	db := &learnDb{perTest: []*models.Mock{recorded("mock-0", 0, "POST", "/a", `{"kind":"order","id":"x"}`, nil, `{}`)}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"kind":"refund","id":"x"}`, nil), db, n, opts); served != nil || miss == nil {
		t.Fatalf("a per-test mock was paired outside every test: served %v", served)
	}
}

// Two leaves of one call recorded with one value that F saw drift to two
// values are not one echo: refused, never served.
func TestLearnMatchAnEchoOfTwoValuesIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{recorded("mock-1", 0, "POST", "/pay", `{"a":"k-1111","b":"k-1111"}`, nil, `{"key":"k-1111"}`)}}
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
		for n, v := range c.Spec.Variables {
			vars(m)[n] = v
		}
	}
	for _, c := range consumed {
		for target, vs := range c.Spec.Binds {
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
	db := &learnDb{perTest: per}
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
				recorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
				recorded("mock-2", time.Second, "GET", "/orders/o-1111?ts=100", "", nil, `{"id":"o-1111","state":"new"}`),
			},
			[]*req{live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), live("GET", "/orders/o-9999?ts=200", "", nil)},
			[]*req{live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil), live("GET", "/orders/o-5555?ts=300", "", nil)},
			"url.query.ts", "mock-2",
		},
		{
			"a path segment",
			[]*models.Mock{
				recorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
				recorded("mock-2", time.Second, "GET", "/orders/o-1111/v/100", "", nil, `{"id":"o-1111","state":"new"}`),
			},
			[]*req{live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), live("GET", "/orders/o-9999/v/200", "", nil)},
			[]*req{live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil), live("GET", "/orders/o-5555/v/300", "", nil)},
			"url.path.3", "mock-2",
		},
		{
			"one array element",
			[]*models.Mock{recorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","items":[{"at":"t-1"}]}`, nil, `{"id":"o-1111"}`)},
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
