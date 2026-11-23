//go:build linux || darwin

package proxy

import (
	"context"
	"fmt"
	"math"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// PairedBytesDrain waits until the proxy has counted the close of every
// paired connection whose application end has ended, or whose close the proxy
// has begun (agent.PairedBytesDrainer): the agent asks before a session's
// last read of its losses, once the application has stopped, so a close made
// as the application stopped is in that read, never in a later session's or
// set's, and never only a log line.
//
// A close's count (pairClosing) runs before the proxy closes the connection,
// and the connection is forgotten (remove) only once its handler is done
// with it: once no connection the drain waits for is held, every loss of its
// close is counted, in the counts or owned. A connection the application
// still holds open (it wrote no FIN, and its socket is there) is not waited
// for: no close of it has begun.
//
// The wait is bounded by progress: it looks again at each removal and on
// looks of its own (stall.Meter's), and the bound starts again whenever a
// connection it waited for was forgotten or its close begun. Once keploy
// could run for drainBudget with neither, the connections still waited for
// are stuck: a handler that never closes the connection. Their closes cannot
// be counted, so each is owned as the session's, app_bytes_untold, naming
// where it went, the application's end, and the session (drainGaveUp): the
// session's read then has it, and leaves its run or set without a verdict,
// never green and never judged on only what was counted. The drain then
// answers, and no later drain waits for them. It ends with ctx too: what it
// waited for then is in the error, and the read it came before is not made.
func (p *Proxy) PairedBytesDrain(ctx context.Context) error {
	meter := stall.NewMeter(p.paired.clk, drainBudget)
	defer meter.Stop()
	var waited map[*pairedConn]bool
	look := meter.Wait()
	for {
		// A removal after the look wakes the wait (closingConns).
		closing, changed := p.closingConns()
		if len(closing) == 0 {
			return nil
		}
		if drainMoved(waited, closing) {
			meter.Reset()
		}
		waited = make(map[*pairedConn]bool, len(closing))
		for _, pc := range closing {
			waited[pc] = pc.atClose.Load() != nil
		}
		if meter.Expired() {
			p.drainGiveUp(closing)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("keploy's proxy had not finished counting the close of %d connection(s) whose application end had ended (the first from %s): %w", len(closing), closing[0].app, ctx.Err())
		case <-changed:
		case <-look:
			look = meter.Wait()
		}
	}
}

// drainBudget is how long, in time the agent could run, a drain waits with
// no connection it waits for forgotten or its close begun, before it holds
// their handlers stuck (PairedBytesDrain): stall.HangBudget, the one hang
// budget. Every wait a handler makes once the application's end has ended is
// bounded by it, from a start before the drain's own or at a close begun
// (which starts the drain's bound again): a parser that stopped reading (the
// parser supervisor), and a close's wait for the application's FIN
// (orphanFINBudget). So a handler the proxy's own bounds will end is never
// held stuck, and the drain answers within the bound the CLI gives the read
// it comes before (the client's capture-loss reads that drain).
const drainBudget = stall.HangBudget

// drainMoved says whether a drain made progress since its last look, at
// which it waited for waited (each with whether its close had begun): one of
// them was forgotten (not closing now), or its close began since.
func drainMoved(waited map[*pairedConn]bool, closing []*pairedConn) bool {
	now := make(map[*pairedConn]bool, len(closing))
	for _, pc := range closing {
		now[pc] = true
		if begun, ok := waited[pc]; ok && !begun && pc.atClose.Load() != nil {
			return true
		}
	}
	for pc := range waited {
		if !now[pc] {
			return true
		}
	}
	return false
}

// drainGiveUp owns, once each, the close of every connection of stuck, whose
// handler a drain held stuck, as the session's: app_bytes_untold (keploy
// cannot tell what its close lost), naming where it went, the application's
// end and the session (pairedConn.drainGaveUp).
func (p *Proxy) drainGiveUp(stuck []*pairedConn) {
	session := p.paired.session.Load()
	for _, pc := range stuck {
		if !pc.drainGaveUp.CompareAndSwap(false, true) {
			continue
		}
		dest := "a destination its handler had not named yet"
		if d := pc.dest.Load(); d != nil && *d != "" {
			dest = *d
		}
		what := fmt.Sprintf("to %s from %s, in session %d, whose handler never finished closing it once the application's end had ended: keploy's proxy waited while it could run for %v with no such close done", dest, pc.app, session, drainBudget)
		p.paired.ownKind(agent.OwnedLoss{Session: session, Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: what}})
		p.logger.Warn("keploy could not count the close of a connection whose application end had ended: its handler never finished closing it, so the run or test set it belongs to has no verdict",
			zap.String("destination", dest), zap.String("from", pc.app.String()), zap.Uint64("session", session),
			zap.String("next_step", "check the agent's log for the handler of that destination, then run again"))
	}
}

// closingConns are the paired connections PairedBytesDrain waits for
// (drainsFor). The application's sockets are read first (pairTargets) and
// each proxy side after (readProxySide): an application socket whose FIN was
// acknowledged between the two reads is seen in the second. When the
// application's sockets cannot be read (pairTargets fails: the waits fail the
// same way), the proxy's side alone tells. One the proxy serves with no
// session counts no loss at its close, and is not waited for (heldNow); nor
// is one whose close a drain already owned (drainGaveUp). changed is closed
// at the next removal after the look (heldNow).
func (p *Proxy) closingConns() (closing []*pairedConn, changed <-chan struct{}) {
	app := map[*pairedConn]*pairTarget{}
	if targets, err := p.pairTargets(math.MaxUint64); err == nil {
		for _, t := range targets {
			app[t.pc] = t
		}
	}
	held, changed := p.paired.heldNow()
	for _, pc := range held {
		if pc.drainGaveUp.Load() {
			continue
		}
		if drainsFor(pc.atClose.Load() != nil, app[pc], func() proxySide { return readProxySide(pc.conn) }) {
			closing = append(closing, pc)
		}
	}
	return closing, changed
}

// drainsFor says whether PairedBytesDrain waits for a paired connection:
// one whose close the proxy has begun (closeBegun), or whose application end
// has ended: its socket is gone, or sent its FIN (app, nil when it was not
// read), or the proxy's side (side, read only when the rest does not tell)
// has taken its FIN or reset. A side that cannot be read is waited for:
// whether it ended cannot be told.
func drainsFor(closeBegun bool, app *pairTarget, side func() proxySide) bool {
	if closeBegun || (app != nil && (app.gone || app.finCounted)) {
		return true
	}
	ps := side()
	return ps.err != nil || peerEnded(ps.state)
}
