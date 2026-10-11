package replay

import (
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
// is demoted to OBSOLETE today, and its test set stays PASSED. Under the switch
// strict failure is on: the test is FAILED and so is its set. Both replay
// phases make that decision, so both are driven.
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

				status := h.run(t)

				if len(h.report.report.Tests) != 1 {
					t.Fatalf("report has %d results, want 1", len(h.report.report.Tests))
				}
				got := h.report.report.Tests[0].Status
				wantTest, wantSet := models.TestStatusObsolete, models.TestSetStatusPassed
				if on {
					wantTest, wantSet = models.TestStatusFailed, models.TestSetStatusFailed
				}
				if got != wantTest || status != wantSet {
					t.Fatalf("test %s, set %s; want test %s, set %s", got, status, wantTest, wantSet)
				}
			})
		}
	}
}
