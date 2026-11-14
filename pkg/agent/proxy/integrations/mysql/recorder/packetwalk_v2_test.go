package recorder

import (
	"strings"
	"testing"
)

// walked feeds w the packets, each split at the given byte offsets of the
// stream: a header or a payload can span the chunks the capture read.
func walked(w *packetWalk, stream []byte, at ...int) {
	from := 0
	for _, to := range append(at, len(stream)) {
		w.feed(stream[from:to])
		from = to
	}
}

// A packet's header gives its length, so the walk follows the headers of a
// stream exactly wherever no byte is lost, however the capture split it, and
// the cut after a response left out is taken only at a packet boundary, after
// a packet of that response, where a packet at the answer's first sequence id
// cannot be numbered on from the one before it: neither the server's last nor
// the client's last inside the exchange.
func TestPacketWalkTakesACutOnlyWhereTheResponseLeftOutEnded(t *testing.T) {
	t.Parallel()
	run := func(ids ...byte) []byte {
		var b []byte
		for _, id := range ids {
			b = append(b, wrapPacket([]byte("row"), id)...)
		}
		return b
	}
	// A run of packets from 1 through 255, and on from 0: the sequence wraps
	// every 256 packets.
	wrapped := func(after ...byte) []byte {
		var ids []byte
		for id := 1; id <= 255; id++ {
			ids = append(ids, byte(id))
		}
		return run(append(ids, after...)...)
	}
	for _, c := range []struct {
		name string
		// unanchored: the walk was never put at a packet boundary.
		unanchored bool
		// start is the sequence id the response left out starts at (1 when
		// zero).
		start byte
		// unproven: the exchange left out was taken up at a cut nothing
		// proved (packetWalk.cut).
		unproven bool
		// stream is what left the server's stream since the answer began,
		// split at the offsets in split.
		stream []byte
		split  []int
		// client: the ids of the client's packets inside the exchange.
		client []byte
		first  byte
		want   string
	}{
		{name: "the response ended at a packet before the cut", stream: run(1, 2, 3), split: []int{2, 9, 11}, first: 1},
		{name: "a header split across chunks", stream: run(1, 2), split: []int{1, 2, 3}, first: 1},
		{name: "never at a packet boundary", unanchored: true, stream: run(1, 2), first: 1, want: "not followed from a packet boundary"},
		{name: "the cut in a header", stream: run(1, 2)[:9], first: 1, want: "inside a packet"},
		{name: "the cut in a payload", stream: run(1, 2)[:13], first: 1, want: "inside a packet"},
		{name: "nothing of the response before the cut", first: 1, want: "no packet of the response left out"},
		{name: "a wrapped run", stream: wrapped(0), first: 1, want: "sequence id 0, so a packet at 1"},
		{name: "a run that wrapped and went on", stream: wrapped(0, 1), first: 1},
		{name: "an answer that starts past 1", stream: run(1, 2), first: 3, want: "sequence id 2, so a packet at 3"},
		// The tail of a response before the one left out, captured after the
		// command whose answer was left out, which was taken up at a cut
		// nothing proved: what it ends at says nothing of where the response
		// left out ended.
		{name: "a first packet off the response's start", unproven: true, stream: run(3), first: 1, want: "first packet walked since the response left out began has sequence id 3, not 1"},
		{name: "a response left out that starts past 1", unproven: true, start: 3, stream: run(3, 4), first: 1},
		// After an answer read whole, nothing of a response before it is
		// left: the first packet is the response's own, off its sequence, and
		// what follows shows where it ended.
		{name: "a first packet off its start after an answer read whole", stream: run(3), first: 1},
		{name: "a run off its start after an answer read whole that ends at 0", stream: run(3, 0), first: 1, want: "sequence id 0, so a packet at 1"},
		{name: "the client's upload ended at 0", stream: run(1), client: []byte{254, 255, 0}, first: 1, want: "client sent a packet at sequence id 0"},
		{name: "the client's upload ended past 0", stream: run(1), client: []byte{255, 0, 1}, first: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var w packetWalk
			if !c.unanchored {
				w.anchor()
			}
			// A packet before the answer: what the mark forgets.
			w.feed(wrapPacket([]byte("ok"), 0))
			w.passed(0)
			start := c.start
			if start == 0 {
				start = 1
			}
			w.mark(start)
			walked(&w, c.stream, c.split...)
			for _, id := range c.client {
				w.passed(id)
			}
			got := w.cut(c.first, c.unproven)
			if c.want == "" && got != "" || !strings.Contains(got, c.want) {
				t.Fatalf("cut(%d) = %q, want %q", c.first, got, c.want)
			}
		})
	}
}

// An anchor puts the walk at a packet boundary: whatever it was in the middle
// of is behind it.
func TestPacketWalkAnchorsAtAPacketBoundary(t *testing.T) {
	t.Parallel()
	var w packetWalk
	w.anchor()
	w.mark(1)
	w.feed(wrapPacket([]byte("row"), 1)[:6])
	if got := w.cut(1, false); !strings.Contains(got, "inside a packet") {
		t.Fatalf("cut(1) = %q in a packet", got)
	}
	w.anchor()
	w.feed(wrapPacket([]byte("row"), 7))
	if got := w.cut(1, false); got != "" {
		t.Fatalf("cut(1) = %q after the anchor and a packet at 7", got)
	}
}

// Where the answer the stream is at may start with an ERR 1835 to a command
// without an answer that did not decode, sent before its own command
// (markUnjudged), a first packet that is one shows nothing of where the
// response left out ended, and the cut is refused. Any other first packet, an
// ERR 1835 after it, and an ERR 1835 after a plain mark (the next exchange's
// among them) are judged as at any cut. The walk judges the first packet since
// the mark by its own first payload bytes, however the capture split them,
// and never by what it kept of a packet before the mark.
func TestPacketWalkRefusesACutAtAnUnjudgedERR1835(t *testing.T) {
	t.Parallel()
	const refused = "is an ERR 1835"
	for _, c := range []struct {
		name string
		// unjudged: the answer was marked with markUnjudged; remarked: the
		// walk was then marked again, plainly, for the next exchange.
		unjudged, remarked bool
		stream             []byte
		split              []int
		want               string
	}{
		{name: "an ERR 1835 first", unjudged: true, stream: malformedERR, want: refused},
		{name: "an ERR 1835 first, split in its header and its code", unjudged: true, stream: malformedERR, split: []int{2, 5, 6}, want: refused},
		{name: "an ERR 1835 after a plain mark", stream: malformedERR},
		{name: "an ERR 1835 after the next exchange's plain mark", unjudged: true, remarked: true, stream: malformedERR},
		{name: "an ERR of another code first", unjudged: true, stream: errReply(1, 1105, "unknown")},
		{name: "an OK first", unjudged: true, stream: okResponseWithAffectedRows(1, 1)},
		{name: "an ERR 1835 after an OK", unjudged: true, stream: append(okResponseWithAffectedRows(1, 1), errReply(2, 1835, "Malformed communication packet")...)},
		{name: "an ERR 1835's code bytes after another first byte", unjudged: true, stream: wrapPacket([]byte{0x00, 0x2b, 0x07}, 1)},
		{name: "a first packet shorter than an ERR's code", unjudged: true, stream: wrapPacket([]byte{0xff, 0x2b}, 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var w packetWalk
			w.anchor()
			// An ERR 1835 before the answer: the first packet the walk saw
			// before the mark, whose bytes it kept.
			w.feed(malformedERR)
			if c.unjudged {
				w.markUnjudged(1)
			} else {
				w.mark(1)
			}
			if c.remarked {
				w.mark(1)
			}
			if w.firstIsMalformedErr() {
				t.Fatal("firstIsMalformedErr() with no packet walked since the mark")
			}
			walked(&w, c.stream, c.split...)
			got := w.cut(1, false)
			if c.want == "" && got != "" || !strings.Contains(got, c.want) {
				t.Fatalf("cut(1) = %q, want %q", got, c.want)
			}
		})
	}
}
