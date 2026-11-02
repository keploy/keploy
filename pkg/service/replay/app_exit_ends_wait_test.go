package replay

import (
	"context"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// marksHooks records whether each test request's context says the
// application's exit ends it (pkg.AppExitEnds).
type marksHooks struct {
	prHooks
	mu    sync.Mutex
	marks []bool
}

func (h *marksHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.marks = append(h.marks, pkg.AppExitEnds(ctx))
	h.mu.Unlock()
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

// The test requests of an application keploy started go out on a context its
// exit ends, so that a refused one waits for its address to accept; those of
// an application keploy did not start (no command) do not, so that a refused
// one keeps the bounded re-send: nothing would end its wait.
func TestTestRequestsWaitOnARefusalOnlyForAnAppKeployStarted(t *testing.T) {
	for _, started := range []bool{true, false} {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.instrument = started
		hooks := &marksHooks{}
		h.replayer.hookImpl = hooks
		h.run(t)
		if len(hooks.marks) != 2 {
			t.Fatalf("started=%v: %d test requests sent, want 2", started, len(hooks.marks))
		}
		for i, marked := range hooks.marks {
			if marked != started {
				t.Errorf("started=%v: request %d's context says the app's exit ends it: %v", started, i+1, marked)
			}
		}
	}
}
