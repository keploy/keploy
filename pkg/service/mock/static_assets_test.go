package mock

import (
	"context"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type optsCapturingIncoming struct {
	Instrumentation
	gotOpts models.IncomingOptions
}

func (o *optsCapturingIncoming) GetIncoming(_ context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	o.gotOpts = opts
	ch := make(chan *models.TestCase)
	close(ch)
	return ch, nil
}

func TestMockIncomingForwardsIgnoreStaticAssets(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		cfg := config.New()
		cfg.Record.IgnoreStaticAssets = ignore

		t.Run("captureCases", func(t *testing.T) {
			instr := &optsCapturingIncoming{}
			svc := &mockService{
				logger:          zap.NewNop(),
				config:          cfg,
				instrumentation: instr,
			}
			var seen atomic.Int64
			cc := svc.captureCases(context.Background(), context.Background(), "default", &seen, true)
			if cc == nil {
				t.Fatal("captureCases returned nil")
			}
			<-cc.done
			if instr.gotOpts.IgnoreStaticAssets != ignore {
				t.Fatalf("captureCases passed IgnoreStaticAssets=%v, want %v", instr.gotOpts.IgnoreStaticAssets, ignore)
			}
		})

		t.Run("watchIncoming", func(t *testing.T) {
			instr := &optsCapturingIncoming{}
			svc := &mockService{
				logger:          zap.NewNop(),
				config:          cfg,
				instrumentation: instr,
			}
			ac := svc.watchIncoming(context.Background(), true)
			if ac == nil {
				t.Fatal("watchIncoming returned nil")
			}
			<-ac.done
			if instr.gotOpts.IgnoreStaticAssets != ignore {
				t.Fatalf("watchIncoming passed IgnoreStaticAssets=%v, want %v", instr.gotOpts.IgnoreStaticAssets, ignore)
			}
		})
	}
}
