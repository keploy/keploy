package replay

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
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
				if !p.AfterTime.Equal(models.BaseTime) {
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
			if len(sent[1]) != 1 || sent[1][0] != want {
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
