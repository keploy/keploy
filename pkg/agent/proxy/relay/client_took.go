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
// on new data and answers a retransmission with an acknowledgement of nothing
// new; once tcp_fin_timeout has passed no socket is left, and the reset comes
// from there). Those bytes reach no application. Recorded, such an answer is a
// mock the application never read: MongoDB's ClientDisconnect reply to the
// waiting hello of a driver's monitor, answered as the app stopped, was served
// at replay to the app's next hello and cleared its driver's pool; a
// dependency's late answer to a call the app gave up on replays an answer for
// a call whose test saw a timeout.
//
// The client's TCP stack says which of the two a client did, chunk by chunk,
// so the parser's protocol need not: the rule reads kernel facts about the
// proxy's end of the client connection, whatever the protocol.
//
//   - The relay looks at the socket as the client's forwarder reads the
//     client's end, before it forwards the FIN (clientFINRead), and before
//     the destination's forwarder writes each chunk to the client, until a
//     look finds the client's FIN there (clientFINBefore). Until then, every
//     chunk is teed as it is written, as before, whatever happens to it
//     after: a client that resets with no FIN before closed with bytes it had
//     not read (an abortive close), and the bytes it did read may be in that
//     very chunk.
//   - A look finds the FIN when the socket says the client's FIN is in
//     (CLOSE_WAIT, or LAST_ACK once the proxy sent its own after it), or, once
//     the relay has read the client's end to its end (io.EOF, which a reset
//     with no FIN before it does not give: that read fails with
//     ECONNRESET), when the socket says the connection is closed: the client
//     reset after its FIN, and a chunk written to a closed connection reaches
//     no one.
//   - Each chunk written once a look found the client's FIN waits for the
//     client's answer for it before it is teed or not (destWritten), judged
//     by its own bytes: the destination's forwarder notes, before the Write,
//     how many bytes had been written to the socket in all (written). The
//     chunk is teed once the client's stack has acknowledged more than that,
//     a byte of the chunk or after it (the client took it), and refused if
//     the connection closes with none of it acknowledged (the client closed
//     before it came, and reset it). A refused chunk ends the destination's
//     stream for the parser: no later chunk is teed (Config.ClientStopped).
//     A closed client acknowledges nothing after its FIN. A half-closed
//     client acknowledges each chunk it receives, so one that reads a chunk
//     and then closes its socket has that chunk teed and every chunk sent
//     after refused.
//   - The answer is waited for off the forwarding path: the forwarder sets
//     the chunk aside (tee.await) and goes on forwarding, and a goroutine of
//     its own (resolve) looks at the socket until every chunk set aside is
//     decided, teeing them in order as the client acknowledges them. Every
//     chunk the forwarder writes meanwhile is set aside behind them. So the
//     client gets every byte, and the destination's FIN, as soon as it would
//     have without the rule: a half-closed client's delayed acknowledgement
//     (some tens of milliseconds, or a retransmission timeout when it reads
//     everything and closes before the acknowledgement goes out) delays only
//     what the parser is given, never the application. resolve ends once no
//     chunk is set aside; the next chunk written after the FIN starts another.
//   - A chunk the client answers neither way by the relay's end (both
//     forwarders have returned, and HalfCloseGrace has passed since: none at
//     all with a negative HalfCloseGrace, where the relay stops at the
//     client's FIN), or by the recording's stop, is decided by one last look:
//     no answer by then refuses nothing. A socket that cannot say (keploy
//     closed it under the relay, or the call failed) refuses nothing.
//   - No mark of an incomplete mock is set for a chunk the parser is not
//     given, its Write failed or not.
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
// and the socket cannot tell the two apart. It goes wrong both ways:
//
//   - Recorded though unread: an acknowledgement is not a read, and a teed
//     chunk is not taken back, so the recording can hold a reply the
//     application did not read. Four paths tee such a chunk:
//       - a chunk a half-closed client's stack acknowledged, which the
//         application then leaves unread and closes its socket on (unread
//         data makes the close a reset). Only the chunks written after the
//         reset are refused.
//       - every chunk to a client that resets with no FIN before (it closed
//         with bytes unread in its receive queue), since what it read may be
//         in the last one.
//       - a chunk written while the client's FIN was still on its way
//         (above).
//       - a chunk the client had answered neither way when the last look
//         came, at the relay's end or the recording's stop (above): no
//         answer by then refuses nothing. That is a client whose host
//         vanished, which sends neither an acknowledgement nor a reset, or a
//         closed client whose reset had not landed when the recording's stop
//         made the next look the last (resolve takes that look at once, so
//         the window is about one loopback round trip).
//   - Refused though read: a half-closed client that resets before its
//     acknowledgement of a chunk went out (it read the chunk in part and
//     closed with the rest unread, or read all of it and closed abortively,
//     with SO_LINGER 0) resets with none of the chunk acknowledged: that chunk
//     is refused although the application read some or all of it. So is the
//     last chunk a half-closed client read whole and then closed its socket
//     on, normally, while its acknowledgement was still delayed (a stack in
//     delayed-ACK mode holds it some tens of milliseconds), when the
//     destination sends more after: the close drops the delayed
//     acknowledgement, and the closed socket resets on the bytes that come
//     next. A destination that sends only its FIN after is acknowledged, chunk
//     and FIN, and the chunk is teed. A reset carries an acknowledgement
//     number, but the kernel does not process the acknowledgement of a reset
//     (tcp_validate_incoming handles the reset before tcp_ack), so no count of
//     the socket's moves for it. Telling such a client from one that closed
//     before the chunk came would take a rule on time (no reset within so
//     long of the chunk), which keploy does not use. And when the client's
//     acknowledgements move between written's two questions on every one of
//     its tries (countAt), written can count more bytes than were written: a
//     chunk no longer than that excess, which the client acknowledged and
//     then closed on, is refused although it took it. Each of these costs a
//     missing mock.
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
// (resolve) looks after that. No two looks run at once. written is the
// destination's forwarder's alone, before each chunk it writes once the FIN
// was found, and may run while resolve looks.
type clientSocket interface {
	look() clientLook
	// written is how many bytes had been written to the socket in all,
	// counted as clientLook.acked counts them; ok is false when the socket
	// cannot say. Where it cannot be had exactly, it is no less than that.
	written() (n uint64, ok bool)
}

// How soon resolve looks again while the client has answered nothing for the
// chunks set aside: from firstLookAgain, doubling up to lastLookAgain, and
// from firstLookAgain again once a look decided a chunk. The pace decides only
// how soon an answer is seen, never what it is: the client's stack answers
// with an acknowledgement or a reset, and the wait ends only on one of those,
// or as the relay ends.
const (
	firstLookAgain = 50 * time.Microsecond
	lastLookAgain  = 5 * time.Millisecond
)

// clientVerdict is what the client's answer, so far, says of a chunk set
// aside.
type clientVerdict uint8

const (
	// verdictNone: the client has answered neither way yet.
	verdictNone clientVerdict = iota
	// verdictTaken: the client's stack acknowledged a byte of the chunk, or
	// after it: teed.
	verdictTaken
	// verdictRefused: the connection closed with none of the chunk
	// acknowledged. It is not teed, nor is any later one.
	verdictRefused
	// verdictAsBefore: no answer can be had (the socket cannot say, or this
	// was the last look): teed, as before the relay asked.
	verdictAsBefore
)

// chunkVerdict is the verdict a look at the client's socket gives for a chunk
// set aside, written once from bytes had been written to the socket before it;
// last when no later look will be taken. An acknowledgement of a byte of the
// chunk decides first: a chunk the client's stack took is never refused,
// whatever the socket says next. Chunks set aside are judged in order, and a
// later one was written from no fewer bytes, so a look that refuses one, or
// decides nothing for it, does the same for every chunk behind it.
func chunkVerdict(l clientLook, from uint64, last bool) clientVerdict {
	switch {
	case l.acked > from:
		return verdictTaken
	case l.said == clientClosed:
		return verdictRefused
	case l.said != clientFinned, last:
		return verdictAsBefore
	}
	return verdictNone
}

// clientFINBefore is the destination's forwarder's look at the client's
// socket before it writes a chunk to it: whether the client's FIN had reached
// the proxy's socket by then, and if so, how many bytes had been written to
// the socket before this chunk (from). Once a look finds the FIN, it stays
// found, and only written is asked before each later chunk. No look is taken
// where none can decide anything any more: no socket to ask, a stream that has
// ended where the client stopped. A socket that cannot say how much was
// written says no FIN: the chunk is teed as before.
func (r *Relay) clientFINBefore() (fin bool, from uint64) {
	if r.client == nil || r.clientStopped.Load() {
		return false, 0
	}
	if !r.clientFIN.Load() && !r.lookForClientFIN() {
		return false, 0
	}
	from, ok := r.client.written()
	return ok, from
}

// clientFINRead is the client's forwarder's look at the client's socket as it
// reads the client's end (io.EOF), before it forwards the FIN to the
// destination: the destination's answer to the FIN comes after it. From here
// on, a look that finds the connection closed finds the FIN too
// (lookForClientFIN): the relay read the client's end to its end, so the FIN
// came in order, and the connection closed after it. The socket says
// clientFinned only once the TCP FIN is in: for a TLS client whose
// close_notify the relay read first, the look before the destination's next
// write finds the FIN.
func (r *Relay) clientFINRead() {
	if r.client == nil {
		return
	}
	r.clientEOF.Store(true)
	if !r.clientFIN.Load() {
		r.lookForClientFIN()
	}
}

// lookForClientFIN looks at the client's socket unless the FIN was found
// already. It reports whether the FIN has been found.
func (r *Relay) lookForClientFIN() bool {
	r.finMu.Lock()
	defer r.finMu.Unlock()
	if !r.clientFIN.Load() {
		if l := r.client.look(); l.said == clientFinned || l.said == clientClosed && r.clientEOF.Load() {
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
	// refused a chunk before it.
	fateRefused
	// fateAwaiting: set aside (tee.await) until the client's answer decides
	// it (resolve).
	fateAwaiting
)

// destWritten decides, on the destination's forwarder, what becomes of the
// chunk c it just wrote to the client. finBefore and from are
// clientFINBefore's answer before the Write; mark is the incomplete-mock mark
// the forwarder would set for it ("write_error", "short_write", or ""), which,
// for a chunk set aside, is set only if the chunk is teed in the end. t is the
// destination's tee, seq its sequence counter, and ctx the relay's: resolve
// uses them.
//
// A chunk written while no FIN had been found is teed now. Once the client
// refused, nothing is. A chunk behind chunks set aside is set aside after
// them, in order (one whose from the socket could not say takes the one before
// it). A chunk written once the FIN was found with none set aside is set
// aside, and a goroutine (resolve) waits for the client's answer for it.
func (r *Relay) destWritten(ctx context.Context, t *tee, seq *atomic.Uint64, c fakeconn.Chunk, finBefore bool, from uint64, mark string) chunkFate {
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
		if prev := r.awaitingFrom[len(r.awaitingFrom)-1]; !finBefore || from < prev {
			from = prev
		}
	case !finBefore:
		return fateTee
	default:
		r.resolverDone = make(chan struct{})
		go r.resolve(ctx, t, seq, r.resolverDone)
	}
	t.await(c)
	r.awaitingFrom = append(r.awaitingFrom, from)
	if mark != "" {
		// Only the last chunk can carry one: the forwarder returns after a
		// failed or short Write.
		r.awaitingMark = mark
	}
	return fateAwaiting
}

// resolve waits for the client's answer for the chunks set aside, off the
// forwarding path, and tees or refuses them as it comes (settle). It looks at
// the client's socket from firstLookAgain to lastLookAgain apart; the
// recording's stop (ctx) or the relay's end (lastLook, closed by run) makes
// the next look the last. It closes done once no chunk is set aside.
func (r *Relay) resolve(ctx context.Context, t *tee, seq *atomic.Uint64, done chan struct{}) {
	defer close(done)
	wait, last := firstLookAgain, false
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		decided, settled := r.settle(t, seq, r.client.look(), last)
		if settled {
			return
		}
		if decided {
			wait = firstLookAgain
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

// settle carries out what the look l says of the chunks set aside, oldest
// first (chunkVerdict): each one taken, or that no answer can be had for, is
// teed, in order; a refused one is refused with every chunk behind it, and
// counted, and the relay says once that the destination's stream ended where
// the client stopped (Config.ClientStopped); the first one the client has not
// answered for yet stops it. The forwarder's incomplete-mock mark is set once
// the last chunk set aside, the one that earned it, is teed. decided is
// whether any chunk was decided, settled whether none is left set aside (when
// resolve ends). Each chunk set aside leaves tee.awaiting only once it is on
// the tee's queue or refused, so the capture watermark never loses sight of
// it.
func (r *Relay) settle(t *tee, seq *atomic.Uint64, l clientLook, last bool) (decided, settled bool) {
	r.verdictMu.Lock()
	defer r.verdictMu.Unlock()
	for ; len(r.awaitingFrom) > 0; decided = true {
		c, _ := t.firstAwaiting()
		switch chunkVerdict(l, r.awaitingFrom[0], last) {
		case verdictNone:
			return decided, false
		case verdictRefused:
			for ok := true; ok; c, ok = t.firstAwaiting() {
				r.notTakenChunks.Add(1)
				r.notTakenBytes.Add(int64(len(c.Bytes)))
				t.doneAwaiting()
			}
			r.awaitingFrom, r.awaitingMark, r.resolverDone = nil, "", nil
			if !r.clientStopped.Swap(true) && r.cfg.ClientStopped != nil {
				r.cfg.ClientStopped()
			}
			return true, true
		}
		c.SeqNo = seq.Add(1)
		if r.cfg.beforeTee != nil {
			r.cfg.beforeTee(t.dir)
		}
		teed := t.push(c)
		r.noteCarried(t, c, teed)
		t.doneAwaiting()
		r.awaitingFrom = r.awaitingFrom[1:]
	}
	r.awaitingFrom, r.resolverDone = nil, nil
	if mark := r.awaitingMark; mark != "" {
		r.awaitingMark = ""
		if r.cfg.OnMarkMockIncomplete != nil {
			r.cfg.OnMarkMockIncomplete(mark)
		}
	}
	return decided, true
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
