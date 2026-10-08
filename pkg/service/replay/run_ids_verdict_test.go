package replay

// Verdicts of whole test sets whose ids are followed. Each test drives a set
// through RunTestSet with the real pkg.SimulateHTTP (template rendering, and
// templates learned from the answer) and the real matcher, against an
// httptest app. Only the agent is a stand-in: it binds the pairs the real
// agent would bind at the app's dependency call, and only for an id the
// replay asked it to follow.

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

const (
	rcR1 = "0f8fad5b-d9cb-469f-a165-70867728950e" // recorded order id
	rcL1 = "7c9e6679-7425-40de-944b-e07fc1f90ae7" // the one made this run
	rcR2 = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b" // recorded second id
	rcL2 = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d" // the one made this run
	rcZ  = "3d594650-3436-4f8c-9a57-2b1d6f0c8e11" // an id nobody made
)

type rcAgent struct {
	*prInstr
	mu    sync.Mutex
	asked []models.OutgoingOptions
	pairs map[string]string
}

func (a *rcAgent) MockOutgoing(_ context.Context, opts models.OutgoingOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, opts)
	return nil
}

func (a *rcAgent) GetIDPairs(context.Context) (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.pairs), nil
}

// bind is the agent binding recorded -> live at the app's dependency call:
// only an id the replay asked it to follow.
func (a *rcAgent) bind(recorded, live string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 || !slices.Contains(a.asked[len(a.asked)-1].RebindValues, recorded) {
		return
	}
	if a.pairs == nil {
		a.pairs = map[string]string{}
	}
	a.pairs[recorded] = live
}

// rcHooks sends every test case to the app as keploy test does.
type rcHooks struct{ prHooks }

func (rcHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	resp, err := pkg.SimulateHTTP(ctx, tc, set, zap.NewNop(), pkg.SimulationConfig{APITimeout: 10, ConfigHost: "127.0.0.1"})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

type rcRun struct {
	*prRun
	agent *rcAgent
	conf  *riConf
	mu    sync.Mutex
	paths []string
}

func (h *rcRun) status(name string) models.TestStatus {
	for _, r := range h.report.results {
		if r.TestCaseID == name {
			return r.Status
		}
	}
	return "NOT-RUN"
}

func (h *rcRun) describe(t *testing.T) {
	t.Helper()
	for _, r := range h.report.results {
		exp, act := "", ""
		if len(r.Result.BodyResult) > 0 {
			exp, act = r.Result.BodyResult[0].Expected, r.Result.BodyResult[0].Actual
		}
		t.Logf("  %-8s %-7s url=%s run_ids=%v\n      expected=%s\n      actual  =%s", r.TestCaseID, r.Status, r.Req.URL, r.RunIDs, exp, act)
	}
}

// rcStart builds a replay of the given test set against app. cases gets the
// app's address; app gets the run (to bind pairs).
func rcStart(t *testing.T, template map[string]interface{}, cases func(addr string) []*models.TestCase, app func(h *rcRun) http.Handler) *rcRun {
	t.Helper()
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })

	base := newPartialRunHarness(t, 0, 0)
	h := &rcRun{prRun: base, agent: &rcAgent{prInstr: base.instr}, conf: &riConf{template: template}}
	handler := app(h)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	tcs := cases(strings.TrimPrefix(srv.URL, "http://"))
	h.cases = tcs
	h.replayer.testDB = &prTestDB{cases: tcs}
	h.replayer.instrumentation = h.agent
	h.replayer.hookImpl = rcHooks{}
	h.replayer.testSetConf = h.conf
	h.replayer.config.Test.Mocking = true
	return h
}

func rcJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func rcCase(name, method, url, reqBody string, status int, respBody string, at time.Time) *models.TestCase {
	return &models.TestCase{
		Version: models.GetVersion(), Kind: models.HTTP, Name: name,
		HTTPReq:  models.HTTPReq{Method: models.Method(method), URL: url, Body: reqBody, Header: map[string]string{"Content-Type": "application/json"}, Timestamp: at},
		HTTPResp: models.HTTPResp{StatusCode: status, Body: respBody, Header: map[string]string{"Content-Type": "application/json"}, Timestamp: at},
		// Headers are out of these probes: the httptest app adds Date and
		// Content-Length, which a recorded test case would carry too.
		Noise: map[string][]string{"header": {}},
	}
}

// A healthy app whose create answers with two values the set's templates hold,
// both written out in its expected response (as `keploy normalize` leaves a
// response): the order id, which the agent binds, and a second value it does
// not. It passes with rebinding off, and must pass with it on.
//
// Comparing the answer only to choose a verdict must not teach the templates
// what it holds: the comparison whose verdict stands then no longer sees the
// recorded value as a template's, and the second value fails.
func TestRunTestSet_ComparingToChooseAVerdictTeachesTheTemplatesNothing(t *testing.T) {
	for name, second := range map[string][2]string{
		"a generated UUID the agent did not bind": {rcR2, rcL2},
		"a token, never followed":                 {"tok_5f2c9a7e1b", "tok_91aa07c3de"},
	} {
		t.Run(name, func(t *testing.T) {
			template, cases, app := rcTwoValueSet(second[0], second[1])

			off := rcStart(t, maps.Clone(template), cases, app)
			off.replayer.config.Test.DisableMockRebinding = true
			if status := off.run(t); status != models.TestSetStatusPassed {
				off.describe(t)
				t.Fatalf("with rebinding off the healthy app passes; got %s", status)
			}

			on := rcStart(t, maps.Clone(template), cases, app)
			if status := on.run(t); status != models.TestSetStatusPassed {
				on.describe(t)
				t.Fatalf("the healthy app passes with rebinding off and must pass with it on: set %s, create %s", status, on.status("create"))
			}
		})
	}
}

func rcTwoValueSet(second, secondLive string) (map[string]interface{}, func(string) []*models.TestCase, func(*rcRun) http.Handler) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	template := map[string]interface{}{"order": rcR1, "receipt": second}
	cases := func(addr string) []*models.TestCase {
		return []*models.TestCase{
			rcCase("create", "POST", "http://"+addr+"/orders", `{"name":"widget"}`, 201, `{"order_id":"`+rcR1+`","receipt":"`+second+`"}`, at),
			rcCase("read", "GET", "http://"+addr+"/orders/{{string .order}}", "", 200, `{"status":"new"}`, at.Add(time.Second)),
			rcCase("receipt", "GET", "http://"+addr+"/receipts/{{string .receipt}}", "", 200, `{"paid":false}`, at.Add(2*time.Second)),
		}
	}
	app := func(h *rcRun) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/orders":
				// The app mints both, and stores the order at its dependency:
				// that call is where the agent binds the order id. The receipt
				// goes to no dependency.
				h.agent.bind(rcR1, rcL1)
				rcJSON(w, 201, `{"order_id":"`+rcL1+`","receipt":"`+secondLive+`"}`)
			case r.URL.Path == "/orders/"+rcL1:
				rcJSON(w, 200, `{"status":"new"}`)
			case r.URL.Path == "/receipts/"+secondLive:
				rcJSON(w, 200, `{"paid":false}`)
			default:
				rcJSON(w, 404, `{"error":"unknown"}`)
			}
		})
	}
	return template, cases, app
}

func rcReadBackSet(readExpected string, wrongDepID bool) (map[string]interface{}, func(string) []*models.TestCase, func(*rcRun) http.Handler) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	template := map[string]interface{}{"id": rcR1}
	cases := func(addr string) []*models.TestCase {
		return []*models.TestCase{
			rcCase("create", "POST", "http://"+addr+"/orders", `{"name":"widget"}`, 201, `{"id":"{{string .id}}"}`, at),
			rcCase("read", "GET", "http://"+addr+"/orders/{{string .id}}", "", 200, readExpected, at.Add(time.Second)),
		}
	}
	app := func(h *rcRun) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/orders":
				h.agent.bind(rcR1, rcL1) // the create reaches the dependency with the new id
				rcJSON(w, 201, `{"id":"`+rcL1+`"}`)
			case strings.HasPrefix(r.URL.Path, "/orders/"):
				asked := strings.TrimPrefix(r.URL.Path, "/orders/")
				item := asked // a healthy app reads the item it was asked for: the mock answers with the live id
				if wrongDepID {
					// It asked the dependency for another id; the closest
					// recording answered, exactly as recorded.
					item = rcR1
				}
				rcJSON(w, 200, `{"asked":"`+asked+`","item":{"id":"`+item+`","name":"widget"}}`)
			default:
				rcJSON(w, 404, `{"error":"unknown"}`)
			}
		})
	}
	return template, cases, app
}

// casesOf and appOf pick the test cases and the app out of a set built by
// rcReadBackSet.
func casesOf(_ map[string]interface{}, cases func(string) []*models.TestCase, _ func(*rcRun) http.Handler) func(string) []*models.TestCase {
	return cases
}
func appOf(_ map[string]interface{}, _ func(string) []*models.TestCase, app func(*rcRun) http.Handler) func(*rcRun) http.Handler {
	return app
}

// An app that asks its dependency for an id nobody made: the agent answers that
// call with the closest recording, as recorded, and the app's answer names the
// recorded id in the item beside the id it was asked for.
//
// Where the read-back's expected response names the id written out, the set
// fails. Where it names the id only through a placeholder, the verdict is the
// one a replay without following gives (see Replayer.compareFollowed): a pass,
// as it always was.
func TestRunTestSet_AnAnswerForAnotherEntity(t *testing.T) {
	for _, c := range []struct {
		name, expected string
		fails          bool
	}{
		{"the id written out", `{"asked":"` + rcR1 + `","item":{"id":"` + rcR1 + `","name":"widget"}}`, true},
		{"the id behind a placeholder where it is asked for", `{"asked":"{{string .id}}","item":{"id":"` + rcR1 + `","name":"widget"}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			healthy := rcStart(t, map[string]interface{}{"id": rcR1}, casesOf(rcReadBackSet(c.expected, false)), appOf(rcReadBackSet(c.expected, false)))
			if status := healthy.run(t); status != models.TestSetStatusPassed {
				healthy.describe(t)
				t.Fatalf("the healthy app must pass; got %s", status)
			}
			// The read-back's verdict. (Without following, the create fails
			// on the id the app made wherever its expected response holds
			// it: that is what following is for.)
			verdict := func(rebinding bool) models.TestStatus {
				bug := rcStart(t, map[string]interface{}{"id": rcR1}, casesOf(rcReadBackSet(c.expected, true)), appOf(rcReadBackSet(c.expected, true)))
				bug.replayer.config.Test.DisableMockRebinding = !rebinding
				bug.run(t)
				return bug.status("read")
			}
			on, off := verdict(true), verdict(false)
			if c.fails && on != models.TestStatusFailed {
				t.Fatalf("the app asked its dependency for an id nobody made, and the read-back %s", on)
			}
			if !c.fails && on != off {
				t.Fatalf("the read-back %s with rebinding on, %s without: the verdict must be the one without", on, off)
			}
		})
	}
}

// A read-back whose expected response names the id only through a placeholder
// answers with an id nobody made. Nothing is swapped into its expected
// response, and the id bound this run must still be asserted there: without
// following, a template's value is accepted whatever the answer holds.
func TestRunTestSet_ABoundIDBehindAPlaceholderIsStillAsserted(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := func(addr string) []*models.TestCase {
		return []*models.TestCase{
			rcCase("create", "POST", "http://"+addr+"/orders", `{"name":"widget"}`, 201, `{"id":"{{string .id}}"}`, at),
			rcCase("read", "GET", "http://"+addr+"/orders/{{string .id}}", "", 200, `{"id":"{{string .id}}","name":"widget"}`, at.Add(time.Second)),
		}
	}
	app := func(h *rcRun) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				h.agent.bind(rcR1, rcL1)
				rcJSON(w, 201, `{"id":"`+rcL1+`"}`)
				return
			}
			rcJSON(w, 200, `{"id":"`+rcZ+`","name":"widget"}`) // another id than the one it made
		})
	}
	h := rcStart(t, map[string]interface{}{"id": rcR1}, cases, app)
	status := h.run(t)
	if h.status("read") != models.TestStatusFailed {
		h.describe(t)
		t.Fatalf("the read-back answered with %s where the id made this run (%s) is expected, and the set %s, the read-back %s", rcZ, rcL1, status, h.status("read"))
	}
}
