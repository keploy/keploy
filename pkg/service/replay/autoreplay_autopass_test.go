package replay

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// additiveResult is the shape the auto-pass exists for: only new response
// fields, graded Low by AssessJSON.
func additiveResult() *models.Result {
	return &models.Result{
		FailureInfo: models.FailureInfo{
			Category:   []models.FailureCategory{models.SchemaAdded},
			Risk:       models.Low,
			Assessment: &models.FailureAssessment{Risk: models.Low},
		},
	}
}

// On a NEWER build a backward-compatible addition is a real API evolution and
// is waved through — today's behaviour, unchanged.
func TestAdditiveAutoPassStillAppliesWhenBuildsMayDiffer(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	if !qualifiesForHTTPResponseSchemaAdditionPass(additiveResult()) {
		t.Fatal("fixture must qualify for the additive pass, else this test proves nothing")
	}
	if got := r.autoPassHTTPResponseSchemaAddition(&models.TestCase{Name: "t"}, &models.HTTPResp{}, "set", nil, additiveResult()); !got {
		t.Fatal("a newer build that added a response field must still auto-pass")
	}
}

// Auto-replay replays the binary it just recorded, so the app CANNOT have
// gained a response field. An addition is nondeterminism and must reach the
// caller as a failure to be graded, not be auto-passed.
func TestAdditiveAutoPassDisabledDuringAutoReplay(t *testing.T) {
	cfg := &config.Config{}
	cfg.Test.AutoReplay = true
	r := &Replayer{logger: zap.NewNop(), config: cfg}
	if got := r.autoPassHTTPResponseSchemaAddition(&models.TestCase{Name: "t"}, &models.HTTPResp{}, "set", nil, additiveResult()); got {
		t.Fatal("auto-replay must NOT auto-pass an additive schema change — it replays the binary it just recorded")
	}
}

// additiveTestCase / additiveActual are a genuinely additive HTTP diff: same
// status, same existing field, one NEW response field. Driven through the real
// matcher so the Result the gate reads is produced rather than hand-built.
func additiveTestCase() *models.TestCase {
	return &models.TestCase{
		Name: "additive-tc",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"id":1}`,
		},
	}
}

func additiveActual() *models.HTTPResp {
	return &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"id":1,"newField":"x"}`,
	}
}

// compareHTTPRespForReplay calls the auto-pass helper TWICE when
// emitFailureLogs is true — once on a quiet pre-match, once on the real one.
// Under auto-replay the pre-match call can never succeed, so it is skipped
// outright; this pins that the skip is logged once per test case, not twice.
// Both directions of the verdict are asserted at this level too, because the
// helper-level tests cannot see the emitFailureLogs=true path at all.
func TestCompareHTTPRespForReplay_AutoReplayLogsSkipOnce(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)

	cfg := &config.Config{}
	cfg.Test.AutoReplay = true
	r := &Replayer{logger: zap.New(core), config: cfg}

	pass, result := r.compareHTTPRespForReplay(additiveTestCase(), additiveActual(), "set", true)
	if pass {
		t.Fatal("auto-replay must not auto-pass an additive response-schema change")
	}
	if !qualifiesForHTTPResponseSchemaAdditionPass(result) {
		t.Fatalf("fixture must be an additive Low-risk diff, else this test proves nothing: risk=%v categories=%v", result.FailureInfo.Risk, result.FailureInfo.Category)
	}

	const msg = "skipping additive response-schema auto-pass during auto-replay"
	if got := logs.FilterMessage(msg).Len(); got != 1 {
		t.Fatalf("skip must be logged exactly once per test case, got %d", got)
	}
}

// The same path with AutoReplay off still auto-passes — the emitFailureLogs=true
// branch is the one real replays take, and this change must not touch it.
func TestCompareHTTPRespForReplay_AdditivePassesWhenNotAutoReplay(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}

	pass, _ := r.compareHTTPRespForReplay(additiveTestCase(), additiveActual(), "set", true)
	if !pass {
		t.Fatal("a non-auto-replay run must still auto-pass a purely additive response-schema change")
	}
}
