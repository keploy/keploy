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

// Under the proven switch keploy's verdict names each miss once, with its
// test, at the end of the run: the agent logs a miss at Debug and hands the
// connection handler a reportedMiss. With the switch off a miss keeps its
// error and Warn lines, and any failure that is not a miss keeps its error
// line under the switch too.
func TestAMissIsQuietUnderTheSwitchOnly(t *testing.T) {
	miss := fmt.Errorf("decode: %w", models.ErrNoMockMatched)
	other := errors.New("parser broke")
	for _, tc := range []struct {
		on       bool
		err      error
		reported bool
		loud     bool
	}{
		{on: true, err: miss, reported: true, loud: false},
		{on: false, err: miss, reported: false, loud: true},
		{on: true, err: other, reported: false, loud: true},
	} {
		t.Run(fmt.Sprintf("switch=%v/%v", tc.on, tc.err), func(t *testing.T) {
			v := "0"
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
			loud := logs.FilterLevelExact(zapcore.ErrorLevel).Len()+logs.FilterLevelExact(zapcore.WarnLevel).Len() > 0
			if loud != tc.loud {
				t.Fatalf("Warn or Error lines logged = %v, want %v: %v", loud, tc.loud, logs.All())
			}
		})
	}
}

// A miss its parser already queued for its test, before answering the app
// (an HTTP 502), comes back marked reported (models.MismatchReported): it is
// logged as any miss is with the switch off, and returned for the connection
// to end, but not queued a second time, where it could count against the next
// test.
func TestAMissItsParserReportedIsNotQueuedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	miss := models.MismatchReported{Err: fmt.Errorf("decode: %w", models.ErrNoMockMatched)}
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
	got := p.mockFailed(logger, miss)
	if !errors.Is(got, models.ErrNoMockMatched) || !models.IsMismatchReported(got) {
		t.Fatalf("returned %v; want the miss, still marked reported", got)
	}
	if n := len(p.errChannel); n != 0 {
		t.Errorf("queued %d errors; want none: the parser queued the miss already", n)
	}
	if logs.FilterMessage("failed to mock the outgoing message").FilterLevelExact(zapcore.ErrorLevel).Len() != 1 {
		t.Errorf("logged %v; want the miss's error line, as with the switch off always", logs.All())
	}
}
