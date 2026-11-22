package pkg

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/utils"
)

// A start that failed on a port another process took (utils.ErrPortTaken) is
// started again at once, with no wait: the wait before a stalled start's
// retry is an hour here, and the context ends long before it. Each retry is
// logged at Warn, the last attempt's failure is returned as it is naming the
// port, and a cancelled context stops after the first attempt without retrying.
func TestRetryAgentSetupStartsAgainAtOnceOnATakenPort(t *testing.T) {
	was := agentSetupBackoff
	agentSetupBackoff = time.Hour
	t.Cleanup(func() { agentSetupBackoff = was })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	taken := utils.PortTakenError(errors.New("the keploy agent could not start: the proxy's port 16789 is taken"))

	core, logs := observer.New(zapcore.WarnLevel)
	calls := 0
	err := RetryAgentSetup(ctx, zap.New(core), func(context.Context, int) error {
		calls++
		if calls < agentSetupAttempts {
			return taken
		}
		return nil
	})
	if err != nil || calls != agentSetupAttempts {
		t.Fatalf("err=%v calls=%d; want success on attempt %d, each retry at once", err, calls, agentSetupAttempts)
	}
	if got := logs.FilterLevelExact(zapcore.WarnLevel).Len(); got != agentSetupAttempts-1 {
		t.Fatalf("logged %d Warn retries; want %d (one per taken-port retry)", got, agentSetupAttempts-1)
	}

	core, logs = observer.New(zapcore.WarnLevel)
	calls = 0
	err = RetryAgentSetup(ctx, zap.New(core), func(context.Context, int) error {
		calls++
		return taken
	})
	if calls != agentSetupAttempts || err != taken {
		t.Fatalf("err=%v calls=%d; want the last attempt's %v after %d", err, calls, taken, agentSetupAttempts)
	}
	if got := logs.FilterLevelExact(zapcore.WarnLevel).Len(); got != agentSetupAttempts-1 {
		t.Fatalf("logged %d Warn retries when giving up; want %d", got, agentSetupAttempts-1)
	}

	// A cancelled run whose Setup failed on a taken port is not retried.
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	core, logs = observer.New(zapcore.WarnLevel)
	calls = 0
	err = RetryAgentSetup(cancelled, zap.New(core), func(context.Context, int) error {
		calls++
		return taken
	})
	if calls != 1 || err != taken {
		t.Fatalf("cancelled run: err=%v calls=%d; want 1 call and %v without retrying", err, calls, taken)
	}
	if got := logs.FilterLevelExact(zapcore.WarnLevel).Len(); got != 0 {
		t.Fatalf("cancelled run logged %d Warn retries; want 0", got)
	}
}
