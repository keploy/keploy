package http

import (
	"context"
	"slices"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// chainCall is a call made on a connection whose once chain is in progress
// (integrations.OnceChainOnConn): next is the recording the chain names
// next, series the series it is of, call the call's method and path shape,
// for the line that says the chain answered it.
type chainCall struct {
	next, series, call string
}

// chainNext answers a call with c.next, the recording a once chain the
// call's connection began names next (integrations.OnceChainOnConn), when
// the call is that very recording (strictDiff) and the call's view still
// shows it unanswered: ahead of every other recording of the call, the open
// test's own included, and before the own-call check
// (integrations.OwnRepeatRead), which takes a call the open test recorded
// for one more than it recorded and hides the background from it. The
// chain is the calls the application made just before a once-per-process
// write on that connection, in order (a declared rule's chain), so the
// call is the chain's: a credential's last-use touch reads the credential
// again before it writes, and that read answered with the open test's own
// later recording (the credential as the touch left it) makes the write
// carry another version than its recording does.
//
// It says so in a line: the series, the recording, the call, and whether
// the open test recorded the call too (owned, asked once answered). nil
// when it is not answered so: the call goes on to the matchers as any call
// does, with the open test's own first. A recording that declares variables
// is left to the variable pass, which binds them. db is the call's held
// store (integrations.HoldToRound), n the strict pass's noise.
func (h *HTTP) chainNext(ctx context.Context, input *req, db integrations.MockMemDb, c chainCall, n strictNoise, owned func() bool) (*models.Mock, error) {
	// A read that fails answers nothing here: the matchers read again.
	perTest, _, pool, _ := strictPool(db)
	i := slices.IndexFunc(pool, func(m *models.Mock) bool { return m.Name == c.next })
	if i < 0 {
		return nil, nil
	}
	m := pool[i]
	if hasVariables(m) || len(strictDiff(ctx, input, liveDestination(ctx, input), m, n)) != 0 {
		return nil, nil
	}
	// Consumed as the strict pass consumes what it picked (strictIndexed):
	// the store tells whose the recording is.
	served, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, openTestOwner(db, perTest)), m, db, nil, nil)
	if !claimed {
		return nil, err
	}
	h.Logger.Info("a once series' chain answered a call: this connection made the series' earlier calls, so the series' next recording answered it ahead of any other recording of the call, the open test's own included",
		zap.String("series", c.series), zap.String("mock", m.Name), zap.String("call", c.call), zap.Bool("the open test recorded this call too", owned()))
	return served, nil
}
