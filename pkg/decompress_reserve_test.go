package pkg

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"math/rand"
	"reflect"
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
	badGiveBack         bool    // a give-back took it below zero
	log                 []int64 // every call's n, in order
}

func (l *reserveLedger) reserve(n int64) bool {
	l.calls++
	l.log = append(l.log, n)
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

// manyTreeBody is n bytes made of segments of seg bytes, each drawn from its
// own small skewed alphabet, so a brotli encoder at quality 9 or more splits
// it into many literal block types and clusters their contexts into many
// prefix codes: 120 to 255 literal trees in the first metablock of the
// streams reserveCases makes of it, where JSON text gets 5 or 6.
func manyTreeBody(n, seg int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		base := byte((i * 37) % 240)
		k := 4 + i%12
		for j := 0; j < seg && len(out) < n; j++ {
			s := 0
			for s < k-1 && r.Intn(3) != 0 {
				s++
			}
			out = append(out, base+byte(s))
		}
	}
	return out
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
	// Streams whose metablocks have many prefix codes, so the decoder's
	// tables are past its other state: the encoder's, and the most the
	// format allows.
	cs = append(cs, encoded{"br-most-tables/100000", "br", mostTablesBrotli(100_000), bytes.Repeat([]byte("a"), 100_000)})
	for _, m := range []struct{ n, q, lgwin int }{{1 << 20, 9, 16}, {512 << 10, 9, 18}, {256 << 10, 11, 18}} {
		body := manyTreeBody(m.n, 2048, 2048)
		cs = append(cs, encoded{fmt.Sprintf("br-many-trees-q%d-w%d/%d", m.q, m.lgwin, m.n), "br", brotliOf(t, body, m.q, m.lgwin), body})
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

// settledHeap is liveHeap once what the encoders pooled is gone:
// andybalholm/brotli's encoder keeps its state in sync.Pools, whose victim
// cache lives through one collection, so a base taken one collection after an
// encode counts that state, and a decode measured against it, as the pool is
// emptied under it, looks smaller than it is.
func settledHeap() int64 {
	runtime.GC()
	runtime.GC()
	return liveHeap()
}

// TestDecompressReservingHoldsNoByteItHasNotReserved pins
// DecompressReserving's books against the decode: at every point it asks for
// room (each time it grows its output, and as it starts), and once it has
// returned, the heap it holds (its output buffer, the decoder's own state:
// gzip's window and tables, brotli's ring buffer and prefix-code tables) is
// within what it has reserved; once it has returned what stays reserved is
// exactly its output's capacity; and the output is the body. A decode whose
// reservations were under what it holds would let a caller that bounds its
// captures' memory hold more than its bound while they decode. The
// many-tree brotli streams hold 0.4 to 0.6 MB of tables, so a decode that
// reserved its decoder's state without them fails here.
func TestDecompressReservingHoldsNoByteItHasNotReserved(t *testing.T) {
	const slack = 16 << 10 // the readers over data, an error value: not the body's
	for _, c := range reserveCases(t) {
		var l reserveLedger
		var worst, worstHeld int64 = -1 << 62, 0
		base := settledHeap()
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

// brotliBits writes a brotli stream's bits, each field least significant bit
// first (RFC 7932 §2).
type brotliBits struct {
	buf []byte
	n   uint
}

func (w *brotliBits) bits(n uint, v uint64) {
	for i := uint(0); i < n; i++ {
		if w.n%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		w.buf[len(w.buf)-1] |= byte(v>>i&1) << (w.n % 8)
		w.n++
	}
}

// count writes a count from 1 to 256 as a VarLenUint8 of count-1 (§9.2).
func (w *brotliBits) count(c int) {
	v := uint64(c - 1)
	if v == 0 {
		w.bits(1, 0)
		return
	}
	w.bits(1, 1)
	nb := uint(bits.Len64(v)) - 1
	w.bits(3, uint64(nb))
	w.bits(nb, v-1<<nb)
}

// oneSymbolCode writes a simple prefix code of one symbol from an alphabet
// of size symbols (§3.4): the symbol then takes no bits.
func (w *brotliBits) oneSymbolCode(size, sym int) {
	w.bits(2, 1) // HSKIP 1: a simple code
	w.bits(2, 0) // NSYM - 1
	w.bits(uint(bits.Len(uint(size-1))), uint64(sym))
}

// mostTablesBrotli is a valid brotli stream of n bytes of 'a' (22,594 <= n
// < 2^24) whose one metablock declares the most prefix codes the format
// allows: 256 literal trees, 256 insert-and-copy block types (so 256 of
// their trees), 256 distance trees over the largest distance alphabet (520
// codes: NPOSTFIX 3, NDIRECT 120). Every tree is a one-symbol code, so the
// whole stream is about 1.3 KB, and its one command inserts the n literals.
// It makes a decoder hold the most tables a stream can.
func mostTablesBrotli(n int) []byte {
	return tablesBrotli(n, 256)
}

// tablesBrotli is a valid brotli stream of one metablock per entry of
// commandTrees, each of n bytes of 'a' (22,594 <= n < 2^24), each shaped as
// mostTablesBrotli's but with that many insert-and-copy block types (1 to
// 256), so as many of their trees.
func tablesBrotli(n int, commandTrees ...int) []byte {
	var w brotliBits
	w.bits(1, 0) // WBITS: a 64 KiB window
	for i, trees := range commandTrees {
		last := i == len(commandTrees)-1
		w.bits(1, map[bool]uint64{true: 1}[last]) // ISLAST
		if last {
			w.bits(1, 0) // ISLASTEMPTY
		}
		mlen := uint64(n - 1)
		nibbles := (bits.Len64(mlen) + 3) / 4
		if nibbles < 4 {
			nibbles = 4
		}
		w.bits(2, uint64(nibbles-4)) // MNIBBLES
		w.bits(uint(4*nibbles), mlen)
		if !last {
			w.bits(1, 0) // ISUNCOMPRESSED
		}
		w.count(1)     // literal block types
		w.count(trees) // insert-and-copy block types
		if trees > 1 {
			w.oneSymbolCode(trees+2, 0) // their block-type code
			w.oneSymbolCode(26, 0)      // their block-count code: count code 0,
			w.bits(2, 0)                // and its extra bits: a first block of 1
		}
		w.count(1)    // distance block types
		w.bits(2, 3)  // NPOSTFIX
		w.bits(4, 15) // NDIRECT >> NPOSTFIX
		w.bits(2, 0)  // the literal block type's context mode
		for range 2 { // the literal context map, then the distance one
			w.count(256)            // trees
			w.bits(1, 0)            // no run-length codes
			w.oneSymbolCode(256, 0) // every context uses tree 0
			w.bits(1, 0)            // no move-to-front
		}
		for i := range 256 {
			w.oneSymbolCode(256, map[bool]int{true: 'a'}[i == 0])
		}
		for i := range trees {
			// Tree 0's symbol, 504, is insert code 23 (22,594 and 24 extra
			// bits) and copy code 0.
			w.oneSymbolCode(704, map[bool]int{true: 504}[i == 0])
		}
		for range 256 {
			w.oneSymbolCode(520, 0)
		}
		w.bits(24, uint64(n-22594)) // the one command's insert length; its literals take no bits
	}
	return w.buf
}

// TestBrotliDecoderStateIsReservedAtTheFormatsLimits pins what a brotli
// decode reserves for its decoder (brotliReaderBytes and brotliTableBytes)
// against github.com/andybalholm/brotli's own, on a stream of about 1.3 KB
// that declares the most prefix codes the format allows: the tables it
// allocates are brotliTableBytes exactly, and the heap it holds once it has
// begun is within the reservation. A version of the decoder that sizes its
// state or its tables otherwise fails here, and the constants must be
// recomputed from it.
func TestBrotliDecoderStateIsReservedAtTheFormatsLimits(t *testing.T) {
	const maxBlockTypes = 256 // RFC 7932 §6: NBLTYPESL, NBLTYPESI, NBLTYPESD
	wire := mostTablesBrotli(100_000)
	base := settledHeap()
	rd := brotli.NewReader(bytes.NewReader(wire))
	if _, err := rd.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	held := liveHeap() - base
	v := reflect.ValueOf(rd).Elem()
	field := func(v reflect.Value, name string) reflect.Value {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("brotli.Reader has no field %q: recompute brotliReaderBytes and brotliTableBytes from this version of the decoder", name)
		}
		return f
	}
	lit, cmd, dist := field(v, "literal_hgroup"), field(v, "insert_copy_hgroup"), field(v, "distance_hgroup")
	if got := int64(field(lit, "codes").Type().Elem().Size()); got != brotliTableEntryBytes {
		t.Fatalf("a table entry is %d bytes, brotliTableEntryBytes says %d", got, brotliTableEntryBytes)
	}
	for _, g := range []struct {
		name    string
		group   reflect.Value
		entries int
	}{
		{"literal", lit, brotliLiteralTableEntries},
		{"insert-and-copy", cmd, brotliCommandTableEntries},
		{"distance", dist, brotliDistanceTableEntries},
	} {
		if got, want := field(g.group, "codes").Len(), brotliMaxTreesPerGroup*g.entries; got != want {
			t.Errorf("the %s group's tables are %d entries, want %d: %d trees at %d", g.name, got, want, brotliMaxTreesPerGroup, g.entries)
		}
	}
	// The rest: its state, its input buffer, its block-type trees, and one
	// metablock's context maps (64 literal and 4 distance contexts per block
	// type, and a mode per literal block type) and tree headers, with one
	// group's headers again for while it regrows them (makeTreesBuffer
	// allocates the larger while it holds the smaller, as it does the tables).
	header := int64(field(lit, "htrees").Type().Elem().Size())
	rest := int64(v.Type().Size()) +
		int64(field(v, "buf").Len()) +
		int64(field(v, "block_type_trees").Len())*brotliTableEntryBytes +
		maxBlockTypes<<6 + maxBlockTypes<<2 + maxBlockTypes +
		4*brotliMaxTreesPerGroup*header
	if rest > brotliReaderBytes {
		t.Errorf("the decoder's state, buffer, block-type trees, context maps and tree headers are %d bytes, past brotliReaderBytes %d", rest, brotliReaderBytes)
	}
	reserved := brotliReaderBytes + brotliTableBytes + brotliTableRegrowBytes + brotliRingBytes(16)
	if held < brotliTableBytes || held > reserved {
		t.Errorf("the decoder held %d bytes once it had begun; want its tables (%d) and no more than a decode reserves for it (%d)", held, brotliTableBytes, reserved)
	}
	runtime.KeepAlive(rd)
	t.Logf("a %d-byte stream: the decoder held %d bytes once it had begun (reserved %d); its tables %d, the rest %d (brotliReaderBytes %d)", len(wire), held, reserved, brotliTableBytes, rest, brotliReaderBytes)
}

// TestABrotliDecodeReservesTheTablesItsDecoderRegrows pins
// brotliTableRegrowBytes against github.com/andybalholm/brotli's own: a
// stream whose first metablock declares 255 insert-and-copy trees and whose
// second declares 256 makes the decoder regrow that group's tables, and it
// allocates the larger buffer while it still holds the smaller one. Read a
// few KiB at a time, the decoder's three groups are read after each read
// (each metablock is 200,000 bytes and the window 64 KiB, so the decoder
// cannot run ahead into the second before most of the first is read): when
// a group's buffer is replaced by a larger one, the tables it held as it
// allocated it were the other groups', the new buffer and the one it
// replaced. That is within brotliTableBytes and brotliTableRegrowBytes, and
// past brotliTableBytes alone, so a decode that reserved one set of tables
// would hold more than it had reserved. A decoder version that regrows no
// table on this stream fails the row, and the constant must be recomputed
// from it. The decode itself, through DecompressReserving, is the body, and
// gives back all but its output's capacity.
func TestABrotliDecodeReservesTheTablesItsDecoderRegrows(t *testing.T) {
	const n = 200_000
	wire := tablesBrotli(n, 255, 256)
	body := bytes.Repeat([]byte{'a'}, 2*n)
	var l reserveLedger
	out, err := DecompressReserving(zap.NewNop(), "br", wire, 5<<20, l.reserve)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatalf("%d bytes, err %v; want the %d-byte body", len(out), err, len(body))
	}
	if l.held != int64(cap(out)) || l.badGiveBack {
		t.Fatalf("%d bytes reserved after the decode (bad give-back %v), want its output's capacity %d", l.held, l.badGiveBack, cap(out))
	}
	state := l.log[0] // what the decode reserved for its decoder, as it started

	rd := brotli.NewReader(bytes.NewReader(wire))
	v := reflect.ValueOf(rd).Elem()
	var groups [3]reflect.Value
	for i, name := range []string{"literal_hgroup", "insert_copy_hgroup", "distance_hgroup"} {
		g := v.FieldByName(name)
		if !g.IsValid() || !g.FieldByName("codes").IsValid() {
			t.Fatalf("brotli.Reader has no %s.codes: recompute brotliTableBytes and brotliTableRegrowBytes from this version of the decoder", name)
		}
		groups[i] = g.FieldByName("codes")
	}
	type buffer struct {
		at    uintptr
		bytes int64
	}
	read := func() (b [3]buffer) {
		for i, c := range groups {
			b[i] = buffer{c.Pointer(), int64(c.Cap()) * brotliTableEntryBytes}
		}
		return b
	}
	var prev [3]buffer
	var regrown int
	var most int64
	p := make([]byte, 4<<10)
	for got := 0; ; {
		k, err := rd.Read(p)
		got += k
		if err == io.EOF {
			if got != len(body) {
				t.Fatalf("the decoder ended after %d bytes, want %d", got, len(body))
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cur := read()
		var held int64
		for i := range cur {
			held += cur[i].bytes
		}
		for i := range cur {
			if prev[i].bytes > 0 && cur[i].at != prev[i].at && cur[i].bytes > prev[i].bytes {
				regrown++
				held += prev[i].bytes // it held the one it replaced while it allocated this one
			}
		}
		most = max(most, held)
		prev = cur
	}
	if regrown != 1 {
		t.Fatalf("the decoder regrew a table %d times on this stream, want once: recompute brotliTableRegrowBytes from this version of the decoder", regrown)
	}
	if most <= brotliTableBytes {
		t.Errorf("the decoder held %d bytes of tables at most, no more than brotliTableBytes %d: this stream no longer regrows past one set", most, brotliTableBytes)
	}
	if most > brotliTableBytes+brotliTableRegrowBytes {
		t.Errorf("the decoder held %d bytes of tables as it regrew them, past brotliTableBytes %d and brotliTableRegrowBytes %d", most, brotliTableBytes, brotliTableRegrowBytes)
	}
	if ring := brotliRingBytes(16); state < most+ring {
		t.Errorf("the decode reserved %d bytes for its decoder, less than the %d of tables it held as it regrew them and the %d of its ring", state, most, ring)
	}
	t.Logf("a %d-byte stream of two metablocks: the decoder held %d bytes of tables as it regrew them; the decode reserved %d for its decoder", len(wire), most, state)
}

// TestADecodeAsksForEachGrownBufferWholeBeforeItGivesTheOldOneBack pins the
// copy as the output grows: while it copies the buffer it outgrew into the
// new one it holds both, so it asks for the new buffer's whole capacity, not
// the growth, and gives the old one back only once the new one is made and
// the old one copied into it. A decode that asked for the growth alone would
// end with the same books (its output's capacity) and hold half as much
// again as it had reserved while it copied. One that gave the old buffer
// back before it made the new one would make the same calls, and hold both
// with the old one's room given back while it made and filled the new one:
// at each give-back the heap must already hold the new buffer whole.
func TestADecodeAsksForEachGrownBufferWholeBeforeItGivesTheOldOneBack(t *testing.T) {
	body := textBody(600<<10, 11) // grows 64 KiB to 1 MiB, and ends short of it
	for _, c := range []encoded{
		{"gzip", "gzip", gzipOf(t, body), body},
		{"br", "br", brotliOf(t, body, 5, 18), body},
	} {
		var l reserveLedger
		var early []string
		base := settledHeap()
		probe := func(n int64) bool {
			// A give-back of the buffer just outgrown: the call before it
			// asked for the new one, twice its size.
			if k := len(l.log); n < 0 && k > 0 && l.log[k-1] == -2*n {
				if held := liveHeap() - base; held < -2*n {
					early = append(early, fmt.Sprintf("gave the %d-byte buffer back holding %d bytes, short of the %d-byte one it grew into", -n, held, -2*n))
				}
			}
			return l.reserve(n)
		}
		out, err := DecompressReserving(zap.NewNop(), c.enc, c.wire, 5<<20, probe)
		if err != nil || !bytes.Equal(out, c.body) {
			t.Fatalf("%s: %d bytes, err %v; want the body", c.name, len(out), err)
		}
		for _, e := range early {
			t.Errorf("%s: %s", c.name, e)
		}
		if len(l.log) == 0 || l.log[0] <= 0 {
			t.Fatalf("%s: calls %v; want the decoder's state asked for first", c.name, l.log)
		}
		state := l.log[0]
		want := []int64{state, 64 << 10}
		for buf := int64(64 << 10); buf < int64(len(body)); buf *= 2 {
			want = append(want, 2*buf, -buf)
		}
		want = append(want, -state)
		if fmt.Sprint(l.log) != fmt.Sprint(want) {
			t.Errorf("%s: asked and gave back %v, want %v", c.name, l.log, want)
		}
	}
}
