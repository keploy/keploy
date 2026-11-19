//go:build linux || darwin

package proxy

import (
	"context"
	"fmt"
	"math"
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
// close is counted, in the counts or owned. The drain waits for removals and
// looks again at each, never on a clock. A connection the application still
// holds open (it wrote no FIN, and its socket is there) is not waited for: no
// close of it has begun. It ends with ctx: what it waited for then is in the
// error, and the read it came before is not made.
func (p *Proxy) PairedBytesDrain(ctx context.Context) error {
	for {
		// Taken before looking: a removal after the look wakes the wait.
		changed := p.paired.changes()
		closing := p.closingConns()
		if len(closing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("keploy's proxy had not finished counting the close of %d connection(s) whose application end had ended (the first from %s): %w", len(closing), closing[0].app, ctx.Err())
		case <-changed:
		}
	}
}

// closingConns are the paired connections PairedBytesDrain waits for: those
// whose close the proxy has begun (atClose), and those whose application end
// has ended: its socket sent its FIN, or is gone (pairTargets, read first),
// or the proxy's side has taken its FIN or reset (peerEnded, read after: an
// application socket whose FIN was acknowledged between the two reads is
// seen in the second). A side that cannot be read is waited for: whether it
// ended cannot be told. One the proxy serves with no session counts no loss
// at its close, and is not waited for. When the application's sockets cannot
// be read (pairTargets fails: the waits fail the same way), the proxy's side
// alone tells.
func (p *Proxy) closingConns() []*pairedConn {
	appEnded := map[*pairedConn]bool{}
	if targets, err := p.pairTargets(math.MaxUint64); err == nil {
		for _, t := range targets {
			appEnded[t.pc] = t.gone || t.finCounted
		}
	}
	var out []*pairedConn
	for _, pc := range p.paired.heldNow() {
		if pc.atClose.Load() != nil || appEnded[pc] {
			out = append(out, pc)
			continue
		}
		if ps := readProxySide(pc.conn); ps.err != nil || peerEnded(ps.state) {
			out = append(out, pc)
		}
	}
	return out
}
