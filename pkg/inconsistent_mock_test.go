package pkg

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A mock whose response is stamped before its request is left out of the
// replay, and the run says so at the default level, naming the mock: a call it
// recorded gets another mock or none, and a silent drop leaves the user a miss
// or a wrong answer with nothing to point at it. The filter runs again at every
// test of a set and sees the same mock each time, so it is said once.
func TestAMockAnsweredBeforeItWasAskedIsLeftOutLoudlyOnce(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	now := time.Now()
	inverted := &models.Mock{
		Name: "inverted-answered-before-asked", Kind: models.PostgresV3, Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now.Add(-37 * time.Microsecond)},
	}
	valid := &models.Mock{
		Name: "valid-answered-after-asked", Kind: models.PostgresV3, Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now.Add(time.Millisecond)},
	}
	for test := 0; test < 2; test++ {
		got := FilterTcsMocks(context.Background(), logger, []*models.Mock{inverted, valid}, now.Add(-time.Hour), now.Add(time.Hour), false)
		if len(got) != 1 || got[0].Name != valid.Name {
			t.Fatalf("test %d is served %v; want only %s", test, got, valid.Name)
		}
	}
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("the run warned %d times; want once, for %s: %v", len(warns), inverted.Name, warns)
	}
	f := warns[0].ContextMap()
	if f["mock"] != inverted.Name || f["kind"] != string(models.PostgresV3) {
		t.Errorf("the warning names mock %v of kind %v; want %s of kind %s", f["mock"], f["kind"], inverted.Name, models.PostgresV3)
	}
	if req, ok := f["request"].(time.Time); !ok || !req.Equal(inverted.Spec.ReqTimestampMock) {
		t.Errorf("the warning says it was asked at %v; want %v", f["request"], inverted.Spec.ReqTimestampMock)
	}
	if res, ok := f["response"].(time.Time); !ok || !res.Equal(inverted.Spec.ResTimestampMock) {
		t.Errorf("the warning says it was answered at %v; want %v", f["response"], inverted.Spec.ResTimestampMock)
	}
	if f["next_step"] == nil {
		t.Error("the warning gives no next step")
	}
}

// Under the proven switch a test owns the mocks its mapping lists whatever
// their recorded times say: a mock whose stamps are in the wrong order (a
// recorder's bug, not a bad answer) is kept for the agent to serve, and
// nothing is said of a mock that is not left out.
func TestUnderTheSwitchAMockAnsweredBeforeItWasAskedIsKept(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	core, logs := observer.New(zapcore.InfoLevel)
	now := time.Now()
	inverted := &models.Mock{
		Name: "inverted-kept-under-the-switch", Kind: models.PostgresV3, Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now.Add(-37 * time.Microsecond)},
	}
	got := FilterTcsMocks(context.Background(), zap.New(core), []*models.Mock{inverted}, now.Add(-time.Hour), now.Add(time.Hour), false)
	if len(got) != 1 || got[0].Name != inverted.Name {
		t.Fatalf("under the switch the test is served %v; want %s", got, inverted.Name)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Errorf("under the switch the run warned %d times of a mock it keeps", n)
	}
}

// The mocks said of are bounded, so a long-lived agent replaying set after
// set does not hold one entry for every such mock it ever saw.
func TestTheMocksSaidOfAreBounded(t *testing.T) {
	now := time.Now()
	for i := 0; i < maxAnsweredBeforeAsked+10; i++ {
		at := now.Add(time.Duration(i) * time.Microsecond)
		WarnAnsweredBeforeAsked(zap.NewNop(), &models.Mock{Name: "bounded", Kind: models.HTTP,
			Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(-time.Nanosecond)}})
	}
	answeredBeforeAskedMu.Lock()
	n := len(answeredBeforeAsked)
	answeredBeforeAskedMu.Unlock()
	if n > maxAnsweredBeforeAsked {
		t.Errorf("%d mocks are held; want at most %d", n, maxAnsweredBeforeAsked)
	}
}
