package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type retrySetupInstr struct {
	setupInstr
	calls int
	errs  []error
}

func (s *retrySetupInstr) Setup(_ context.Context, _ string, _ models.SetupOptions) error {
	s.calls++
	if s.calls <= len(s.errs) {
		return s.errs[s.calls-1]
	}
	return nil
}

// setupTestSet routes instrumentation.Setup through pkg.RetryAgentSetup so a
// port taken between the CLI's check and the agent's bind is retried at once
// (logged at Warn), while a deterministic error ends Setup on the first try.
func TestSetupTestSetRetriesAgentSetupOnATakenPort(t *testing.T) {
	t.Run("retries ErrPortTaken and succeeds", func(t *testing.T) {
		taken := utils.PortTakenError(errors.New("the keploy agent could not start: the proxy's port 16789 is taken"))
		instr := &retrySetupInstr{errs: []error{taken, nil}}
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Runner{
			logger:          zap.New(core),
			instrumentation: instr,
			config:          &config.Config{},
			mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
			mockDB:          &skippingMockDB{},
		}
		setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
		if err != nil {
			t.Fatalf("setupTestSet: %v", err)
		}
		defer setup.cleanup()
		if instr.calls != 2 {
			t.Fatalf("Setup called %d time(s); want 2 (retried once after ErrPortTaken)", instr.calls)
		}
		if got := logs.FilterLevelExact(zapcore.WarnLevel).Len(); got != 1 {
			t.Fatalf("logged %d Warn retries; want 1", got)
		}
	})

	t.Run("does not retry a deterministic error", func(t *testing.T) {
		boom := errors.New("missing privileges")
		instr := &retrySetupInstr{errs: []error{boom}}
		r := &Runner{
			logger:          zap.NewNop(),
			instrumentation: instr,
			config:          &config.Config{},
			mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
			mockDB:          &skippingMockDB{},
		}
		_, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
		if !errors.Is(err, boom) {
			t.Fatalf("setupTestSet err=%v; want %v", err, boom)
		}
		if instr.calls != 1 {
			t.Fatalf("Setup called %d time(s); want 1 for a deterministic failure", instr.calls)
		}
	})
}
