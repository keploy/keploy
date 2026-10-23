package replay

import (
	"context"
	"crypto/md5" //nolint:gosec // an ETag form, not security
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These drive templated test sets through RunTestSet with the real replay
// hooks: every test request goes through Hooks.SimulateRequest →
// pkg.SimulateHTTP to a stand-in app, and every verdict through
// compareHTTPRespForReplay, exactly as `keploy test` runs them. The template
// map comes from the test set's config.yaml (testSetConf.Read), and the
// assertions read the report the run persisted.
//
// Fixtures use the forms `keploy templatize` writes: {{capture ...}} where a
// response produces a value, a use placeholder at each later place, and a
// JSON number's placeholder in quotes ("{{int .k}}").

// tmplConf is config.yaml: Read hands out a copy of the stored test set, and
// Write replaces it, so a test can tell whether a run wrote it. keploy
// templatize asks for the file it would write (ConfigFile) and puts it in
// place with its test cases (tzTestDB.ReplaceFiles, which calls put).
type tmplConf struct {
	mu     sync.Mutex
	stored models.TestSet
	staged *models.TestSet
}

func copyTestSet(ts models.TestSet) *models.TestSet {
	cp := ts
	cp.Template = make(map[string]interface{}, len(ts.Template))
	for k, v := range ts.Template {
		cp.Template[k] = v
	}
	return &cp
}

func (c *tmplConf) Read(context.Context, string) (*models.TestSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyTestSet(c.stored), nil
}
func (c *tmplConf) ReadForUpdate(ctx context.Context, id string) (*models.TestSet, error) {
	return c.Read(ctx, id)
}
func (c *tmplConf) Write(_ context.Context, _ string, ts *models.TestSet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = *copyTestSet(*ts)
	return nil
}
func (c *tmplConf) ReadSecret(context.Context, string) (map[string]interface{}, error) {
	return nil, nil
}
func (c *tmplConf) ConfigFile(_ context.Context, _ string, ts *models.TestSet) (setfile.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.staged = copyTestSet(*ts)
	return setfile.File{Path: "config.yaml"}, nil
}

// put puts the config templatize staged in place.
func (c *tmplConf) put() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.staged != nil {
		c.stored, c.staged = *c.staged, nil
	}
}
func (c *tmplConf) template() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyTestSet(c.stored).Template
}

// tmplApp is the stand-in application: a mux whose handlers record what they
// were sent.
type tmplApp struct {
	srv  *httptest.Server
	addr string
	mu   sync.Mutex
	got  []tmplSeen
}

type tmplSeen struct {
	method, path string
	rawQuery     string
	header       http.Header
	body         []byte
}

func newTmplApp(t *testing.T, routes func(a *tmplApp, mux *http.ServeMux)) *tmplApp {
	t.Helper()
	a := &tmplApp{}
	mux := http.NewServeMux()
	routes(a, mux)
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	a.addr = strings.TrimPrefix(a.srv.URL, "http://")
	return a
}

func (a *tmplApp) record(r *http.Request) tmplSeen {
	body, _ := io.ReadAll(r.Body)
	s := tmplSeen{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone(), body: body}
	a.mu.Lock()
	a.got = append(a.got, s)
	a.mu.Unlock()
	return s
}

// sent is every request the app got for method and a path with this prefix.
func (a *tmplApp) sent(method, pathPrefix string) []tmplSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []tmplSeen
	for _, s := range a.got {
		if s.method == method && strings.HasPrefix(s.path, pathPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// recorded is a response as record mode wrote it for this app and templatize
// then rewrote: the headers Go's server adds, with the given ones on top, and
// body as it is on disk. Content-Length stays what the app sent at record
// time, the body with each placeholder holding its config.yaml value. Date is
// a per-request header replay forgives.
func recorded(status int, header map[string]string, body string, tmpl map[string]interface{}) models.HTTPResp {
	atRecord := body
	atRecord = templates.NewRun(nil, tmpl, nil).Expected(models.HTTPResp{Body: body}, models.HTTPResp{}, templates.ExpectOptions{}).RecordedBody
	h := map[string]string{
		"Date":           "Mon, 05 Oct 2026 10:00:00 GMT",
		"Content-Length": strconv.Itoa(len(atRecord)),
	}
	if body != "" {
		h["Content-Type"] = "application/json"
	}
	for k, v := range header {
		h[k] = v
	}
	return models.HTTPResp{StatusCode: status, StatusMessage: http.StatusText(status), Header: h, Body: body}
}

var tmplBase = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

func tmplCase(i int, addr, method, path string, header map[string]string, body string, resp models.HTTPResp) *models.TestCase {
	if header == nil {
		header = map[string]string{}
	}
	at := tmplBase.Add(time.Duration(i) * time.Second)
	resp.Timestamp = at.Add(100 * time.Millisecond)
	return &models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    fmt.Sprintf("test-%d", i),
		HTTPReq: models.HTTPReq{
			Method:    models.Method(method),
			URL:       "http://" + addr + path,
			Header:    header,
			Body:      body,
			Timestamp: at,
		},
		HTTPResp: resp,
	}
}

type tmplHarness struct {
	r      *Replayer
	report *prReportDB
}

// newTmplHarness is a RunTestSet over cases with the real hooks, so requests go
// to the stand-in app through pkg.SimulateHTTP.
func newTmplHarness(t *testing.T, conf *tmplConf, cases []*models.TestCase, tune func(*config.Config)) *tmplHarness {
	t.Helper()
	cfg := &config.Config{}
	cfg.Path = t.TempDir()
	cfg.Test.Host = "127.0.0.1"
	cfg.Test.APITimeout = 5
	cfg.Test.HealthPollTimeout = 2 * time.Second
	// The stand-in app routes by path; a readiness probe would be one more
	// request to it. Nothing here waits on the app to come up.
	cfg.Test.DisableAppReadyProbe = true
	if tune != nil {
		tune(cfg)
	}
	instr := &prInstr{appStopped: make(chan struct{})}
	report := &prReportDB{}
	r := &Replayer{
		logger:               prLogger(),
		testDB:               &prTestDB{cases: cases},
		mockDB:               &prMockDB{},
		mappingDB:            &prMappingDB{},
		reportDB:             report,
		testSetConf:          conf,
		telemetry:            prTelemetry{},
		instrumentation:      instr,
		config:               cfg,
		instrument:           true,
		hookImpl:             newHooks(prLogger(), cfg, instr, false),
		completeTestReport:   make(map[string]TestReportVerdict),
		failedTCsBySetID:     make(map[string][]string),
		mockMismatchFailures: NewTestFailureStore(),
		consumedMockNames:    make(map[string]struct{}),
	}
	return &tmplHarness{r: r, report: report}
}

// run runs the set once and returns the status RunTestSet gives it, which
// decides keploy test's exit code.
func (h *tmplHarness) run(t *testing.T) models.TestSetStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.r.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if err != nil {
		t.Fatalf("RunTestSet returned an error, which is a different path from the one under test: %v", err)
	}
	return status
}

func (h *tmplHarness) result(t *testing.T, name string) models.TestResult {
	t.Helper()
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	for _, r := range h.report.results {
		if r.TestCaseID == name {
			return r
		}
	}
	t.Fatalf("no result for %s in the report", name)
	return models.TestResult{}
}

// why says what the report holds for a result that did not pass.
func why(r models.TestResult) string {
	var parts []string
	if !r.Result.StatusCode.Normal {
		parts = append(parts, fmt.Sprintf("status %d, want %d", r.Result.StatusCode.Actual, r.Result.StatusCode.Expected))
	}
	for _, b := range r.Result.BodyResult {
		parts = append(parts, fmt.Sprintf("body normal=%v expected=%s actual=%s", b.Normal, b.Expected, b.Actual))
	}
	for _, hr := range r.Result.HeadersResult {
		if !hr.Normal {
			parts = append(parts, fmt.Sprintf("header %s expected=%v actual=%v", hr.Expected.Key, hr.Expected.Value, hr.Actual.Value))
		}
	}
	return strings.Join(parts, "; ")
}

func assertStatus(t *testing.T, h *tmplHarness, name string, want models.TestStatus, msg string) {
	t.Helper()
	r := h.result(t, name)
	if r.Status != want {
		t.Errorf("%s: %s, want %s — %s\n  report: %s", name, r.Status, want, msg, why(r))
	}
}

// THE PRODUCER FALSE FAIL (fails on main). The test case that issues a value
// passes when the app issues a fresh one: its capture takes the app's answer
// before anything is compared. On main the whole test case, expected response
// included, was rendered with the values held before the request was sent.
func TestRunTestSet_TemplateProducerPassesWhenTheAppIssuesAFreshValue(t *testing.T) {
	cases := []struct {
		name     string
		template map[string]interface{}
		routes   func(a *tmplApp, mux *http.ServeMux)
		producer func(addr string) *models.TestCase
	}{
		{
			name:     "token in the body",
			template: map[string]interface{}{"token": "T0rec00001"},
			routes: func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"token":"T1live0001"}`)
				})
			},
			producer: func(addr string) *models.TestCase {
				return tmplCase(1, addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
					recorded(200, nil, `{"token":"{{capture string .token}}"}`, map[string]interface{}{"token": "T0rec00001"}))
			},
		},
		{
			name:     "session cookie in Set-Cookie",
			template: map[string]interface{}{"sid": "S0rec00001"},
			routes: func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Set-Cookie", "sid=S1live0001; Path=/; HttpOnly")
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			},
			producer: func(addr string) *models.TestCase {
				return tmplCase(1, addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
					recorded(200, map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/; HttpOnly"}, `{"ok":true}`, nil))
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, c.routes)
			conf := &tmplConf{stored: models.TestSet{Template: c.template}}
			h := newTmplHarness(t, conf, []*models.TestCase{c.producer(app.addr)}, nil)
			h.run(t)
			assertStatus(t, h, "test-1", models.TestStatusPassed, "the app issued a fresh value where the test case says a template value goes")
		})
	}
}

// THE FALSE PASS, through a run (fails on main). A later expected response
// that has the id written out, or a use placeholder, fails when the app
// answers another id. On main the matcher found the expected id among the
// template VALUES, took the app's answer and passed the test (PASSED with
// body_result.normal=false), and later tests sent the wrong id.
//
// The producer here answers the recorded id (assigned by the database the
// mocks replay), so the template still holds it when the later GET runs.
func TestRunTestSet_LaterResponseAssertsTheValueWrittenOut(t *testing.T) {
	const recID = "usr_01J9REC"
	for _, c := range []struct {
		name     string
		answerID string
		// readBack is the read-back's expected body: the id written out, or
		// the use placeholder templatize writes there now.
		readBack string
		want     models.TestStatus
	}{
		{name: "control: the app answers the id it created", answerID: recID, readBack: `{"id":"` + recID + `","name":"alice"}`, want: models.TestStatusPassed},
		{name: "regression: the app answers another id", answerID: "usr_OTHER01", readBack: `{"id":"` + recID + `","name":"alice"}`, want: models.TestStatusFailed},
		{name: "control, as templatize writes it", answerID: recID, readBack: `{"id":"{{string .id}}","name":"alice"}`, want: models.TestStatusPassed},
		{name: "regression, as templatize writes it", answerID: "usr_OTHER01", readBack: `{"id":"{{string .id}}","name":"alice"}`, want: models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+recID+`","name":"alice"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"`+c.answerID+`","name":"alice"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": recID}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"`+recID+`","name":"alice"}`, nil)),
				tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
					recorded(200, nil, c.readBack, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			assertStatus(t, h, "test-1", models.TestStatusPassed, "control: the create answered as recorded")
			assertStatus(t, h, "test-2", c.want, "the GET answered id "+c.answerID+" where the test case says "+recID)
		})
	}
}

// DEAD HEADER LEARNING, through a run (fails on main). Login issues a session
// in Set-Cookie; the next test sends it. On main the value never reached the
// template map (the copy SimulateHTTP kept to learn from shared
// tc.HTTPResp.Header, which the in-place render overwrote before sending), so
// the later request sent the recorded session and the app turned it away.
func TestRunTestSet_SessionCookieIssuedAtLoginReachesTheNextRequest(t *testing.T) {
	const live = "S1live0001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Set-Cookie", "sid="+live+"; Path=/; HttpOnly")
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if c, err := r.Cookie("sid"); err != nil || c.Value != live {
				writeJSON(w, http.StatusUnauthorized, `{"error":"no session"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"sid": "S0rec00001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
			recorded(200, map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/; HttpOnly"}, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/me", map[string]string{"Cookie": "theme=dark; sid={{string .sid}}"}, "",
			recorded(200, nil, `{"user":"alice"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)

	if me := app.sent("GET", "/me"); len(me) != 1 || me[0].header.Get("Cookie") != "theme=dark; sid="+live {
		var got []string
		for _, s := range me {
			got = append(got, s.header.Get("Cookie"))
		}
		t.Errorf("GET /me was sent Cookie %q; want %q", got, "theme=dark; sid="+live)
	}
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the request must carry the session the app issued at login")
	login := h.result(t, "test-1")
	if len(login.Captured) != 1 || login.Captured[0] != (models.TemplateCapture{Key: "sid", Value: live, Where: "header Set-Cookie.sid"}) {
		t.Errorf("the login's report does not say what it captured: %+v", login.Captured)
	}
	if got := h.result(t, "test-2").Req.Header["Cookie"]; got != "theme=dark; sid="+live {
		t.Errorf("the report shows the request as %q, not as it was sent", got)
	}
}

// NUMBERS OF ONE MILLION AND UP, through a run (fails on main). The app issues
// order id 7654321; a capture keeps the answer's digits for the path and the
// body. On main it was learned as the text "7.654321e+06", so the path sent
// the exponent and {{int}} rendered 0. (The path here is written by hand:
// templatize never puts a JSON number in a URL.)
func TestRunTestSet_LargeNumericIDReachesLaterRequestsExactly(t *testing.T) {
	const live = 7654321
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"order_id":%d}`, live))
		})
		mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if r.URL.Path != fmt.Sprintf("/orders/%d", live) {
				writeJSON(w, http.StatusNotFound, `{"error":"no such order"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"item":"book"}`)
		})
		mux.HandleFunc("/payments", func(w http.ResponseWriter, r *http.Request) {
			s := a.record(r)
			var p struct {
				OrderID json.Number `json:"order_id"`
			}
			if err := json.Unmarshal(s.body, &p); err != nil || p.OrderID.String() != strconv.Itoa(live) {
				writeJSON(w, http.StatusBadRequest, `{"error":"unknown order"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"paid":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"orderId": "1234567"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/orders", map[string]string{"Content-Type": "application/json"}, `{"item":"book"}`,
			recorded(200, nil, `{"order_id":"{{capture int .orderId}}"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "GET", "/orders/{{string .orderId}}", nil, "",
			recorded(200, nil, `{"item":"book"}`, nil)),
		tmplCase(3, app.addr, "POST", "/payments", map[string]string{"Content-Type": "application/json"}, `{"order_id":"{{int .orderId}}"}`,
			recorded(200, nil, `{"paid":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)

	for _, s := range app.sent("GET", "/orders/") {
		if s.path != fmt.Sprintf("/orders/%d", live) {
			t.Errorf("GET was sent path %q; want /orders/%d", s.path, live)
		}
	}
	for _, s := range app.sent("POST", "/payments") {
		if !strings.Contains(string(s.body), strconv.Itoa(live)) {
			t.Errorf("POST /payments was sent %s; want order_id %d", s.body, live)
		}
	}
	assertStatus(t, h, "test-1", models.TestStatusPassed, "the create's capture takes the number the app issued")
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the path must carry the order id the app issued")
	assertStatus(t, h, "test-3", models.TestStatusPassed, "the body must carry the order id the app issued")
}

// expressETag is the weak ETag Express sends by default: W/"<len hex>-<sha1
// base64, 27 chars>".
func expressETag(body string) string {
	sum := sha1.Sum([]byte(body))
	return fmt.Sprintf(`W/"%x-%s"`, len(body), base64.StdEncoding.EncodeToString(sum[:])[:27])
}

// rackETag is Rails' default ETag (Rack::ETag): weak, the first 32 hex digits
// of the body's SHA-256.
func rackETag(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `W/"` + hex.EncodeToString(sum[:])[:32] + `"`
}

// springETag is the ETag of Spring's ShallowEtagHeaderFilter: "0" and the
// body's hex MD5.
func springETag(body string) string {
	sum := md5.Sum([]byte(body)) //nolint:gosec // an ETag form, not security
	return `"0` + hex.EncodeToString(sum[:]) + `"`
}

// THE ETAG/DIGEST FALSE FAIL AFTER RENDERING (fails on main with this
// hand-written use placeholder). A later expected response rendered to the
// run's id is byte-identical to the app's answer, but the ETag on disk is
// Express's digest of the RECORDED body. The matcher is told the recorded
// body: an ETag that is a digest of it is asserted as the same digest of the
// answer's body.
func TestRunTestSet_DigestHeaderOfARenderedResponseDoesNotFail(t *testing.T) {
	const recID, liveID = "usr_01J9REC", "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+liveID+`"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			id := strings.TrimPrefix(r.URL.Path, "/users/")
			body := `{"id":"` + id + `","name":"alice"}`
			w.Header().Set("ETag", expressETag(body))
			writeJSON(w, http.StatusOK, body)
		})
	})
	recordedBody := `{"id":"` + recID + `","name":"alice"}`
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": recID}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
			recorded(200, map[string]string{http.CanonicalHeaderKey("ETag"): expressETag(recordedBody)}, `{"id":"{{string .id}}","name":"alice"}`, conf.stored.Template)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)

	if got := app.sent("GET", "/users/"); len(got) != 1 || got[0].path != "/users/"+liveID {
		t.Fatalf("precondition: the GET must go to the created id; it went to %v", got)
	}
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the body matched; the ETag only follows the id the app issued")
}

// A RE-SEND IS NOT THE SAME CASE, through a run. The login's first send is cut
// off before any mock is consumed, so replay re-sends it (retryResetOnce). The
// re-send is the rendered copy, and the expected response is resolved from
// the case as written, so the token the app issues on the re-send is the one
// captured and sent next. (This passes on main too, where forgiveness by value
// hid the stale re-send; main's bug shows only at the SimulateHTTP level. It
// guards against resolving from an already-rendered case.)
func TestRunTestSet_ReSentProducerStillHandsItsValueOn(t *testing.T) {
	const live = "T1live0001"
	for _, c := range []struct {
		name       string
		resetFirst bool
	}{
		{name: "control: login answered on the first send", resetFirst: false},
		{name: "login answered on the re-send after a reset", resetFirst: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			logins := 0
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					mu.Lock()
					logins++
					n := logins
					mu.Unlock()
					if c.resetFirst && n == 1 {
						if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
							_ = conn.Close()
						}
						return
					}
					writeJSON(w, http.StatusOK, `{"token":"`+live+`"}`)
				})
				mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					if r.Header.Get("Authorization") != "Bearer "+live {
						writeJSON(w, http.StatusUnauthorized, `{"error":"bad token"}`)
						return
					}
					writeJSON(w, http.StatusOK, `{"user":"alice"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"token": "T0rec00001"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
					recorded(200, nil, `{"token":"{{capture string .token}}"}`, map[string]interface{}{"token": "T0rec00001"})),
				tmplCase(2, app.addr, "GET", "/me", map[string]string{"Authorization": "Bearer {{string .token}}"}, "",
					recorded(200, nil, `{"user":"alice"}`, nil)),
			}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			assertStatus(t, h, "test-1", models.TestStatusPassed, "the login's capture takes the token of the answer it got")

			wantLogins := 1
			if c.resetFirst {
				wantLogins = 2
			}
			if got := len(app.sent("POST", "/login")); got != wantLogins {
				t.Fatalf("precondition: login was sent %d times; want %d", got, wantLogins)
			}
			for _, s := range app.sent("GET", "/me") {
				if got := s.header.Get("Authorization"); got != "Bearer "+live {
					t.Errorf("GET /me was sent %q; want the token the app issued (%q)", got, "Bearer "+live)
				}
			}
			assertStatus(t, h, "test-2", models.TestStatusPassed, "the next request must carry the token the login issued")
		})
	}
}

// --update-template NEVER WRITES config.yaml (rule 5; fails on main). A value
// that holds a `"` stays as recorded, and the next run sends it. On main the
// run's values were written back after RemoveDoubleQuotes deleted every `"`,
// so the next run sent a value the recording never held.
func TestRunTestSet_UpdateTemplateKeepsAValueWithAQuote(t *testing.T) {
	const etag = `W/"x1-abc"`
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/doc", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if r.Header.Get("If-None-Match") != etag {
				writeJSON(w, http.StatusPreconditionFailed, `{"error":"stale"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"fresh":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"etag": etag}}}
	cases := func() []*models.TestCase {
		return []*models.TestCase{
			tmplCase(1, app.addr, "GET", "/doc", map[string]string{"If-None-Match": "{{string .etag}}"}, "",
				recorded(200, nil, `{"fresh":true}`, nil)),
		}
	}
	updateTemplate := func(cfg *config.Config) { cfg.Test.UpdateTemplate = true }

	first := newTmplHarness(t, conf, cases(), updateTemplate)
	first.run(t)
	assertStatus(t, first, "test-1", models.TestStatusPassed, "control: the header is sent as config.yaml holds it")
	if got := conf.template()["etag"]; got != etag {
		t.Errorf("after --update-template config.yaml holds etag %q; want %q", got, etag)
	}

	second := newTmplHarness(t, conf, cases(), nil)
	second.run(t)
	assertStatus(t, second, "test-1", models.TestStatusPassed, "the run after --update-template sends what config.yaml now holds")
}

// EACH PASS STARTS FROM THE RECORDED VALUES (rule 5). With RetryPassing every
// cycle re-runs the passing tests as a fresh forward pass: a test that sends
// a key before any response of the pass captured it must send the recorded
// value every time, not what the previous cycle's login captured.
func TestRunTestSet_EachRetryCycleStartsFromTheRecordedValues(t *testing.T) {
	const recorded0 = "T0rec00001"
	var mu sync.Mutex
	minted := 0
	latest := ""
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if r.URL.Query().Get("token") != recorded0 {
				writeJSON(w, http.StatusUnauthorized, `{"error":"not the invite token"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			mu.Lock()
			minted++
			latest = fmt.Sprintf("T%dlive%04d", minted, minted)
			tok := latest
			mu.Unlock()
			writeJSON(w, http.StatusOK, `{"token":"`+tok+`"}`)
		})
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			mu.Lock()
			want := "Bearer " + latest
			mu.Unlock()
			if r.Header.Get("Authorization") != want {
				writeJSON(w, http.StatusUnauthorized, `{"error":"bad token"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
		// A test that fails is what makes RetryPassing run the passing ones
		// again as a new pass.
		mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusInternalServerError, `{"error":"down"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"token": recorded0}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/check?token={{string .token}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
			recorded(200, nil, `{"token":"{{capture string .token}}"}`, conf.stored.Template)),
		tmplCase(3, app.addr, "GET", "/me", map[string]string{"Authorization": "Bearer {{string .token}}"}, "", recorded(200, nil, `{"user":"alice"}`, nil)),
		tmplCase(4, app.addr, "GET", "/broken", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
	h.run(t)

	checks := app.sent("GET", "/check")
	if len(checks) < 2 {
		t.Fatalf("precondition: RetryPassing re-ran /check %d time(s); want at least 2", len(checks))
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	for _, r := range h.report.results {
		if r.TestCaseID != "test-4" && r.Status != models.TestStatusPassed {
			t.Errorf("%s: %s in some pass — %s", r.TestCaseID, r.Status, why(r))
		}
		if r.TestCaseID == "test-1" && !strings.Contains(r.Req.URL, "token="+recorded0) {
			t.Errorf("a pass sent /check with %q; every pass starts from the recorded token", r.Req.URL)
		}
	}
}

// REFUSED WHEN THE SET LOADS (rule 9). A capture in a request cannot mean
// anything: the test case fails, unsent, with the reason, and the rest of the
// set runs.
func TestRunTestSet_ARefusedPlaceholderFailsItsTestCaseUnsent(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"usr_1"}`)
		})
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"id":"{{capture string .id}}"}`,
			recorded(201, nil, `{"id":"usr_1"}`, nil)),
		tmplCase(2, app.addr, "GET", "/health", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Errorf("the set is %s; a set with a refused test case fails", status)
	}
	if got := app.sent("POST", "/users"); len(got) != 0 {
		t.Errorf("the refused test case was sent %d time(s)", len(got))
	}
	refusals := logs.FilterMessageSnippet("a test case's placeholder is refused").All()
	if len(refusals) != 1 || refusals[0].Level != zap.ErrorLevel || refusals[0].ContextMap()["testcase"] != "test-1" || refusals[0].ContextMap()["placeholder"] != "{{capture string .id}}" {
		t.Errorf("the refusal was logged %d time(s): %+v; want once, as an error naming test-1 and its placeholder", len(refusals), refusals)
	}
	assertRefusedUnsent(t, h, logs, "test-1", "request body", "{{capture string .id}}", "belongs in an expected response")
	// Its report holds the request as written: it was never rendered or sent.
	if req := h.result(t, "test-1").Req; req.URL != "http://"+app.addr+"/users" || req.Body != `{"id":"{{capture string .id}}"}` {
		t.Errorf("the refused test case's report holds the request %+v; want it as written", req)
	}
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the rest of the set runs")
	// RS2': the refused test case counts as a failure in the set's report,
	// never as skipped.
	h.report.mu.Lock()
	rep := h.report.report
	h.report.mu.Unlock()
	if rep == nil || rep.Failure != 1 || rep.Success != 1 || rep.Total != 2 {
		t.Errorf("the set's report counts %+v; want 1 failure (the refused test case), 1 success, 2 in all", rep)
	}
}

// assertRefusedUnsent checks what a test case refused when its set loaded
// leaves behind: FAILED, its refusal saved as its template failure (where,
// placeholder, why) graded TEMPLATE_VARIABLE and HIGH, no comparison row and
// no category taken from comparing a request never sent, and the refusal's
// own next step logged.
func assertRefusedUnsent(t *testing.T, h *tmplHarness, logs *observer.ObservedLogs, name, where, placeholder, why string) {
	t.Helper()
	r := h.result(t, name)
	if r.Status != models.TestStatusFailed {
		t.Errorf("%s: %s; a refused test case fails", name, r.Status)
	}
	if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != where || r.TemplateFailures[0].Placeholder != placeholder || !r.TemplateFailures[0].Refused ||
		!strings.Contains(r.TemplateFailures[0].Why, "refused when the test set loaded") || !strings.Contains(r.TemplateFailures[0].Why, why) {
		t.Errorf("%s: template failures %+v; want the refusal at %s of %s, %q", name, r.TemplateFailures, where, placeholder, why)
	}
	if r.FailureInfo.Risk != models.High || len(r.FailureInfo.Category) != 1 || r.FailureInfo.Category[0] != models.TemplateVariable {
		t.Errorf("%s: graded %s %v; want HIGH and only TEMPLATE_VARIABLE", name, r.FailureInfo.Risk, r.FailureInfo.Category)
	}
	if !r.Result.StatusCode.Normal || len(r.Result.HeadersResult) != 0 || len(r.Result.BodyResult) != 0 || r.Res.StatusCode != 0 || r.Res.Body != "" {
		t.Errorf("%s: the result holds a comparison of a request never sent: %+v, response %+v", name, r.Result, r.Res)
	}
	said := false
	for _, e := range logs.FilterMessageSnippet("failed the test without sending it").All() {
		if e.ContextMap()["testcase"] != name {
			continue
		}
		said = true
		if e.ContextMap()["next_step"] != nextStepRefused {
			t.Errorf("%s's failure says next_step %q; want the refusal's own, %q", name, e.ContextMap()["next_step"], nextStepRefused)
		}
	}
	if !said {
		t.Errorf("%s's failure was not logged", name)
	}
}

// decryptingHooks stands in for enterprise's DecryptingTestHooks: it changes
// the test case it is handed IN PLACE (here, decrypting "ENC:" values in its
// expected body and headers) before sending it.
type decryptingHooks struct{ TestHooks }

func (h decryptingHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	tc.HTTPResp.Body = strings.ReplaceAll(tc.HTTPResp.Body, "ENC:alice", "alice")
	for k, v := range tc.HTTPResp.Header {
		tc.HTTPResp.Header[k] = strings.ReplaceAll(v, "ENC:alice", "alice")
	}
	return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
}

// THE EXPECTED RESPONSE IS THE CASE AS SENT (rule 4). It is resolved from the
// copy SimulateRequest got, after any hook changed it in place, not from the
// test case as loaded; and that copy shares no map with the loaded case, so
// the hook's change never reaches it.
func TestRunTestSet_TheExpectedResponseIsTheCaseAsSent(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("X-Owner", "alice")
			writeJSON(w, http.StatusCreated, `{"id":"usr_LIVE","name":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
			recorded(201, map[string]string{"X-Owner": "ENC:alice"}, `{"id":"{{capture string .id}}","name":"ENC:alice"}`, map[string]interface{}{"id": "usr_REC"})),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.r.hookImpl = decryptingHooks{TestHooks: h.r.hookImpl}
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "the expected response must be the one the hook decrypted")
	if cases[0].HTTPResp.Body != `{"id":"{{capture string .id}}","name":"ENC:alice"}` {
		t.Errorf("the test case as loaded was changed: %s", cases[0].HTTPResp.Body)
	}
	if got := cases[0].HTTPResp.Header["X-Owner"]; got != "ENC:alice" {
		t.Errorf("the hook's change reached the loaded test case's header map: X-Owner %q", got)
	}
}

// A CAPTURED VALUE IS NEVER WRITTEN TO config.yaml (rule 5), --update-template
// or not: it is this run's. config.yaml keeps the recorded value, which every
// later run starts from.
func TestRunTestSet_ACapturedValueIsNeverWrittenToConfigYaml(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"token":"T1live0001"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"token": "T0rec00001"}}}
	for _, update := range []bool{false, true} {
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
				recorded(200, nil, `{"token":"{{capture string .token}}"}`, conf.stored.Template)),
		}
		h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.UpdateTemplate = update })
		h.run(t)
		assertStatus(t, h, "test-1", models.TestStatusPassed, "the login captures the token it is issued")
		if got := h.result(t, "test-1").Captured; len(got) != 1 || got[0].Value != "T1live0001" {
			t.Errorf("the report does not say the login captured T1live0001: %+v", got)
		}
		if got := conf.template()["token"]; got != "T0rec00001" {
			t.Errorf("with --update-template=%v config.yaml holds token %q; want the recorded T0rec00001", update, got)
		}
	}
}

// RULES 6 AND 9: a set with no templates whose recorded traffic holds
// placeholder-shaped text (an app that stores keploy test cases, a mail
// template service) is sent and compared as written, as on main: no
// placeholder of an undefined key is refused.
func TestRunTestSet_ASetWithoutTemplatesIsSentAndComparedAsWritten(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/testcases", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"stored":"resp: {{capture string .id}}"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/testcases", map[string]string{"Content-Type": "application/json"}, `{"yaml":"body: {{capture string .id}}"}`,
			recorded(201, nil, `{"stored":"resp: {{capture string .id}}"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "a set without templates is sent and compared as written")
	if got := app.sent("POST", "/testcases"); len(got) != 1 || string(got[0].body) != `{"yaml":"body: {{capture string .id}}"}` {
		t.Errorf("the test case was sent %d time(s), last %q; want once, as written", len(got), got)
	}
}

// THE LOADED TEST CASE IS NEVER WRITTEN. A header capture resolves on copies
// of the expected response's header map, so under RetryPassing the next
// pass's login captures again from its own Set-Cookie instead of asserting
// the session the first pass took.
func TestRunTestSet_AHeaderCaptureLeavesTheLoadedCaseAsWritten(t *testing.T) {
	var mu sync.Mutex
	minted, latest := 0, ""
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			mu.Lock()
			minted++
			latest = fmt.Sprintf("S%dlive%04d", minted, minted)
			sid := latest
			mu.Unlock()
			w.Header().Set("Set-Cookie", "sid="+sid+"; Path=/; HttpOnly")
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			mu.Lock()
			want := latest
			mu.Unlock()
			if c, err := r.Cookie("sid"); err != nil || c.Value != want {
				writeJSON(w, http.StatusUnauthorized, `{"error":"no session"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
		mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusInternalServerError, `{"error":"down"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"sid": "S0rec00001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"user":"alice"}`,
			recorded(200, map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/; HttpOnly"}, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/me", map[string]string{"Cookie": "sid={{string .sid}}"}, "", recorded(200, nil, `{"user":"alice"}`, nil)),
		tmplCase(3, app.addr, "GET", "/broken", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
	h.run(t)
	if n := len(app.sent("POST", "/login")); n < 2 {
		t.Fatalf("precondition: RetryPassing re-ran login %d time(s)", n)
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	for _, r := range h.report.results {
		if r.TestCaseID != "test-3" && r.Status != models.TestStatusPassed {
			t.Errorf("%s: %s in some pass — %s", r.TestCaseID, r.Status, why(r))
		}
	}
	if got := cases[0].HTTPResp.Header["Set-Cookie"]; got != "sid={{capture string .sid}}; Path=/; HttpOnly" {
		t.Errorf("the loaded test case's Set-Cookie was rewritten to %q", got)
	}
}

// RETRYPASSING KEEPS A SKIPPED PRODUCER'S CAPTURE. The producer fails on a
// field of its own (a date) but its capture took the id; the consumer passed.
// A later cycle re-runs only the consumer: at its position a whole pass gives
// the key the producer's capture, so it is sent the live id again, as on
// main, not the recorded one.
func TestRunTestSet_RetryPassingKeepsASkippedProducersCapture(t *testing.T) {
	const live = "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+live+`","created":"2026-10-08"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if r.URL.Path != "/users/"+live {
				writeJSON(w, http.StatusNotFound, `{"error":"no such user"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"name":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}","created":"2026-10-05"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"name":"alice"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
	h.run(t)
	gets := app.sent("GET", "/users/")
	if len(gets) < 2 {
		t.Fatalf("precondition: RetryPassing re-ran the read %d time(s); want at least 2", len(gets))
	}
	if n := len(app.sent("POST", "/users")); n != 1 {
		t.Fatalf("precondition: the failing producer ran %d times; want once", n)
	}
	for i, g := range gets {
		if g.path != "/users/"+live {
			t.Errorf("pass %d sent the read to %q; want the id the producer captured (%s)", i+1, g.path, live)
		}
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	for _, r := range h.report.results {
		if r.TestCaseID == "test-2" && r.Status != models.TestStatusPassed {
			t.Errorf("test-2: %s in some pass — %s", r.Status, why(r))
		}
	}
}

// STREAMED TEST CASES RUN AFTER THE CYCLES, with the values a whole pass ends
// with: the producer, skipped by the last cycle and after the last test case
// it ran, still gives its capture; it is sent, and the streamed expected
// response asserts it. The stream comparison reads the recorded frames
// (StreamBody, as a recorded NDJSON response loads), so their uses are
// rendered: a stream naming another id fails.
func TestRunTestSet_AStreamedCaseAfterRetryCyclesSeesTheWholePassValue(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name     string
		streamed func(id string) string // the id the stream names
		want     models.TestStatus
	}{
		{"the stream names the id it was sent", func(id string) string { return id }, models.TestStatusPassed},
		{"the stream names another id", func(string) string { return "usr_OTHER01" }, models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+live+`","created":"2026-10-08"}`)
				})
				mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					id := c.streamed(strings.TrimPrefix(r.URL.Path, "/events/"))
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"user":"`+id+`","n":1}`+"\n"+`{"user":"`+id+`","n":2}`+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			frame := func(n int) models.HTTPStreamChunk {
				return models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: fmt.Sprintf(`{"user":"{{string .id}}","n":%d}`, n)}}}
			}
			stream := tmplCase(3, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`,
				StreamBody: []models.HTTPStreamChunk{frame(1), frame(2)},
			})
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "GET", "/ping", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
				tmplCase(2, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"{{capture string .id}}","created":"2026-10-05"}`, conf.stored.Template)),
				stream,
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
			h.run(t)
			if n := len(app.sent("GET", "/ping")); n < 2 {
				t.Fatalf("precondition: RetryPassing ran a second cycle %d time(s)", n)
			}
			events := app.sent("GET", "/events/")
			if len(events) != 1 || events[0].path != "/events/"+live {
				t.Errorf("the streamed case was sent %v; want once, to /events/%s", events, live)
			}
			assertStatus(t, h, "test-3", c.want, "the stream's frames against the captured id")
			if got := stream.HTTPResp.StreamBody[0].Data[0].Value; got != `{"user":"{{string .id}}","n":1}` {
				t.Errorf("the loaded case's frames were rendered in place: %q", got)
			}
		})
	}
}

// A capture in a streamed response is refused when the set loads (streamed
// test cases run after the others, so it could feed nothing): the case fails,
// unsent, with the reason.
func TestRunTestSet_ARefusedStreamedCaptureFailsUnsent(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"id":"x"}`+"\n")
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/events", nil, "", models.HTTPResp{
			StatusCode: 200, Header: map[string]string{"Content-Type": "application/x-ndjson"},
			Body: `{"id":"{{capture string .id}}"}` + "\n",
		}),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Errorf("the set is %s; a set with a refused streamed test case fails", status)
	}
	if got := app.sent("GET", "/events"); len(got) != 0 {
		t.Errorf("the refused streamed case was sent %d time(s)", len(got))
	}
	assertRefusedUnsent(t, h, logs, "test-1", "response body frame 0.id", "{{capture string .id}}", "a streamed response does not capture")
	// RS2' for the streamed loop: the refused test case counts as a failure
	// in the set's report.
	h.report.mu.Lock()
	rep := h.report.report
	h.report.mu.Unlock()
	if rep == nil || rep.Failure != 1 || rep.Total != 1 {
		t.Errorf("the set's report counts %+v; want 1 failure (the refused streamed test case), 1 in all", rep)
	}
}

// grpcStubHooks answers gRPC test cases itself and keeps what it was sent; an
// HTTP test case goes to the real hooks.
type grpcStubHooks struct {
	TestHooks
	mu     sync.Mutex
	sent   []models.GrpcReq
	answer func(models.GrpcReq) *models.GrpcResp
}

func (h *grpcStubHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.Kind != models.GRPC_EXPORT {
		return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
	}
	h.mu.Lock()
	h.sent = append(h.sent, tc.GrpcReq)
	h.mu.Unlock()
	return h.answer(tc.GrpcReq), nil
}

// A gRPC test case is sent with the run's values in its headers and body, and
// its expected response asserts them: a value an HTTP test case captured
// reaches the gRPC call, and an answer naming another id fails.
func TestRunTestSet_AGRPCCaseSendsAndAssertsTheRunsValue(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name     string
		answerID string
		// headerID is the id the answer's x-user header names; "" = answerID.
		headerID string
		noise    map[string]map[string][]string // the run's global noise
		// unsent: the call's request does not send the id, so a failure's
		// next step is not a read-back's.
		unsent bool
		// diverged: the recording maps a mock to the call that this replay
		// never consumes, so a failure would be demoted to OBSOLETE.
		diverged bool
		want     models.TestStatus
	}{
		{name: "the call answers the id it was sent", answerID: live, want: models.TestStatusPassed},
		{name: "the call answers another id", answerID: "usr_OTHER01", want: models.TestStatusFailed},
		// A gRPC template failure is never demoted to OBSOLETE (P2).
		{name: "the call answers another id, its mock set diverged", answerID: "usr_OTHER01", diverged: true, want: models.TestStatusFailed},
		// The gRPC matcher compares no ordinary header but content-type, and
		// the noise covers x-user or the message: the use is the only
		// assertion there, and it holds.
		{name: "a header use under noise answering another id", answerID: live, headerID: "usr_OTHER01", noise: map[string]map[string][]string{"header": {"x-user": {}}}, want: models.TestStatusFailed},
		{name: "a message use under noise answering another id", answerID: "usr_OTHER01", headerID: live, noise: map[string]map[string][]string{"body": {"decoded_data": {}}}, want: models.TestStatusFailed},
		{name: "every use under noise holding its value", answerID: live, noise: map[string]map[string][]string{"header": {"x-user": {}}, "body": {"decoded_data": {}}}, want: models.TestStatusPassed},
		// gRPC header names are case-insensitive: a noise entry X-User covers
		// x-user, and is named as covering the use there.
		{name: "a header use under noise written in another case", answerID: live, headerID: "usr_OTHER01", noise: map[string]map[string][]string{"header": {"X-User": {}}}, want: models.TestStatusFailed},
		// A gRPC response never captures (W-d): the next step of a use it
		// fails on offers no capture, also where its request did not send
		// the key.
		{name: "a call that does not send the id answers another", answerID: "usr_OTHER01", unsent: true, want: models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			grpcResp := func(id, headerID string) models.GrpcResp {
				resp := models.GrpcResp{
					Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc", "x-user": headerID}},
					Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
				}
				resp.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "` + id + `"` + "\n" + `2: "alice"`}})
				return resp
			}
			sends := "{{string .id}}"
			if c.unsent {
				sends = "me"
			}
			req := models.GrpcReq{Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":authority": app.addr, ":path": "/users.Users/Get", ":method": "POST", ":scheme": "http"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc", "x-user-id": sends},
			}}
			req.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "` + sends + `"`}})
			grpcCase := &models.TestCase{
				Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "test-2",
				GrpcReq: req, GrpcResp: grpcResp("{{string .id}}", "{{string .id}}"),
			}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				grpcCase,
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.GlobalNoise.Global = c.noise })
			stub := &grpcStubHooks{TestHooks: h.r.hookImpl, answer: func(models.GrpcReq) *models.GrpcResp {
				headerID := c.headerID
				if headerID == "" {
					headerID = c.answerID
				}
				resp := grpcResp(c.answerID, headerID)
				return &resp
			}}
			h.r.hookImpl = stub
			if c.diverged {
				h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-2": {{Name: "mock-2", Kind: string(models.GRPC_EXPORT)}}}}
			}
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			st := h.run(t)
			if c.diverged && (st != models.TestSetStatusFailed || len(logs.FilterMessageSnippet("mock mapping mismatch detected; marking testcase as FAILED").All()) != 1) {
				t.Errorf("a gRPC template failure with a diverged mock set: set %s; want FAILED, said as FAILED", st)
			}
			stub.mu.Lock()
			sent := stub.sent
			stub.mu.Unlock()
			if len(sent) != 1 {
				t.Fatalf("the gRPC case was sent %d times; want once", len(sent))
			}
			if c.unsent {
				assertStatus(t, h, "test-2", c.want, "the gRPC answer's id against the captured one")
				var nexts []string
				for _, e := range logs.FilterMessageSnippet("a template variable's place does not hold what it must").All() {
					if e.ContextMap()["testcase"] == "test-2" {
						nexts = append(nexts, fmt.Sprint(e.ContextMap()["next_step"]))
					}
				}
				if len(nexts) == 0 {
					t.Fatalf("the gRPC template failure was not logged")
				}
				for _, next := range nexts {
					if strings.Contains(next, "{{capture") || !strings.Contains(next, "captured") {
						t.Errorf("a gRPC failure's next step is %q; a gRPC response never captures, so it offers no capture", next)
					}
				}
				return
			}
			if got := sent[0].Headers.OrdinaryHeaders["x-user-id"]; got != live {
				t.Errorf("the gRPC header was sent %q; want the captured %q", got, live)
			}
			if got := sent[0].Body.DecodedData; got != `1: "`+live+`"` {
				t.Errorf("the gRPC body was sent %q; want the captured id", got)
			}
			if got := grpcCase.GrpcReq.Headers.OrdinaryHeaders["x-user-id"]; got != "{{string .id}}" {
				t.Errorf("the loaded gRPC case was rendered in place: %q", got)
			}
			assertStatus(t, h, "test-2", c.want, "the gRPC answer's id against the captured one")
			if r := h.result(t, "test-2"); c.want == models.TestStatusFailed {
				if len(r.TemplateFailures) == 0 {
					t.Errorf("test-2 failed without naming the gRPC use that did not hold: %+v", r.Result)
				}
				if !hasCategory(r, models.TemplateVariable) || r.FailureInfo.Risk != models.High {
					t.Errorf("the gRPC template failure is graded %q %v; want HIGH and TEMPLATE_VARIABLE", r.FailureInfo.Risk, r.FailureInfo.Category)
				}
			}
			// Each noise entry over a gRPC use is named when the set loads.
			for kind, entries := range c.noise {
				for entry := range entries {
					named := false
					for _, e := range logs.FilterMessageSnippet("a noise entry covers a template variable's place").All() {
						if e.ContextMap()["noise"] == kind+"."+entry+" (globalNoise)" && e.ContextMap()["testcase"] == "test-2" {
							named = true
						}
					}
					if !named {
						t.Errorf("the noise entry %s.%s over a gRPC use was not named", kind, entry)
					}
				}
			}
		})
	}
}

// A MISSED CAPTURE FAILS ITS TEST CASE, whatever the comparison concluded: a
// create that answers an empty body, a body that is not JSON (compared only
// with compareAll), or the id under noise, takes no id. The create fails,
// and its report names the place and why.
func TestRunTestSet_AMissedCaptureFailsWhateverTheComparisonSays(t *testing.T) {
	for _, c := range []struct {
		name, answer, expected string
		noise                  map[string][]string
	}{
		{name: "an empty body", answer: ``, expected: `{"id":"{{capture string .id}}","name":"alice"}`},
		{name: "a truncated body", answer: `{"id":"usr_LIVE`, expected: `{"id":"{{capture string .id}}","name":"alice"}`},
		{name: "the id under noise, missing", answer: `{"name":"alice"}`, expected: `{"id":"{{capture string .id}}","name":"alice"}`, noise: map[string][]string{"body.id": {}}},
		{name: "the id under noise, a number", answer: `{"id":7,"name":"alice"}`, expected: `{"id":"{{capture string .id}}","name":"alice"}`, noise: map[string][]string{"body.id": {}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, c.answer)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			create := tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
				recorded(201, nil, c.expected, conf.stored.Template))
			create.Noise = c.noise
			// Content-Length follows the body; what is under test is the capture.
			delete(create.HTTPResp.Header, "Content-Length")
			h := newTmplHarness(t, conf, []*models.TestCase{create}, nil)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != models.TestStatusFailed {
				t.Fatalf("test-1: %s; a capture that took nothing must fail its test case — %s", r.Status, why(r))
			}
			if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "body.id" || r.TemplateFailures[0].Placeholder != "{{capture string .id}}" || r.TemplateFailures[0].Why == "" {
				t.Errorf("the report does not name the capture's place and why: %+v", r.TemplateFailures)
			}
		})
	}
}

// A VARIABLE WINS OVER NOISE. A use placeholder under a noise entry is still
// asserted equal to its key's value (the noise still covers the rest), and
// the noise entry is named once when the set loads.
func TestRunTestSet_AUseUnderNoiseIsStillAsserted(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, answerID string
		want           models.TestStatus
	}{
		{"the read-back answers the captured id", live, models.TestStatusPassed},
		{"the read-back answers another id", "usr_OTHER01", models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"`+c.answerID+`","seen":"2026-10-08"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			read := tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
				recorded(200, nil, `{"id":"{{string .id}}","seen":"2026-10-05"}`, conf.stored.Template))
			delete(read.HTTPResp.Header, "Content-Length")
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				read,
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"id": {}, "seen": {}}}
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-2")
			if r.Status != c.want {
				t.Errorf("test-2: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			named, overCapture := 0, 0
			for _, e := range logs.FilterMessageSnippet("a noise entry covers a template variable's place").All() {
				next := e.ContextMap()["next_step"]
				if e.ContextMap()["testcase"] == "test-2" && e.ContextMap()["noise"] == "body.id (globalNoise)" {
					named++
					if next != nextStepNoiseOverUse {
						t.Errorf("the warning over test-2's use says next_step %q; want %q", next, nextStepNoiseOverUse)
					}
				}
				if e.ContextMap()["testcase"] == "test-1" {
					overCapture++
					// Following the use's advice (replace the placeholder with
					// the recorded value) would drop the capture, and every
					// later request would be sent the recorded id.
					if next != nextStepNoiseOverCapture {
						t.Errorf("the warning over test-1's capture says next_step %q; want %q", next, nextStepNoiseOverCapture)
					}
				}
			}
			if named != 1 {
				t.Errorf("the noise entry over test-2's {{string .id}} was named %d times; want once", named)
			}
			if overCapture != 1 {
				t.Errorf("the noise entry over test-1's capture was named %d times; want once", overCapture)
			}
		})
	}
}

// RULE 6'S WARNING: a misspelled key is literal text, sent and compared as
// written, and named once when the set loads.
func TestRunTestSet_AnUndefinedKeyIsNamedOnce(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"userId": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/users/{{string .userID}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/users/{{string .userID}}/x", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	warned := logs.FilterMessageSnippet("names a key the test set's config.yaml does not define").All()
	if len(warned) != 1 || warned[0].ContextMap()["placeholder"] != "{{string .userID}}" || warned[0].ContextMap()["testcase"] != "test-1" {
		t.Errorf("the misspelled key was named %d times: %+v; want once, at test-1", len(warned), warned)
	}
	if got := app.sent("GET", "/users/"); len(got) != 2 || !strings.Contains(got[0].path, "{{string .userID}}") {
		t.Errorf("the undefined key was not sent as written: %+v", got)
	}
}

// A KEY OUTSIDE THE GRAMMAR ({{string .user-id}}, which config.yaml may well
// hold) is text: sent as written, and named once as no template key.
func TestRunTestSet_AKeyOutsideTheGrammarIsNamedOnce(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"user-id": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/users/{{string .user-id}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/users/{{string .user-id}}/x", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	warned := logs.FilterMessageSnippet("names a key that is not a template key").All()
	if len(warned) != 1 || warned[0].ContextMap()["placeholder"] != "{{string .user-id}}" || warned[0].ContextMap()["testcase"] != "test-1" {
		t.Errorf("the key outside the grammar was named %d times: %+v; want once, at test-1", len(warned), warned)
	}
	if got := app.sent("GET", "/users/"); len(got) != 2 || !strings.Contains(got[0].path, "{{string .user-id}}") {
		t.Errorf("the key outside the grammar was not sent as written: %+v", got)
	}
}

// AN EXPRESSION ({{ .userId | print }}, which an older keploy rendered to the
// key's value) is text to keploy test: sent and compared as written, and
// named once, as an expression, with what to write in its place; also where
// it stands only in an expected response's body. The answer that holds the
// text itself passes: nothing is rendered.
func TestRunTestSet_AnExpressionIsNamedOnceAndSentAsWritten(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"len":"{{ .userId | len }}"}`)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"userId": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/users/{{ .userId | print }}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/users/{{ .userId | print }}/x", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(3, app.addr, "GET", "/echo", nil, "", recorded(200, nil, `{"len":"{{ .userId | len }}"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	warned := logs.FilterMessageSnippet("template text is an expression").All()
	if len(warned) != 2 {
		t.Fatalf("expressions were named %d times: %+v; want each of the two once", len(warned), warned)
	}
	for i, want := range []struct{ placeholder, testcase, where string }{
		{"{{ .userId | print }}", "test-1", "request url"},
		{"{{ .userId | len }}", "test-3", "response body.len"},
	} {
		got := warned[i].ContextMap()
		if got["placeholder"] != want.placeholder || got["testcase"] != want.testcase || got["where"] != want.where {
			t.Errorf("expression %d was named %+v; want %+v", i, got, want)
		}
		if next := fmt.Sprint(got["next_step"]); !strings.Contains(next, "write the value the expression stood for in its place") {
			t.Errorf("the next step does not say what to write: %s", next)
		}
	}
	if got := app.sent("GET", "/users/"); len(got) != 2 || !strings.Contains(got[0].path, "{{ .userId | print }}") {
		t.Errorf("the expression was not sent as written: %+v", got)
	}
	assertStatus(t, h, "test-3", models.TestStatusPassed, "an answer holding the expression's own text is the text as written")
	if named := logs.FilterMessageSnippet("names a key that is not a template key").All(); len(named) != 0 {
		t.Errorf("an expression was also named as a key outside the grammar: %+v", named)
	}
}

// A MAP OR A LIST IN config.yaml never stops the run: the test cases that
// name the key fail, unsent, with the key named; the rest run.
func TestRunTestSet_AMapValueFailsOnlyTheTestCasesThatUseIt(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"user": map[string]interface{}{"id": 1}, "id": "1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/users/{{string .user}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	r := h.result(t, "test-1")
	if r.Status != models.TestStatusFailed || len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Placeholder != "{{string .user}}" ||
		!r.TemplateFailures[0].Refused || !strings.Contains(r.TemplateFailures[0].Why, `"user"`) || !hasCategory(r, models.TemplateVariable) {
		t.Errorf("test-1: %s with %+v; want FAILED, refused, naming the key", r.Status, r.TemplateFailures)
	}
	assertStatus(t, h, "test-2", models.TestStatusPassed, "a test case that does not name the key runs")
}

// THE ETAG RULE. An ETag that is a digest of the recorded body is asserted as
// the same digest of the answer's body; any other ETag is compared as
// written. A version ETag that changed fails, as on main.
func TestRunTestSet_ADigestETagFollowsTheAnswerAndAVersionETagIsCompared(t *testing.T) {
	const recID, liveID = "usr_01J9REC", "usr_LIVE001"
	recordedBody := `{"id":"` + recID + `","name":"alice"}`
	for _, c := range []struct {
		name     string
		recorded string                   // the ETag on disk
		answer   func(body string) string // the ETag the app sends
		want     models.TestStatus
	}{
		{"a digest that matches the answer's body", expressETag(recordedBody), expressETag, models.TestStatusPassed},
		{"a digest of something else", expressETag(recordedBody), func(string) string { return expressETag(recordedBody) }, models.TestStatusFailed},
		{"Rails' Rack::ETag that matches the answer's body", rackETag(recordedBody), rackETag, models.TestStatusPassed},
		{"Rails' Rack::ETag of something else", rackETag(recordedBody), func(string) string { return rackETag(recordedBody) }, models.TestStatusFailed},
		{"Spring's ShallowEtagHeaderFilter that matches the answer's body", springETag(recordedBody), springETag, models.TestStatusPassed},
		{"Spring's ShallowEtagHeaderFilter of something else", springETag(recordedBody), func(string) string { return springETag(`{"id":"usr_OTHER01","name":"alice"}`) }, models.TestStatusFailed},
		{"a version ETag that stayed", `"1"`, func(string) string { return `"1"` }, models.TestStatusPassed},
		{"a version ETag that changed", `"1"`, func(string) string { return `"7"` }, models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+liveID+`"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					body := `{"id":"` + strings.TrimPrefix(r.URL.Path, "/users/") + `","name":"alice"}`
					w.Header().Set("ETag", c.answer(body))
					writeJSON(w, http.StatusOK, body)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": recID}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
					recorded(200, map[string]string{"Etag": c.recorded}, `{"id":"{{string .id}}","name":"alice"}`, conf.stored.Template)),
			}
			for _, disable := range []bool{false, true} {
				h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
					cfg.Test.DisableAutoHeaderNoise = disable
					// With auto header noise off, Date is noised as a user
					// would: the ETag is what is under test.
					cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"Date": {}}}
				})
				h.run(t)
				assertStatus(t, h, "test-2", c.want, fmt.Sprintf("--disableAutoHeaderNoise=%v", disable))
			}
		})
	}
}

// A COOKIE CHAIN. Login sets two cookies in one answer, one with an Expires
// date; templatize captures each. The next request carries both live values.
func TestRunTestSet_TwoCookiesOfOneLoginBothReachTheNextRequest(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Add("Set-Cookie", "csrftoken=C1live0001; Path=/; Expires="+time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
			w.Header().Add("Set-Cookie", "sessionid=S1live0001; Path=/; HttpOnly")
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			c1, e1 := r.Cookie("csrftoken")
			c2, e2 := r.Cookie("sessionid")
			if e1 != nil || e2 != nil || c1.Value != "C1live0001" || c2.Value != "S1live0001" || r.Header.Get("X-CSRFToken") != "C1live0001" {
				writeJSON(w, http.StatusForbidden, `{"error":"csrf"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"csrftoken": "C0rec0001", "sessionid": "S0rec0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"u":"a"}`,
			// Expires an hour after the recorded Date: the cookie lives an
			// hour, as the app's does.
			recorded(200, map[string]string{"Set-Cookie": "csrftoken={{capture string .csrftoken}}; Path=/; Expires=Mon, 05 Oct 2026 11:00:00 GMT,sessionid={{capture string .sessionid}}; Path=/; HttpOnly"}, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "POST", "/transfer", map[string]string{"Content-Type": "application/json", "Cookie": "csrftoken={{string .csrftoken}}; sessionid={{string .sessionid}}", "X-CSRFToken": "{{string .csrftoken}}"}, `{"amt":1}`,
			recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "each cookie is captured by identity; its Expires gives it the recorded lifetime")
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the transfer carries both cookies the login set")
	if got := h.result(t, "test-1").Captured; len(got) != 2 {
		t.Errorf("the login's report says it captured %+v; want both cookies", got)
	}
}

// A RENDERED URL GOES OUT AS IT STANDS. A value holding '{', '+' and '&' is
// escaped for its place, and nothing unescapes the whole URL again after.
func TestRunTestSet_ARenderedURLKeepsEachValueAsItIs(t *testing.T) {
	const cursor = `{"after":"a+b&c"}`
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if strings.TrimPrefix(r.URL.Path, "/page/") != cursor || r.URL.Query().Get("cursor") != cursor || r.URL.Query().Get("x") != "1" {
				writeJSON(w, http.StatusBadRequest, `{"error":"cursor"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"cursor": cursor}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/page/{{string .cursor}}?cursor={{string .cursor}}&x=1", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "the app reads the value as config.yaml holds it, in the path and in the query")
}

// A LOGIN THAT DELETES ITS SESSION FAILS. A cookie that holds a capture must
// be set: a Max-Age of 0, an Expires before the answer's Date and an empty
// value (Django's flush: sessionid="") each fail the login, also when the
// user noised Set-Cookie (the capture is asserted on its own). An Expires
// date gives the cookie its lifetime: a fresh date with the recorded
// lifetime passes, and Max-Age is compared as recorded.
func TestRunTestSet_ALoginThatDeletesItsSessionFails(t *testing.T) {
	const live = "S1live0001"
	// Recorded at 10:00:00 with a 15-minute session.
	const recordedCookie = "sessionid={{capture string .sid}}; expires=Mon, 05 Oct 2026 10:15:00 GMT; Max-Age=900; Path=/; HttpOnly"
	for _, c := range []struct {
		name   string
		cookie func() string // the app's Set-Cookie
		want   models.TestStatus
	}{
		{"a fresh Expires with the recorded lifetime", func() string {
			return "sessionid=" + live + "; expires=" + time.Now().Add(900*time.Second).UTC().Format(http.TimeFormat) + "; Max-Age=900; Path=/; HttpOnly"
		}, models.TestStatusPassed},
		{"Max-Age=0", func() string {
			return "sessionid=" + live + "; expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Path=/; HttpOnly"
		}, models.TestStatusFailed},
		{"an Expires in 1970", func() string {
			return "sessionid=" + live + "; expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/; HttpOnly"
		}, models.TestStatusFailed},
		{"Django's flush: an empty session, deleted", func() string {
			return `sessionid=""; expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Path=/`
		}, models.TestStatusFailed},
	} {
		for _, noised := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, Set-Cookie noised %v", c.name, noised), func(t *testing.T) {
				app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
					mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
						a.record(r)
						w.Header().Set("Set-Cookie", c.cookie())
						writeJSON(w, http.StatusOK, `{"ok":true}`)
					})
				})
				conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"sid": "S0rec0001"}}}
				cases := []*models.TestCase{
					tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"u":"a"}`,
						recorded(200, map[string]string{"Set-Cookie": recordedCookie}, `{"ok":true}`, nil)),
				}
				h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
					if noised {
						cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"Set-Cookie": {}}}
					}
				})
				h.run(t)
				r := h.result(t, "test-1")
				if r.Status != c.want {
					t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
				}
				if c.want == models.TestStatusFailed && noised && len(r.TemplateFailures) == 0 {
					t.Errorf("under noise the deleted session must fail on its capture: %+v", r)
				}
			})
		}
	}
}

// A CLEAR-THEN-SET LOGIN (Express res.clearCookie, then res.cookie) passes
// with the live value: of two cookies of one name, Domain and Path, the
// later replaces the earlier, and the capture is in the later one.
func TestRunTestSet_AClearThenSetLoginPassesWithTheLiveValue(t *testing.T) {
	const live = "S1live0001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Add("Set-Cookie", "sid=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT")
			w.Header().Add("Set-Cookie", "sid="+live+"; Path=/; HttpOnly")
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if ck, err := r.Cookie("sid"); err != nil || ck.Value != live {
				writeJSON(w, http.StatusUnauthorized, `{"error":"no session"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"sid": "S0rec0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"u":"a"}`,
			recorded(200, map[string]string{"Set-Cookie": "sid=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT,sid={{capture string .sid}}; Path=/; HttpOnly"}, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/me", map[string]string{"Cookie": "sid={{string .sid}}"}, "", recorded(200, nil, `{"user":"alice"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "the capture takes the cookie that stays")
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the next request carries the live session")
	if got := h.result(t, "test-1").Captured; len(got) != 1 || got[0].Value != live {
		t.Errorf("the login captured %+v; want %s", got, live)
	}
}

// A CREATE'S OWN LOCATION is a use of the id the body captures: asserted equal
// to it, under noise too (the noise entry is named once), and anchored on the
// text around it, so /users/15 does not hold the id 5.
func TestRunTestSet_ACreatesLocationIsAssertedAsTheCapturedID(t *testing.T) {
	for _, c := range []struct {
		name, location string // the Location the app answers; %s is the id
		noised         bool
		want           models.TestStatus
	}{
		{"the Location names the created id", "/users/%s", false, models.TestStatusPassed},
		{"the Location names another id", "/users/1%s", false, models.TestStatusFailed},
		{"the Location names another id, Location noised", "/users/1%s", true, models.TestStatusFailed},
		{"the Location names the created id, Location noised", "/users/%s", true, models.TestStatusPassed},
		{"the Location has more path after the id, Location noised", "/users/%s/extra", true, models.TestStatusFailed},
		{"the Location has more path before it, Location noised", "/v2/users/%s", true, models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			var n int
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					n++
					id := strconv.Itoa(4 + n)
					w.Header().Set("Location", fmt.Sprintf(c.location, id))
					writeJSON(w, http.StatusCreated, `{"id":"`+id+`"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "1"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, map[string]string{"Location": "/users/{{string .id}}"}, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				if c.noised {
					cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"Location": {}}}
				}
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			named := 0
			for _, e := range logs.FilterMessageSnippet("a noise entry covers a template variable's place").All() {
				if e.ContextMap()["noise"] == "header.Location (globalNoise)" && e.ContextMap()["variable"] == "{{string .id}}" {
					named++
				}
			}
			if want := map[bool]int{true: 1}[c.noised]; named != want {
				t.Errorf("the noise entry over the Location's use was named %d times; want %d", named, want)
			}
			if c.want == models.TestStatusFailed {
				if !strings.Contains(fmt.Sprint(r.TemplateFailures), "header Location") || !hasCategory(r, models.TemplateVariable) {
					t.Errorf("the failure does not name the Location and its category: %+v %+v", r.TemplateFailures, r.FailureInfo)
				}
				// HIGH: an auto-replay learns noise from a failure below it,
				// and a variable place is never noise.
				if r.FailureInfo.Risk != models.High {
					t.Errorf("a template failure is graded %q; want HIGH", r.FailureInfo.Risk)
				}
				warned := 0
				for _, e := range logs.FilterMessageSnippet("a template variable's place does not hold what it must").All() {
					m := e.ContextMap()
					next := fmt.Sprint(m["next_step"])
					if m["testcase"] == "test-1" && m["where"] == "header Location" && m["placeholder"] == "{{string .id}}" &&
						strings.Contains(fmt.Sprint(m["why"]), "id's value") &&
						strings.Contains(next, "this run's value of id is what test-1/response body.id captured from this same answer") &&
						strings.Contains(next, "re-record") && !strings.Contains(next, "normalize") {
						warned++
					}
				}
				if warned != 1 {
					t.Errorf("the Location's failure was logged %d time(s) with its place, why and next step; want once", warned)
				}
			}
		})
	}
}

func hasCategory(r models.TestResult, c models.FailureCategory) bool {
	for _, x := range r.FailureInfo.Category {
		if x == c {
			return true
		}
	}
	return false
}

// A HEADER CAPTURE THAT FINDS NOTHING FAILS, where the comparison does not
// look: X-Request-Id is a header auto header noise forgives, and noise can
// cover a header the answer lacks.
func TestRunTestSet_AHeaderCaptureThatFindsNothingFails(t *testing.T) {
	for _, c := range []struct {
		name, expected string
		answer         string // the X-Request-Id the app sends; "" = none
		noise          map[string]map[string][]string
	}{
		{"the header is missing, under noise", "{{capture string .rid}}", "", map[string]map[string][]string{"header": {"X-Request-Id": {}}}},
		{"the text around the capture is not there, auto header noise", "req-{{capture string .rid}}", "abc123", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					if c.answer != "" {
						w.Header().Set("X-Request-Id", c.answer)
					}
					writeJSON(w, http.StatusAccepted, `{"ok":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"rid": "R0rec0001"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/jobs", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(202, map[string]string{"X-Request-Id": c.expected}, `{"ok":true}`, nil)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.GlobalNoise.Global = c.noise })
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != models.TestStatusFailed || len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "header X-Request-Id" {
				t.Errorf("test-1: %s with %+v; a header capture that took nothing must fail its test case — %s", r.Status, r.TemplateFailures, why(r))
			}
		})
	}
}

// A USE IN A BODY THAT IS NOT JSON is asserted, also where the comparison
// does not read the body (compareAll off), anchored on the text around it.
func TestRunTestSet_AUseInABodyThatIsNotJSONIsAsserted(t *testing.T) {
	for _, c := range []struct {
		answer string
		want   models.TestStatus
	}{
		{"user=usr_REC0001 count=5", models.TestStatusPassed},
		{"user=usr_OTHER01 count=5", models.TestStatusFailed},
		{"user=Xusr_REC0001 count=5", models.TestStatusFailed},
	} {
		t.Run(c.answer, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, c.answer)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			resp := recorded(200, map[string]string{"Content-Type": "text/plain"}, "user={{string .id}} count=5", conf.stored.Template)
			delete(resp.Header, "Content-Length")
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/whoami", nil, "", resp)}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
		})
	}
}

// A COOKIE THAT HOLDS A USE MUST BE SET: a later response that re-issues the
// session (a rolling session) fails when it sets none, also with Set-Cookie
// noised.
func TestRunTestSet_ACookieUseTheAnswerDoesNotSetFails(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"sid": "S0rec0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/me", map[string]string{"Cookie": "sid={{string .sid}}"}, "",
			recorded(200, map[string]string{"Set-Cookie": "sid={{string .sid}}; Path=/; HttpOnly"}, `{"user":"alice"}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
		cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"Set-Cookie": {}}}
	})
	h.run(t)
	r := h.result(t, "test-1")
	if r.Status != models.TestStatusFailed || len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "header Set-Cookie.sid" {
		t.Errorf("test-1: %s with %+v; the session use whose cookie the answer does not set must fail", r.Status, r.TemplateFailures)
	}
}

// A USE IN A PART OF A COOKIE NO RUN READS IS REFUSED, NEVER WARNED AS
// ASSERTED. Under a noise entry over Set-Cookie, a use in a cookie's flag
// (HttpOnly), an attribute's name or the cookie's name passed any answer while
// keploy test said "the variable is asserted there anyway": a run reads only a
// cookie's value and its attributes' values. So did a use whose recorded value
// starts another cookie after a comma, which failed even the recorded answer
// without the noise. Each is refused when the set loads, unsent, with its
// edit, and no noise warning names it.
func TestRunTestSet_AUseInACookiesNameOrFlagIsRefused(t *testing.T) {
	for _, c := range []struct {
		name, expected, answer, where, placeholder, why string
	}{
		{"a flag (F3)", "sid=abc; {{string .flag}}", "sid=abc; Secure", "response header Set-Cookie.sid", "{{string .flag}}", "the cookie's name, an attribute's name and an attribute with no value"},
		{"a flag the answer drops (F4)", "sid=abc; {{string .flag}}", "sid=abc", "response header Set-Cookie.sid", "{{string .flag}}", "the cookie's name, an attribute's name and an attribute with no value"},
		{"an attribute's name (G4)", "sid=abc; {{string .an}}=Lax", "sid=abc; Other=Lax", "response header Set-Cookie.sid", "{{string .an}}", "the cookie's name, an attribute's name and an attribute with no value"},
		{"the cookie's name (H3)", "{{string .n}}=abc; Path=/", "other=abc; Path=/", "response header Set-Cookie.{{string .n}}", "{{string .n}}", "the cookie's name, an attribute's name and an attribute with no value"},
		{"a value that starts a cookie (SC15)", "a=1; Path=/, {{string .c}}", "a=1; Path=/, sid=abc", "response header Set-Cookie.a", "{{string .c}}", "a ',' that starts a name="},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Set-Cookie", c.answer)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"flag": "HttpOnly", "an": "SameSite", "n": "sid", "c": "sid=abc"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "GET", "/a", nil, "", recorded(200, map[string]string{"Set-Cookie": c.expected}, `{"ok":true}`, nil)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"Set-Cookie": {}}}
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			if got := app.sent("GET", "/a"); len(got) != 0 {
				t.Errorf("the refused test case was sent %d time(s)", len(got))
			}
			assertRefusedUnsent(t, h, logs, "test-1", c.where, c.placeholder, c.why)
			if said := logs.FilterMessageSnippet("a noise entry covers a template variable's place").All(); len(said) != 0 {
				t.Errorf("a refused use is warned as asserted under noise: %+v", said)
			}
		})
	}
}

// A URL THAT IS NOT RENDERED goes out as it always has been (a set without
// templates replays as before): a "%7B" in it is unescaped whole.
func TestRunTestSet_AURLThatIsNotRenderedIsUnescapedAsBefore(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	for _, tmpl := range []map[string]interface{}{nil, {"id": "usr_1"}} {
		conf := &tmplConf{stored: models.TestSet{Template: tmpl}}
		cases := []*models.TestCase{tmplCase(1, app.addr, "GET", `/search?f=%7B%22a%22%3A1%7D`, nil, "", recorded(200, nil, `{"ok":true}`, nil))}
		h := newTmplHarness(t, conf, cases, nil)
		h.run(t)
		assertStatus(t, h, "test-1", models.TestStatusPassed, "the search answers")
	}
	got := app.sent("GET", "/search")
	if len(got) != 2 || got[0].rawQuery != `f={"a":1}` || got[1].rawQuery != `f={"a":1}` {
		var qs []string
		for _, s := range got {
			qs = append(qs, s.rawQuery)
		}
		t.Errorf("the unrendered URL reached the app with query %q; want it unescaped whole, as before: f={\"a\":1}", qs)
	}
}

// A PLACEHOLDER IN A JSON OBJECT KEY is sent at the run's value.
func TestRunTestSet_AnObjectKeyPlaceholderIsSentAtTheRunsValue(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"usr_LIVE001"}`)
		})
		mux.HandleFunc("/flags", func(w http.ResponseWriter, r *http.Request) {
			s := a.record(r)
			writeJSON(w, http.StatusOK, string(s.body))
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "POST", "/flags", map[string]string{"Content-Type": "application/json"}, `{"{{string .id}}":true}`,
			recorded(200, nil, `{"{{string .id}}":true}`, conf.stored.Template)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	if got := app.sent("POST", "/flags"); len(got) != 1 || string(got[0].body) != `{"usr_LIVE001":true}` {
		t.Errorf("the object key was sent %+v; want the captured id", got)
	}
	assertStatus(t, h, "test-2", models.TestStatusPassed, "the answer names the key at the run's value")
}

// TOKEN REFRESH ACROSS RETRYPASSING CYCLES. The login fails on a field of its
// own (a date) but its capture took the token; the refresh sends it and
// captures a new one; the call sends that. A later cycle re-runs only the
// refresh and the call: at the refresh's place a whole pass gives the key the
// login's token, never the refresh's own last capture, which the app would
// turn away.
func TestRunTestSet_RetryPassingRefreshesFromTheTokenBeforeIt(t *testing.T) {
	var logins, refreshes int
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			logins++
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"token":"L-%d","at":"2026-10-08"}`, logins))
		})
		mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer L-") {
				writeJSON(w, http.StatusUnauthorized, `{"error":"a refresh takes a login token"}`)
				return
			}
			refreshes++
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"token":"R-%d"}`, refreshes))
		})
		mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer R-") {
				writeJSON(w, http.StatusUnauthorized, `{"error":"stale"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"user":"alice"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"token": "L-0"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{}`,
			recorded(200, nil, `{"token":"{{capture string .token}}","at":"2026-10-05"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "POST", "/refresh", map[string]string{"Authorization": "Bearer {{string .token}}"}, "",
			recorded(200, nil, `{"token":"{{capture string .token}}"}`, conf.stored.Template)),
		tmplCase(3, app.addr, "GET", "/me", map[string]string{"Authorization": "Bearer {{string .token}}"}, "",
			recorded(200, nil, `{"user":"alice"}`, nil)),
	}
	for _, tc := range cases {
		delete(tc.HTTPResp.Header, "Content-Length")
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
	h.run(t)
	if n := len(app.sent("POST", "/refresh")); n < 2 {
		t.Fatalf("precondition: RetryPassing re-ran the refresh %d time(s); want at least 2", n)
	}
	if n := len(app.sent("POST", "/login")); n != 1 {
		t.Fatalf("precondition: the failing login ran %d times; want once", n)
	}
	for i, s := range app.sent("POST", "/refresh") {
		if got := s.header.Get("Authorization"); got != "Bearer L-1" {
			t.Errorf("pass %d sent the refresh %q; want the login's token, Bearer L-1", i+1, got)
		}
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	for _, r := range h.report.results {
		if r.TestCaseID != "test-1" && r.Status != models.TestStatusPassed {
			t.Errorf("%s: %s in some pass — %s", r.TestCaseID, r.Status, why(r))
		}
	}
}

// A CAPTURE THAT MISSED LEAVES NO STALE VALUE. The create captures an id in
// the first cycle and misses it in the second (it fails there and is skipped
// in the third); in the third cycle the read is sent what a whole pass gives
// it at its place: the recorded id, since the create's last run captured
// nothing, not the first cycle's id.
func TestRunTestSet_RetryPassingForgetsACaptureThatLaterMissed(t *testing.T) {
	var creates int
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusInternalServerError, `{"ok":false}`)
		})
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			creates++
			if creates == 1 {
				writeJSON(w, http.StatusCreated, `{"id":"usr_LIVE001"}`)
				return
			}
			writeJSON(w, http.StatusCreated, `{"name":"alice"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/flaky", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		tmplCase(3, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	for _, tc := range cases {
		delete(tc.HTTPResp.Header, "Content-Length")
	}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.RetryPassing = true })
	h.run(t)
	var paths []string
	for _, s := range app.sent("GET", "/users/") {
		paths = append(paths, s.path)
	}
	want := []string{"/users/usr_LIVE001", "/users/usr_REC0001", "/users/usr_REC0001"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("the read was sent %q over the cycles; want %q", paths, want)
	}
}

// RULE 6'S OTHER WARNING: a request that sends a key no earlier test case
// captures is named once, with what to do.
func TestRunTestSet_AKeyNoEarlierCaseCapturesIsNamed(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}/x", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	warned := logs.FilterMessageSnippet("a request sends a template key that no earlier test case captures").All()
	if len(warned) != 1 || warned[0].ContextMap()["testcase"] != "test-1" || warned[0].ContextMap()["placeholder"] != "{{string .id}}" ||
		!strings.Contains(fmt.Sprint(warned[0].ContextMap()["next_step"]), "{{capture string .id}}") {
		t.Errorf("the uncaptured key was named %d times: %+v; want once, at test-1, with what to write", len(warned), warned)
	}
}

// A STREAMED USE IS ASSERTED UNDER NOISE, like any other variable place: each
// recorded frame's use against the frame received at its position, and a
// header's against the answer's header, whatever the stream comparison's
// noise lets through; the noise entry over it is named when the set loads.
func TestRunTestSet_AStreamedUseIsAssertedUnderNoise(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name                string
		frameID, headerID   string
		noise               map[string]map[string][]string
		tcNoise             map[string][]string
		want                models.TestStatus
		failsAt, namedNoise string
	}{
		{name: "the frames name another id, the frame field noised", frameID: "usr_OTHER01", headerID: live,
			noise: map[string]map[string][]string{"body": {"user": {}}}, want: models.TestStatusFailed, failsAt: "body frame 0.user", namedNoise: "body.user (globalNoise)"},
		{name: "the frames name another id, the test case's own noise over the frame field", frameID: "usr_OTHER01", headerID: live,
			tcNoise: map[string][]string{"body.user": {}}, want: models.TestStatusFailed, failsAt: "body frame 0.user", namedNoise: "body.user (this test case)"},
		{name: "the header names another id, the header noised", frameID: live, headerID: "usr_OTHER01",
			noise: map[string]map[string][]string{"header": {"X-User": {}}}, want: models.TestStatusFailed, failsAt: "header X-User", namedNoise: "header.X-User (globalNoise)"},
		{name: "every use holds its value under the noise", frameID: live, headerID: live,
			noise: map[string]map[string][]string{"body": {"user": {}}, "header": {"X-User": {}}}, want: models.TestStatusPassed, namedNoise: "body.user (globalNoise)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.Header().Set("X-User", c.headerID)
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"user":"`+c.frameID+`","n":1}`+"\n"+`{"user":"`+c.frameID+`","n":2}`+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
			frame := func(n int) models.HTTPStreamChunk {
				return models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: fmt.Sprintf(`{"user":"{{string .id}}","n":%d}`, n)}}}
			}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "X-User": "{{string .id}}", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`,
				StreamBody: []models.HTTPStreamChunk{frame(1), frame(2)},
			})}
			cases[0].Noise = c.tcNoise
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.GlobalNoise.Global = c.noise })
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Fatalf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if c.want == models.TestStatusFailed {
				if len(r.TemplateFailures) == 0 || r.TemplateFailures[0].Where != c.failsAt || !hasCategory(r, models.TemplateVariable) || r.FailureInfo.Risk != models.High {
					t.Errorf("the failure does not name %s with its category and risk: %+v %+v", c.failsAt, r.TemplateFailures, r.FailureInfo)
				}
			}
			named := false
			for _, e := range logs.FilterMessageSnippet("a noise entry covers a template variable's place").All() {
				if e.ContextMap()["noise"] == c.namedNoise && e.ContextMap()["testcase"] == "test-1" {
					named = true
				}
			}
			if !named {
				t.Errorf("the noise entry %s over a streamed use was not named", c.namedNoise)
			}
		})
	}
}

// A URL-VALUED HEADER IS READ DECODED FOR ITS PLACE, in both directions. A
// create answering Location: /posts/th%C3%A9-43 gives the read-back the path
// segment thé-43, escaped once when it is sent; a create whose body issues
// urn:x:2 answers Location: /things/urn%3Ax%3A2, which holds that id, and
// one naming another id fails.
func TestRunTestSet_AURLValuedHeaderIsReadDecodedForItsPlace(t *testing.T) {
	t.Run("a capture in a Location reaches the next request", func(t *testing.T) {
		var n int
		app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
			mux.HandleFunc("/posts", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				n++
				w.Header().Set("Location", fmt.Sprintf("/posts/th%%C3%%A9-%d", 42+n))
				w.WriteHeader(http.StatusCreated)
			})
			mux.HandleFunc("/posts/", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				writeJSON(w, http.StatusOK, `{"ok":true}`)
			})
		})
		conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"posts": "thé-42"}}}
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "POST", "/posts", nil, "", recorded(201, map[string]string{"Location": "/posts/{{capture string .posts}}", "Content-Length": "0"}, "", nil)),
			tmplCase(2, app.addr, "GET", "/posts/{{string .posts}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
		}
		h := newTmplHarness(t, conf, cases, nil)
		h.run(t)
		assertStatus(t, h, "test-1", models.TestStatusPassed, "the Location's capture holds the decoded id")
		assertStatus(t, h, "test-2", models.TestStatusPassed, "the read-back answers")
		if got := app.sent("GET", "/posts/"); len(got) != 1 || got[0].path != "/posts/thé-43" {
			t.Errorf("the read-back was sent %+v; want the path segment thé-43, escaped once", got)
		}
		if r := h.result(t, "test-1"); len(r.Captured) != 1 || r.Captured[0].Value != "thé-43" {
			t.Errorf("the capture took %+v; want thé-43", r.Captured)
		}
	})
	for _, c := range []struct {
		name   string
		offset int // the id the Location names, after the body's
		want   models.TestStatus
	}{
		{"a use in the create's own Location holds the escaped id", 0, models.TestStatusPassed},
		{"a Location naming another id fails", 1, models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Location", fmt.Sprintf("/things/urn%%3Ax%%3A%d", 2+c.offset))
					writeJSON(w, http.StatusCreated, `{"id":"urn:x:2"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "urn:x:1"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/things", nil, "", recorded(201, map[string]string{"Location": "/things/{{string .id}}"}, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
		})
	}
}

// tzTestDB is the test-case store keploy templatize writes to, in memory:
// its files are the cases as testdb stores them, and putting a run's files in
// place puts conf's staged config.yaml in place with them.
type tzTestDB struct {
	cases []*models.TestCase
	conf  *tmplConf
}

func (d *tzTestDB) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0"}, nil
}
func (d *tzTestDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return d.cases, nil
}
func (d *tzTestDB) UpdateTestCase(context.Context, *models.TestCase, string, bool) error { return nil }
func (d *tzTestDB) DeleteTests(context.Context, string, []string) error                  { return nil }
func (d *tzTestDB) DeleteTestSet(context.Context, string) error                          { return nil }
func (d *tzTestDB) TestCaseFiles(ctx context.Context, id string) ([]setfile.File, error) {
	db := testdb.New(zap.NewNop(), "keploy")
	var out []setfile.File
	for _, tc := range d.cases {
		// Encoding a test case adds the noise it finds to its map: a copy's.
		cp := *tc
		cp.Noise = map[string][]string{}
		for k, v := range tc.Noise {
			cp.Noise[k] = v
		}
		f, err := db.TestCaseFile(ctx, &cp, id)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
func (d *tzTestDB) TestCaseFile(_ context.Context, tc *models.TestCase, _ string) (setfile.File, error) {
	return setfile.File{Path: tc.Name}, nil
}
func (d *tzTestDB) ReplaceFiles(context.Context, []setfile.File) error {
	d.conf.put()
	return nil
}

// A NESTED CREATE, END TO END: recorded, templatized by keploy templatize,
// then replayed. The create of a user under an org answers only a Location;
// templatize writes the org there as a use and the new user's id as a capture
// beside it, so the read-back is sent the user this run created, and a
// Location naming another org fails the create.
func TestRunTestSet_ANestedCreateTemplatizedEndToEnd(t *testing.T) {
	for _, c := range []struct {
		name      string
		answerOrg func(org string) string // the org the nested create's Location names
		want      models.TestStatus
	}{
		{"the Location names the org it was sent", func(org string) string { return org }, models.TestStatusPassed},
		{"the Location names another org", func(string) string { return "org_OTH01" }, models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The app issues fresh ids: this run's org and user, never the
			// recorded ones.
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				org, user := "org_LIV01", "usr_LIV02"
				mux.HandleFunc("/orgs", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+org+`"}`)
				})
				mux.HandleFunc("/orgs/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/")
					switch {
					case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == org:
						w.Header().Set("Location", "/orgs/"+c.answerOrg(org)+"/users/"+user)
						w.WriteHeader(http.StatusCreated)
					case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == org && parts[2] == user:
						writeJSON(w, http.StatusOK, `{"user":"`+user+`"}`)
					default:
						writeJSON(w, http.StatusNotFound, `{"error":"no such org or user"}`)
					}
				})
			})
			jsonType := map[string]string{"Content-Type": "application/json"}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/orgs", jsonType, `{"name":"acme"}`, recorded(201, nil, `{"id":"org_REC01"}`, nil)),
				tmplCase(2, app.addr, "POST", "/orgs/org_REC01/users", jsonType, `{"name":"alice"}`,
					recorded(201, map[string]string{"Location": "/orgs/org_REC01/users/usr_REC02", "Content-Length": "0"}, "", nil)),
				tmplCase(3, app.addr, "GET", "/orgs/org_REC01/users/usr_REC02", nil, "", recorded(200, nil, `{"user":"usr_REC02"}`, nil)),
			}
			conf := &tmplConf{}
			tz, ok := tools.NewTools(zap.NewNop(), conf, &tzTestDB{cases: cases, conf: conf}, nil, nil, &config.Config{}).(*tools.Tools)
			if !ok {
				t.Fatal("tools.NewTools is not a *tools.Tools")
			}
			if err := tz.ProcessTestCasesV2(context.Background(), cases, "test-set-0"); err != nil {
				t.Fatalf("templatize: %v", err)
			}
			if got := cases[1].HTTPResp.Header["Location"]; got != "/orgs/{{string .id}}/users/{{capture string .users}}" {
				t.Fatalf("templatize wrote the nested create's Location %q", got)
			}
			// The body of the read-back holds the user id, which its request
			// sent: a use.
			if got := cases[2].HTTPResp.Body; got != `{"user":"{{string .users}}"}` {
				t.Fatalf("templatize wrote the read-back's body %q", got)
			}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			assertStatus(t, h, "test-2", c.want, "the nested create's Location against the org it was sent")
			if c.want == models.TestStatusPassed {
				assertStatus(t, h, "test-3", models.TestStatusPassed, "the read-back is sent this run's user")
				if got := app.sent("GET", "/orgs/"); len(got) != 1 || got[0].path != "/orgs/org_LIV01/users/usr_LIV02" {
					t.Errorf("the read-back was sent %+v; want this run's org and user", got)
				}
			}
		})
	}
}

// A SET TEMPLATIZED BY AN OLDER KEPLOY wrote {{string .id}} where the value is
// issued. Nothing captures it, so the warning for the request that sends it
// names that place and the exact edit.
func TestRunTestSet_AnOlderSetsProducerIsNamedWithItsEdit(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_1"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", nil, `{}`, recorded(200, nil, `{"id":"{{string .id}}"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"ok":true}`, nil)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	warned := logs.FilterMessageSnippet("a request sends a template key that no earlier test case captures").All()
	next := ""
	if len(warned) == 1 {
		next = fmt.Sprint(warned[0].ContextMap()["next_step"])
	}
	if !strings.Contains(next, "if the app issues this value anew each run: change {{string .id}} at test-1/response body.id to {{capture string .id}}") ||
		!strings.Contains(next, `if it is a constant or what the request sent: write its recorded value "usr_1" in place of every placeholder of id`) {
		t.Errorf("the uncaptured key was named %d times: %+v; want once, with both edits at test-1's body.id", len(warned), warned)
	}
}

// A STREAMED BODY A RECORDING KEPT FLAT (no frames stored) is read frame by
// frame (S2): each use is asserted against the frame the stream delivered at
// its position, and the stream comparison, given those frames, compares each
// one, where before it was given none and matched any stream.
func TestRunTestSet_AFlatStreamedBodyIsAssertedFrameByFrame(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name      string
		frames    []string // what the app streams
		want      models.TestStatus
		failsAt   string
		noFailure bool
	}{
		{name: "the frames hold the id", frames: []string{`{"user":"` + live + `","n":1}`, `{"user":"` + live + `","n":2}`}, want: models.TestStatusPassed},
		{name: "a frame holds another id", frames: []string{`{"user":"` + live + `","n":1}`, `{"user":"usr_OTHER01","n":2}`}, want: models.TestStatusFailed, failsAt: "body frame 1.user"},
		{name: "a frame differs elsewhere", frames: []string{`{"user":"` + live + `","n":1}`, `{"user":"` + live + `","n":3}`}, want: models.TestStatusFailed, noFailure: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, strings.Join(c.frames, "\n")+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:   `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`,
			})}
			h := newTmplHarness(t, conf, cases, nil)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Fatalf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			switch {
			case c.failsAt != "":
				if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != c.failsAt || !hasCategory(r, models.TemplateVariable) {
					t.Errorf("the failure does not name %s: %+v %+v", c.failsAt, r.TemplateFailures, r.FailureInfo)
				}
			case c.noFailure && len(r.TemplateFailures) != 0:
				t.Errorf("every use held its value, yet: %+v", r.TemplateFailures)
			}
		})
	}
}

// multipartBody is a multipart/mixed stream of one JSON part.
func multipartBody(json string) string {
	return "--XYZ\r\nContent-Type: application/json\r\n\r\n" + json + "\r\n--XYZ--\r\n"
}

// A VARIABLE IN A MULTIPART STREAMED BODY IS REFUSED (S1): a recording keeps
// such a body whole, the stream comparison reads it under its own rules, and
// no frame holds the variable, so it could not be asserted at its place, and
// noise over it would pass any value. The test case fails unsent, with the
// reason; one whose body holds no variable runs as before.
func TestRunTestSet_AVariableInAMultipartStreamIsRefused(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, expected string
		refused        bool
	}{
		{"a use in the JSON part, under noise", `{"user":"{{string .id}}"}`, true},
		{"the part written out", `{"user":"` + live + `"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/parts", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "multipart/mixed; boundary=XYZ")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, multipartBody(`{"user":"`+live+`"}`))
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/parts", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"Content-Type": "multipart/mixed; boundary=XYZ", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:   multipartBody(c.expected),
			})}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"user": {}}}
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			if !c.refused {
				if n := len(app.sent("GET", "/parts")); n != 1 {
					t.Errorf("a multipart case without a variable was sent %d time(s)", n)
				}
				assertStatus(t, h, "test-1", models.TestStatusPassed, "a multipart stream without a variable compares as before")
				return
			}
			if n := len(app.sent("GET", "/parts")); n != 0 {
				t.Errorf("the refused case was sent %d time(s)", n)
			}
			assertRefusedUnsent(t, h, logs, "test-1", "response body", "{{string .id}}", "kept whole")
		})
	}
}

// wholeHooks answers every HTTP test case whole, a streamed one included: a
// hook that does not stream (it reads the answer to its end, as a proxy or an
// enterprise hook may) hands the replay a models.HTTPResp.
type wholeHooks struct{ TestHooks }

func (h wholeHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.Kind != models.HTTP {
		return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
	}
	return pkg.SimulateHTTP(ctx, tc, testSetID, zap.NewNop(), pkg.SimulationConfig{APITimeout: 5})
}

// A STREAMED CASE ANSWERED WHOLE is asserted frame by frame all the same
// (ST1): its body is what the app delivered, split as a recording splits a
// stream, so a frame holding another id fails, and so does a frame the answer
// lacks, though no stream comparison runs and the matcher does not read a
// body that is not JSON.
func TestRunTestSet_AStreamedCaseAnsweredWholeIsAssertedFrameByFrame(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, body, failsAt, why string
		want                     models.TestStatus
	}{
		{name: "every frame holds the id", body: `{"user":"` + live + `","n":1}` + "\n" + `{"user":"` + live + `","n":2}` + "\n", want: models.TestStatusPassed},
		{name: "a frame holds another id", body: `{"user":"usr_OTHER01","n":1}` + "\n" + `{"user":"` + live + `","n":2}` + "\n", want: models.TestStatusFailed, failsAt: "body frame 0.user"},
		{name: "a frame is missing", body: `{"user":"` + live + `","n":1}` + "\n", want: models.TestStatusFailed, failsAt: "body frame 1", why: "needs frame 1 of the answer; the answer delivered 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, c.body)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
			frame := func(n int) models.HTTPStreamChunk {
				return models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: fmt.Sprintf(`{"user":"{{string .id}}","n":%d}`, n)}}}
			}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`,
				StreamBody: []models.HTTPStreamChunk{frame(1), frame(2)},
			})}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				// The whole answer carries the Content-Length a stream does not.
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"user": {}}, "header": {"Content-Length": {}}}
			})
			h.r.hookImpl = wholeHooks{TestHooks: h.r.hookImpl}
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Fatalf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if c.failsAt != "" && (len(r.TemplateFailures) == 0 || r.TemplateFailures[0].Where != c.failsAt || !strings.Contains(r.TemplateFailures[0].Why, c.why)) {
				t.Errorf("the failure does not name %s (%q): %+v", c.failsAt, c.why, r.TemplateFailures)
			}
		})
	}
}

// EVERY TEMPLATE FAILURE IS SAVED, and each is logged with the next step for
// both things it can mean (RF1): a read-back answering another id in its body
// and in a header saves both places; the next step says which test case's
// capture gave the run its value, and to edit the expected value or
// re-record when the value changed on purpose, never to run keploy normalize
// (which skips a HIGH-risk failure).
func TestRunTestSet_EveryTemplateFailureIsSavedWithItsNextStep(t *testing.T) {
	const live = "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("X-User", "usr_OTHER02")
			writeJSON(w, http.StatusOK, `{"id":"usr_OTHER01"}`)
		})
		mux.HandleFunc("/newest", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"newest":"usr_OTHER03"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	read := tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
		recorded(200, map[string]string{"X-User": "{{string .id}}"}, `{"id":"{{string .id}}"}`, conf.stored.Template))
	delete(read.HTTPResp.Header, "Content-Length")
	// A response that holds the id its request never sent: not a read-back.
	newest := tmplCase(3, app.addr, "GET", "/newest", nil, "", recorded(200, nil, `{"newest":"{{string .id}}"}`, conf.stored.Template))
	delete(newest.HTTPResp.Header, "Content-Length")
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		read,
		newest,
	}
	h := newTmplHarness(t, conf, cases, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	r := h.result(t, "test-2")
	var where []string
	for _, f := range r.TemplateFailures {
		where = append(where, f.Where)
	}
	if strings.Join(where, ",") != "body.id,header X-User" {
		t.Errorf("test-2 saved template failures at %v; want body.id and header X-User", where)
	}
	logged := logs.FilterMessageSnippet("a template variable's place does not hold what it must").All()
	if len(logged) != 3 {
		t.Fatalf("logged %d template failures; want 3", len(logged))
	}
	for _, e := range logged {
		next := fmt.Sprint(e.ContextMap()["next_step"])
		tc := fmt.Sprint(e.ContextMap()["testcase"])
		for _, want := range []string{"this run's value of id is what test-1/response body.id captured", "if the value changed on purpose, edit the expected value at " + tc + "/response " + fmt.Sprint(e.ContextMap()["where"]), "re-record"} {
			if !strings.Contains(next, want) {
				t.Errorf("the next step %q does not say %q", next, want)
			}
		}
		if strings.Contains(next, "normalize") {
			t.Errorf("the next step points at keploy normalize, which skips a HIGH-risk failure: %q", next)
		}
		switch tc {
		case "test-2":
			// PL2: test-2's own request sent the id (a read-back). A capture
			// there would accept any id and send it on: never offered.
			if strings.Contains(next, "{{capture") || !strings.Contains(next, "the app answered a different value than the one captured at test-1/response body.id") {
				t.Errorf("the read-back's next step %q offers a capture, or does not say the app answered another value", next)
			}
		case "test-3":
			// Its request did not send the id: the place may issue a value
			// of its own, and the capture edit is offered (P7), under a key
			// of its own, and the same key's capture only for a place that
			// replaces the value from then on.
			if !strings.Contains(next, `change {{string .id}} at test-3/response body.newest to {{capture string .id2}}, give id2 its recorded value "usr_REC0001"`) ||
				!strings.Contains(next, "change {{string .id}} there to {{capture string .id}} instead, which points every later use of id at this answer") {
				t.Errorf("test-3's next step %q does not offer both capture edits", next)
			}
		}
	}
}

// A SET WRITTEN BY keploy sanitize holds {{string .secret.K}} where a secret
// the recording held was redacted: not a variable, the literal it stands for
// (K1, K2). In an expected response it is compared as that literal, and the
// user's noise over a token the app issues anew each run applies to it as it
// did before: the case passes. Without the noise the literal is compared,
// and the case fails on the comparison, with no template failure.
func TestRunTestSet_ASanitizedSecretIsComparedAsTheLiteralItRedacts(t *testing.T) {
	for _, c := range []struct {
		name    string
		header  bool // the secret is in a response header, not the body
		tcNoise map[string][]string
		global  map[string]map[string][]string
		want    models.TestStatus
	}{
		{name: "K1 a body secret under the test case's noise", tcNoise: map[string][]string{"body.access_token": {}}, want: models.TestStatusPassed},
		{name: "K2 a header secret under global header noise", header: true, global: map[string]map[string][]string{"header": {"X-Auth-Token": {}}}, want: models.TestStatusPassed},
		{name: "a body secret without noise", want: models.TestStatusFailed},
		{name: "a header secret without noise", header: true, want: models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			n := 0
			var sentAuth []string
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					mu.Lock()
					n++
					fresh := fmt.Sprintf("eyJhbGciOiJIUzI1NiJ9.fresh%d.sig", n)
					sentAuth = append(sentAuth, r.Header.Get("Authorization"))
					mu.Unlock()
					w.Header().Set("X-Auth-Token", fresh)
					writeJSON(w, http.StatusOK, `{"access_token":"`+fresh+`","user":"alice"}`)
				})
			})
			body, header := `{"access_token":"{{string .secret.Jwt }}","user":"alice"}`, map[string]string{"X-Auth-Token": "eyJhbGciOiJIUzI1NiJ9.fresh1.sig"}
			if c.header {
				body, header = `{"access_token":"eyJhbGciOiJIUzI1NiJ9.fresh1.sig","user":"alice"}`, map[string]string{"X-Auth-Token": "{{string .secret.Jwt }}"}
			}
			login := tmplCase(1, app.addr, "POST", "/login", map[string]string{"Authorization": "Bearer {{string .secret.Api }}"}, "", recorded(200, header, body, nil))
			delete(login.HTTPResp.Header, "Content-Length")
			login.Noise = c.tcNoise
			conf := &tmplConf{stored: models.TestSet{Secret: map[string]interface{}{"Jwt": "eyJhbGciOiJIUzI1NiJ9.recorded.sig", "Api": "sk_live_REC"}}}
			h := newTmplHarness(t, conf, []*models.TestCase{login}, func(cfg *config.Config) { cfg.Test.GlobalNoise.Global = c.global })
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if len(r.TemplateFailures) != 0 || hasCategory(r, models.TemplateVariable) {
				t.Errorf("a secret is no variable, yet: %+v %v", r.TemplateFailures, r.FailureInfo.Category)
			}
			if len(sentAuth) != 1 || sentAuth[0] != "Bearer sk_live_REC" {
				t.Errorf("the request was sent %v; want the secret's value", sentAuth)
			}
			if n := len(logs.FilterMessageSnippet("a noise entry covers a template variable's place").All()); n != 0 {
				t.Errorf("noise over a secret was named %d time(s) as covering a variable", n)
			}
		})
	}
}

// A SET keploy sanitize WROTE, WITH A SECRET IN A REQUEST URL AND IN A FORM
// BODY, is sent exactly as recorded, as main sends it. sanitize replaces the
// whole scalar that holds a secret and keeps it as recorded in secret.yaml:
// the URL with its query escaped, the form body. The test cases here are
// written by testdb, redacted by the real SanitizeFileInPlace and loaded
// back by testdb, as keploy sanitize and keploy test do. Escaping that text
// again sent http:%2F%2F… (no request reached the app) and token%3D…%26… (the
// app turned it away): a regression for sets with no template.
func TestRunTestSet_ASetSanitizeWroteIsSentAsRecorded(t *testing.T) {
	const (
		apiKey = "sk_" + "live_51H8xYzAbCdEfGhIjKlMnOpQrStUvWx" // split so the source holds no literal key
		slack  = "xo" + "xb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"
		query  = "api_key=" + apiKey + "&note=a%2Bb+c"
		form   = "token=" + slack + "&user=alice&pw=p%40ss+w0rd"
	)
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/v1/charges", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			if r.URL.RawQuery != query || r.URL.Query().Get("note") != "a+b c" {
				writeJSON(w, http.StatusUnauthorized, `{"ok":false}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			s := a.record(r)
			if string(s.body) != form {
				writeJSON(w, http.StatusUnauthorized, `{"ok":false}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	ok := recorded(200, nil, `{"ok":true}`, nil)
	dir := t.TempDir()
	db := testdb.New(zap.NewNop(), dir)
	ctx := context.Background()
	for _, tc := range []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/v1/charges?"+query, nil, "", ok),
		tmplCase(2, app.addr, "POST", "/login", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form, ok),
	} {
		tc.Noise = map[string][]string{} // as record mode hands a test case to testdb
		if err := db.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
			t.Fatal(err)
		}
	}
	secrets := map[string]string{}
	for _, name := range []string{"test-1", "test-2"} {
		if err := tools.SanitizeFileInPlace(filepath.Join(dir, "test-set-0", "tests", name+".yaml"), name, secrets); err != nil {
			t.Fatal(err)
		}
	}
	cases, err := db.GetTestCases(ctx, "test-set-0")
	if err != nil || len(cases) != 2 {
		t.Fatalf("loading the sanitized set: %v, %d test cases", err, len(cases))
	}
	byName := map[string]*models.TestCase{}
	for _, tc := range cases {
		byName[tc.Name] = tc
	}
	if !strings.HasPrefix(byName["test-1"].HTTPReq.URL, "{{string .secret.") || !strings.HasPrefix(byName["test-2"].HTTPReq.Body, "{{string .secret.") {
		t.Fatalf("sanitize did not redact the URL and the form body: %q %q", byName["test-1"].HTTPReq.URL, byName["test-2"].HTTPReq.Body)
	}
	sec := map[string]interface{}{}
	for k, v := range secrets {
		sec[k] = v
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Secret: sec}}, cases, nil)
	if st := h.run(t); st != models.TestSetStatusPassed {
		t.Errorf("the sanitized set: %s, want PASSED", st)
	}
	for _, name := range []string{"test-1", "test-2"} {
		assertStatus(t, h, name, models.TestStatusPassed, "a secret is sent as the recording held it")
	}
	if got := app.sent("GET", "/v1/charges"); len(got) != 1 || got[0].rawQuery != query {
		t.Errorf("the app got the query %v; want %q", got, query)
	}
	if got := app.sent("POST", "/login"); len(got) != 1 || string(got[0].body) != form {
		t.Errorf("the app got the form body %v; want %q", got, form)
	}
}

// A STREAM READ ONLY UP TO A MISMATCH asserts no frame it never read: the
// stream comparison stops at the first frame that does not match (which fails
// the test case on its own) and closes the stream, so a use in a later frame
// is not called missing. The stand-in app sends its second frame only after
// the replay has hung up, so nothing after the mismatch is ever read.
func TestRunTestSet_AStreamCutAtAMismatchAssertsNoFrameItNeverRead(t *testing.T) {
	const live = "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"user":"`+live+`","n":9}`+"\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
	frame := func(n int) models.HTTPStreamChunk {
		return models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: fmt.Sprintf(`{"user":"{{string .id}}","n":%d}`, n)}}}
	}
	cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
		StatusCode: 200, StatusMessage: "OK",
		Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
		Body:       `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`,
		StreamBody: []models.HTTPStreamChunk{frame(1), frame(2)},
	})}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	r := h.result(t, "test-1")
	if r.Status != models.TestStatusFailed {
		t.Fatalf("test-1: %s; frame 0 does not match", r.Status)
	}
	if len(r.TemplateFailures) != 0 || hasCategory(r, models.TemplateVariable) {
		t.Errorf("a frame the replay never read was asserted: %+v %v", r.TemplateFailures, r.FailureInfo.Category)
	}
}

// failingHooks fails one HTTP test case without an answer, as a request the
// app never answers does (a refused connection, a timeout).
type failingHooks struct {
	TestHooks
	fail string
}

func (h failingHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.Name == h.fail {
		return nil, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	}
	return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
}

// A TEST CASE THAT GOT NO ANSWER is reported with the run's values: its
// request as sent and its expected response as the run expected it, the id
// the create captured, never the placeholder or the recorded value.
func TestRunTestSet_ATestCaseWithNoAnswerReportsTheRunsValues(t *testing.T) {
	const live = "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"id":"{{string .id}}"}`, conf.stored.Template)),
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.r.hookImpl = failingHooks{TestHooks: h.r.hookImpl, fail: "test-2"}
	h.run(t)
	r := h.result(t, "test-2")
	if r.Status != models.TestStatusFailed {
		t.Fatalf("test-2: %s; a test case with no answer fails", r.Status)
	}
	if !strings.Contains(r.Req.URL, "/users/"+live) {
		t.Errorf("the report's request is %q; want the one sent, with the captured id", r.Req.URL)
	}
	var expected []string
	for _, b := range r.Result.BodyResult {
		expected = append(expected, b.Expected)
	}
	got := strings.Join(expected, "\n")
	if !strings.Contains(got, live) || strings.Contains(got, "{{") || strings.Contains(got, "usr_REC0001") {
		t.Errorf("the report's expected body is %q; want it at the run's value %s", got, live)
	}
}

// A USE INSIDE AN ARRAY THAT NOTHING TIES TO ITS ELEMENT IS REFUSED (R1).
// Under test.ignoreOrdering (on by default) a use inside an array is
// satisfied by any element of the answer's array that holds the key's value
// at its place. Without noise the comparison's own match of the elements ties
// it to its element; a noise entry over its field or over the element's
// array (from globalNoise, the test set's noise or the test case's own),
// --schema-match (which compares no value), and assertions that judge the
// test case in place of the comparison without json_equal (AS1, AS6, AS7)
// remove that tie, and then the paid order's id moved onto the pending order
// passed (set PASSED, no template failure). Such a use is refused when the
// set loads, naming what unties it and the edits; the test case fails
// unsent. An object-key use and a use inside a longer string are untied
// alike. Without these, with json_equal among the assertions, or with
// ignoreOrdering off, the use runs as before.
func TestRunTestSet_AUseInAnArrayNoiseUntiesIsRefused(t *testing.T) {
	const (
		rec   = `{"orders":[{"id":"{{string .orderA}}","status":"paid"},{"id":"ord_lit","status":"pending"}]}`
		paid  = `{"orders":[{"id":"ord_REC1","status":"paid"},{"id":"ord_lit","status":"pending"}]}`
		moved = `{"orders":[{"id":"ord_lit","status":"paid"},{"id":"ord_REC1","status":"pending"}]}`

		rec3            = `{"orders":[{"id":"{{string .orderA}}","status":"paid"},{"id":"ord_lit","status":"pending"},{"id":"ord_z","status":"shipped"}]}`
		moved3Added     = `{"orders":[{"id":"ord_lit","status":"paid","x":1},{"id":"ord_REC1","status":"pending","x":1},{"id":"ord_z","status":"shipped","x":1}]}`
		moved3AddedLast = `{"orders":[{"id":"ord_lit","status":"paid"},{"id":"ord_REC1","status":"pending"},{"id":"ord_z","status":"shipped","x":1}]}`
		paid3Added      = `{"orders":[{"id":"ord_z","status":"shipped","x":1},{"id":"ord_REC1","status":"paid","x":1},{"id":"ord_lit","status":"pending","x":1}]}`
	)
	noise := func(field string) map[string]map[string][]string {
		return map[string]map[string][]string{"body": {field: {}}}
	}
	status := map[models.AssertionType]interface{}{models.StatusCode: 200}
	for _, c := range []struct {
		name                  string
		recorded, answer      string
		noise                 map[string]map[string][]string
		testSetNoise          map[string]map[string][]string
		tcNoise               map[string][]string
		assertions            map[models.AssertionType]interface{}
		ignoreOrdering        bool
		schemaMatch           bool
		want                  models.TestStatus
		refusedAt, refusedWhy string
		edit                  string
		failsAt               string
		proven                bool
	}{
		{name: "R1 noise over the use's field", recorded: rec, answer: moved, noise: noise("id"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the noise entry body.id (globalNoise) covers its place",
			edit: "remove that noise entry (the variable is asserted anyway), turn ignoreOrdering off for this set, or write orderA's recorded value in place of the placeholder"},
		{name: "the test set's noise over the use's field", recorded: rec, answer: moved, testSetNoise: noise("id"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the noise entry body.id (globalNoise) covers its place", edit: "remove that noise entry"},
		{name: "the test case's noise over the use's field", recorded: rec, answer: moved, tcNoise: map[string][]string{"body.id": {}}, ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the noise entry body.id (this test case) covers its place", edit: "remove that noise entry"},
		{name: "noise over the element's array", recorded: rec, answer: moved, noise: noise("orders"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the noise entry body.orders (globalNoise) covers its place", edit: "remove that noise entry"},
		{name: "--schema-match", recorded: rec, answer: moved, ignoreOrdering: true, schemaMatch: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "--schema-match compares no value",
			edit: "run this set without --schema-match, turn ignoreOrdering off for it, or write orderA's recorded value in place of the placeholder"},
		{name: "AS1 a status_code assertion judges in place of the comparison", recorded: rec, answer: moved, assertions: status, ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the test case's assertions (status_code) judge its answer in place of the comparison and read no whole body",
			edit: "add json_equal to its assertions (it compares the body as written), remove its assertions, turn ignoreOrdering off for this set, or write orderA's recorded value in place of the placeholder"},
		{name: "AS6 a header_exists assertion alone", recorded: rec, answer: moved, assertions: map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"Content-Type"}}, ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the test case's assertions (header_exists) judge its answer", edit: "add json_equal to its assertions"},
		{name: "a json_contains assertion reads no whole body", recorded: rec, answer: moved, assertions: map[models.AssertionType]interface{}{models.StatusCode: 200, models.JsonContains: map[string]interface{}{"orders": []interface{}{}}}, ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].id", refusedWhy: "the test case's assertions (json_contains, status_code) judge its answer", edit: "add json_equal to its assertions"},
		{name: "AS7 an object-key use under a status_code assertion", recorded: `{"orders":[{"{{string .orderA}}":"paid"},{"ord_lit":"pending"}]}`, answer: `{"orders":[{"ord_lit":"paid"},{"ord_REC1":"pending"}]}`,
			assertions: status, ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body key orders[0].{{string .orderA}}", refusedWhy: "the test case's assertions (status_code) judge its answer", edit: "add json_equal to its assertions"},
		{name: "AS5 json_equal among the assertions ties the body: a moved id fails", recorded: rec, answer: moved, assertions: map[models.AssertionType]interface{}{models.StatusCode: 200, models.JsonEqual: true}, ignoreOrdering: true, want: models.TestStatusFailed},
		{name: "json_equal among the assertions, the answer as recorded", recorded: rec, answer: paid, assertions: map[models.AssertionType]interface{}{models.StatusCode: 200, models.JsonEqual: true}, ignoreOrdering: true, want: models.TestStatusPassed},
		{name: "a noise assertion alone leaves the comparison", recorded: rec, answer: paid, assertions: map[models.AssertionType]interface{}{models.NoiseAssertion: map[string]interface{}{}}, ignoreOrdering: true, want: models.TestStatusPassed},
		{name: "AS8 assertions with ignoreOrdering off: the use is asserted at its own index", recorded: rec, answer: moved, assertions: status, want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		{name: "AS3 assertions over a use outside any array: asserted at its path", recorded: `{"first":"{{string .orderA}}"}`, answer: `{"first":"ord_OTHER"}`,
			assertions: status, ignoreOrdering: true, want: models.TestStatusFailed, failsAt: "body.first"},
		{name: "a use in an object key", recorded: `{"orders":[{"{{string .orderA}}":"paid"},{"ord_lit":"pending"}]}`, answer: `{"orders":[{"ord_lit":"paid"},{"ord_REC1":"pending"}]}`,
			noise: noise("orders"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body key orders[0].{{string .orderA}}", refusedWhy: "the noise entry body.orders (globalNoise) covers its place", edit: "remove that noise entry"},
		{name: "a use inside a longer string", recorded: `{"orders":[{"href":"/o/{{string .orderA}}","status":"paid"},{"href":"/o/ord_lit","status":"pending"}]}`,
			answer: `{"orders":[{"href":"/o/ord_lit","status":"paid"},{"href":"/o/ord_REC1","status":"pending"}]}`, noise: noise("href"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.orders[0].href", refusedWhy: "the noise entry body.href (globalNoise) covers its place", edit: "remove that noise entry"},
		{name: "no noise: the comparison ties the use to its element", recorded: rec, answer: moved, ignoreOrdering: true, want: models.TestStatusFailed},
		// The additive-schema auto-pass keeps the tie too (AD4, AD5): it
		// passes only an answer whose elements each contain a recorded one,
		// so a field the app added does not let the id move.
		{name: "AD4 no noise, a field added to every element: the auto-pass keeps the tie", recorded: rec3, answer: moved3Added, ignoreOrdering: true, want: models.TestStatusFailed},
		{name: "AD5 no noise, a field added to the last element: the auto-pass keeps the tie", recorded: rec3, answer: moved3AddedLast, ignoreOrdering: true, want: models.TestStatusFailed},
		{name: "a field added to every element, the ids where they were: passed by the auto-pass", recorded: rec3, answer: paid3Added, ignoreOrdering: true, want: models.TestStatusPassed},
		// An answer encoding/json rejects is not read as JSON by the
		// template layer either (the comparison reads it as text and, with
		// compareAll off, skips it): the use finds no value at its place.
		{name: "NJ7 an answer with a raw TAB in a string", recorded: rec, answer: "{\"orders\":[{\"id\":\"ord_lit\",\"status\":\"pa\tid\"},{\"id\":\"ord_REC1\",\"status\":\"pending\"}]}",
			ignoreOrdering: true, want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		{name: "NJ3 an answer with a raw control byte in a string", recorded: rec, answer: "{\"orders\":[{\"id\":\"ord_lit\",\"status\":\"pa\x01id\"},{\"id\":\"ord_REC1\",\"status\":\"pending\"}]}",
			ignoreOrdering: true, want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		{name: "NJ4 an answer with a bad escape in a string", recorded: rec, answer: `{"orders":[{"id":"ord_lit","status":"pa\xid"},{"id":"ord_REC1","status":"pending"}]}`,
			ignoreOrdering: true, want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		// A dotted noise key names its place anywhere in the path, as the
		// comparison applies it (a substring of the path), not only at the
		// root: orders.id covers data.orders[0].id.
		{name: "a dotted noise key below the root", recorded: `{"data":{"orders":[{"id":"{{string .orderA}}","status":"paid"},{"id":"ord_lit","status":"pending"}]}}`,
			answer: `{"data":{"orders":[{"id":"ord_lit","status":"paid"},{"id":"ord_REC1","status":"pending"}]}}`, noise: noise("orders.id"), ignoreOrdering: true, want: models.TestStatusFailed,
			refusedAt: "response body.data.orders[0].id", refusedWhy: "the noise entry body.orders.id (globalNoise) covers its place", edit: "remove that noise entry"},
		{name: "no noise, the answer as recorded", recorded: rec, answer: paid, ignoreOrdering: true, want: models.TestStatusPassed},
		{name: "ignoreOrdering off: the use is asserted at its own index", recorded: rec, answer: moved, noise: noise("id"), want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		{name: "a use outside any array is asserted at its path under noise, not refused", recorded: `{"first":"{{string .orderA}}"}`, answer: `{"first":"ord_OTHER"}`,
			noise: noise("first"), ignoreOrdering: true, want: models.TestStatusFailed, failsAt: "body.first"},
		// Under the proven switch the comparison reads arrays in order
		// whatever ignoreOrdering says (matcher.ResponseIgnoreOrdering), so
		// noise over the use's field unties nothing: the use is asserted at
		// its own index, as with ignoreOrdering off, and not refused.
		{name: "proven: noise over the use's field, a moved id fails at its own index", recorded: rec, answer: moved, noise: noise("id"), ignoreOrdering: true, proven: true, want: models.TestStatusFailed, failsAt: "body.orders[0].id"},
		{name: "proven: noise over the use's field, the answer as recorded", recorded: rec, answer: paid, noise: noise("id"), ignoreOrdering: true, proven: true, want: models.TestStatusPassed},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, c.answer)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"orderA": "ord_REC1"}}}
			tc := tmplCase(1, app.addr, "GET", "/orders", nil, "", recorded(200, nil, c.recorded, conf.stored.Template))
			tc.Noise = c.tcNoise
			tc.Assertions = c.assertions
			h := newTmplHarness(t, conf, []*models.TestCase{tc}, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = c.noise
				if c.testSetNoise != nil {
					cfg.Test.GlobalNoise.Testsets = config.TestsetNoise{"test-set-0": c.testSetNoise}
				}
				cfg.Test.IgnoreOrdering = c.ignoreOrdering
				cfg.Test.SchemaMatch = c.schemaMatch
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			if st := h.run(t); (st == models.TestSetStatusPassed) != (c.want == models.TestStatusPassed) {
				t.Errorf("the set is %s; want its test case %s", st, c.want)
			}
			if c.refusedAt != "" {
				assertRefusedUnsent(t, h, logs, "test-1", c.refusedAt, "{{string .orderA}}", c.refusedWhy)
				if n := len(app.sent("GET", "/orders")); n != 0 {
					t.Errorf("the refused test case was sent %d time(s)", n)
				}
				if !strings.Contains(h.result(t, "test-1").TemplateFailures[0].Why, c.edit) {
					t.Errorf("the refusal does not give the edit %q: %+v", c.edit, h.result(t, "test-1").TemplateFailures)
				}
				if n := len(logs.FilterMessageSnippet("a noise entry covers a template variable's place").All()); n != 0 {
					t.Errorf("a refused test case's place was also named as asserted under noise (%d)", n)
				}
				return
			}
			if n := len(app.sent("GET", "/orders")); n != 1 {
				t.Errorf("the test case was sent %d time(s); it is not refused", n)
			}
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Errorf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if c.failsAt == "" && len(r.TemplateFailures) != 0 {
				t.Errorf("test-1: template failures %+v; want none (any element holding the value satisfies the use; the comparison ties it)", r.TemplateFailures)
			}
			if c.failsAt != "" && (len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != c.failsAt || r.TemplateFailures[0].Refused) {
				t.Errorf("test-1: template failures %+v; want one at %s", r.TemplateFailures, c.failsAt)
			}
		})
	}
}

// A STREAMED USE INSIDE AN ARRAY IS ASSERTED AT ITS OWN INDEX. The stream
// comparison reads a frame's JSON with its arrays in order (ignoreOrdering is
// not read there), so the use is tied to its element by its index, under
// noise too: not refused, and the paid order's id moved onto the pending one
// fails. Its next step offers no capture: a streamed test case never
// captures (P8).
func TestRunTestSet_AStreamedUseInAnArrayIsAssertedAtItsIndex(t *testing.T) {
	const (
		rec   = `{"orders":[{"id":"{{string .orderA}}","status":"paid"},{"id":"ord_lit","status":"pending"}]}`
		paid  = `{"orders":[{"id":"ord_REC1","status":"paid"},{"id":"ord_lit","status":"pending"}]}`
		moved = `{"orders":[{"id":"ord_lit","status":"paid"},{"id":"ord_REC1","status":"pending"}]}`
	)
	for _, c := range []struct {
		name, answer string
		want         models.TestStatus
	}{
		{"the paid order's id moved onto the pending one", moved, models.TestStatusFailed},
		{"the answer as recorded", paid, models.TestStatusPassed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"ord_REC1"}`)
				})
				mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, c.answer+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"orderA": "ord_REC1"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/orders", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(201, nil, `{"id":"{{capture string .orderA}}"}`, conf.stored.Template)),
				tmplCase(2, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
					StatusCode: 200, StatusMessage: "OK",
					Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
					Body:       rec,
					StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: rec}}}},
				}),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.IgnoreOrdering = true
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"id": {}}}
			})
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			if n := len(app.sent("GET", "/feed")); n != 1 {
				t.Fatalf("the streamed test case was sent %d time(s); it is not refused", n)
			}
			r := h.result(t, "test-2")
			if r.Status != c.want {
				t.Fatalf("test-2: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if c.want == models.TestStatusPassed {
				return
			}
			if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "body frame 0.orders[0].id" {
				t.Errorf("test-2: template failures %+v; want one at body frame 0.orders[0].id", r.TemplateFailures)
			}
			for _, e := range logs.FilterMessageSnippet("a template variable's place does not hold what it must").All() {
				next := fmt.Sprint(e.ContextMap()["next_step"])
				if strings.Contains(next, "{{capture") || !strings.Contains(next, "is what test-1/response body.id captured") {
					t.Errorf("a streamed test case's next step %q offers a capture, or does not name the capture", next)
				}
			}
		})
	}
}

// THE ADDITIVE-SCHEMA AUTO-PASS NEVER PASSES A TEST CASE THAT FAILED ON A
// VARIABLE (PL2b). The read-back's id is noise, so the comparison sees only
// an added field, which the replay passes by default; but the id is asserted
// anyway, and the answer holds another. The test case fails, the auto-pass
// is not logged, and its comparison rows stay as compared: SCHEMA_ADDED
// beside TEMPLATE_VARIABLE.
func TestRunTestSet_TheAdditiveSchemaPassNeverPassesATemplateFailure(t *testing.T) {
	for _, c := range []struct {
		name, answerID string
		want           models.TestStatus
	}{
		{"the read-back answers another id", "usr_OTHER01", models.TestStatusFailed},
		{"the read-back answers the captured id", "usr_LIVE001", models.TestStatusPassed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"usr_LIVE001"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"`+c.answerID+`","name":"alice","added":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			// Content-Length stays as recorded: the added field changes it,
			// which the auto-pass reads as part of an additive change.
			read := tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"id":"{{string .id}}","name":"alice"}`, conf.stored.Template))
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				read,
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"id": {}}}
			})
			core, logs := observer.New(zap.InfoLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-2")
			if r.Status != c.want {
				t.Fatalf("test-2: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			autoPassed := false
			for _, e := range logs.FilterMessageSnippet("Additive response schema change detected").All() {
				if e.ContextMap()["testcase"] == "test-2" {
					autoPassed = true
				}
			}
			if c.want == models.TestStatusPassed {
				if !autoPassed {
					t.Errorf("the added field alone is passed by default, and said so")
				}
				return
			}
			if autoPassed {
				t.Errorf("the auto-pass was logged for a test case that failed on its variable")
			}
			if !hasCategory(r, models.TemplateVariable) || !hasCategory(r, models.SchemaAdded) || r.FailureInfo.Risk != models.High {
				t.Errorf("test-2 graded %s %v; want HIGH with SCHEMA_ADDED and TEMPLATE_VARIABLE, its comparison rows kept", r.FailureInfo.Risk, r.FailureInfo.Category)
			}
			normal := true
			for _, b := range r.Result.BodyResult {
				normal = normal && b.Normal
			}
			if normal {
				t.Errorf("test-2's body row was cleared as if it had passed: %+v", r.Result.BodyResult)
			}
		})
	}
}

// mappedDB is prMappingDB whose recording maps per-test mocks to test cases:
// the mapping-based strategy, under which a test case whose mapped mocks were
// not consumed has a diverged mock set.
type mappedDB struct {
	prMappingDB
	tests map[string][]models.MockEntry
}

func (m *mappedDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return m.tests, true, nil
}

// A TEMPLATE FAILURE IS NEVER DEMOTED TO OBSOLETE (P2). A failed test case
// whose mock set diverged is demoted to OBSOLETE unless --strict-failure is
// set, and an OBSOLETE test case leaves its set passing. A test case that
// failed on a template variable's place stays FAILED and fails its set,
// whatever its mock set did, with its template failures, its
// TEMPLATE_VARIABLE category and HIGH risk saved, and the log line says why
// it is not obsolete. Any other failure is demoted as before (the control).
func TestRunTestSet_ATemplateFailureIsNeverDemotedToObsolete(t *testing.T) {
	for _, c := range []struct {
		name string
		// readBack is test-2's expected body; the app answers
		// {"id":"usr_OTHER01"}.
		readBack string
		strict   bool
		want     models.TestStatus
	}{
		{"a template failure stays FAILED", `{"id":"{{string .id}}"}`, false, models.TestStatusFailed},
		{"with --strict-failure too", `{"id":"{{string .id}}"}`, true, models.TestStatusFailed},
		{"control: another failure is demoted", `{"name":"alice"}`, false, models.TestStatusObsolete},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"usr_LIVE001"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"usr_OTHER01"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, c.readBack, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				// The id is noise: the comparison matches, and only the
				// variable fails the read-back.
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"id": {}}}
				cfg.Test.StrictFailure = c.strict
			})
			core, logs := observer.New(zap.InfoLevel)
			h.r.logger = zap.New(core)
			// The recording maps a per-test mock to the read-back that this
			// replay never consumes: its mock set diverged.
			h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-2": {{Name: "mock-2", Kind: string(models.HTTP)}}}}
			st := h.run(t)
			r := h.result(t, "test-2")
			if r.Status != c.want {
				t.Fatalf("test-2: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if (st == models.TestSetStatusFailed) != (c.want == models.TestStatusFailed) {
				t.Errorf("the set is %s; want it failed exactly when test-2 is FAILED (%s)", st, c.want)
			}
			if c.want == models.TestStatusObsolete {
				if len(logs.FilterMessageSnippet("marking testcase as obsolete").All()) != 1 {
					t.Errorf("the demotion was not said")
				}
				return
			}
			if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "body.id" || r.TemplateFailures[0].Refused {
				t.Errorf("test-2 saved template failures %+v; want the one at body.id", r.TemplateFailures)
			}
			if !hasCategory(r, models.TemplateVariable) || r.FailureInfo.Risk != models.High {
				t.Errorf("test-2 graded %s %v; want HIGH with TEMPLATE_VARIABLE", r.FailureInfo.Risk, r.FailureInfo.Category)
			}
			if len(logs.FilterMessageSnippet("marking testcase as obsolete").All()) != 0 {
				t.Errorf("a template failure was said to be obsolete")
			}
			said := logs.FilterMessageSnippet("mock mapping mismatch detected; marking testcase as FAILED").All()
			if len(said) != 1 {
				t.Fatalf("the mismatch was said %d time(s); want once, as FAILED", len(said))
			}
			if reason := fmt.Sprint(said[0].ContextMap()["reason"]); !c.strict && reason != vetoTemplateFailure {
				t.Errorf("the line gives the reason %q; want %q", reason, vetoTemplateFailure)
			}
			// The diverged mock set is still recorded for the end-of-run
			// mismatch table, as for any failure it would have demoted.
			if !mismatchRecorded(h.r, "test-2") {
				t.Errorf("the mock mismatch of test-2 is not recorded: %+v", h.r.GetMockMismatchFailures())
			}
		})
	}
}

// mismatchRecorded reports whether the run recorded a mock mismatch for the
// test case, for the end-of-run mismatch table.
func mismatchRecorded(r *Replayer, testCase string) bool {
	for _, f := range r.GetMockMismatchFailures() {
		if f.TestSetID == "test-set-0" && f.TestID == testCase {
			return true
		}
	}
	return false
}

// consumingHooks reports, once a test case has been sent, a per-test mock
// that test case's mapping does not hold: the streamed loop's mock set then
// diverged.
type consumingHooks struct {
	TestHooks
	mu   sync.Mutex
	sent bool
}

func (h *consumingHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	h.mu.Lock()
	h.sent = true
	h.mu.Unlock()
	return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
}

func (h *consumingHooks) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sent {
		return nil, nil
	}
	return []models.MockState{{Name: "mock-unmapped", Kind: models.HTTP}}, nil
}

// A STREAMED TEMPLATE FAILURE IS NEVER DEMOTED TO OBSOLETE either: the
// streamed test cases run in their own loop, which demotes a failed test
// case whose mock set diverged as the main loop does (P2).
func TestRunTestSet_AStreamedTemplateFailureIsNeverDemotedToObsolete(t *testing.T) {
	const (
		rec   = `{"orders":[{"id":"{{string .orderA}}","status":"paid"}]}`
		other = `{"orders":[{"id":"ord_OTHER","status":"paid"}]}`
	)
	for _, c := range []struct {
		name, expected string
		want           models.TestStatus
	}{
		{"a template failure stays FAILED", rec, models.TestStatusFailed},
		{"control: another failure is demoted", `{"orders":[{"id":"ord_LIT","status":"paid"}]}`, models.TestStatusObsolete},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, other+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"orderA": "ord_REC1"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
					StatusCode: 200, StatusMessage: "OK",
					Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
					Body:       c.expected,
					StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: c.expected}}}},
				}),
			}
			h := newTmplHarness(t, conf, cases, nil)
			h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-1": {{Name: "mock-1", Kind: string(models.HTTP)}}}}
			h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
			core, logs := observer.New(zap.InfoLevel)
			h.r.logger = zap.New(core)
			st := h.run(t)
			r := h.result(t, "test-1")
			if r.Status != c.want {
				t.Fatalf("test-1: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if (st == models.TestSetStatusFailed) != (c.want == models.TestStatusFailed) {
				t.Errorf("the set is %s; want it failed exactly when test-1 is FAILED (%s)", st, c.want)
			}
			if c.want == models.TestStatusObsolete {
				if len(logs.FilterMessageSnippet("mock mapping mismatch detected for streaming testcase; marking as obsolete").All()) != 1 {
					t.Errorf("the demotion was not said")
				}
				return
			}
			if len(r.TemplateFailures) != 1 || r.TemplateFailures[0].Where != "body frame 0.orders[0].id" {
				t.Errorf("test-1 saved template failures %+v; want the one at body frame 0.orders[0].id", r.TemplateFailures)
			}
			said := logs.FilterMessageSnippet("mock mapping mismatch detected for streaming testcase; marking it as FAILED").All()
			if len(said) != 1 || said[0].ContextMap()["reason"] != vetoTemplateFailure {
				t.Errorf("the mismatch was said %+v; want once, as FAILED, with the template failure as its reason", said)
			}
			if !mismatchRecorded(h.r, "test-1") {
				t.Errorf("the mock mismatch of the streamed test-1 is not recorded: %+v", h.r.GetMockMismatchFailures())
			}
		})
	}
}

// A SET A KILLED keploy templatize LEFT HALF-WRITTEN IS REFUSED (founder
// decision). The set holds a test case file's new bytes and its file as it
// was beside it, as a templatize stopped between its renames leaves them:
// keploy test runs none of its test cases, fails the set with the steps
// keploy templatize prints (each file named) as its failure reason, and logs
// them once with a next step. Done as written, the steps give the set back,
// and the next run replays it.
func TestRunTestSet_ASetAnInterruptedTemplatizeLeftIsRefused(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/health", nil, "", recorded(200, nil, `{"ok":true}`, nil))}
	h := newTmplHarness(t, &tmplConf{}, cases, nil)
	db := testdb.New(zap.NewNop(), h.r.config.Path)
	for _, tc := range cases {
		cp := *tc
		cp.Noise = map[string][]string{}
		if err := db.UpdateTestCase(context.Background(), &cp, "test-set-0", false); err != nil {
			t.Fatal(err)
		}
	}
	h.r.testDB = db
	tests := filepath.Join(h.r.config.Path, "test-set-0", "tests")
	recordedFile, err := os.ReadFile(filepath.Join(tests, "test-1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(tests, "test-1.yaml"+setfile.ReplaceOldSuffix), filepath.Join(tests, "test-1.yaml"+setfile.ReplaceNewSuffix)
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, recordedFile, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("the set is %s; a set an interrupted templatize left is refused", status)
	}
	if got := app.sent("GET", "/health"); len(got) != 0 {
		t.Fatalf("the refused set's test case was sent %d time(s)", len(got))
	}
	h.report.mu.Lock()
	rep := h.report.report
	h.report.mu.Unlock()
	steps := "rename tests/test-1.yaml.keploy-old to tests/test-1.yaml; delete tests/test-1.yaml.keploy-new; then run the command again"
	if rep == nil || rep.Status != string(models.TestSetStatusFailed) || !strings.Contains(rep.FailureReason, steps) || rep.Total != 1 || rep.Failure != 1 {
		t.Fatalf("the set's report %+v; want FAILED, 1 of 1 failed, and the steps %q as its reason", rep, steps)
	}
	said := logs.FilterMessageSnippet("keploy test refused this test set").All()
	if len(said) != 1 || said[0].Level != zap.ErrorLevel || said[0].ContextMap()["next_step"] == "" || !strings.Contains(fmt.Sprint(said[0].ContextMap()["error"]), steps) {
		t.Fatalf("the refusal was logged as %+v; want one error with the steps and a next step", said)
	}
	// Each test case of the set has a FAILED result refused unsent with the
	// steps as its reason, so every report format and --must-pass read it as
	// refused; the set's report holds them, graded HIGH.
	res := h.result(t, "test-1")
	if res.Status != models.TestStatusFailed || len(res.TemplateFailures) != 1 || !res.TemplateFailures[0].Refused ||
		res.TemplateFailures[0].Where != "test set test-set-0" || !strings.Contains(res.TemplateFailures[0].Why, steps) || res.FailureInfo.Risk != models.High {
		t.Fatalf("test-1's result %+v; want FAILED, refused, at test set test-set-0, with the steps, HIGH", res)
	}
	// A refused set is no template variable's failure: its own category, so
	// a report or dashboard grouping by category does not file it there.
	if !slices.Equal(res.FailureInfo.Category, []models.FailureCategory{models.TestSetRefused}) ||
		!slices.Equal(res.Result.FailureInfo.Category, []models.FailureCategory{models.TestSetRefused}) {
		t.Fatalf("test-1's categories %v (result %v); want only TEST_SET_REFUSED", res.FailureInfo.Category, res.Result.FailureInfo.Category)
	}
	if len(rep.Tests) != 1 || rep.Tests[0].TestCaseID != "test-1" || rep.HighRisk != 1 {
		t.Fatalf("the set's report holds %d result(s), high risk %d; want test-1's, high risk 1", len(rep.Tests), rep.HighRisk)
	}
	// The run's summary and totals count the set as failed.
	h.r.completeTestReportMu.Lock()
	verdict, total, failed := h.r.completeTestReport["test-set-0"], h.r.totalTests, h.r.totalTestFailed
	h.r.completeTestReportMu.Unlock()
	if verdict.status || verdict.total != 1 || verdict.failed != 1 || total != 1 || failed != 1 {
		t.Fatalf("the run's summary has %+v with totals %d/%d failed; want the set failed, 1 of 1", verdict, failed, total)
	}

	if err := os.Rename(old, filepath.Join(tests, "test-1.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fresh); err != nil {
		t.Fatal(err)
	}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("after the steps the set is %s, want PASSED", status)
	}
}

// A SANITIZED SECRET IN AN ASSERTION'S VALUE IS COMPARED AS THE PLACEHOLDER'S
// TEXT (disclosed verdict change 3). An assertion is read as written, so
// header_equal: {X-User: "{{string .secret.k}}"} holds only for an app that
// answers that text, and an app that answers the secret fails it. main
// rendered the whole test case, assertions included, and passed the secret.
// A false fail on a hand-written assertion, never a false pass for a real
// answer.
func TestRunTestSet_ASecretInAnAssertionIsComparedAsItsText(t *testing.T) {
	for _, c := range []struct {
		name, answer string
		want         models.TestStatus
	}{
		{"the app answers the secret", "u_SECRET", models.TestStatusFailed},
		{"the app answers the placeholder's text", "{{string .secret.k}}", models.TestStatusPassed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("X-User", c.answer)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Secret: map[string]interface{}{"k": "u_SECRET"}}}
			tc := tmplCase(1, app.addr, "GET", "/me", nil, "", recorded(200, map[string]string{"X-User": "u_SECRET"}, `{"ok":true}`, nil))
			tc.Assertions = map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-User": "{{string .secret.k}}"}}
			h := newTmplHarness(t, conf, []*models.TestCase{tc}, nil)
			h.run(t)
			assertStatus(t, h, "test-1", c.want, "the assertion is compared as written")
		})
	}
}

// --update-template IS NAMED ONCE PER TEMPLATED SET: the run warns that it no
// longer writes config.yaml, with the next step, and writes nothing. A set
// without templates has nothing it could have written, and is not named.
func TestRunTestSet_UpdateTemplateIsNamedOncePerTemplatedSet(t *testing.T) {
	for _, c := range []struct {
		name string
		tmpl map[string]interface{}
		want int
	}{
		{"a templated set", map[string]interface{}{"id": "u_1"}, 1},
		{"a set without templates", nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: c.tmpl}}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/health", nil, "", recorded(200, nil, `{"ok":true}`, nil))}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.UpdateTemplate = true })
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			said := logs.FilterMessageSnippet("--update-template no longer writes this run's template values into config.yaml").All()
			if len(said) != c.want {
				t.Fatalf("named %d time(s), want %d", len(said), c.want)
			}
			if c.want == 1 && said[0].ContextMap()["next_step"] != "remove --update-template (or test.updateTemplate) from your command or keploy.yml" {
				t.Fatalf("the warning's next step is %v", said[0].ContextMap()["next_step"])
			}
			if conf.staged != nil {
				t.Fatalf("config.yaml was written: %+v", conf.staged)
			}
		})
	}
}

// A STREAMED TEST CASE'S REPORT HOLDS THE REQUEST AS SENT: its URL with the
// id the earlier test case captured, not the placeholder.
func TestRunTestSet_AStreamedReportHoldsTheRequestAsSent(t *testing.T) {
	const live = "usr_LIVE001"
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
		})
		mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"n":1}`+"\n")
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
	stream := tmplCase(2, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
		StatusCode: 200, Header: map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       `{"n":1}` + "\n",
		StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: `{"n":1}`}}}},
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
		stream,
	}
	h := newTmplHarness(t, conf, cases, nil)
	h.run(t)
	if got, want := h.result(t, "test-2").Req.URL, "http://"+app.addr+"/events/"+live; got != want {
		t.Fatalf("the streamed report's request URL is %q, want the one sent, %q", got, want)
	}
}

// A NOISE ENTRY OVER A HEADER THAT HOLDS TWO USES IS NAMED ONCE: the warning
// is per test case, place and entry, not per placeholder.
func TestRunTestSet_ANoiseEntryOverTwoUsesOfOneHeaderIsNamedOnce(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/pair", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("X-Pair", "u_1-o_1")
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"a": "u_1", "b": "o_1"}}}
	cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/pair", nil, "", recorded(200, map[string]string{"X-Pair": "{{string .a}}-{{string .b}}"}, `{"ok":true}`, nil))}
	h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
		cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"header": {"X-Pair": {}}}
	})
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "both uses hold")
	if said := logs.FilterMessageSnippet("a noise entry covers a template variable's place").All(); len(said) != 1 {
		t.Fatalf("the noise entry was named %d time(s), want once: %+v", len(said), said)
	}
}

// A VARIABLE IN A STREAMED TEST CASE'S ASSERTIONS IS REFUSED (founder
// decision), as in any other test case's: the test case fails unsent, with
// the edit.
func TestRunTestSet_AVariableInAStreamedCasesAssertionsIsRefused(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-User", "u_REC1")
			_, _ = io.WriteString(w, `{"n":1}`+"\n")
		})
	})
	conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "u_REC1"}}}
	tc := tmplCase(1, app.addr, "GET", "/events", nil, "", models.HTTPResp{
		StatusCode: 200, Header: map[string]string{"Content-Type": "application/x-ndjson", "X-User": "u_REC1"},
		Body: `{"n":1}` + "\n",
	})
	tc.Assertions = map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-User": "{{string .id}}"}}
	h := newTmplHarness(t, conf, []*models.TestCase{tc}, nil)
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Errorf("the set is %s; a set with a refused streamed test case fails", status)
	}
	if got := app.sent("GET", "/events"); len(got) != 0 {
		t.Errorf("the refused streamed case was sent %d time(s)", len(got))
	}
	assertRefusedUnsent(t, h, logs, "test-1", "assertions header_equal.X-User", "{{string .id}}", "an assertion is read as written")
}

// --must-pass KEEPS A TEST CASE REFUSED WHEN ITS SET LOADED. It was never
// sent, so there is no failure to remove it for: --must-pass removes none of
// it, says why with the next step, and the set fails until it is edited.
// Before, the refused test case was deleted as a failing one.
func TestMustPass_KeepsATestCaseRefusedWhenItsSetLoaded(t *testing.T) {
	app := newBPApp(t)
	port := closedLocalPort(t)
	refused := bpTestCase("test-2", port)
	refused.HTTPReq.Header["X-Id"] = "{{capture string .id}}"
	run := newBasePathRunOf(t, app.URL, map[string]interface{}{"id": "u_1"}, bpTestCase("test-1", port), refused)
	run.replayer.config.Test.MustPass = true
	run.replayer.config.Test.MaxFailAttempts = 5

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := run.tests.deleted.Load(); got != 0 {
		t.Fatalf("--must-pass deleted %d test case(s); a refused one was never sent", got)
	}
	kept := run.logs.FilterMessageSnippet("--must-pass keeps the test cases refused when their test set loaded").All()
	if len(kept) == 0 || fmt.Sprint(kept[0].ContextMap()["testCaseIDs"]) != "[test-2]" {
		t.Fatalf("--must-pass said %+v; want it to name test-2 as kept", kept)
	}
	if code != 1 {
		t.Fatalf("a run with a refused test case exited %d, want 1", code)
	}
}

// unreadableSets is a test store whose check for an interrupted write fails
// for another reason: a set directory it cannot read.
type unreadableSets struct{ *prTestDB }

func (unreadableSets) Interrupted(context.Context, string) error {
	return errors.New("open keploy/test-set-0: permission denied")
}

// A SET WHOSE FILES CANNOT BE READ IS NOT CALLED HALF-WRITTEN. Only what an
// interrupted keploy templatize left refuses a set; any other error of the
// store's is the run's error, as a set whose test cases cannot be read is,
// and no result or report says the set was refused.
func TestRunTestSet_ASetWhoseFilesCannotBeReadIsAnError(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/health", nil, "", recorded(200, nil, `{"ok":true}`, nil))}
	h := newTmplHarness(t, &tmplConf{}, cases, nil)
	h.r.testDB = unreadableSets{prTestDB: &prTestDB{cases: cases}}
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.r.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if err == nil || !strings.Contains(err.Error(), "permission denied") || status != models.TestSetStatusFailed {
		t.Fatalf("RunTestSet: %s, %v; want FAILED with the store's error", status, err)
	}
	if got := app.sent("GET", "/health"); len(got) != 0 {
		t.Fatalf("the set's test case was sent %d time(s)", len(got))
	}
	h.report.mu.Lock()
	rep, results := h.report.report, len(h.report.results)
	h.report.mu.Unlock()
	if rep != nil || results != 0 {
		t.Fatalf("a report %+v and %d result(s) were saved for a set that was not refused", rep, results)
	}
	if said := logs.FilterMessageSnippet("keploy test refused this test set").All(); len(said) != 0 {
		t.Fatalf("an unreadable set was called half-written: %+v", said)
	}
}

// deletionRecorder is a base-path run's test store that removes the test
// cases --must-pass deletes, and keeps their names.
type deletionRecorder struct {
	*bpTestDB
	deleted []string
}

func (d *deletionRecorder) DeleteTests(ctx context.Context, set string, ids []string) error {
	d.deleted = append(d.deleted, ids...)
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	kept := d.prTestDB.cases[:0]
	for _, tc := range d.prTestDB.cases {
		if !gone[tc.Name] {
			kept = append(kept, tc)
		}
	}
	d.prTestDB.cases = kept
	return d.bpTestDB.DeleteTests(ctx, set, ids)
}

// --must-pass REMOVES A FAILING TEST CASE AND KEEPS A REFUSED ONE, AND DOES
// NOT RERUN A SET ONLY REFUSALS FAIL. With a refused test case and one that
// fails, --must-pass removes the failing one, reruns the set, and stops once
// only the refusal fails (no rerun can change a refusal); it says it keeps
// the refused one once. With only the refused one failing, it runs the set
// once, however many flaky checks are configured.
func TestMustPass_RemovesTheFailingAndStopsAtTheRefused(t *testing.T) {
	for _, c := range []struct {
		name      string
		failing   bool
		wantGone  []string
		wantRuns  int
		flakyRuns uint32
	}{
		{"a refused and a failing test case", true, []string{"test-3"}, 2, 3},
		{"only a refused test case", false, nil, 1, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newBPApp(t)
			port := closedLocalPort(t)
			refused := bpTestCase("test-2", port)
			refused.HTTPReq.Header["X-Id"] = "{{capture string .id}}"
			cases := []*models.TestCase{bpTestCase("test-1", port), refused}
			if c.failing {
				app.wrong["/test-3"] = true
				cases = append(cases, bpTestCase("test-3", port))
			}
			run := newBasePathRunOf(t, app.URL, map[string]interface{}{"id": "u_1"}, cases...)
			rec := &deletionRecorder{bpTestDB: run.tests}
			run.replayer.testDB = rec
			run.replayer.config.Test.MustPass = true
			run.replayer.config.Test.MaxFailAttempts = 5
			run.replayer.config.Test.MaxFlakyChecks = c.flakyRuns

			code, err := run.start(t)

			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !slices.Equal(rec.deleted, c.wantGone) {
				t.Fatalf("--must-pass deleted %v, want %v", rec.deleted, c.wantGone)
			}
			runs := 0
			for _, asked := range app.asked() {
				if asked == "GET /test-1" {
					runs++
				}
			}
			if runs != c.wantRuns {
				t.Fatalf("the set ran %d time(s), want %d", runs, c.wantRuns)
			}
			if kept := run.logs.FilterMessageSnippet("--must-pass keeps the test cases refused when their test set loaded").All(); len(kept) != 1 {
				t.Fatalf("the refused test case was named %d time(s), want once", len(kept))
			}
			if code != 1 {
				t.Fatalf("a run with a refused test case exited %d, want 1", code)
			}
		})
	}
}

// A test case's request lists every header either side holds, each once,
// sorted: one the sent request alone holds is a part of it as one the
// recording alone holds is, "" on the other side.
func TestHeaderNames(t *testing.T) {
	got := headerNames(map[string]string{"X-B": "1", "X-A": "2"}, map[string]string{"X-A": "3", "X-C": "4"})
	if want := []string{"X-A", "X-B", "X-C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names %v; want %v", got, want)
	}
	if got := headerNames(nil, nil); len(got) != 0 {
		t.Fatalf("no headers: %v", got)
	}
}
