package replay

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// varInstr is the stand-in agent of a replay that follows test variables: it
// answers the read-back of what each test bound.
type varInstr struct {
	*prInstr
	bound func() ([]models.VariableBinding, error)
	reads int
}

func (v *varInstr) TestVariables(context.Context) ([]models.VariableBinding, error) {
	v.reads++
	return v.bound()
}

// createThenRead is a set whose create captures the id the app answers and
// whose read-back sends it: config.yaml holds the recorded id.
func createThenRead(t *testing.T, answered string) (*tmplHarness, *prInstr) {
	t.Helper()
	tmpl := map[string]interface{}{"id": "u_rec_0001"}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"`+answered+`"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			s := a.record(r)
			writeJSON(w, http.StatusOK, `{"id":"`+strings.TrimPrefix(s.path, "/users/")+`"}`)
		})
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, tmpl)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
			recorded(200, nil, `{"id":"{{string .id}}"}`, tmpl)),
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
	return h, h.r.instrumentation.(*prInstr)
}

// Under the proven switch each test case begins with the template variables
// its request sends bound in the agent: the read-back's id, as sent. With the
// switch off nothing is sent.
func TestRunTestSet_ATestCaseSendsTheAgentTheVariablesItsRequestCarries(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			h, instr := createThenRead(t, "u_live_0002")
			vi := &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, nil }}
			h.r.instrumentation = vi
			h.run(t)
			if proven == "0" && vi.reads != 0 {
				t.Fatalf("switch off: the agent's variables were read %d times", vi.reads)
			}
			var sent [][]models.VariableBinding
			instr.mu.Lock()
			for _, p := range instr.allParams {
				perTest := !p.AfterTime.Equal(models.BaseTime)
				if p.Exact != nil {
					// Under the switch: the filter that opens the test case
					// (exact serving), not the pool after it or the staging.
					perTest = p.Exact.Test != ""
				}
				if perTest {
					sent = append(sent, p.Variables)
				}
			}
			instr.mu.Unlock()
			if len(sent) != 2 {
				t.Fatalf("%d per-test filters, want 2", len(sent))
			}
			if len(sent[0]) != 0 {
				t.Fatalf("the create's request carries no variable, sent %+v", sent[0])
			}
			if proven == "0" {
				if len(sent[1]) != 0 {
					t.Fatalf("switch off: sent %+v", sent[1])
				}
				return
			}
			want := models.VariableBinding{Name: "id", Value: "u_live_0002", Recorded: "u_rec_0001", Mock: "test case test-2", Place: "request", Given: true}
			// Every field, the clock reads (VariableBinding.Clock) too: none
			// for a value the test case's request gave.
			if len(sent[1]) != 1 || !reflect.DeepEqual(sent[1][0], want) {
				t.Fatalf("the read-back sent %+v, want [%+v]", sent[1], want)
			}
		})
	}
}

// A capture of a key a dependency call bound must take the value the app sent
// that dependency: an app that stores one id and answers another fails.
func TestRunTestSet_ACaptureHoldsTheValueTheAppSentItsDependency(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name   string
		stored string
		want   models.TestStatus
	}{
		{"the id it stored", "u_live_0002", models.TestStatusPassed},
		{"another id than it stored", "u_other_0003", models.TestStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, instr := createThenRead(t, "u_live_0002")
			calls := 0
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) {
				calls++
				if calls > 1 {
					return []models.VariableBinding{{Name: "id", Value: "u_live_0002", Recorded: "u_rec_0001", Mock: "test case test-2", Place: "request", Given: true}}, nil
				}
				return []models.VariableBinding{{Name: "id", Value: tc.stored, Recorded: "u_rec_0001", Mock: "mock-3", Place: "body.id"}}, nil
			}}
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			r := h.result(t, "test-1")
			if said := logs.FilterMessageSnippet("is not the one the application sent its dependency").All(); (len(said) == 1) != (tc.want == models.TestStatusFailed) {
				t.Fatalf("the mismatch was logged %d time(s)", len(said))
			} else if len(said) == 1 && said[0].ContextMap()["next_step"] == nil {
				t.Fatalf("the log gives no next step: %+v", said[0].ContextMap())
			}
			if r.Status != tc.want {
				t.Fatalf("the create: %s, want %s (%+v)", r.Status, tc.want, r.TemplateFailures)
			}
			if len(r.Variables) != 1 || r.Variables[0].Mock != "mock-3" {
				t.Fatalf("the report holds %+v, want the dependency's binding", r.Variables)
			}
			if tc.want == models.TestStatusFailed {
				if len(r.TemplateFailures) != 1 || !strings.Contains(r.TemplateFailures[0].Why, `sent "u_other_0003" as id to its dependency (mock-3 body.id)`) {
					t.Fatalf("the failure does not name the dependency's value: %+v", r.TemplateFailures)
				}
				return
			}
			// A binding the test case's own request made is no dependency's: the
			// read-back's capture-free answer is compared as written.
			assertStatus(t, h, "test-2", models.TestStatusPassed, "the read-back of this run's id")
		})
	}
}

// What keploy itself cannot read is no failure of the app: a read-back that
// fails for a test case that captured something leaves the set not judged
// (INTERNAL_ERR) and fails no test case; so does a keploy that cannot read
// the agent's variables at all.
func TestRunTestSet_AnUnreadableBindingLeavesTheSetNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, instr := createThenRead(t, "u_live_0002")
	h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) {
		return nil, errors.New("agent gone")
	}}
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("the set is %s, want INTERNAL_ERR (not judged)", status)
	}
	if r := h.result(t, "test-1"); r.Status != models.TestStatusPassed || len(r.TemplateFailures) != 0 || !strings.Contains(r.FailureInfo.VariablesUnchecked, "agent gone") {
		t.Fatalf("the create: %s %+v, unchecked %q; want its verdict untouched and its row marked", r.Status, r.TemplateFailures, r.FailureInfo.VariablesUnchecked)
	}
	if r := h.result(t, "test-2"); r.FailureInfo.VariablesUnchecked != "" {
		t.Fatalf("the read-back captured nothing, yet its row is marked: %q", r.FailureInfo.VariablesUnchecked)
	}
	if reason := h.report.report.FailureReason; !strings.Contains(reason, "not judged") || !strings.Contains(reason, "test-1") || !strings.Contains(reason, "agent gone") {
		t.Fatalf("failure reason %q does not say the set is not judged, for which test, and why", reason)
	}
	if !h.r.notJudged("test-set-0") {
		t.Fatal("the set is not recorded as not judged: the run would stop after it")
	}
	if n := len(logs.FilterMessageSnippet("the report lists none").All()); n != 1 {
		t.Fatalf("the failed read of a test case that captured nothing was said %d times, want once per set", n)
	}

	h, instr = createThenRead(t, "u_live_0002")
	h.r.instrumentation = struct{ Instrumentation }{instr} // cannot read the agent's variables
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("a keploy that cannot read the agent's variables judged the set: %s", status)
	}
}

// Under --retry-passing-test a test case's row is its last cycle's, and so
// is whether what it captured was checked: a create whose first cycle's
// check could not read the agent's variables, and whose next cycle's could,
// has its verdict, and its set is judged (P019). The unchecked list kept
// every cycle's, and left the set not judged. A cycle runs again, the
// passing test cases only, when one failed: the read-back answers another
// id.
func TestRunTestSet_AnUncheckedCaptureALaterCycleCheckedLeavesTheSetJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tmpl := map[string]interface{}{"id": "u_rec_0001"}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"u_live_0002"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"id":"another"}`)
		})
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, tmpl)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
			recorded(200, nil, `{"id":"{{string .id}}"}`, tmpl)),
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
	h.r.config.RetryPassing = true
	reads := 0
	h.r.instrumentation = &varInstr{prInstr: h.r.instrumentation.(*prInstr), bound: func() ([]models.VariableBinding, error) {
		reads++
		if reads == 1 {
			return nil, errors.New("agent gone")
		}
		return nil, nil
	}}

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("set %s (%s); want FAILED, judged: the create's last cycle checked what it captured, and the read-back failed", status, h.report.report.FailureReason)
	}
	if reads < 2 {
		t.Fatalf("%d reads of the agent's variables; the create was not run again", reads)
	}
	if r := h.result(t, "test-1"); r.FailureInfo.VariablesUnchecked != "" {
		t.Fatalf("the create's row says %q; its last cycle checked what it captured", r.FailureInfo.VariablesUnchecked)
	}
}

// Without an agent (a base path) nothing is read and nothing fails.
func TestRunTestSet_WithoutAnAgentNoVariableIsRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, instr := createThenRead(t, "u_live_0002")
	vi := &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, errors.New("no agent") }}
	h.r.instrumentation = vi
	h.r.instrument = false
	h.run(t)
	if vi.reads != 0 {
		t.Fatalf("read the agent's variables %d times without an agent", vi.reads)
	}
	assertStatus(t, h, "test-1", models.TestStatusPassed, "no agent, nothing to check")
}

// A capture is checked against the value a dependency call bound under the
// capture's key: a variable recorded with another value than config.yaml's is
// another variable under the key's name, refused; a value the test case gave
// is never checked against itself; a variable under another name is not the
// key's, whatever value it was recorded with (two counters both at 1 when
// recorded are two values).
func TestRunTestSet_ACaptureIsCheckedOnlyAgainstItsOwnVariable(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name  string
		bound models.VariableBinding
		want  models.TestStatus
	}{
		{"recorded with another value", models.VariableBinding{Name: "id", Value: "u_other_0003", Recorded: "u_rec_9999", Mock: "mock-3", Place: "body.id"}, models.TestStatusPassed},
		{"given by the test case", models.VariableBinding{Name: "id", Value: "u_other_0003", Recorded: "u_rec_0001", Mock: "test case test-1", Place: "request", Given: true}, models.TestStatusPassed},
		{"another name recorded with the key's value", models.VariableBinding{Name: "orderId", Value: "u_other_0003", Recorded: "u_rec_0001", Mock: "mock-3", Place: "body.order_id"}, models.TestStatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, instr := createThenRead(t, "u_live_0002")
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return []models.VariableBinding{tc.bound}, nil }}
			status := h.run(t)
			r := h.result(t, "test-1")
			if r.Status != tc.want || len(r.TemplateFailures) != 0 {
				t.Fatalf("%s: %+v", r.Status, r.TemplateFailures)
			}
			// Recorded under the key's name with another value: the learn step
			// and the templates disagree, keploy's fault, so not judged.
			if wantSet := map[bool]models.TestSetStatus{true: models.TestSetStatusInternalErr, false: models.TestSetStatusPassed}[tc.bound.Recorded != "u_rec_0001"]; status != wantSet {
				t.Fatalf("the set is %s, want %s", status, wantSet)
			}
		})
	}
}

// A capture is compared with its dependency's value as its placeholder types
// it: a string as written, so an app that stored "100" and answered "1e2" for
// a string id fails; a number by its value, so 1.50 is the 1.5 it sent.
func TestRunTestSet_ACaptureIsComparedAsItsPlaceholderTypesIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name, placeholder, answered string
		tmpl                        interface{}
		bound                       models.VariableBinding
		want                        models.TestStatus
	}{
		{"a string that reads as a number", `"{{capture string .id}}"`, `"1e2"`, "u_rec_0001",
			models.VariableBinding{Name: "id", Value: "100", Recorded: "u_rec_0001", Mock: "mock-3", Place: "body.id"}, models.TestStatusFailed},
		{"a float written another way", `"{{capture float .id}}"`, `1.50`, 2.25,
			models.VariableBinding{Name: "id", Value: "1.5", Recorded: "2.250", Mock: "mock-3", Place: "body.price"}, models.TestStatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := map[string]interface{}{"id": tc.tmpl}
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":`+tc.answered+`}`)
				})
			})
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/things", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
					recorded(201, nil, `{"id":`+tc.placeholder+`}`, tmpl)),
			}
			h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
			instr := h.r.instrumentation.(*prInstr)
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return []models.VariableBinding{tc.bound}, nil }}
			status := h.run(t)
			if r := h.result(t, "test-1"); r.Status != tc.want {
				t.Fatalf("%s, want %s (%+v)", r.Status, tc.want, r.TemplateFailures)
			}
			if wantSet := map[models.TestStatus]models.TestSetStatus{models.TestStatusPassed: models.TestSetStatusPassed, models.TestStatusFailed: models.TestSetStatusFailed}[tc.want]; status != wantSet {
				t.Fatalf("the set is %s, want %s", status, wantSet)
			}
		})
	}
}

// A set that already failed is not judged when keploy could not check what a
// test captured: its verdict is not one keploy can vouch for.
func TestRunTestSet_AFailedSetWithUncheckedCapturesIsNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tmpl := map[string]interface{}{"id": "u_rec_0001"}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"u_live_0002"}`)
		})
		mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"n":2}`)
		})
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, tmpl)),
		tmplCase(2, app.addr, "GET", "/other", nil, "", recorded(200, nil, `{"n":1}`, tmpl)),
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
	instr := h.r.instrumentation.(*prInstr)
	h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, errors.New("agent gone") }}
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("a failed set whose captures were not checked is %s, want INTERNAL_ERR", status)
	}
}

// A failed read is said once per set however many of its test cases capture
// nothing, and the set's reason gives the first test's reason.
func TestRunTestSet_AFailedReadIsSaidOncePerSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tmpl := map[string]interface{}{"id": "u_rec_0001"}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusCreated, `{"id":"u_live_0002"}`)
		})
		mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/ping", nil, "", recorded(200, nil, `{"ok":true}`, tmpl)),
		tmplCase(2, app.addr, "GET", "/ping", nil, "", recorded(200, nil, `{"ok":true}`, tmpl)),
		tmplCase(3, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, tmpl)),
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
	instr := h.r.instrumentation.(*prInstr)
	h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, errors.New("agent gone") }}
	core, logs := observer.New(zap.WarnLevel)
	h.r.logger = zap.New(core)
	h.run(t)
	if n := len(logs.FilterMessageSnippet("the report lists none").All()); n != 1 {
		t.Fatalf("the failed read was said %d times for two test cases that capture nothing, want once", n)
	}
	if reason := h.report.report.FailureReason; !strings.Contains(reason, "test-3") || !strings.Contains(reason, "agent gone") {
		t.Fatalf("failure reason %q", reason)
	}
}

// A gRPC test case's request binds no value (keploy test sends the values of
// HTTP test cases only, as the learn step's contract says).
func TestRequestVariables_AGRPCTestCaseGivesNone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	run := templates.NewRun(nil, map[string]interface{}{"id": "u_rec"}, nil)
	tc := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "test-1", HTTPReq: models.HTTPReq{URL: "http://h/users/{{string .id}}"}}
	if got := requestVariables(run, tc); len(got) != 0 {
		t.Fatalf("a gRPC test case gave %+v", got)
	}
	tc.Kind = models.HTTP
	if got := requestVariables(run, tc); len(got) != 1 || !got[0].Given {
		t.Fatalf("an HTTP test case gave %+v", got)
	}
}

// routeVarInstr is a stand-in agent that keeps its test variables as the
// agent's route does (pkg/agent/routes/replay.go UpdateMockParams): under the
// switch a filter that begins a test's variables (BeginsTestVariables)
// starts the table with the values it gives; exact serving's pool call
// begins none.
type routeVarInstr struct {
	*prInstr
	tbl *variables.Table
}

func (v *routeVarInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	if err := v.prInstr.UpdateMockParams(ctx, params); err != nil {
		return err
	}
	if provenswitch.On() && params.BeginsTestVariables() {
		v.tbl.Start(params.Variables)
	}
	return nil
}

func (v *routeVarInstr) TestVariables(context.Context) ([]models.VariableBinding, error) {
	return v.tbl.Snapshot(), nil
}

// Under exact serving keploy test serves the pool again right after a test
// case's response, before it reads what the test case's dependency calls
// bound: that pool call must not forget them. An app that stores one id in
// its dependency and answers another fails.
func TestRunTestSet_ThePoolCallKeepsWhatTheTestCaseBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tbl := &variables.Table{}
	tmpl := map[string]interface{}{"id": "u_rec_0001"}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			// The app's dependency call, as the agent's matcher binds it.
			tbl.Bind(models.VariableBinding{Name: "id", Value: "u_other_0003", Recorded: "u_rec_0001", Mock: "mock-3", Place: "body.id"})
			writeJSON(w, http.StatusCreated, `{"id":"u_live_0002"}`)
		})
		mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
			s := a.record(r)
			writeJSON(w, http.StatusOK, `{"id":"`+strings.TrimPrefix(s.path, "/users/")+`"}`)
		})
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"a"}`,
			recorded(201, nil, `{"id":"{{capture string .id}}"}`, tmpl)),
		tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "",
			recorded(200, nil, `{"id":"{{string .id}}"}`, tmpl)),
	}
	h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, cases, nil)
	h.r.instrumentation = &routeVarInstr{prInstr: h.r.instrumentation.(*prInstr), tbl: tbl}
	h.run(t)
	r := h.result(t, "test-1")
	if r.Status != models.TestStatusFailed {
		t.Fatalf("the app answered u_live_0002 but sent u_other_0003 to its dependency; the create is %s (%+v)", r.Status, r.Variables)
	}
	if len(r.TemplateFailures) != 1 || !strings.Contains(r.TemplateFailures[0].Why, `sent "u_other_0003" as id to its dependency (mock-3 body.id)`) {
		t.Fatalf("the failure does not name the dependency's value: %+v", r.TemplateFailures)
	}
}

// In record's full round (the learn step's round, under the proven switch)
// the filter that opens each test case carries the test case's own request,
// as recorded and as sent with this run's template values, part by part: the
// learn step's echo reads it. Outside the full round, and with the switch
// off, no filter carries it.
func TestRunTestSet_TheFullRoundSendsEachTestCasesRequest(t *testing.T) {
	for _, tc := range []struct {
		proven string
		learn  bool
	}{{"1", true}, {"1", false}, {"0", true}} {
		t.Run("KEPLOY_PROVEN="+tc.proven+",learn="+strconv.FormatBool(tc.learn), func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			h, instr := createThenRead(t, "u_live_0002")
			h.r.config.Test.Learn = tc.learn
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, nil }}
			h.run(t)
			var sent []*models.TestRequest
			instr.mu.Lock()
			for _, p := range instr.allParams {
				perTest := !p.AfterTime.Equal(models.BaseTime)
				if p.Exact != nil {
					perTest = p.Exact.Test != ""
				}
				if perTest {
					sent = append(sent, p.TestRequest)
				} else if p.TestRequest != nil {
					t.Errorf("a filter that opens no test case carries a request: %+v", p)
				}
			}
			instr.mu.Unlock()
			if len(sent) != 2 {
				t.Fatalf("%d per-test filters, want 2", len(sent))
			}
			if tc.proven != "1" || !tc.learn {
				if sent[0] != nil || sent[1] != nil {
					t.Fatalf("outside the full round: sent %+v, %+v", sent[0], sent[1])
				}
				return
			}
			parts := func(r *models.TestRequest) map[string]models.TestRequestPart {
				out := map[string]models.TestRequestPart{}
				for _, p := range r.Parts {
					out[p.Name] = p
				}
				return out
			}
			create, read := sent[0], sent[1]
			if create == nil || create.Test != "test-1" || create.Unread != "" || parts(create)["body"].Recorded != `{"name":"a"}` || parts(create)["body"].Sent != `{"name":"a"}` || parts(create)["header.Content-Type"].Sent != "application/json" {
				t.Fatalf("the create's request %+v", create)
			}
			if create.Parts[0].Name != "url" {
				t.Errorf("the create's parts begin %q; want the url first", create.Parts[0].Name)
			}
			u := parts(read)["url"]
			if read == nil || read.Test != "test-2" || !strings.HasSuffix(u.Recorded, "/users/u_rec_0001") || !strings.HasSuffix(u.Sent, "/users/u_live_0002") {
				t.Fatalf("the read-back's request %+v; want its URL as recorded, with the recorded id, and as sent with this run's id", read)
			}
		})
	}
}

// A test case's request is read part by part in a stable order (url,
// headers, body, binary, form parts in the order recorded), a gRPC test
// case's headers and every message it sends too; a body, a form value or a
// file kept apart from the test case is read back from its asset; a part
// keploy cannot read, or a request too large, says why, with no parts.
func TestTestRequestOf(t *testing.T) {
	run := templates.NewRun(nil, map[string]interface{}{"id": "u_rec"}, nil)
	tc := &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPReq: models.HTTPReq{URL: "http://h/u/{{string .id}}",
		Header: map[string]string{"B": "2", "A": "1"}, Body: `{"at":"2026-10-22T11:14:00Z"}`,
		Form: []models.FormData{{Key: "slot", Values: []string{"11:14", "11:15"}}}}}
	got := testRequestOf(run, tc, "")
	var names []string
	for _, p := range got.Parts {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "url,header.A,header.B,body,form.slot" || got.Parts[0].Sent != "http://h/u/u_rec" || got.Parts[4].Recorded != "11:14\n11:15" || got.Unread != "" {
		t.Fatalf("the HTTP request %+v", got)
	}
	// As recorded, each placeholder holds the value it was recorded with,
	// never its text: a template value in the URL, the body, a header and a
	// form field, and a secret in a header's name, which the recording held
	// as the request as sent does.
	run2 := templates.NewRun(nil, map[string]interface{}{"id": "u_rec", "slot": "11:14"}, map[string]interface{}{"tenant": "acme7"})
	run2.Apply([]templates.Capture{{Key: "id", Value: "u_live"}})
	keyed := &models.TestCase{Kind: models.HTTP, Name: "test-6", HTTPReq: models.HTTPReq{URL: "http://h/u/{{string .id}}",
		Header: map[string]string{"X-Tenant-{{string .secret.tenant}}": "{{string .id}}"}, Body: `{"slot":"{{string .slot}}","id":"{{string .id}}"}`,
		Form: []models.FormData{{Key: "who", Values: []string{"{{string .id}}"}}}}}
	got = testRequestOf(run2, keyed, "")
	names = nil
	for _, p := range got.Parts {
		names = append(names, p.Name+"="+p.Recorded+">"+p.Sent)
	}
	if want := `url=http://h/u/u_rec>http://h/u/u_live,header.X-Tenant-acme7=u_rec>u_live,body={"slot":"11:14","id":"u_rec"}>{"slot":"11:14","id":"u_live"},form.who=u_rec>u_live`; strings.Join(names, ",") != want || got.Unread != "" {
		t.Fatalf("a templated request:\n%v\nwant\n%s", strings.Join(names, ","), want)
	}
	g2 := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "test-7", GrpcReq: models.GrpcReq{
		Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-id": "{{string .id}}"}}, Body: models.GrpcLengthPrefixedMessage{DecodedData: `id: "{{string .id}}"`}}}
	got = testRequestOf(run2, g2, "")
	names = nil
	for _, p := range got.Parts {
		names = append(names, p.Name+"="+p.Recorded+">"+p.Sent)
	}
	if want := `header.x-id=u_rec>u_live,body=id: "u_rec">id: "u_live"`; strings.Join(names, ",") != want {
		t.Fatalf("a templated gRPC request:\n%v\nwant\n%s", strings.Join(names, ","), want)
	}
	// A multipart form records one part per value: each is a part of its
	// own, a repeated key's numbered; a value and a file kept apart, and a
	// body kept apart, are read back from their assets.
	dir := t.TempDir()
	write := func(name, text string) string {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	multi := &models.TestCase{Kind: models.HTTP, Name: "test-5", HTTPReq: models.HTTPReq{URL: "http://h/book",
		BodyRef: models.BodyRef{Path: write("body.bin", "slots=11:13"), Size: 11},
		Form: []models.FormData{
			{Key: "slot", Values: []string{"11:14"}},
			{Key: "slot", Values: []string{"11:15"}},
			{Key: "note", Values: []string{""}, Paths: []string{write("note.txt", "after 11:16")}},
			{Key: "timetable", FileNames: []string{"t.csv"}, Paths: []string{write("t.csv", "11:17\n11:18")}},
		}}}
	got = testRequestOf(run, multi, dir)
	parts := map[string]string{}
	names = nil
	for _, p := range got.Parts {
		names = append(names, p.Name)
		parts[p.Name] = p.Recorded + "|" + p.Sent
	}
	if got.Unread != "" || strings.Join(names, ",") != "url,body,form.slot,form.slot#2,form.note,form.timetable" ||
		parts["body"] != "slots=11:13|slots=11:13" || parts["form.slot"] != "11:14|11:14" || parts["form.slot#2"] != "11:15|11:15" ||
		parts["form.note"] != "after 11:16|after 11:16" || parts["form.timetable"] != "11:17\n11:18|11:17\n11:18" {
		t.Fatalf("the multipart request %+v", got)
	}
	g := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "test-2", GrpcReq: models.GrpcReq{
		Headers:            models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc/Book"}, OrdinaryHeaders: map[string]string{"x-day": "2026-10-22"}},
		Body:               models.GrpcLengthPrefixedMessage{DecodedData: `slot: "11:14"`},
		AdditionalMessages: []models.GrpcLengthPrefixedMessage{{DecodedData: `slot: "11:15"`}}}}
	got = testRequestOf(run, g, "")
	names = nil
	for _, p := range got.Parts {
		names = append(names, p.Name+"="+p.Recorded)
	}
	if strings.Join(names, ",") != `header.:path=/svc/Book,header.x-day=2026-10-22,body=slot: "11:14",body#2=slot: "11:15"` || got.Unread != "" {
		t.Fatalf("the gRPC request %+v", got)
	}
	if got := testRequestOf(run, &models.TestCase{Kind: models.Kind("Redis"), Name: "test-3"}, ""); got.Unread == "" || len(got.Parts) != 0 {
		t.Fatalf("a request of another kind %+v; want it unread", got)
	}
	gone := &models.TestCase{Kind: models.HTTP, Name: "test-4", HTTPReq: models.HTTPReq{URL: "http://h/x", BodyRef: models.BodyRef{Path: "assets/test-4.body", Size: 2 << 20}}}
	if got := testRequestOf(run, gone, dir); got.Unread == "" || len(got.Parts) != 0 || !strings.Contains(got.Unread, "assets/test-4.body") {
		t.Fatalf("a body kept apart that cannot be read %+v; want it unread", got)
	}
	file := &models.TestCase{Kind: models.HTTP, Name: "test-6", HTTPReq: models.HTTPReq{URL: "http://h/x",
		Form: []models.FormData{{Key: "f", FileNames: []string{"a"}, Paths: []string{"missing.csv"}}}}}
	if got := testRequestOf(run, file, dir); got.Unread == "" || len(got.Parts) != 0 || !strings.Contains(got.Unread, "form.f") {
		t.Fatalf("a file that cannot be read %+v; want it unread", got)
	}
	big := &models.TestCase{Kind: models.HTTP, Name: "test-7", HTTPReq: models.HTTPReq{URL: "http://h/x", Body: strings.Repeat("x", maxTestRequest/2+1)}}
	if got := testRequestOf(run, big, ""); got.Unread == "" || len(got.Parts) != 0 {
		t.Fatalf("a request too large: %d parts, %q; want it unread", len(got.Parts), got.Unread)
	}
}

// A test case's opening sent without exact serving (the set's windows serve
// it) carries its request as exact serving's does: the filter that opens the
// test case hands it to the agent's learn step with the values it gives.
func TestSendMockFilterParamsCarriesTheTestCasesOpening(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	req := &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "/a", Sent: "/a"}}}
	vars := []models.VariableBinding{{Name: "id", Value: "u_1"}}
	if err := h.replayer.SendMockFilterParamsToAgent(context.Background(), nil, time.Now().Add(-time.Second), time.Now(), nil, false, recordedSetShape{}, &testOpening{vars: vars, request: req}); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if got := h.instr.lastParams; got.TestRequest != req || !reflect.DeepEqual(got.Variables, vars) {
		t.Fatalf("the filter carries %+v and %+v; want the opening's request and values", got.TestRequest, got.Variables)
	}
	// The harness's agent cannot say it read the request: the test case is
	// noted as one whose request it did not read.
	if unread := h.replayer.requestsUnread.set(); !unread["test-1"] {
		t.Fatalf("tests whose request the agent did not read: %v; want test-1", unread)
	}
	h.replayer.requestsUnread.reset()
	h.replayer.instrumentation = &readsInstr{prInstr: h.instr, reads: true}
	if err := h.replayer.SendMockFilterParamsToAgent(context.Background(), nil, time.Now().Add(-time.Second), time.Now(), nil, false, recordedSetShape{}, &testOpening{vars: vars, request: req}); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if unread := h.replayer.requestsUnread.set(); unread != nil {
		t.Fatalf("an agent that read the request: noted %v", unread)
	}
	if err := h.replayer.SendMockFilterParamsToAgent(context.Background(), nil, time.Now().Add(-time.Second), time.Now(), nil, false, recordedSetShape{}, nil); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if got := h.instr.lastParams; got.TestRequest != nil || got.Variables != nil {
		t.Fatalf("a filter with no opening carries %+v and %+v", got.TestRequest, got.Variables)
	}
}
