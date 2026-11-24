package proxy

import (
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Mock-miss deduplication is logging hygiene in both modes: the agent logs
// "failed to mock the outgoing message" at Debug and hands the connection
// handler a reportedMiss regardless of the proven switch (keeping the single
// structured Warn line for in-window misses when the switch is off, and Debug
// under the switch). Any failure that is not a miss keeps its Error line in
// both modes.
func TestAMissIsQuietUnderTheSwitchOnly(t *testing.T) {
	miss := fmt.Errorf("decode: %w", models.ErrNoMockMatched)
	other := errors.New("parser broke")
	for _, tc := range []struct {
		on       bool
		err      error
		reported bool
		wantErr  int
		wantWarn int
	}{
		{on: true, err: miss, reported: true, wantErr: 0, wantWarn: 0},
		{on: false, err: miss, reported: true, wantErr: 0, wantWarn: 1},
		{on: true, err: other, reported: false, wantErr: 1, wantWarn: 0},
		{on: false, err: other, reported: false, wantErr: 1, wantWarn: 1},
	} {
		t.Run(fmt.Sprintf("switch=%v/%v", tc.on, tc.err), func(t *testing.T) {
			v := ""
			if tc.on {
				v = "1"
			}
			t.Setenv(provenswitch.Env, v)
			core, logs := observer.New(zapcore.DebugLevel)
			logger := zap.New(core)
			p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
			got := p.mockFailed(logger, tc.err)
			if _, ok := got.(reportedMiss); ok != tc.reported {
				t.Fatalf("returned %T, reportedMiss = %v, want %v", got, ok, tc.reported)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("the returned error %v does not wrap %v", got, tc.err)
			}
			if gotErr := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); gotErr != tc.wantErr {
				t.Fatalf("Error lines = %d, want %d: %v", gotErr, tc.wantErr, logs.All())
			}
			if gotWarn := logs.FilterLevelExact(zapcore.WarnLevel).Len(); gotWarn != tc.wantWarn {
				t.Fatalf("Warn lines = %d, want %d: %v", gotWarn, tc.wantWarn, logs.All())
			}
		})
	}
}

// A miss its parser already queued for its test, before answering the app
// (an HTTP 502), comes back marked reported (models.MismatchReported): it is
// logged at Debug and returned as reportedMiss for the connection to end, and
// not queued a second time, where it could count against the next test.
func TestAMissItsParserReportedIsNotQueuedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	miss := models.MismatchReported{Err: fmt.Errorf("decode: %w", models.ErrNoMockMatched)}
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
	got := p.mockFailed(logger, miss)
	if !errors.Is(got, models.ErrNoMockMatched) || !models.IsMismatchReported(got) {
		t.Fatalf("returned %v; want the miss, still marked reported", got)
	}
	if _, ok := got.(reportedMiss); !ok {
		t.Fatalf("returned %T; want reportedMiss", got)
	}
	if n := len(p.errChannel); n != 0 {
		t.Errorf("queued %d errors; want none: the parser queued the miss already", n)
	}
	if logs.FilterMessage("failed to mock the outgoing message").FilterLevelExact(zapcore.DebugLevel).Len() != 1 {
		t.Errorf("logged %v; want the miss's debug line", logs.All())
	}
	if logs.FilterLevelExact(zapcore.ErrorLevel).Len() != 0 {
		t.Errorf("logged %v; want zero error lines for a miss", logs.All())
	}
}
