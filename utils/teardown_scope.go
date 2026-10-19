package utils

import (
	"context"
	"sync/atomic"
)

// TeardownScope counts the movements keploy's CLI sees of one run's
// teardown, which the drains of that run's goroutines wait on while they
// move (DrainErrGroupProgress): a stopping keploy agent's report that took a
// step of its teardown or is busy in one (pkg/platform/http, stopWatch), and
// a progress reset of keploy's stop of the run's application, of the daemons
// it left, and of its compose project's down. Only movement keploy saw ticks
// it, never a timer or a look that found nothing new: a drain bounded by it
// gives a group that has stopped moving up on its bound, and never cuts one
// short that is still moving.
//
// A scope is its run's own (NewTeardownScope, at the run's start): another
// run's teardown in the same process (a long-lived keploy mock, a run started
// again) does not move it, and what one run's teardown did is never counted
// for another's drain. A run's part with drains of its own (a test set) has a
// scope of its own inside the run's, whose movements are the run's too.
type TeardownScope struct {
	moves  atomic.Uint64
	parent *TeardownScope
}

// Note records one movement of the teardown: this scope's, and every scope it
// is part of. Nil-safe: a stop outside any run's scope moves none.
func (s *TeardownScope) Note() {
	for ; s != nil; s = s.parent {
		s.moves.Add(1)
	}
}

// Moves is how many movements of the teardown the scope has seen: what its
// drains wait on while it changes. Nil-safe (0).
func (s *TeardownScope) Moves() uint64 {
	if s == nil {
		return 0
	}
	return s.moves.Load()
}

type teardownScopeKey struct{}

// NewTeardownScope returns ctx carrying a new scope for the run (or the part
// of one) that ctx begins, and the scope. It is part of the scope ctx carried
// already, if any: a test set's scope is part of its run's.
func NewTeardownScope(ctx context.Context) (context.Context, *TeardownScope) {
	s := &TeardownScope{parent: TeardownScopeOf(ctx)}
	return context.WithValue(ctx, teardownScopeKey{}, s), s
}

// TeardownScopeOf is the teardown scope ctx carries (NewTeardownScope), nil
// when it carries none.
func TeardownScopeOf(ctx context.Context) *TeardownScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(teardownScopeKey{}).(*TeardownScope)
	return s
}
