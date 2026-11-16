package http

import (
	"context"
	"slices"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// chainCall is a call made on a connection whose once chain is in progress
// (integrations.OnceChainOnConn): next is the recording the chain names
// next, series the series it is of, call the call's method and path shape,
// for the line that says the chain answered it.
type chainCall struct {
	// call is the call's shape (shapeSummary), tag its path tag
	// (models.URLPathTag): what a line names it by.
	next, series, call, tag string
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
// does, with the open test's own first, and a line says why the chain did
// not answer it (chainDeclined). A recording that declares variables is
// left to the variable pass, which binds them. db is the call's held store
// (integrations.HoldToRound), n the strict pass's noise.
func (h *HTTP) chainNext(ctx context.Context, input *req, db integrations.MockMemDb, c chainCall, n strictNoise, owned func() bool) (*models.Mock, error) {
	// A read that fails answers nothing here: the matchers read again.
	perTest, _, pool, _ := strictPool(db)
	i := slices.IndexFunc(pool, func(m *models.Mock) bool { return m.Name == c.next })
	if i < 0 {
		h.chainDeclined(c, chainNotHeld, chainNotHeldWhy)
		return nil, nil
	}
	m := pool[i]
	if hasVariables(m) {
		h.chainDeclined(c, chainVariables, chainVariablesWhy)
		return nil, nil
	}
	if diffs := strictDiff(ctx, input, liveDestination(ctx, input), m, n); len(diffs) != 0 {
		h.chainDeclined(c, chainDiffers, chainDiffersWhy,
			zap.Strings("differs", models.MissLogDiffers(&models.MockMismatchReport{FieldDiffs: fieldDiffs(diffs)})))
		return nil, nil
	}
	// Consumed as the strict pass consumes what it picked (strictIndexed):
	// the store tells whose the recording is.
	served, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, openTestOwner(db, perTest)), m, db, nil, nil)
	if !claimed {
		if err == nil {
			h.chainDeclined(c, chainClaimed, "another call claimed the series' next recording first")
		}
		return nil, err
	}
	h.chainAnswered(c, owned())
	return served, nil
}

// chainAnswered says, in a line, that the once chain in progress on a
// call's connection answered the call with c.next, the series' next
// recording, and whether the open test recorded the call too (owned).
func (h *HTTP) chainAnswered(c chainCall, owned bool) {
	h.Logger.Info("a once series' chain answered a call: this connection made the series' earlier calls, so the series' next recording answered it ahead of any other recording of the call, the open test's own included",
		zap.String("series", c.series), zap.String("mock", c.next), zap.String("call", c.call), utils.PathTagField(c.tag), zap.Bool("the open test recorded this call too", owned))
}

// chainEnded says, in a line, that the once chain f found in progress on a
// call's connection (f.Next set) ended at the call: the replay's view moved
// since the connection's last call of the series (OnceFollow.For gave "").
// Nothing is said where no chain was in progress.
func (h *HTTP) chainEnded(f integrations.OnceFollow, c chainCall) {
	if f.Next == "" {
		return
	}
	h.Logger.Info("a once series' chain ended: the replay's view moved since this connection's last call of the series (a test began or ended, or a test set was staged), so the series' next recording does not answer this call ahead of the open test's own; the call is answered as any call is",
		zap.String("series", f.Series), zap.String("next", f.Next), zap.String("call", c.call), utils.PathTagField(c.tag))
}

// learnChain is the once chain in progress on a call's connection as the
// learn step of record's full round reads it (learnMatchIn): what the
// chain names (integrations.OnceFollow), the call's names for the lines
// (chainCall) and the strict pass's noise the chain's next recording is
// compared with (chainNext's). picked is the recording the chain chose on
// the learn step's last pass that read it, "" where it chose none; said,
// that a line said why the chain did not answer the call (once a call).
type learnChain struct {
	follow integrations.OnceFollow
	c      chainCall
	strict strictNoise
	picked string
	said   bool
}

// pick is the learn step's verdict on input, a call marked call, when the
// once chain in progress on its connection, carried on in call's view,
// names a next recording sc offers and the call is that very recording
// (strictDiff, as chainNext compares it in a replay: a field a clock rule
// of keploy.yml declares is checked by that rule): Exact, on that
// recording, whatever else sc offers, the open test's own identical
// recording included. So the learn step answers the chain's calls as a
// replay does, and learns nothing from them. ok false otherwise, with a
// line saying why where a chain was in progress in the call's view
// (chainDeclined, once a call). sc offers the recordings of its view
// (sc.http) with the call's method, destination and shape that declare no
// variables: one it holds that sc does not offer is the variable pass's,
// or another call's, said by where the call differs from it. A nil c (a
// call of no connection's chain: another parser's) picks nothing.
func (c *learnChain) pick(ctx context.Context, h *HTTP, input *req, call integrations.CallRound, sc scope) (learn.Result, bool) {
	if c == nil {
		return learn.Result{}, false
	}
	c.picked = ""
	next := c.follow.For(call)
	if next == "" {
		return learn.Result{}, false
	}
	names := c.c
	names.next = next
	named := func(m *models.Mock) bool { return m != nil && m.Name == next }
	i := slices.IndexFunc(sc.mocks, named)
	if i < 0 {
		switch j := slices.IndexFunc(sc.http, named); {
		case j < 0:
			c.decline(h, names, chainNotHeld, chainNotHeldWhy)
		case hasVariables(sc.http[j]):
			c.decline(h, names, chainVariables, chainVariablesWhy)
		default:
			f, _ := c.differs(ctx, input, sc.http[j])
			c.decline(h, names, chainDiffers, chainDiffersWhy, f)
		}
		return learn.Result{}, false
	}
	if sc.cands[i].Spent {
		c.decline(h, names, chainNotHeld, chainNotHeldWhy)
		return learn.Result{}, false
	}
	if f, differs := c.differs(ctx, input, sc.mocks[i]); differs {
		c.decline(h, names, chainDiffers, chainDiffersWhy, f)
		return learn.Result{}, false
	}
	c.picked = next
	return learn.Result{Outcome: learn.Exact, Index: i}, true
}

// differs is the line's field that names the places input differs from m
// at by the strict comparison (strictDiff), never their values, and
// whether it differs at all.
func (c *learnChain) differs(ctx context.Context, input *req, m *models.Mock) (zap.Field, bool) {
	diffs := strictDiff(ctx, input, liveDestination(ctx, input), m, c.strict)
	return zap.Strings("differs", models.MissLogDiffers(&models.MockMismatchReport{FieldDiffs: fieldDiffs(diffs)})), len(diffs) != 0
}

// decline is chainDeclined, once a call: the learn step reads a call again
// when its take was refused, and the first pass that declined said why.
func (c *learnChain) decline(h *HTTP, names chainCall, check, why string, fields ...zap.Field) {
	if c.said {
		return
	}
	c.said = true
	h.chainDeclined(names, check, why, fields...)
}

// The stable names of why a once chain left a call to the matchers
// (chainDeclined's check), and their words.
const (
	chainNotHeld   = "chainNextNotHeld"
	chainVariables = "chainNextDeclaresVariables"
	chainDiffers   = "chainNextDiffers"
	chainClaimed   = "chainNextClaimed"

	chainNotHeldWhy   = "the series' next recording is not one the call's view holds unanswered (answered already, or not staged)"
	chainVariablesWhy = "the series' next recording declares variables, which the variable pass binds"
	chainDiffersWhy   = "the call is not the series' next recording"
)

// chainDeclined says, in a line, that the once chain in progress on a
// call's connection did not answer the call, and why: check is the reason's
// stable name, why its words, fields more of it (the places the call differs
// at, never their values). The call goes on to the matchers, the open
// test's own recording first. Seldom more than one such line a chain: an
// answer other than the chain's next recording ends the chain
// (integrations.OnceChainOnConn.Answered).
func (h *HTTP) chainDeclined(c chainCall, check, why string, fields ...zap.Field) {
	h.Logger.Info("a once series' chain did not answer a call on its connection: "+why+"; the call is answered as any call is, the open test's own first",
		append([]zap.Field{zap.String("check", check), zap.String("series", c.series), zap.String("next", c.next), zap.String("call", c.call), utils.PathTagField(c.tag)}, fields...)...)
}
