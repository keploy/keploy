package pkg

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"testing"

	"github.com/andybalholm/brotli"
	"go.uber.org/zap"
)

// reserveLedger is a DecompressReserving reserve func's books: what it holds
// now, the most it held, all it ever let the decode hold, and the calls.
type reserveLedger struct {
	held, peak, granted int64
	calls               int
	room                int64 // it refuses n > 0 past this (0: never)
	refused             int
	badGiveBack         bool // a give-back took it below zero
}

func (l *reserveLedger) reserve(n int64) bool {
	l.calls++
	if n > 0 && l.room > 0 && l.held+n > l.room {
		l.refused++
		return false
	}
	l.held += n
	if n > 0 {
		l.granted += n
	}
	if l.held < 0 {
		l.badGiveBack = true
	}
	if l.held > l.peak {
		l.peak = l.held
	}
	return true
}

// textBody is n bytes of JSON-like records with some entropy, so the
// encoders emit several blocks for a large one.
func textBody(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	var b bytes.Buffer
	for b.Len() < n {
		fmt.Fprintf(&b, `{"id":%d,"name":"user-%x","score":%d},`, r.Intn(1e6), r.Int63(), r.Intn(100))
	}
	return b.Bytes()[:n]
}

func gzipOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func brotliOf(t *testing.T, body []byte, quality, lgwin int) []byte {
	t.Helper()
	var b bytes.Buffer
	w := brotli.NewWriterOptions(&b, brotli.WriterOptions{Quality: quality, LGWin: lgwin})
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type encoded struct {
	name, enc string
	wire      []byte
	body      []byte
}

func reserveCases(t *testing.T) []encoded {
	var cs []encoded
	for _, n := range []int{2 << 10, 100 << 10, 512 << 10, 4 << 20} {
		body := textBody(n, int64(n))
		cs = append(cs, encoded{fmt.Sprintf("gzip/%d", n), "gzip", gzipOf(t, body), body})
		for _, q := range []int{1, 6, 11} {
			for _, lgwin := range []int{16, 22} {
				if q == 11 && n > 512<<10 {
					continue // slow, and the window is what matters
				}
				cs = append(cs, encoded{fmt.Sprintf("br-q%d-w%d/%d", q, lgwin, n), "br", brotliOf(t, body, q, lgwin), body})
			}
		}
	}
	return cs
}

// liveHeap is the heap the program holds, after a collection.
func liveHeap() int64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestDecompressReservingHoldsNoByteItHasNotReserved pins
// DecompressReserving's books against the decode: at every point it asks for
// room (each time it grows its output, and as it starts), and once it has
// returned, the heap it holds (its output buffer, the decoder's own state:
// gzip's window and tables, brotli's ring buffer and tables) is within what
// it has reserved; once it has returned what stays reserved is exactly its
// output's capacity; and the output is the body. A decode whose reservations
// were under what it holds would let a caller that bounds its captures'
// memory hold more than its bound while they decode.
func TestDecompressReservingHoldsNoByteItHasNotReserved(t *testing.T) {
	const slack = 16 << 10 // the readers over data, an error value: not the body's
	for _, c := range reserveCases(t) {
		var l reserveLedger
		var worst, worstHeld int64 = -1 << 62, 0
		base := liveHeap()
		probe := func(n int64) bool {
			// What the decode holds now, against what it had reserved
			// before this call.
			if over := liveHeap() - base - l.held; over > worst {
				worst, worstHeld = over, l.held
			}
			return l.reserve(n)
		}
		out, err := DecompressReserving(zap.NewNop(), c.enc, c.wire, 5<<20, probe)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if over := liveHeap() - base - l.held; over > worst {
			worst, worstHeld = over, l.held
		}
		if !bytes.Equal(out, c.body) {
			t.Fatalf("%s: the decoded body differs from the one encoded", c.name)
		}
		if worst > slack {
			t.Errorf("%s: the decode held %d bytes past the %d it had reserved", c.name, worst, worstHeld)
		}
		if l.held != int64(cap(out)) {
			t.Errorf("%s: %d bytes still reserved after the decode, want its output's capacity %d", c.name, l.held, cap(out))
		}
		if l.badGiveBack {
			t.Errorf("%s: the decode gave back more than it reserved", c.name)
		}
		t.Logf("%s: wire %d, body %d, reserved %d at most, held past it at most %d, held after %d", c.name, len(c.wire), len(c.body), l.peak, worst, l.held)
	}
}

// TestADecodeRefusedRoomStopsAndHoldsNothing pins a refusal: the decode
// stops at the first reservation refused, returns ErrDecodeRefused (not a
// decode failure, nor ErrDecompressedTooLarge), holds no more than its room
// at any time, and leaves nothing reserved. So does a body over the limit.
func TestADecodeRefusedRoomStopsAndHoldsNothing(t *testing.T) {
	body := textBody(1<<20, 7)
	for _, c := range []encoded{
		{"gzip", "gzip", gzipOf(t, body), body},
		{"br", "br", brotliOf(t, body, 5, 18), body},
	} {
		for _, room := range []int64{1, 32 << 10, 300 << 10, 900 << 10} {
			l := reserveLedger{room: room}
			out, err := DecompressReserving(zap.NewNop(), c.enc, c.wire, 5<<20, l.reserve)
			if !errors.Is(err, ErrDecodeRefused) || errors.Is(err, ErrDecompressedTooLarge) || out != nil {
				t.Fatalf("%s, room %d: got %d bytes, err %v; want ErrDecodeRefused", c.name, room, len(out), err)
			}
			if l.refused != 1 {
				t.Errorf("%s, room %d: refused %d times, want once: the decode must stop at the first", c.name, room, l.refused)
			}
			if l.peak > room {
				t.Errorf("%s, room %d: held %d at most, past its room", c.name, room, l.peak)
			}
			if l.held != 0 || l.badGiveBack {
				t.Errorf("%s, room %d: %d bytes still reserved after the refusal (bad give-back %v), want 0", c.name, room, l.held, l.badGiveBack)
			}
		}
		l := reserveLedger{}
		if _, err := DecompressReserving(zap.NewNop(), c.enc, c.wire, 256<<10, l.reserve); !errors.Is(err, ErrDecompressedTooLarge) {
			t.Fatalf("%s over the limit: err %v, want ErrDecompressedTooLarge", c.name, err)
		}
		if l.held != 0 || l.badGiveBack {
			t.Errorf("%s over the limit: %d bytes still reserved, want 0", c.name, l.held)
		}
	}
	// What it does not decode, it does not reserve for.
	for _, enc := range []string{"", "identity", "zstd"} {
		l := reserveLedger{room: 1}
		if out, err := DecompressReserving(zap.NewNop(), enc, body, 5<<20, l.reserve); err != nil || len(out) != len(body) || l.calls != 0 {
			t.Errorf("encoding %q: %d bytes, err %v, %d reserve calls; want the body as is and none", enc, len(out), err, l.calls)
		}
	}
}

// TestBrotliWindowBitsReadsTheStreamsWindow pins the window DecompressReserving
// reserves a brotli decode's ring buffer by: the one the encoder wrote.
func TestBrotliWindowBitsReadsTheStreamsWindow(t *testing.T) {
	body := textBody(64<<10, 3)
	for lgwin := 10; lgwin <= 24; lgwin++ {
		got, ok := brotliWindowBits(brotliOf(t, body, 5, lgwin))
		if !ok || int(got) != lgwin {
			t.Errorf("lgwin %d: read %d (ok %v)", lgwin, got, ok)
		}
	}
	if _, ok := brotliWindowBits(nil); ok {
		t.Error("an empty stream declared a window")
	}
}
