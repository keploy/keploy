package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSetMocksWithWindowAdvancesEngineAfterFirst(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, nil)
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}

	// getMockManager() returns nil for this bare Proxy, so the MockManager
	// path is skipped; only the async advance runs.
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 0 {
		t.Fatalf("after first window completed=%d want 0", got)
	}
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 1 {
		t.Fatalf("after second window completed=%d want 1", got)
	}
}
