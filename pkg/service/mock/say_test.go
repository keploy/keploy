package mock

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func marked(e observer.LoggedEntry, mark zap.Field) bool {
	for _, f := range e.Context {
		if f.Key == mark.Key {
			return true
		}
	}
	return false
}

func TestAgainReplacesAFlagTheUserAlreadyPassed(t *testing.T) {
	prev := os.Args
	t.Cleanup(func() { os.Args = prev })
	os.Args = []string{"/bin/keploy", "--debug", "mock", "replay", "--on-miss", "fail", "--on-miss=passthrough", "-c", "go test", "--", "x"}
	if got, want := Again("--on-miss", "record"), "keploy --debug mock replay -c 'go test' --on-miss record -- x"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

type lateAgent struct {
	*runnerInstr
	logger *zap.Logger
}

func (a lateAgent) NotifyGracefulShutdown(context.Context) error {
	a.logger.Info("agent fact", log.Show)
	return nil
}

func TestTheSummaryComesAfterWhatTheStoppingAgentSays(t *testing.T) {
	for _, run := range []string{"record", "replay"} {
		t.Run(run, func(t *testing.T) {
			base := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}), mocks: []*models.Mock{mockAt("mock-0", runnerT0)}}
			cfg := instrConfig(base.composeInstr, utils.Native, "go test ./...")
			cfg.Path = t.TempDir()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			svc := New(logger, lateAgent{base, logger}, stubMockDB{}, nil, &countingStore{}, nil, cfg)
			if run == "record" {
				_ = svc.Record(context.Background())
			} else {
				_ = svc.Replay(context.Background())
			}
			var shown []string
			for _, e := range logs.All() {
				if marked(e, log.Show) {
					shown = append(shown, e.Message)
				}
			}
			at := slices.Index(shown, "agent fact")
			if at < 0 || at == len(shown)-1 {
				t.Fatalf("shown lines: %q", shown)
			}
			for _, l := range shown[at+1:] {
				if !strings.HasPrefix(l, "recorded ") && !strings.HasPrefix(l, "the test command exited") && !strings.HasPrefix(l, "outgoing calls:") {
					t.Errorf("%q came after the summary started", l)
				}
			}
		})
	}
}
