package pkg

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestRetryAgentSetup(t *testing.T) {
	// speed: the helper sleeps 3s between attempts; shrink the test's patience by
	// cancelling only where a case needs it. Cases that succeed within the first
	// two attempts still incur real backoff, so keep failing-attempt counts low.
	t.Run("succeeds first try, no retry", func(t *testing.T) {
		calls := 0
		err := RetryAgentSetup(context.Background(), zap.NewNop(), func(_ context.Context, _ int) error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v, want 1 call and nil", calls, err)
		}
	})

	t.Run("retries only ErrAgentNotReady, then succeeds", func(t *testing.T) {
		calls := 0
		err := RetryAgentSetup(context.Background(), zap.NewNop(), func(_ context.Context, _ int) error {
			calls++
			if calls == 1 {
				return fmt.Errorf("wrap: %w", ErrAgentNotReady)
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("calls=%d err=%v, want 2 calls and nil", calls, err)
		}
	})

	t.Run("does NOT retry a deterministic error", func(t *testing.T) {
		deterministic := errors.New("missing kernel privileges")
		calls := 0
		err := RetryAgentSetup(context.Background(), zap.NewNop(), func(_ context.Context, _ int) error {
			calls++
			return deterministic
		})
		if calls != 1 || !errors.Is(err, deterministic) {
			t.Fatalf("calls=%d err=%v, want 1 call and the original error", calls, err)
		}
	})

	t.Run("gives up after all attempts, returns the sentinel", func(t *testing.T) {
		calls := 0
		err := RetryAgentSetup(context.Background(), zap.NewNop(), func(_ context.Context, _ int) error {
			calls++
			return ErrAgentNotReady
		})
		if calls != agentSetupAttempts || !errors.Is(err, ErrAgentNotReady) {
			t.Fatalf("calls=%d err=%v, want %d calls and the sentinel", calls, err, agentSetupAttempts)
		}
	})

	t.Run("stops immediately on context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		err := RetryAgentSetup(ctx, zap.NewNop(), func(_ context.Context, _ int) error {
			calls++
			return ErrAgentNotReady
		})
		// first attempt returns the sentinel, then the loop hits the backoff select
		// and observes cancellation -> returns ctx.Err(); at most 1 call.
		if calls != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("calls=%d err=%v, want 1 call and context.Canceled", calls, err)
		}
	})
}
