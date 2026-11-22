package replay

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestStrictFailureIsAlwaysOnUnderTheProvenSwitch(t *testing.T) {
	r := &Replayer{config: &config.Config{}}
	t.Setenv(provenswitch.Env, "")
	if r.strictFailure() {
		t.Fatal("switch off and test.strictFailure unset: the OBSOLETE demotion must stay as it is")
	}
	r.config.Test.StrictFailure = true
	if !r.strictFailure() {
		t.Fatal("test.strictFailure must still turn the veto on")
	}
	r.config.Test.StrictFailure = false
	t.Setenv(provenswitch.Env, "1")
	if !r.strictFailure() {
		t.Fatal("KEPLOY_PROVEN=1: strict failure must be on for everyone, whatever keploy.yml says")
	}
}

// The additive auto-pass passes a response that only gained a field. The proven
// model asserts every response field, so under the switch it fails like any
// other difference.
func TestAdditiveAutoPassIsOffUnderTheProvenSwitch(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	t.Setenv(provenswitch.Env, "1")
	if r.autoPassHTTPResponseSchemaAddition(&models.TestCase{Name: "t"}, &models.HTTPResp{}, "set", nil, additiveResult()) {
		t.Fatal("KEPLOY_PROVEN=1: a response that gained a field was auto-passed")
	}
}

// Through the path real replays take (emitFailureLogs=true calls the helper
// twice), the verdict is FAILED and the reason is logged once, not twice.
func TestCompareHTTPRespForReplay_ProvenSwitchFailsAnAdditiveChange(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{logger: zap.New(core), config: &config.Config{}}

	t.Setenv(provenswitch.Env, "")
	if pass, _ := r.compareHTTPRespForReplay(additiveTestCase(), additiveActual(), "set", true); !pass {
		t.Fatal("control: with the switch off an additive change still auto-passes")
	}

	t.Setenv(provenswitch.Env, "1")
	pass, result := r.compareHTTPRespForReplay(additiveTestCase(), additiveActual(), "set", true)
	if pass {
		t.Fatal("KEPLOY_PROVEN=1: an additive response-schema change passed")
	}
	if !qualifiesForHTTPResponseSchemaAdditionPass(result) {
		t.Fatalf("fixture must be an additive Low-risk diff, else this proves nothing: %+v", result.FailureInfo)
	}
	const msg = "not auto-passing an additive response-schema change: every response field is asserted (KEPLOY_PROVEN=1)"
	if got := logs.FilterMessage(msg).Len(); got != 1 {
		t.Fatalf("the refusal must be logged exactly once per test case, got %d", got)
	}
}

// A test whose response failed while its mock set diverged from the recording
// is demoted to OBSOLETE with the switch off, and its test set is not judged
// (INTERNAL_ERR): a wrong answer is never a pass. Under the switch strict
// failure is on: the test is FAILED and so is its set. Both replay phases make
// that decision, so both are driven.
func TestProvenSwitchVetoesTheObsoleteDemotion(t *testing.T) {
	for _, phase := range []string{"main loop", "streaming pass"} {
		for _, on := range []bool{false, true} {
			name := phase + "/switch off"
			if on {
				name = phase + "/switch on"
			}
			t.Run(name, func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				h := newPartialRunHarness(t, 1, 0)
				h.mappings.onDisk = map[string][]models.MockEntry{"test-1": {{Name: "mock-recorded", Kind: string(models.HTTP)}}}
				hooks := prHooks{wrongBody: map[string]bool{"test-1": true}}
				if phase == "streaming pass" {
					h.streaming("test-1")
					// The streaming pass calls a mock set diverged when the test
					// consumed a per-test mock its mapping does not list.
					hooks.consumed = []models.MockState{{Name: "mock-other", Kind: models.HTTP}}
				}
				h.replayer.hookImpl = hooks
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)

				status := h.run(t)

				if len(h.report.report.Tests) != 1 {
					t.Fatalf("report has %d results, want 1", len(h.report.report.Tests))
				}
				got := h.report.report.Tests[0].Status
				wantTest, wantSet := models.TestStatusObsolete, models.TestSetStatusInternalErr
				if on {
					wantTest, wantSet = models.TestStatusFailed, models.TestSetStatusFailed
				}
				if got != wantTest || status != wantSet {
					t.Fatalf("test %s, set %s; want test %s, set %s", got, status, wantTest, wantSet)
				}
				if !on {
					return
				}
				// What is said is the verdict, and the switch is named as
				// what keeps the test FAILED, not a flag never passed. (The
				// streamed test here also left its recorded call unconsumed,
				// which O-2 names as the reason; either way it is the switch.)
				said := logs.FilterMessageSnippet("mock mapping mismatch detected").All()
				if len(said) != 1 || !strings.Contains(said[0].Message, "as FAILED") || !strings.Contains(fmt.Sprint(said[0].ContextMap()["reason"]), provenswitch.Env+"=1") || strings.Contains(fmt.Sprint(said[0].ContextMap()["reason"]), "--strict-failure") {
					t.Errorf("the mismatch was said %+v; want once, as FAILED, with %s=1 as the reason", said, provenswitch.Env)
				}
			})
		}
	}
}

// Under the switch the response comparison reads arrays in order whatever
// test.ignoreOrdering says, so a capture inside an array takes the element at
// its own index: the set loads it rather than refusing it, as with
// ignoreOrdering off, and the next request sends the captured value. Without
// the switch, ignoreOrdering on refuses it (which element it takes cannot be
// decided).
func TestRunTestSet_ProvenSwitchCapturesInAnArrayAtItsIndex(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"orders":[{"id":"ord_LIVE"}]}`)
				})
				mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"ok":true}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "ord_REC"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/orders", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(201, nil, `{"orders":[{"id":"{{capture string .id}}"}]}`, conf.stored.Template)),
				tmplCase(2, app.addr, "GET", "/orders/{{string .id}}", nil, "", recorded(200, nil, `{"ok":true}`, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.IgnoreOrdering = true })
			core, logs := observer.New(zap.WarnLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			if !on {
				assertRefusedUnsent(t, h, logs, "test-1", "response body.orders[0].id", "{{capture string .id}}", "which element it takes cannot be decided")
				return
			}
			assertStatus(t, h, "test-1", models.TestStatusPassed, "the capture is loaded and takes the element at its index")
			if got := app.sent("GET", "/orders/ord_LIVE"); len(got) != 1 {
				t.Errorf("the next request was sent with the captured value %d time(s), want once", len(got))
			}
		})
	}
}

// With the switch off a test whose answer failed and whose mock set diverged
// is demoted to OBSOLETE, which leaves its set not judged (INTERNAL_ERR),
// unless a call it made got no recorded answer: the miss explains the failure,
// not an out-of-date recording, so the test stays FAILED and its set fails, in
// the main loop and the streaming pass. A DNS miss is not a data-plane call and
// keeps the demotion; so does a test with no miss.
func TestAMissVetoesTheObsoleteDemotionWithTheSwitchOff(t *testing.T) {
	miss := func(protocol, summary string) func() ([]models.UnmatchedCall, error) {
		return func() ([]models.UnmatchedCall, error) {
			return []models.UnmatchedCall{{Protocol: protocol, ActualSummary: summary}}, nil
		}
	}
	for _, phase := range []string{"main loop", "streaming pass"} {
		for _, c := range []struct {
			name    string
			miss    func() ([]models.UnmatchedCall, error)
			want    models.TestStatus
			wantSet models.TestSetStatus
		}{
			{"no miss", nil, models.TestStatusObsolete, models.TestSetStatusInternalErr},
			{"a DNS miss", miss("DNS", "A auth.internal"), models.TestStatusObsolete, models.TestSetStatusInternalErr},
			{"an HTTP miss", miss("HTTP", "POST /oauth/token"), models.TestStatusFailed, models.TestSetStatusFailed},
		} {
			t.Run(phase+"/"+c.name, func(t *testing.T) {
				setSwitch(t, false)
				h := newPartialRunHarness(t, 1, 0)
				h.mappings.onDisk = map[string][]models.MockEntry{"test-1": {{Name: "mock-recorded", Kind: string(models.HTTP)}}}
				hooks := prHooks{wrongBody: map[string]bool{"test-1": true}}
				if phase == "streaming pass" {
					h.streaming("test-1")
					hooks.consumed = []models.MockState{{Name: "mock-other", Kind: models.HTTP}}
				}
				h.replayer.hookImpl = hooks
				if c.miss != nil {
					h.instr.mockErrorsFor = missesByTest(c.miss)
				}
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)

				status := h.run(t)

				got := resultOf(t, h, "test-1")
				if got.Status != c.want || status != c.wantSet {
					t.Fatalf("test %s, set %s; want test %s, set %s", got.Status, status, c.want, c.wantSet)
				}
				if c.miss != nil && len(got.FailureInfo.UnmatchedCalls) != 1 {
					t.Fatalf("unmatched calls %+v; want the one miss attached to the test", got.FailureInfo.UnmatchedCalls)
				}
				if c.want != models.TestStatusFailed {
					return
				}
				said := logs.FilterMessageSnippet("mock mapping mismatch detected").All()
				if len(said) != 1 || !strings.Contains(said[0].Message, "as FAILED") || said[0].ContextMap()["reason"] != vetoUnmatchedCallSwitchOff {
					t.Fatalf("the mismatch was said %+v; want once, as FAILED, for the miss", said)
				}
				if n := len(logs.FilterMessageSnippet("obsolete").All()) - len(logs.FilterMessageSnippet("not obsolete").All()); n != 0 {
					t.Fatalf("the test was also said obsolete (%d lines)", n)
				}
			})
		}
	}
}
