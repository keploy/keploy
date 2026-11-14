package pkg

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/utils"
)

// A start that failed on a port another process took (utils.ErrPortTaken) is
// started again at once, with no wait: the wait before a stalled start's
// retry is an hour here, and the context ends long before it. The last
// attempt's failure is returned as it is, naming the port.
func TestRetryAgentSetupStartsAgainAtOnceOnATakenPort(t *testing.T) {
	was := agentSetupBackoff
	agentSetupBackoff = time.Hour
	t.Cleanup(func() { agentSetupBackoff = was })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	taken := utils.PortTakenError(errors.New("the keploy agent could not start: the proxy's port 16789 is taken"))

	calls := 0
	err := RetryAgentSetup(ctx, zap.NewNop(), func(context.Context, int) error {
		calls++
		if calls < agentSetupAttempts {
			return taken
		}
		return nil
	})
	if err != nil || calls != agentSetupAttempts {
		t.Fatalf("err=%v calls=%d; want success on attempt %d, each retry at once", err, calls, agentSetupAttempts)
	}

	calls = 0
	err = RetryAgentSetup(ctx, zap.NewNop(), func(context.Context, int) error {
		calls++
		return taken
	})
	if calls != agentSetupAttempts || err != taken {
		t.Fatalf("err=%v calls=%d; want the last attempt's %v after %d", err, calls, taken, agentSetupAttempts)
	}
}
