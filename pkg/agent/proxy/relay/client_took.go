package relay

import (
	"context"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// What the destination sends once the client has closed its side is captured
// only if the client took it.
//
// A client's FIN says it will send nothing more. It does not say whether it
// still reads. A client that only half-closed (shutdown(SHUT_WR): an
// EOF-delimited request, Node's socket.end(data)) reads the answer the
// destination sends after it, and the relay forwards that answer for it
// (Config.HalfCloseGrace). A client that closed its socket (close(2): a driver
// closing a pooled connection as the application stops, an HTTP client giving
// up on a call at its timeout) reads nothing more. Its TCP stack answers every
// byte that reaches it after with a reset and acknowledges none of them (RFC
// 1122 4.2.2.13; Linux: tcp_rcv_state_process resets on data for a socket shut
// down for reading, and once its FIN is acknowledged the closed socket lives
// on as a FIN_WAIT2 timewait socket, which tcp_timewait_state_process resets
// on new data; once tcp_fin_timeout has passed no socket is left, and the
// reset comes from there). Those bytes reach no application. Recorded, such an
// answer is a mock the application never read: MongoDB's ClientDisconnect
// reply to the waiting hello of a driver's monitor, answered as the app
// stopped, was served at replay to the app's next hello and cleared its
// driver's pool; a dependency's late answer to a call the app gave up on
// replays an answer for a call whose test saw a timeout.
//
// The client's TCP stack says which of the two a client did, so the parser's
// protocol need not: the rule reads kernel facts about the proxy's end of the
// client connection, whatever the protocol.
//
//   - The relay looks at the socket as the client's forwarder reads the
//     client's end, before it forwards the FIN (clientFINRead), and before
//     the destination's forwarder writes each chunk to the client, until a
//     look finds the client's FIN there (clientFINBefore). Until then, every
//     chunk is teed as it is written, as before, whatever happens to it
//     after: a client that resets with no FIN before closed with bytes it had
//     not read (an abortive close), and the bytes it did read may be in that
//     very chunk.
//   - A chunk written once a look found the client's FIN waits for the
//     client's answer before it is teed or not (destWritten): it is refused
//     only if the client then resets the connection without having
//     acknowledged a byte since that look. A closed client acknowledges
//     nothing after its FIN, and resets. A client that acknowledges a byte
//     after that look has only half-closed: it reads on, the chunks are teed,
//     and nothing is refused from then on (no socket is asked again either).
//     The look as the relay reads the FIN comes first, so an acknowledgement
//     of bytes in flight at the FIN that comes in before the destination's
//     next write counts as reading on.
//   - The answer is waited for off the forwarding path: the forwarder sets
//     the chunk aside (tee.await) and goes on forwarding, and a goroutine of
//     its own (resolve) looks at the socket until the client answers, then
//     tees the chunks set aside, in order, or refuses them. Every chunk the
//     forwarder writes meanwhile is set aside behind them. So the client gets
//     every byte, and the destination's FIN, as soon as it would have without
//     the rule: a half-closed client's delayed acknowledgement (some tens of
//     milliseconds, or a retransmission timeout when it reads everything and
//     closes before the acknowledgement goes out) delays only what the parser
//     is given, never the application.
//   - A client that answers neither way by the relay's end (both forwarders
//     have returned, and HalfCloseGrace has passed since: none at all with a
//     negative HalfCloseGrace, where the relay stops at the client's FIN), or
//     by the recording's stop, is decided by one last look: no answer by then
//     refuses nothing. A socket that cannot say (keploy closed it under the
//     relay, or the call failed) refuses nothing.
//   - Once the chunks are refused, no later one is teed: the destination's
//     stream ends where the client stopped (Config.ClientStopped), and no mock
//     is marked incomplete for a chunk the parser is not given, its Write
//     failed or not.
//
// The bytes are forwarded either way: only what the parser is given changes.
//
// The client's FIN counts once it has reached the proxy's socket, whether or
// not the relay has read it yet: the socket says so, not the relay's own
// reading. A chunk written while the FIN was still on its way is teed as
// before, even if the closed client resets it: the relay could only learn of
// the FIN once it had handed the chunk on.
//
// While chunks wait for the client's answer, the capture watermark waits for
// them (they are unconsumed: tee.awaiting), and the parser reads them only
// once the answer came.
//
// What a client's stack acknowledged is not always what the application read,
// and the socket cannot tell the two apart. A half-closed client that resets
// before its acknowledgement of a chunk went out (it read the chunk in part and
// closed with the rest unread, or read all of it and closed abortively, with
// SO_LINGER 0) resets with nothing acknowledged: that chunk is refused although
// the application read some or all of it. So is a chunk a half-closed client
// read whole and then closed its socket on, normally, while its acknowledgement
// was still delayed (a stack in delayed-ACK mode holds it some tens of
// milliseconds), when the destination sends more after: the close drops the
// delayed acknowledgement, and the closed socket resets on the bytes that come
// next, so the reset follows a chunk the client had taken. A destination that
// sends only its FIN after is acknowledged, chunk and FIN, and the client reads
// on. A reset carries an acknowledgement number, but the kernel does not
// process the acknowledgement of a reset (tcp_validate_incoming handles the
// reset before tcp_ack), so no count of the socket's moves for it.
//
// Only a client connection that is a TCP socket on Linux can be asked
// (clientSocketOf). Elsewhere every chunk is teed as it is written, as before,
// and one numbered after the client's FIN carries
// fakeconn.Chunk.AfterClientClose for the parser to judge by.

// clientSaid is what the proxy's end of the client connection says of its
// client.
type clientSaid uint8

const (
	// clientOpen: the client has sent no FIN.
	clientOpen clientSaid = iota
	// clientFinned: the client's FIN has reached the proxy's socket, and the
	// connection is not closed (the proxy may have sent its own FIN since).
	clientFinned
	// clientClosed: the connection is closed: reset by the client, timed out
	// by the proxy's stack once the client stopped answering (a
	// retransmission, keepalive or user timeout), or, once the proxy sent its
	// own FIN, closed by the client's acknowledgement of it. Which of these
	// it was, the client's acknowledgements tell (clientLook.acked).
	clientClosed
	// clientUnknown: the socket cannot say (keploy closed it, or the call
	// failed).
	clientUnknown
)

// clientLook is one look at the proxy's end of the client connection: what it
// says of the client, and how many of the bytes written to it the client's TCP
// stack has acknowledged, in all (0 when it cannot say).
type clientLook struct {
	said  clientSaid
	acked uint64
}

// clientSocket looks at the proxy's end of the client connection. The
// client's forwarder looks as it reads the client's end, and the destination's
// forwarder before it writes, until one of them finds the client's FIN (one at
// a time: Relay.finMu); the goroutine that waits for the client's answer
// (resolve) looks after that. No two looks run at once.
type clientSocket interface {
	look() clientLook
}

// How soon resolve looks again while the client has answered nothing for the
// chunks set aside: from firstLookAgain, doubling up to lastLookAgain. The
// pace decides only how soon an answer is seen, never what it is: the client's
// stack answers with an acknowledgement or a reset, and the wait ends only on
// one of those, or as the relay ends.
const (
	firstLookAgain = 50 * time.Microsecond
	lastLookAgain  = 5 * time.Millisecond
)

// clientVerdict is what the client's answer, so far, says of the chunks set
// aside.
type clientVerdict uint8

const (
	// verdictNone: the client has answered neither way yet.
	verdictNone clientVerdict = iota
	// verdictReadsOn: the client acknowledged a byte since the look that found
	// its FIN: it reads on. The chunks are teed, and so is every later one.
	verdictReadsOn
	// verdictRefused: the connection closed with nothing acknowledged since
	// that look. The chunks are not teed, nor is any later one.
	verdictRefused
	// verdictAsBefore: no answer can be had (the socket cannot say, or this
	// was the last look): the chunks are teed, as before the relay asked.
	verdictAsBefore
)

// verdictOf is the verdict a look at the client's socket gives, last when no
// later look will be taken. A byte acknowledged since the look that found the
// client's FIN (clientFINAcked) decides first: a client that took bytes after
// its FIN is never taken to have refused them, whatever the socket says next.
func (r *Relay) verdictOf(l clientLook, last bool) clientVerdict {
	switch {
	case l.acked > r.clientFINAcked:
		return verdictReadsOn
	case l.said == clientClosed:
		return verdictRefused
	case l.said != clientFinned, last:
		return verdictAsBefore
	}
	return verdictNone
}

// clientFINBefore is the destination's forwarder's look at the client's
// socket before it writes a chunk to it: whether the client's FIN had reached
// the proxy's socket by then. Once a look finds it, it stays found, with what
// the client's stack had acknowledged at that look (clientFINAcked). No look
// is taken where none can decide anything any more: no socket to ask, a client
// that reads on, a stream that has ended where the client stopped, a FIN
// already found.
func (r *Relay) clientFINBefore() bool {
	if r.client == nil || r.clientReadsOn.Load() || r.clientStopped.Load() {
		return false
	}
	return r.clientFIN.Load() || r.lookForClientFIN()
}

// clientFINRead is the client's forwarder's look at the client's socket as it
// reads the client's end (io.EOF), before it forwards the FIN to the
// destination: the destination's answer to the FIN comes after it. A client
// that half-closed with bytes in flight acknowledges them after its FIN.
// Counted from this look, an acknowledgement that comes in before the
// destination's next write shows that the client reads on. Counted from the
// look before that write, it would be lost. The socket says clientFinned only
// once the TCP FIN is in: for a TLS client whose close_notify the relay read
// first, the look before the destination's next write finds the FIN.
func (r *Relay) clientFINRead() {
	if r.client != nil && !r.clientFIN.Load() {
		r.lookForClientFIN()
	}
}

// lookForClientFIN looks at the client's socket unless the FIN was found
// already, and keeps what a look that finds it says (clientFINAcked). It
// reports whether the FIN has been found.
func (r *Relay) lookForClientFIN() bool {
	r.finMu.Lock()
	defer r.finMu.Unlock()
	if !r.clientFIN.Load() {
		if l := r.client.look(); l.said == clientFinned {
			r.clientFINAcked = l.acked
			r.clientFIN.Store(true)
		}
	}
	return r.clientFIN.Load()
}

// chunkFate is what becomes of a destination chunk the forwarder wrote to the
// client (destWritten).
type chunkFate uint8

const (
	// fateTee: the forwarder tees it now, as it always did.
	fateTee chunkFate = iota
	// fateRefused: not teed, and not told of (Config.OnCarried): the client
	// refused a chunk before it, or this one.
	fateRefused
	// fateAwaiting: set aside (tee.await) until the client's answer decides
	// it (resolve).
	fateAwaiting
)

// destWritten decides, on the destination's forwarder, what becomes of the
// chunk c it just wrote to the client. finBefore is clientFINBefore's answer
// before the Write; mark is the incomplete-mock mark the forwarder would set
// for it ("write_error", "short_write", or ""), which, for a chunk set aside,
// is set only if the chunk is teed in the end. t is the destination's tee, seq
// its sequence counter, and ctx the relay's: resolve uses them.
//
// A chunk written while no FIN had been found, or to a client that reads on,
// is teed now. Once the client refused, nothing is. A chunk behind chunks set
// aside is set aside after them, in order. The first chunk written once the
// FIN was found is set aside, and a goroutine (resolve) waits for the client's
// answer for it.
func (r *Relay) destWritten(ctx context.Context, t *tee, seq *atomic.Uint64, c fakeconn.Chunk, finBefore bool, mark string) chunkFate {
	if r.client == nil {
		return fateTee
	}
	r.verdictMu.Lock()
	defer r.verdictMu.Unlock()
	switch {
	case r.clientStopped.Load():
		r.notTakenChunks.Add(1)
		r.notTakenBytes.Add(int64(len(c.Bytes)))
		return fateRefused
	case r.resolverDone != nil:
		// Behind a chunk the client's answer has not decided yet: set aside
		// after it.
	case r.clientReadsOn.Load() || !finBefore:
		return fateTee
	default:
		r.resolverDone = make(chan struct{})
		go r.resolve(ctx, t, seq, r.resolverDone)
	}
	t.await(c)
	if mark != "" {
		// Only the last chunk can carry one: the forwarder returns after a
		// failed or short Write.
		r.awaitingMark = mark
	}
	return fateAwaiting
}

// resolve waits for the client's answer for the chunks set aside, off the
// forwarding path, and then tees or refuses them (settle). It looks at the
// client's socket from firstLookAgain to lastLookAgain apart; the recording's
// stop (ctx) or the relay's end (lastLook, closed by run) makes the next look
// the last. It closes done once the chunks are decided.
func (r *Relay) resolve(ctx context.Context, t *tee, seq *atomic.Uint64, done chan struct{}) {
	defer close(done)
	wait, last := firstLookAgain, false
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if v := r.verdictOf(r.client.look(), last); v != verdictNone {
			r.settle(t, seq, v)
			return
		}
		timer.Reset(wait)
		select {
		case <-timer.C:
			wait = min(2*wait, lastLookAgain)
		case <-ctx.Done():
			last = true
		case <-r.lastLook:
			last = true
		}
	}
}

// settle carries out the verdict v on the chunks set aside: teed, in order,
// with the forwarder's incomplete-mock mark for the last one, or refused, and
// counted. A refusal ends the destination's stream for the parser, and the
// relay says so once (Config.ClientStopped). Each chunk set aside leaves
// tee.awaiting only once it is on the tee's queue or refused, so the capture
// watermark never loses sight of it.
func (r *Relay) settle(t *tee, seq *atomic.Uint64, v clientVerdict) {
	r.verdictMu.Lock()
	defer r.verdictMu.Unlock()
	mark := r.awaitingMark
	r.awaitingMark = ""
	r.resolverDone = nil
	if v == verdictRefused {
		for c, ok := t.firstAwaiting(); ok; c, ok = t.firstAwaiting() {
			r.notTakenChunks.Add(1)
			r.notTakenBytes.Add(int64(len(c.Bytes)))
			t.doneAwaiting()
		}
		if !r.clientStopped.Swap(true) && r.cfg.ClientStopped != nil {
			r.cfg.ClientStopped()
		}
		return
	}
	if v == verdictReadsOn {
		r.clientReadsOn.Store(true)
	}
	for c, ok := t.firstAwaiting(); ok; c, ok = t.firstAwaiting() {
		c.SeqNo = seq.Add(1)
		if r.cfg.beforeTee != nil {
			r.cfg.beforeTee(t.dir)
		}
		teed := t.push(c)
		r.noteCarried(t, c, teed)
		t.doneAwaiting()
	}
	if mark != "" && r.cfg.OnMarkMockIncomplete != nil {
		r.cfg.OnMarkMockIncomplete(mark)
	}
}

// awaitClientVerdict is run's wait, once both forwarders have returned, for
// the client's answer for chunks still set aside: until the answer comes, or
// HalfCloseGrace has passed (none when it is negative), when resolve takes its
// last look. The client got everything by then, the destination's FIN
// included, which a half-closed client acknowledges at once; a closed client
// resets on the first chunk. Only a client that answers nothing at all (a full
// receive window, a host gone) waits out the grace.
func (r *Relay) awaitClientVerdict() {
	r.verdictMu.Lock()
	done := r.resolverDone
	r.verdictMu.Unlock()
	if done == nil {
		close(r.lastLook)
		return
	}
	timer := time.NewTimer(max(r.cfg.HalfCloseGrace, 0))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	close(r.lastLook)
	<-done
}
