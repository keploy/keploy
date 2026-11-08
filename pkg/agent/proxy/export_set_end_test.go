package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// WorkerStoresFileTheSetsEnd says whether a call held on a replay test
// worker's own store is filed at its set's end: in a session whose client
// reads each set's end (the run's manager is told so, as Proxy.Mock tells
// it), a test worker's manager (ServeInstance) takes a parser's AtSetEnd
// registration, and the proxy's read of the set's end (FileAtSetEnd) runs
// it. It is exported for the row in workers_set_end_test.go, which reads
// pkg/service/agent's MaxWorkers, a package that imports this one.
func WorkerStoresFileTheSetsEnd(t *testing.T) bool {
	t.Helper()
	p := &Proxy{logger: zap.NewNop()}
	p.mockManager = setEndManager(t)
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	wm := p.instanceManager(1)
	t.Cleanup(wm.Close)
	ran := false
	cancel, ok := wm.AtSetEnd(models.KAFKA, func(context.Context) { ran = true })
	if !ok {
		return false
	}
	defer cancel()
	if err := p.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ran
}
