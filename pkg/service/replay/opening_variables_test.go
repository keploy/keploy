package replay

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// openingProviderHooks wraps TestHooks and implements both TestCaseMutator and
// OpeningVariablesProvider so tests can verify BeforeTestCaseRun runs before
// OpeningVariables and declared bindings flow into FilterParams.Variables at
// both the standard and streaming call sites.
type openingProviderHooks struct {
	TestHooks
	mu           sync.Mutex
	order        []string
	openingCalls int
	provided     map[string][]models.VariableBinding
	makeVars     func(testSetID string, tc *models.TestCase) []models.VariableBinding
	beforeErr    func(testSetID string, tc *models.TestCase) error
}

func (h *openingProviderHooks) BeforeTestCaseRun(_ context.Context, tc *models.TestCase, testSetID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.order = append(h.order, "before:"+tc.Name)
	if h.beforeErr != nil {
		if err := h.beforeErr(testSetID, tc); err != nil {
			return err
		}
	}
	if h.provided == nil {
		h.provided = map[string][]models.VariableBinding{}
	}
	if h.makeVars != nil {
		h.provided[testSetID+"/"+tc.Name] = h.makeVars(testSetID, tc)
	}
	return nil
}

func (h *openingProviderHooks) OpeningVariables(testSetID, testCaseName string) []models.VariableBinding {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.openingCalls++
	h.order = append(h.order, "opening:"+testCaseName)
	return h.provided[testSetID+"/"+testCaseName]
}

func TestOpeningVariablesProviderMergesWithTemplateBindings(t *testing.T) {
	tmplRun := templates.NewRun(zap.NewNop(), map[string]interface{}{"id": "u_rec_1"}, nil)
	tc := &models.TestCase{
		Name: "test-1",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: models.Method("GET"),
			URL:    "http://app.local/users/{{string .id}}",
			Header: map[string]string{"Authorization": "Bearer live.jwt"},
		},
	}

	declaredAuth := models.VariableBinding{
		Name:      "declared:auth",
		Value:     "Bearer live.jwt",
		Recorded:  "Bearer rec.jwt",
		Mock:      "test case test-1",
		Place:     "header.Authorization",
		Given:     true,
		Sensitive: true,
	}
	collidingID := models.VariableBinding{
		Name:     "id",
		Value:    "should-not-override-template",
		Recorded: "u_rec_1",
		Mock:     "declared",
		Place:    "header.X-Id",
		Given:    true,
	}
	dupDeclared := models.VariableBinding{
		Name:      "declared:auth",
		Value:     "Bearer second.jwt",
		Recorded:  "Bearer rec.jwt",
		Mock:      "test case test-1",
		Place:     "header.Authorization",
		Given:     true,
		Sensitive: true,
	}

	t.Run("KEPLOY_PROVEN=1_collision_and_provider_dup_both_warn", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		core, logs := observer.New(zap.WarnLevel)
		prov := &openingProviderHooks{
			provided: map[string][]models.VariableBinding{
				"test-set-0/test-1": {collidingID, declaredAuth, dupDeclared},
			},
		}
		r := &Replayer{config: &config.Config{}, logger: zap.New(core), hookImpl: prov}
		open := r.openingOf(tmplRun, "test-set-0", tc)
		want := []models.VariableBinding{
			{Name: "id", Value: "u_rec_1", Recorded: "u_rec_1", Mock: "test case test-1", Place: "request", Given: true},
			declaredAuth,
		}
		if !reflect.DeepEqual(open.vars, want) {
			t.Fatalf("openingOf().vars = %+v, want %+v", open.vars, want)
		}
		tmplWarns := logs.FilterMessageSnippet("collides with template variable").All()
		if len(tmplWarns) != 1 || tmplWarns[0].ContextMap()["variable"] != "id" {
			t.Fatalf("template collision warnings = %+v, want 1 for variable=id", tmplWarns)
		}
		dupWarns := logs.FilterMessageSnippet("duplicate declared opening variable").All()
		if len(dupWarns) != 1 || dupWarns[0].ContextMap()["variable"] != "declared:auth" {
			t.Fatalf("provider duplicate warnings = %+v, want 1 for variable=declared:auth", dupWarns)
		}
	})

	t.Run("KEPLOY_PROVEN=1_no_collision_no_warning", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		core, logs := observer.New(zap.WarnLevel)
		prov := &openingProviderHooks{
			provided: map[string][]models.VariableBinding{
				"test-set-0/test-1": {declaredAuth},
			},
		}
		r := &Replayer{config: &config.Config{}, logger: zap.New(core), hookImpl: prov}
		open := r.openingOf(tmplRun, "test-set-0", tc)
		if len(open.vars) != 2 {
			t.Fatalf("openingOf().vars = %+v, want 2 entries", open.vars)
		}
		if logs.Len() != 0 {
			t.Fatalf("unexpected warnings without collision: %+v", logs.All())
		}
	})

	t.Run("KEPLOY_PROVEN=1_learning_skips_provider", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		prov := &openingProviderHooks{
			provided: map[string][]models.VariableBinding{
				"test-set-0/test-1": {declaredAuth},
			},
		}
		cfg := &config.Config{}
		cfg.Test.Learn = true
		r := &Replayer{config: cfg, logger: zap.NewNop(), hookImpl: prov}
		open := r.openingOf(tmplRun, "test-set-0", tc)
		if prov.openingCalls != 0 {
			t.Fatalf("OpeningVariables called %d times during learning, want 0", prov.openingCalls)
		}
		want := []models.VariableBinding{
			{Name: "id", Value: "u_rec_1", Recorded: "u_rec_1", Mock: "test case test-1", Place: "request", Given: true},
		}
		if !reflect.DeepEqual(open.vars, want) {
			t.Fatalf("learning openingOf().vars = %+v, want template-only %+v", open.vars, want)
		}
		if open.request == nil {
			t.Fatal("learning openingOf().request = nil, want non-nil")
		}
	})

	t.Run("KEPLOY_PROVEN=1_forces_given_and_sensitive", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		prov := &openingProviderHooks{
			provided: map[string][]models.VariableBinding{
				"test-set-0/test-1": {
					{
						Name:      "declared:unmarked",
						Value:     "Bearer secret-live",
						Recorded:  "Bearer secret-rec",
						Mock:      "test case test-1",
						Place:     "header.Authorization",
						Given:     false,
						Sensitive: false,
					},
				},
			},
		}
		r := &Replayer{config: &config.Config{}, logger: zap.NewNop(), hookImpl: prov}
		open := r.openingOf(tmplRun, "test-set-0", tc)
		if len(open.vars) != 2 {
			t.Fatalf("openingOf().vars = %+v, want 2 entries", open.vars)
		}
		got := open.vars[1]
		if !got.Given || !got.Sensitive {
			t.Fatalf("provider binding was not forced Given=true, Sensitive=true: %+v", got)
		}
	})

	t.Run("KEPLOY_PROVEN=0", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		prov := &openingProviderHooks{
			provided: map[string][]models.VariableBinding{
				"test-set-0/test-1": {declaredAuth},
			},
		}
		r := &Replayer{config: &config.Config{}, logger: zap.NewNop(), hookImpl: prov}
		open := r.openingOf(tmplRun, "test-set-0", tc)
		if len(open.vars) != 0 {
			t.Fatalf("switch off: openingOf().vars = %+v, want empty", open.vars)
		}
	})
}

func TestRunTestSet_OpeningVariablesProviderBothCallSites(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)

			tmpl := map[string]interface{}{"id": "u_rec_0001"}
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"u_rec_0001"}`)
				})
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header()["Date"] = nil
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("data: ok\n\n"))
				})
			})

			normalTC := tmplCase(1, app.addr, "GET", "/users/{{string .id}}", nil, "",
				recorded(200, nil, `{"id":"{{string .id}}"}`, tmpl))
			streamTC := tmplCase(2, app.addr, "GET", "/events/{{string .id}}", nil, "",
				models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "text/event-stream"},
					Body:       "data: ok\n\n",
				})

			h := newTmplHarness(t, &tmplConf{stored: models.TestSet{Template: tmpl}}, []*models.TestCase{normalTC, streamTC}, nil)
			instr := h.r.instrumentation.(*prInstr)
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, nil }}

			hooks := &openingProviderHooks{
				TestHooks: h.r.hookImpl,
				makeVars: func(_ string, tc *models.TestCase) []models.VariableBinding {
					return []models.VariableBinding{
						{
							Name:     "declared:auth:" + tc.Name,
							Value:    "Bearer live-" + tc.Name,
							Recorded: "Bearer rec-" + tc.Name,
							Mock:     "test case " + tc.Name,
							Place:    "header.Authorization",
							Given:    true,
						},
					}
				},
			}
			h.r.hookImpl = hooks

			h.run(t)

			var sent [][]models.VariableBinding
			instr.mu.Lock()
			for _, p := range instr.allParams {
				perTest := !p.AfterTime.Equal(models.BaseTime)
				if p.Exact != nil {
					perTest = p.Exact.Test != ""
				}
				if perTest {
					sent = append(sent, p.Variables)
				}
			}
			instr.mu.Unlock()

			if len(sent) != 2 {
				t.Fatalf("got %d per-test filter updates, want 2 (normal + streaming)", len(sent))
			}

			if proven == "0" {
				for i, vars := range sent {
					if len(vars) != 0 {
						t.Fatalf("switch off: test %d sent %+v, want empty", i, vars)
					}
				}
				return
			}

			// Verify both call sites merged template + declared bindings (forced Sensitive=true).
			wantNormal := []models.VariableBinding{
				{Name: "id", Value: "u_rec_0001", Recorded: "u_rec_0001", Mock: "test case test-1", Place: "request", Given: true},
				{Name: "declared:auth:test-1", Value: "Bearer live-test-1", Recorded: "Bearer rec-test-1", Mock: "test case test-1", Place: "header.Authorization", Given: true, Sensitive: true},
			}
			wantStream := []models.VariableBinding{
				{Name: "id", Value: "u_rec_0001", Recorded: "u_rec_0001", Mock: "test case test-2", Place: "request", Given: true},
				{Name: "declared:auth:test-2", Value: "Bearer live-test-2", Recorded: "Bearer rec-test-2", Mock: "test case test-2", Place: "header.Authorization", Given: true, Sensitive: true},
			}
			if !reflect.DeepEqual(sent[0], wantNormal) {
				t.Fatalf("normal call site sent %+v, want %+v", sent[0], wantNormal)
			}
			if !reflect.DeepEqual(sent[1], wantStream) {
				t.Fatalf("streaming call site sent %+v, want %+v", sent[1], wantStream)
			}

			// BeforeTestCaseRun must precede OpeningVariables for both test cases.
			wantOrder := []string{"before:test-1", "opening:test-1", "before:test-2", "opening:test-2"}
			if !reflect.DeepEqual(hooks.order, wantOrder) {
				t.Fatalf("hook call order = %v, want %v", hooks.order, wantOrder)
			}
		})
	}
}

func TestRunTestSet_SensitiveOpeningVariablesRedactedInReport(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")

	const liveJWT = "Bearer eyJhbGciOiJIUzI1NiJ9.live-payload.live-sig"
	const recJWT = "Bearer eyJhbGciOiJIUzI1NiJ9.rec-payload.rec-sig"

	for _, echoSensitive := range []bool{true, false} {
		name := "agent_echoes_sensitive"
		if !echoSensitive {
			name = "older_agent_drops_sensitive"
		}
		t.Run(name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			})
			tc := tmplCase(1, app.addr, "GET", "/orders", nil, "",
				recorded(200, nil, `{"ok":true}`, nil))

			h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tc}, nil)
			instr := h.r.instrumentation.(*prInstr)
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) {
				return []models.VariableBinding{
					{
						Name:            "declared:fresh-jwt",
						Value:           liveJWT,
						Recorded:        recJWT,
						Mock:            "test case test-1",
						Place:           "header.Authorization",
						Given:           true,
						Sensitive:       echoSensitive,
						WordRun:         "Bearer-live.jwt.token",
						WordRunRecorded: "Bearer-rec.jwt.token",
					},
					{
						Name:            "orderId",
						Value:           "ord-live-42",
						Recorded:        "ord-rec-1",
						Mock:            "mock-1",
						Place:           "body.id",
						WordRun:         "ord-live-42",
						WordRunRecorded: "ord-rec-1",
					},
				}, nil
			}}
			h.r.hookImpl = &openingProviderHooks{
				TestHooks: h.r.hookImpl,
				makeVars: func(_ string, tc *models.TestCase) []models.VariableBinding {
					return []models.VariableBinding{
						{
							Name:     "declared:fresh-jwt",
							Value:    liveJWT,
							Recorded: recJWT,
							Mock:     "test case " + tc.Name,
							Place:    "header.Authorization",
							// Intentionally leave Given/Sensitive false to verify fail-closed forcing.
						},
					}
				},
			}

			h.run(t)

			// (a) Agent filter params MUST receive the real live & recorded JWT for matching,
			// with Given=true and Sensitive=true forced fail-closed.
			var wireVars []models.VariableBinding
			instr.mu.Lock()
			for _, p := range instr.allParams {
				if p.Exact != nil && p.Exact.Test == "test-1" {
					wireVars = p.Variables
				}
			}
			instr.mu.Unlock()
			if len(wireVars) != 1 || wireVars[0].Value != liveJWT || wireVars[0].Recorded != recJWT || !wireVars[0].Sensitive || !wireVars[0].Given {
				t.Fatalf("wire MockFilterParams.Variables = %+v, want intact live/recorded JWT with Given=true, Sensitive=true", wireVars)
			}

			// (b) TestResult.Variables MUST redact Value and Recorded to "[REDACTED]"
			// and blank WordRun/WordRunRecorded, while keeping non-sensitive bindings intact.
			res := h.result(t, "test-1")
			wantVars := []models.VariableBinding{
				{
					Name:      "declared:fresh-jwt",
					Value:     "[REDACTED]",
					Recorded:  "[REDACTED]",
					Mock:      "test case test-1",
					Place:     "header.Authorization",
					Given:     true,
					Sensitive: true,
				},
				{
					Name:            "orderId",
					Value:           "ord-live-42",
					Recorded:        "ord-rec-1",
					Mock:            "mock-1",
					Place:           "body.id",
					WordRun:         "ord-live-42",
					WordRunRecorded: "ord-rec-1",
				},
			}
			if !reflect.DeepEqual(res.Variables, wantVars) {
				t.Fatalf("TestResult.Variables =\n%+v\nwant:\n%+v", res.Variables, wantVars)
			}
		})
	}
}

type interceptingReportDB struct {
	ReportDB
	resultsInserted int
	reportsInserted int
}

func (d *interceptingReportDB) InsertTestCaseResult(ctx context.Context, testRunID, testSetID string, result *models.TestResult) error {
	d.resultsInserted++
	return d.ReportDB.InsertTestCaseResult(ctx, testRunID, testSetID, result)
}

func (d *interceptingReportDB) InsertReport(ctx context.Context, testRunID, testSetID string, testReport *models.TestReport) error {
	d.reportsInserted++
	return d.ReportDB.InsertReport(ctx, testRunID, testSetID, testReport)
}

func TestReplayer_SetAndGetReportDB(t *testing.T) {
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			writeJSON(w, http.StatusOK, `{"ok":true}`)
		})
	})
	tc := tmplCase(1, app.addr, "GET", "/ping", nil, "", recorded(200, nil, `{"ok":true}`, nil))
	h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tc}, nil)

	orig := h.r.GetReportDB()
	if orig == nil {
		t.Fatal("GetReportDB() = nil, want non-nil")
	}
	// SetReportDB(nil) is a no-op.
	h.r.SetReportDB(nil)
	if h.r.GetReportDB() != orig {
		t.Fatal("SetReportDB(nil) replaced existing reportDB")
	}

	wrapped := &interceptingReportDB{ReportDB: orig}
	h.r.SetReportDB(wrapped)
	if h.r.GetReportDB() != wrapped {
		t.Fatal("GetReportDB() did not return wrapped ReportDB")
	}

	h.run(t)

	if wrapped.resultsInserted < 1 {
		t.Fatalf("wrapped InsertTestCaseResult calls = %d, want >= 1", wrapped.resultsInserted)
	}
	if wrapped.reportsInserted < 1 {
		t.Fatalf("wrapped InsertReport calls = %d, want >= 1", wrapped.reportsInserted)
	}
}

func TestRunTestSet_BeforeTestCaseRunRefusalFailsUnsent(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			var mu sync.Mutex
			hits := map[string]int{}
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/refused-normal", func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					hits["refused-normal"]++
					mu.Unlock()
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
				mux.HandleFunc("/non-refusal-normal", func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					hits["non-refusal-normal"]++
					mu.Unlock()
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
				mux.HandleFunc("/refused-stream", func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					hits["refused-stream"]++
					mu.Unlock()
					w.Header()["Date"] = nil
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("data: ok\n\n"))
				})
				mux.HandleFunc("/ok-stream", func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					hits["ok-stream"]++
					mu.Unlock()
					w.Header()["Date"] = nil
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("data: ok\n\n"))
				})
			})

			refusedNormal := tmplCase(1, app.addr, "GET", "/refused-normal", nil, "", recorded(200, nil, `{"ok":true}`, nil))
			nonRefusalNormal := tmplCase(2, app.addr, "GET", "/non-refusal-normal", nil, "", recorded(200, nil, `{"ok":true}`, nil))
			refusedStream := tmplCase(3, app.addr, "GET", "/refused-stream", nil, "", models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/event-stream"},
				Body:       "data: ok\n\n",
			})
			okStream := tmplCase(4, app.addr, "GET", "/ok-stream", nil, "", models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/event-stream"},
				Body:       "data: ok\n\n",
			})

			h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{refusedNormal, nonRefusalNormal, refusedStream, okStream}, nil)
			instr := h.r.instrumentation.(*prInstr)
			h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) { return nil, nil }}
			h.r.hookImpl = &openingProviderHooks{
				TestHooks: h.r.hookImpl,
				beforeErr: func(_ string, tc *models.TestCase) error {
					switch tc.Name {
					case "test-1":
						return &models.TestCaseRefusal{TestCase: tc.Name, Err: errors.New("REFUSE_FROZEN_TIME_SHIFT: normal")}
					case "test-2":
						return errors.New("transient non-refusal error")
					case "test-3":
						return &models.TestCaseRefusal{TestCase: tc.Name, Err: errors.New("REFUSE_FROZEN_TIME_SHIFT: stream")}
					default:
						return nil
					}
				},
			}

			status := h.run(t)
			if status != models.TestSetStatusFailed {
				t.Fatalf("RunTestSet status = %s, want FAILED", status)
			}

			mu.Lock()
			gotRefusedNormal := hits["refused-normal"]
			gotNonRefusal := hits["non-refusal-normal"]
			gotRefusedStream := hits["refused-stream"]
			gotOKStream := hits["ok-stream"]
			mu.Unlock()

			if gotRefusedNormal != 0 {
				t.Fatalf("refused normal test was sent %d times, want 0", gotRefusedNormal)
			}
			if gotRefusedStream != 0 {
				t.Fatalf("refused streaming test was sent %d times, want 0", gotRefusedStream)
			}
			if gotNonRefusal != 1 {
				t.Fatalf("non-refusal error test hits = %d, want 1 (existing log-and-continue behavior)", gotNonRefusal)
			}
			if gotOKStream != 1 {
				t.Fatalf("subsequent streaming test hits = %d, want 1", gotOKStream)
			}

			for _, name := range []string{"test-1", "test-3"} {
				r := h.result(t, name)
				if r.Status != models.TestStatusFailed {
					t.Fatalf("%s status = %s, want FAILED", name, r.Status)
				}
				if len(r.TemplateFailures) != 1 || !r.TemplateFailures[0].Refused || !strings.Contains(r.TemplateFailures[0].Why, "REFUSE_FROZEN_TIME_SHIFT") {
					t.Fatalf("%s TemplateFailures = %+v, want Refused=true with REFUSE_FROZEN_TIME_SHIFT", name, r.TemplateFailures)
				}
				wantCats := []models.FailureCategory{models.TestCaseRefused}
				if !reflect.DeepEqual(r.FailureInfo.Category, wantCats) || !reflect.DeepEqual(r.Result.FailureInfo.Category, wantCats) {
					t.Fatalf("%s categories = (%v, %v), want %v (not TEST_SET_REFUSED)", name, r.FailureInfo.Category, r.Result.FailureInfo.Category, wantCats)
				}
			}
			if r := h.result(t, "test-2"); r.Status != models.TestStatusPassed {
				t.Fatalf("test-2 (non-refusal) status = %s, want PASSED", r.Status)
			}
			if r := h.result(t, "test-4"); r.Status != models.TestStatusPassed {
				t.Fatalf("test-4 (subsequent stream) status = %s, want PASSED", r.Status)
			}
		})
	}
}
