//go:build linux || darwin

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// The bytes wait itself (WaitPairedBytes), shared by the platforms that can
// read how much the application wrote to a paired connection: each says, in
// its own pairTargets and pairReached, what it reads of the two sockets, and
// this waits, event-driven, until every target is reached.

// pairTarget is what the wait needs of one paired connection.
type pairTarget struct {
	pc *pairedConn
	// want is how many bytes (and a FIN) the application had written to
	// the connection when the wait began (appWritten). The proxy's side has
	// received all of them once its own count (tcpi_bytes_received, which a
	// FIN also moves) reaches it.
	want uint64
	// finCounted: want counts the application's FIN, which its socket had
	// sent when the wait began (finSent): the last of want may be that FIN.
	// Otherwise every byte of want is data.
	finCounted bool
	// base is where the proxy's side's count of what it received of the
	// connection begins (targetBase.received): want and seen count from it.
	base targetBase
	// gone: the application's socket was already gone (closed, or reset)
	// when the wait began. Its FIN, after everything it wrote, or a reset,
	// is what the proxy's side waits for.
	gone bool
	// seen is the most the proxy's side was seen to have received: more is
	// progress, for the watchdog.
	seen uint64
	// orphan is, while the wait waits for the proxy's socket of a
	// connection it closed one FIN short to take that FIN (pairReached),
	// the socket's state at the last look (FIN_WAIT1, FIN_WAIT2, or
	// ESTABLISHED for a close that has not reached the kernel); 0 when it
	// does not.
	orphan uint8
	// done: the wait needs no more of it (pairReached).
	done bool
}

// orphanFINBudget is how long, in time the agent could run, a bytes wait
// waits with nothing moving for an orphan to take the application's FIN
// (waitPairTargets) before it answers unknown: the one hang budget
// (stall.HangBudget).
const orphanFINBudget = stall.HangBudget

// waitPairTargets waits until each of pending is reached (pairReached).
//
// An orphan's taking a FIN is no event of the proxy's: the proxy has closed
// the socket, so no read or close of its own follows, and the kernel tells no
// one when a timewait socket takes a FIN. While one waits for the
// application's FIN, the wait looks at it again on looks of its own (the first
// a stall.Tick on, then twice as far apart each time nothing moved, up to a
// second), and answers unknown only once the agent could run for
// orphanFINBudget with nothing it waits on moving: no connection received
// more, was resolved, or saw its orphan change state. A starved agent accrues
// that budget only as fast as it gets to run (stall.Meter). The looks only
// say when to look: what ends the wait is what the orphan shows.
//
// A wait that ends first (ctx's end, or an answer of unknown) leaves each
// connection it had not seen reached marked with the test whose end it is
// (agent.EndOf) and what it waited for (pairedConn.cutShort): bytes written
// before it that are lost later are that test's, not the one open then. The
// end of a test worker's test (agent.EndWorkerOf) marks only that worker's
// connections: another worker's bytes are its own open test's, and a loss of
// them is charged there as it is counted.
func (p *Proxy) waitPairTargets(ctx context.Context, pending []*pairTarget) (err error) {
	if len(pending) == 0 {
		return nil
	}
	all := append([]*pairTarget(nil), pending...)
	defer func() {
		if err == nil {
			return
		}
		test, worker := agent.EndOf(ctx), agent.EndWorkerOf(ctx)
		for _, t := range all {
			if !t.done && (worker == 0 || t.pc.instance == worker) {
				t.pc.cutShort(test, t.want)
			}
		}
	}()
	looks := orphanLooks{clk: p.paired.clk}
	defer looks.close()
	for {
		// Taken before looking, so a read or close after the look still
		// wakes.
		reads := p.inflight.Changed()
		closes := p.paired.changes()
		if err := p.paired.failure(); err != nil {
			return err
		}
		moved := false
		next := pending[:0]
		for _, t := range pending {
			seen, orphan := t.seen, t.orphan
			reached, err := p.pairReached(t)
			if err != nil {
				return err
			}
			if reached {
				t.done = true
				p.paired.progress.Add(1)
				moved = true
				continue
			}
			moved = moved || t.seen != seen || t.orphan != orphan
			next = append(next, t)
		}
		pending = next
		if len(pending) == 0 {
			return nil
		}
		if err := looks.arm(pending, moved); err != nil {
			return err
		}
		select {
		case <-reads:
		case <-closes:
		case <-looks.c:
			looks.c, looks.stop = nil, nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// orphanLooks are a bytes wait's own looks at the orphans it waits on to take
// the application's FIN (waitPairTargets), and its measure of how long the
// agent could run with nothing the wait waits on moving.
type orphanLooks struct {
	clk   stall.Clock // nil: the process's (stall.Process)
	meter *stall.Meter
	c     <-chan time.Time // the next look's, nil when none is armed
	stop  func() bool
	every time.Duration // the last look's distance; 0 after a move
}

// arm, after a look at pending that saw them move or not (moved), arms the
// next look while any of them waits for its orphan, unless the agent has
// since run for orphanFINBudget with nothing moving: then it answers unknown,
// saying what the wait waited for. The look after a move is a stall.Tick on
// (one already armed stays), and each look that saw none puts the next twice
// as far, up to stall.CheckEvery(orphanFINBudget). With none waiting for an
// orphan, no look is armed, and the measure ends.
func (o *orphanLooks) arm(pending []*pairTarget, moved bool) error {
	if !awaitsOrphan(pending) {
		o.disarm()
		o.meter.Stop()
		o.meter = nil
		return nil
	}
	switch {
	case o.meter == nil:
		o.meter, o.every = stall.NewMeter(o.clk, orphanFINBudget), 0
	case moved:
		o.meter.Reset()
		o.every = 0
	case o.meter.Expired():
		return orphansUnmoved(pending)
	}
	if o.c != nil {
		return nil
	}
	if o.every == 0 {
		o.every = stall.Tick
	} else {
		o.every = min(2*o.every, stall.CheckEvery(orphanFINBudget))
	}
	if o.clk == nil {
		o.c, o.stop = stall.ProcessClock().After(o.every)
	} else {
		o.c, o.stop = o.clk.After(o.every)
	}
	return nil
}

// awaitsOrphan says whether any of pending waits for its orphan to take the
// application's FIN.
func awaitsOrphan(pending []*pairTarget) bool {
	for _, t := range pending {
		if t.orphan != 0 || t.base.looks() {
			return true
		}
	}
	return false
}

// orphansUnmoved is the wait's answer once the agent could run for
// orphanFINBudget with nothing moving: unknown, naming each connection whose
// orphan it waited on, and the state the orphan was in, and each whose
// application socket it read from its holder (targetBase.looks).
func orphansUnmoved(pending []*pairTarget) error {
	var waited, held []string
	for _, t := range pending {
		switch {
		case t.orphan == orphanEstablished:
			waited = append(waited, fmt.Sprintf("%s (its socket still ESTABLISHED: the proxy's close had not reached the kernel, so it was no orphan yet)", t.pc.app))
		case t.orphan != 0:
			waited = append(waited, fmt.Sprintf("%s (its orphan in %s)", t.pc.app, tcpStateName(t.orphan)))
		case t.base.looks():
			held = append(held, t.pc.app.String())
		}
	}
	var why []string
	if len(waited) > 0 {
		what := "the connection from "
		if len(waited) > 1 {
			what = "the connections from "
		}
		why = append(why, fmt.Sprintf("keploy waited for the application's FIN to reach the proxy's socket of %s%s, which the proxy closed one FIN short of what the application wrote, and the socket took neither that FIN nor a byte it would refuse", what, strings.Join(waited, ", ")))
	}
	if len(held) > 0 {
		why = append(why, fmt.Sprintf("macOS's TCP table does not list the application's end of the connections to the proxy from %s to keploy's agent, so keploy read each from the descriptor table of the process holding it, and the proxy's side received no more while the application's send buffer kept bytes it had not acknowledged", strings.Join(held, ", ")))
	}
	return fmt.Errorf("%w: while keploy could run for %v, nothing it waited on moved: %s", models.ErrProxyConnsUnsupported, orphanFINBudget, strings.Join(why, "; and "))
}

// disarm releases the armed look, if any.
func (o *orphanLooks) disarm() {
	if o.stop != nil {
		o.stop()
	}
	o.c, o.stop = nil, nil
}

// close releases the armed look and the meter.
func (o *orphanLooks) close() {
	o.disarm()
	o.meter.Stop()
}

// side is the proxy's side of pc: read from its socket while the proxy holds
// it, or as it was when the proxy closed it (closed).
func (pc *pairedConn) side() (proxySide, bool) {
	select {
	case <-pc.done:
		if ps := pc.atClose.Load(); ps != nil {
			return *ps, true
		}
		return proxySide{err: errors.New("the proxy closed its socket before it could be read")}, true
	default:
	}
	ps := readProxySide(pc.conn)
	if errors.Is(ps.err, net.ErrClosed) {
		// Closed since: the close read it first.
		if at := pc.atClose.Load(); at != nil {
			return *at, true
		}
	}
	return ps, false
}
