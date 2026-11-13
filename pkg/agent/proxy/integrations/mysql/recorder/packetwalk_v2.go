package recorder

import (
	"fmt"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
)

// packetWalk follows the packet headers of a connection's server stream,
// told of every byte as it leaves the stream (fakeconn.FakeConn.Tap): read,
// dropped by the floor, or discarded by a re-align's cut. A MySQL packet's
// header gives its length, so the headers can be walked exactly wherever no
// byte is lost, even through a response the recorder cannot frame, whose
// meaning it cannot parse.
//
// It is what proves a re-align's cut (handleCommandsV2's realignAnswer). The
// cut is made by capture time, at the client's next command (C2) after a
// response left out (A1). A client that pipelined C2 into A1's tail, though it
// had not pipelined before, has A1's tail captured after C2, and the cut lands
// in it. A tail packet at sequence id 1 (the sequence wraps every 256 packets)
// that frames as an answer to C2 was taken for C2's answer, and a client that
// pipelined its following command into A1 too confirmed it (takenUp): C2 was
// recorded with A1's bytes. So the cut is taken only where the walk shows
// A1's run of sequence ids ended (cut): every packet of an exchange is
// numbered on from the one before it, the client's and the server's alike, so
// a packet at the id C2's answer starts at (1) that no packet of A1's exchange
// is numbered just before is not A1's; and the server answers in order, so the
// first packet after A1 that is not A1's starts C2's answer.
type packetWalk struct {
	// on: the walk is at a packet boundary, or rest bytes into a packet's
	// payload, of the stream as it was sent (anchor). Off until the recorder
	// first stands at a packet boundary of its own reads.
	on bool
	// hdrN header bytes of the next packet are in hdr; rest bytes of the
	// current packet's payload are still to come.
	hdrN int
	hdr  [4]byte
	rest int
	// Since the answer the stream is at began (mark): seen is set once a
	// header was walked, last is the sequence id of the last one.
	seen bool
	last byte
	// client: the client sent packets inside that exchange
	// (nextCommandAfterFault passes them over: a LOCAL INFILE upload, an auth
	// switch response), the last one numbered clientLast. The exchange takes
	// turns: the server's next packet in it is numbered on from the client's
	// last, and the client sends again only once that packet came, so no
	// earlier client packet is the one a packet after the cut is numbered on
	// from.
	client     bool
	clientLast byte
}

// feed is the stream's tap: b are the stream's next bytes.
func (w *packetWalk) feed(b []byte) {
	for w.on && len(b) > 0 {
		if w.rest > 0 {
			n := min(w.rest, len(b))
			w.rest -= n
			b = b[n:]
			continue
		}
		n := copy(w.hdr[w.hdrN:], b)
		w.hdrN += n
		b = b[n:]
		if w.hdrN < len(w.hdr) {
			return
		}
		w.hdrN = 0
		w.rest = int(mysqlUtils.GetPayloadLength(w.hdr[:3]))
		w.seen, w.last = true, w.hdr[3]
	}
}

// anchor says the recorder's server stream is at a packet boundary: its own
// reads take whole packets, so it is once it has read a response whole.
func (w *packetWalk) anchor() {
	w.on, w.hdrN, w.rest = true, 0, 0
}

// mark says the server's next bytes start the answer to a command the client
// sent: a new exchange.
func (w *packetWalk) mark() {
	w.seen, w.client = false, false
}

// passed says the client sent a packet inside the exchange, numbered seq.
func (w *packetWalk) passed(seq byte) {
	w.client, w.clientLast = true, seq
}

// cut says why the server's next bytes, after a re-align's cut, cannot be
// taken for the start of the answer to the command taken up, which starts at
// sequence id first; empty when they can. The answer's first packet is checked
// to be at first as it is read (packetSeq).
func (w *packetWalk) cut(first byte) string {
	prev := first - 1
	switch {
	case !w.on:
		return "the server's packets were not followed from a packet boundary to it"
	case w.hdrN != 0 || w.rest != 0:
		return "it falls inside a packet of the server's"
	case !w.seen:
		return "no packet of the response left out came before it"
	case w.last == prev:
		return fmt.Sprintf("the last packet before it has sequence id %d, so a packet at %d after it may be more of the response left out", prev, first)
	case w.client && w.clientLast == prev:
		return fmt.Sprintf("the client sent a packet at sequence id %d inside the exchange left out, so a packet at %d after it may be more of that exchange", prev, first)
	}
	return ""
}
