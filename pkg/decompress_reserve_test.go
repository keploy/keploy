package pkg

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/bits"
	"math/rand"
	"path"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
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
	// A gzip stream whose two members make its decoder hold the most the
	// format allows (mostStateGzipMember).
	m, _ := mostStateGzipMember(100_000)
	cs = append(cs, encoded{"gzip-most-state/200000", "gzip", append(append([]byte{}, m...), m...), bytes.Repeat([]byte("a"), 200_000)})
	for _, m := range []struct{ n, q, lgwin int }{{1 << 20, 9, 16}, {512 << 10, 9, 18}, {256 << 10, 11, 18}} {
		body := manyTreeBody(m.n, 2048, 2048)
		cs = append(cs, encoded{fmt.Sprintf("br-many-trees-q%d-w%d/%d", m.q, m.lgwin, m.n), "br", brotliOf(t, body, m.q, m.lgwin), body})
	}
	return cs
}

// heldSlack is what a decode may hold past what it reserved, as stackHeap
// counts it: small values that are neither the body nor the decoder's
// state, such as the reader over the data.
const heldSlack = 512

// countEveryAllocation makes the runtime's heap profile record every
// allocation and every free, with its stack (MemProfileRate 1), for the rest
// of the row, and turns the collector's own cycles off for it (GC percent
// -1): the row's collections (stackHeap.read's) are then the only ones, so
// no cycle runs while a decode does, and the runtime takes no sudog on the
// decode's stack to assist one. It restores both at the row's end.
func countEveryAllocation(t *testing.T) {
	t.Helper()
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	percent := debug.SetGCPercent(-1)
	t.Cleanup(func() {
		debug.SetGCPercent(percent)
		runtime.MemProfileRate = rate
	})
}

// gcPercent is the collector's GC percent (debug.SetGCPercent), as it is.
func gcPercent() int {
	p := debug.SetGCPercent(-1)
	debug.SetGCPercent(p)
	return p
}

// TestEveryAllocationIsRecordedWithNoCollectionButTheRowsOwn pins
// countEveryAllocation: in the row, the heap profile records every
// allocation and the collector runs no cycle of its own (GC percent -1);
// once the row has ended, both are as they were.
func TestEveryAllocationIsRecordedWithNoCollectionButTheRowsOwn(t *testing.T) {
	rate, percent := runtime.MemProfileRate, gcPercent()
	t.Run("row", func(t *testing.T) {
		countEveryAllocation(t)
		if runtime.MemProfileRate != 1 || gcPercent() != -1 {
			t.Errorf("in the row: MemProfileRate %d, GC percent %d; want 1 and -1", runtime.MemProfileRate, gcPercent())
		}
	})
	if runtime.MemProfileRate != rate || gcPercent() != percent {
		t.Errorf("after the row: MemProfileRate %d, GC percent %d; want them as before, %d and %d", runtime.MemProfileRate, gcPercent(), rate, percent)
	}
}

// stackHeap counts what a row's decode holds, not what the process holds,
// from the heap profile under countEveryAllocation. It counts every
// allocation of the window (from open to held) but three kinds:
//   - one whose stack is made only of the runtime's frames (a frame is the
//     runtime's when its file is in the runtime's own directory, as
//     runtimeFrame reads it: the runtime names some of its closures, such
//     as goready's, "_.goready.func1"). The runtime allocates on its own
//     stacks while a decode runs and keeps what it allocated: each OS
//     thread it starts holds about 5.5 KB for as long as the thread lives
//     (its m, two g's, two profiling stacks), and a GC worker takes sudogs;
//     the more loaded the host, the more threads it starts. On a host at
//     2.4 load per CPU the process heap held 21 KB past one decode's
//     reservations.
//   - one the runtime makes for a cache it keeps for the process, read
//     outward from the allocation before any other frame decides
//     (runtimeCacheFrame): the type-assertion and type-switch caches it
//     builds, on the stack of the code that asserts, about one assertion
//     in 1,024, so on a decode's stack too.
//   - one whose stack, read outward from the allocation, meets a frame of
//     this file other than root first: a reserve func a row hands the
//     decode, a row's books, the count itself.
//
// Any other allocation counts: the decode's (through root, a function of
// this file), one a decoder makes on a goroutine of its own, and any other
// code's, so what the count leaves out is only what is shown not to be the
// decode's. Each stack counts what it holds past what it held as the window
// opened, and never less than nothing: a free of what it held before says
// nothing of the window.
type stackHeap struct {
	root      string               // the function the decode's stacks run through
	file      string               // this file, as the runtime names it
	allocated bool                 // count every byte allocated in the window, freed or not
	seen      map[[32]uintptr]bool // a stack's verdict, once read
	base      map[[32]uintptr]int64
	recs      []runtime.MemProfileRecord
}

func newStackHeap(root string) *stackHeap {
	_, file, _, _ := runtime.Caller(0)
	return &stackHeap{root: root, file: file, seen: map[[32]uintptr]bool{}}
}

// funcName is the name the runtime gives fn's frames.
func funcName(fn any) string {
	return runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
}

// countedDecode is the rows' call of DecompressReserving: the stacks
// stackHeap counts as the decode's run through it.
//
//go:noinline
func countedDecode(enc string, wire []byte, limit int64, reserve func(int64) bool) ([]byte, error) {
	return DecompressReserving(nopDecodeLogger, enc, wire, limit, reserve)
}

var nopDecodeLogger = zap.NewNop()

// read is what each counted stack holds now. It collects first: runtime.GC
// returns once its sweep is done and it has published the heap profile, so
// the profile then holds every allocation made before it and every free of
// what was dropped.
func (h *stackHeap) read() map[[32]uintptr]int64 {
	runtime.GC()
	for {
		n, ok := runtime.MemProfile(h.recs[:cap(h.recs)], h.allocated)
		if ok {
			h.recs = h.recs[:n]
			break
		}
		h.recs = make([]runtime.MemProfileRecord, 0, n+n/4+64)
	}
	return h.byStack(h.recs)
}

// byStack is what recs hold, by stack, of the stacks h counts: the bytes in
// use, or every byte allocated when h counts allocations.
func (h *stackHeap) byStack(recs []runtime.MemProfileRecord) map[[32]uintptr]int64 {
	by := map[[32]uintptr]int64{}
	for i := range recs {
		r := &recs[i]
		v, ok := h.seen[r.Stack0]
		if !ok {
			v = h.counts(r.Stack())
			h.seen[r.Stack0] = v
		}
		if !v {
			continue
		}
		if h.allocated {
			by[r.Stack0] += r.AllocBytes
		} else {
			by[r.Stack0] += r.InUseBytes()
		}
	}
	return by
}

// open begins the window.
func (h *stackHeap) open() { h.base = h.read() }

// held is what the counted stacks hold now past what they held as the
// window opened.
func (h *stackHeap) held() int64 { return pastBase(h.read(), h.base) }

// pastBase sums what each stack holds in now past what it held in base,
// never less than nothing.
func pastBase(now, base map[[32]uintptr]int64) int64 {
	var held int64
	for s, n := range now {
		if d := n - base[s]; d > 0 {
			held += d
		}
	}
	return held
}

// counts reports whether an allocation of this stack is counted, as
// countsFrames reads its frames.
func (h *stackHeap) counts(stack []uintptr) bool {
	frames := runtime.CallersFrames(stack)
	return h.countsFrames(frames.Next)
}

// countsFrames reads a stack's frames, from next, outward from the
// allocation: root counts it; another frame of this file first leaves it
// out, and so does a frame of the runtime's caches (runtimeCacheFrame); a
// stack of the runtime's frames alone (runtimeFrame) is left out. A stack
// the profile cut at its 32 innermost frames is read as it was kept, so one
// cut inside a decoder counts.
func (h *stackHeap) countsFrames(next func() (runtime.Frame, bool)) bool {
	runtimeOnly := true
	for {
		f, more := next()
		if f.Function == h.root {
			return true
		}
		if f.File == h.file || runtimeCacheFrame(f) {
			return false
		}
		runtimeOnly = runtimeOnly && runtimeFrame(f)
		if !more {
			return !runtimeOnly
		}
	}
}

// runtimeDir is the directory of the runtime's own source files, as the
// binary names them: GOROOT/src/runtime, or runtime when built with
// -trimpath. It is read from a function of the runtime.
var runtimeDir = func() string {
	pc := reflect.ValueOf(runtime.GC).Pointer()
	file, _ := runtime.FuncForPC(pc).FileLine(pc)
	return path.Dir(file)
}()

// runtimeFrame reports whether f is a frame of the runtime's own code: its
// file is in runtimeDir itself (not in a package under it, such as
// runtime/pprof). The file decides, not the name: the runtime names some of
// its closures without its package's prefix.
func runtimeFrame(f runtime.Frame) bool {
	return path.Dir(f.File) == runtimeDir
}

// runtimeCacheFrame reports whether f is the runtime building a cache it
// keeps for the process: a call site's type-assertion or type-switch cache.
// An allocation made under it is that cache, whoever asserted.
func runtimeCacheFrame(f runtime.Frame) bool {
	return runtimeFrame(f) && (f.Function == "runtime.buildTypeAssertCache" || f.Function == "runtime.buildInterfaceSwitchCache")
}

// TestDecompressReservingHoldsNoByteItHasNotReserved pins
// DecompressReserving's books against the decode: at every point it asks for
// room (each time it grows its output, and as it starts), and once it has
// returned, what it holds (its output buffer, the decoder's own state: gzip's
// window, tables and headers, brotli's ring buffer and prefix-code tables, as
// stackHeap counts them) is within what it has reserved; once it has returned
// what stays reserved is exactly its output's capacity; and the output is the
// body. A decode whose reservations were under what it holds would let a
// caller that bounds its captures' memory hold more than its bound while they
// decode. The many-tree brotli streams hold 0.4 to 0.6 MB of tables, and the
// gzip stream at the format's limits 108 KB of headers and 40 KB of link
// tables, so a decode that reserved its decoder's state without them fails
// here.
func TestDecompressReservingHoldsNoByteItHasNotReserved(t *testing.T) {
	cases := reserveCases(t)
	countEveryAllocation(t)
	decode := newStackHeap(funcName(countedDecode))
	for _, c := range cases {
		var l reserveLedger
		var worst, worstHeld int64 = -1 << 62, 0
		decode.open()
		probe := func(n int64) bool {
			// What the decode holds now, against what it had reserved
			// before this call.
			if over := decode.held() - l.held; over > worst {
				worst, worstHeld = over, l.held
			}
			return l.reserve(n)
		}
		out, err := countedDecode(c.enc, c.wire, 5<<20, probe)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if over := decode.held() - l.held; over > worst {
			worst, worstHeld = over, l.held
		}
		if !bytes.Equal(out, c.body) {
			t.Fatalf("%s: the decoded body differs from the one encoded", c.name)
		}
		if worst > heldSlack {
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

// TestADecodeHoldsTheCapacityItReserves pins the output buffers' size: each
// is whole heap pages, the last one too (limit+1 bytes rounded up to whole
// pages, and no more), so the allocator gives a buffer exactly the capacity
// the decode reserved for it (as stackHeap counts what it holds once it has
// returned), and the decode reads no more than limit+1 bytes. A body of the
// limit decodes; one a byte past it is too large.
func TestADecodeHoldsTheCapacityItReserves(t *testing.T) {
	countEveryAllocation(t)
	decode := newStackHeap(funcName(countedDecode))
	for _, c := range []struct{ limit, last int64 }{
		{100, 8 << 10}, {8 << 10, 16 << 10}, {20_000, 24 << 10}, {64<<10 - 1, 64 << 10}, {100_000, 104 << 10}, {300_000, 296 << 10},
	} {
		limit := c.limit
		for _, n := range []int64{limit, limit + 1} {
			body := textBody(int(n), n)
			var l reserveLedger
			decode.open()
			out, err := countedDecode("gzip", gzipOf(t, body), limit, l.reserve)
			held := decode.held()
			if n > limit {
				if !errors.Is(err, ErrDecompressedTooLarge) || l.held != 0 {
					t.Errorf("limit %d, body %d: err %v, %d bytes still reserved; want ErrDecompressedTooLarge and none", limit, n, err, l.held)
				}
				continue
			}
			if err != nil || !bytes.Equal(out, body) {
				t.Fatalf("limit %d, body %d: %d bytes, err %v; want the body", limit, n, len(out), err)
			}
			if got := int64(cap(out)); got != c.last || l.held != got {
				t.Errorf("limit %d: capacity %d, %d bytes reserved; want %d (%d bytes in whole pages), all reserved", limit, got, l.held, c.last, limit+1)
			}
			if held < int64(cap(out)) || held > int64(cap(out))+heldSlack {
				t.Errorf("limit %d: the decode holds %d bytes once it has returned, want its output's capacity %d and at most %d more", limit, held, cap(out), heldSlack)
			}
			runtime.KeepAlive(out)
		}
	}
	// Of a longer stream it reads limit+1 bytes, and no more, though its
	// last buffer may hold more.
	for _, limit := range []int64{100, 20_000, 100_000} {
		r := &countingReader{r: bytes.NewReader(make([]byte, 3*limit))}
		if _, err := readAllReserved(r, limit, nil); !errors.Is(err, ErrDecompressedTooLarge) || int64(r.n) != limit+1 {
			t.Errorf("limit %d: read %d bytes of a %d-byte stream, err %v; want %d and ErrDecompressedTooLarge", limit, r.n, 3*limit, err, limit+1)
		}
	}
}

// countedRead is the rows' call of readAllReserved alone, over a reader that
// holds nothing: what its calls hold is its output buffers.
//
//go:noinline
func countedRead(r io.Reader, limit int64, reserve func(int64) bool) ([]byte, error) {
	return readAllReserved(r, limit, reserve)
}

// TestAReadHoldsNoByteButTheBuffersItReserved pins readAllReserved's books
// with no decoder's state beside them (a decode reserves that at the most its
// stream can make it hold, so what it holds of it is mostly less): at every
// point it asks for room, what it holds (as stackHeap counts countedRead's
// calls) is within what it has reserved; all it allocates in the read
// (counted by allocation, which only grows, so a buffer it drops between
// two reserve calls counts too) is within all it was let hold; and once it
// has returned it holds its output's capacity. A read that allocated any
// buffer of its own past the ones it reserved fails here.
func TestAReadHoldsNoByteButTheBuffersItReserved(t *testing.T) {
	countEveryAllocation(t)
	read, made := newStackHeap(funcName(countedRead)), newStackHeap(funcName(countedRead))
	made.allocated = true
	for _, c := range []struct{ n, limit int64 }{{600 << 10, 5 << 20}, {300_000, 300_000}, {2_000, 2_000}} {
		body := textBody(int(c.n), c.n)
		src := bytes.NewReader(body)
		var l reserveLedger
		var worst int64
		read.open()
		made.open()
		probe := func(n int64) bool {
			worst = max(worst, read.held()-l.held)
			return l.reserve(n)
		}
		out, err := countedRead(src, c.limit, probe)
		held, allocated := read.held(), made.held()
		if err != nil || !bytes.Equal(out, body) {
			t.Fatalf("%d bytes, limit %d: read %d bytes, err %v; want the body", c.n, c.limit, len(out), err)
		}
		if worst > heldSlack {
			t.Errorf("%d bytes, limit %d: the read held %d bytes past what it had reserved", c.n, c.limit, worst)
		}
		if allocated < l.granted || allocated > l.granted+heldSlack {
			t.Errorf("%d bytes, limit %d: the read allocated %d bytes, %d granted in all; want its buffers, all granted, and at most %d more", c.n, c.limit, allocated, l.granted, heldSlack)
		}
		if l.held != int64(cap(out)) || held < l.held || held > l.held+heldSlack {
			t.Errorf("%d bytes, limit %d: the read holds %d bytes once it has returned, %d reserved; want its output's capacity %d and at most %d more", c.n, c.limit, held, l.held, cap(out), heldSlack)
		}
		runtime.KeepAlive(out)
	}
}

// TestAReadTakesWholePagesUpToTheLargestLimit pins wholePages at the ends of
// an int64: it rounds up to whole pages, and an n past the most whole pages
// an int64 holds is that most, never a wrapped, negative size. A read under
// a limit that close to math.MaxInt64 (which no buffer reaches) still grows
// its buffers in whole pages, reserved, and returns the body.
func TestAReadTakesWholePagesUpToTheLargestLimit(t *testing.T) {
	if maxWholePages%heapPageBytes != 0 || math.MaxInt64-maxWholePages >= heapPageBytes {
		t.Fatalf("maxWholePages %d is not the most whole pages an int64 holds", int64(maxWholePages))
	}
	for _, c := range []struct{ n, want int64 }{
		{0, 0}, {1, 8 << 10}, {8 << 10, 8 << 10}, {8<<10 + 1, 16 << 10},
		{maxWholePages - 1, maxWholePages}, {maxWholePages, maxWholePages}, {maxWholePages + 1, maxWholePages}, {math.MaxInt64, maxWholePages},
	} {
		if got := wholePages(c.n); got != c.want {
			t.Errorf("wholePages(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	body := textBody(100_000, 9)
	for _, limit := range []int64{maxWholePages - 1, maxWholePages, math.MaxInt64 - 1, math.MaxInt64} {
		var l reserveLedger
		out, err := readAllReserved(bytes.NewReader(body), limit, l.reserve)
		if err != nil || !bytes.Equal(out, body) {
			t.Fatalf("limit %d: read %d bytes, err %v; want the body", limit, len(out), err)
		}
		if cap(out) != 128<<10 || l.held != int64(cap(out)) || !slices.Equal(l.log, []int64{64 << 10, 128 << 10, -64 << 10}) {
			t.Errorf("limit %d: capacity %d, %d reserved, reserve calls %v; want 64 KiB, then 128 KiB, the first given back", limit, cap(out), l.held, l.log)
		}
	}
	if out, err := Decompress(nopDecodeLogger, "gzip", gzipOf(t, body), math.MaxInt64); err != nil || !bytes.Equal(out, body) {
		t.Errorf("a gzip decode under limit math.MaxInt64: %d bytes, err %v; want the body", len(out), err)
	}
}

// TestTheDecodeCountLeavesOutWhatItsReserveFuncAndTheRuntimeHold pins
// stackHeap against the process heap it replaced, with what made that one
// fail: OS threads the runtime starts while a decode runs. In the decode's
// first reserve call the row locks more goroutines to OS threads than the
// process has threads, so the runtime must start at least 8 (each new
// thread's m and g's are allocated on the scheduler's stacks), and it holds
// them until the decode has been counted. The runtime allocates more than
// heldSlack for them in the decode (counted by what it allocated, which only
// grows, so threads that end in the window take nothing off), so a count of
// the process heap fails here. The reserve func also keeps 1 MiB of its own,
// as a caller's books would: it is called from inside the decode, and what it
// holds is the caller's. stackHeap counts the decode's output buffer and no
// byte of either.
func TestTheDecodeCountLeavesOutWhatItsReserveFuncAndTheRuntimeHold(t *testing.T) {
	body := textBody(256<<10, 5)
	wire := gzipOf(t, body)
	countEveryAllocation(t)
	decode, threads := newStackHeap(funcName(countedDecode)), newStackHeap("runtime.allocm")
	threads.allocated = true
	release := make(chan struct{})
	var locked sync.WaitGroup
	t.Cleanup(func() { close(release); locked.Wait() })
	decode.open()
	threads.open()
	before, _ := runtime.ThreadCreateProfile(nil) // the threads there are as the count begins
	var books []byte
	out, err := countedDecode("gzip", wire, 5<<20, func(int64) bool {
		if books == nil {
			books = make([]byte, 1<<20)
			// Each goroutine keeps its thread while it waits, so the
			// last 8 of them cannot run on a thread the process had.
			for range before + 8 {
				ready := make(chan struct{})
				locked.Add(1)
				go func() {
					defer locked.Done()
					runtime.LockOSThread() // it exits locked: its thread ends with it
					close(ready)
					<-release
				}()
				<-ready
			}
		}
		return true
	})
	got, theirs := decode.held(), threads.held()
	runtime.KeepAlive(books)
	if err != nil || !bytes.Equal(out, body) { // out is live through the count
		t.Fatalf("%d bytes, err %v; want the %d-byte body", len(out), err, len(body))
	}
	if theirs <= heldSlack {
		t.Fatalf("the runtime allocated %d bytes for the threads it started in the decode, no more than heldSlack %d: the row no longer shows what a count of the heap would add", theirs, heldSlack)
	}
	if got < int64(cap(out)) || got > int64(cap(out))+heldSlack {
		t.Errorf("the decode holds %d bytes as counted, want its output's capacity %d and at most %d more: the threads' %d bytes and the reserve func's %d are not the decode's", got, cap(out), heldSlack, theirs, len(books))
	}
	now, _ := runtime.ThreadCreateProfile(nil)
	t.Logf("the decode holds %d bytes (its output's capacity %d); the runtime allocated %d for the threads it started in it (%d threads then, %d now)", got, cap(out), theirs, before, now)
}

// TestTheDecodeCountReadsAStackByItsFrames pins stackHeap's verdict on a
// stack, read outward from the allocation: the root counts it; a frame of
// this file before the root leaves it out, and so does one of the runtime's
// caches; a stack of the runtime's frames alone is left out, wherever they
// are, and a frame is the runtime's by its file, whatever its name; any
// other stack counts: a decoder's on a goroutine of its own, other code's,
// one the profile cut at its 32 frames inside a decoder, one with no frame.
func TestTheDecodeCountReadsAStackByItsFrames(t *testing.T) {
	h := newStackHeap(funcName(countedDecode))
	if _, file, _, _ := runtime.Caller(0); (runtimeDir != "runtime" && !strings.HasSuffix(runtimeDir, "/runtime")) || path.Dir(file) == runtimeDir {
		t.Fatalf("the runtime's directory read as %q (this file %s): no frame could be told the runtime's", runtimeDir, file)
	}
	// Frames as the profile's stacks give them: the runtime's by their
	// file, under names that are and are not the runtime's.
	rt := func(name, file string) runtime.Frame {
		return runtime.Frame{Function: name, File: runtimeDir + "/" + file}
	}
	decoder := runtime.Frame{Function: "compress/flate.(*decompressor).nextBlock", File: path.Dir(runtimeDir) + "/compress/flate/inflate.go"}
	root := runtime.Frame{Function: funcName(countedDecode), File: h.file}
	for _, c := range []struct {
		name   string
		frames []runtime.Frame
		want   bool
	}{
		{"a thread the runtime starts from goready", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), rt("runtime.allocm", "proc.go"), rt("runtime.ready", "proc.go"), rt("_.goready.func1", "proc.go"), rt("runtime.systemstack", "asm_amd64.s")}, false},
		{"a runtime-named frame of another file", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), {Function: "runtime.allocm", File: path.Dir(runtimeDir) + "/vendor/runtime/proc.go"}}, true},
		{"a frame of a package under the runtime's directory", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), {Function: "runtime/pprof.StartCPUProfile", File: runtimeDir + "/pprof/pprof.go"}}, true},
		{"a type-assertion cache on the decode's stack", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), rt("runtime.buildTypeAssertCache", "iface.go"), rt("runtime.typeAssert", "iface.go"), decoder, root}, false},
		{"a type-switch cache on the decode's stack", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), rt("runtime.buildInterfaceSwitchCache", "iface.go"), rt("runtime.interfaceSwitch", "iface.go"), decoder, root}, false},
		{"a cache's name in another file", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), {Function: "runtime.buildTypeAssertCache", File: decoder.File}, decoder, root}, true},
		{"a value the decoder boxes, made in the caches' file", []runtime.Frame{rt("runtime.mallocgc", "malloc.go"), rt("runtime.convT", "iface.go"), decoder, root}, true},
	} {
		i := 0
		next := func() (runtime.Frame, bool) {
			i++
			return c.frames[i-1], i < len(c.frames)
		}
		if got := h.countsFrames(next); got != c.want {
			t.Errorf("%s: counted %v, want %v", c.name, got, c.want)
		}
	}
	pc := func(fns ...any) []uintptr {
		var s []uintptr
		for _, fn := range fns {
			s = append(s, reflect.ValueOf(fn).Pointer()+1) // a return address inside fn
		}
		return s
	}
	cut := func(fns ...any) []uintptr {
		s := pc(fns...)
		for len(s) < 32 {
			s = append(s, s[len(s)-1])
		}
		return s
	}
	for _, c := range []struct {
		name  string
		stack []uintptr
		want  bool
	}{
		{"the decode", pc(runtime.GC, runtime.Gosched, gzip.NewReader, DecompressReserving, countedDecode, textBody), true},
		{"the root alone", pc(countedDecode), true},
		{"a decoder on a goroutine of its own", pc(runtime.GC, gzip.NewReader, strings.Repeat, runtime.Gosched), true},
		{"other code, then the runtime", pc(strings.Repeat, runtime.GC), true},
		{"the runtime, then other code", pc(runtime.GC, runtime.Gosched, strings.Repeat), true},
		{"a decoder, cut", cut(runtime.GC, brotli.NewReader), true},
		{"no frame", nil, true},
		{"the runtime alone", pc(runtime.GC, runtime.Gosched, runtime.NumGoroutine), false},
		{"the runtime alone, cut", cut(runtime.GC, runtime.Gosched), false},
		{"a reserve func in the decode", pc(runtime.GC, textBody, DecompressReserving, countedDecode), false},
		{"this file's, under the decoder", pc(runtime.GC, gzip.NewReader, textBody, countedDecode), false},
		{"this file's", pc(strings.Repeat, textBody), false},
	} {
		if got := h.counts(c.stack); got != c.want {
			t.Errorf("%s: counted %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTheDecodeCountAddsWhatEachStackHoldsPastTheWindowsStart pins what
// stackHeap sums: of the stacks it counts, what each holds past what it held
// as the window opened, and nothing for one that holds less (its frees were
// of what it held before); in use, or every byte allocated when it counts
// allocations.
func TestTheDecodeCountAddsWhatEachStackHoldsPastTheWindowsStart(t *testing.T) {
	h := newStackHeap(funcName(countedDecode))
	rec := func(fn any, alloc, free int64) runtime.MemProfileRecord {
		var r runtime.MemProfileRecord
		r.Stack0[0] = reflect.ValueOf(fn).Pointer() + 1
		r.AllocBytes, r.FreeBytes = alloc, free
		return r
	}
	before := []runtime.MemProfileRecord{rec(countedDecode, 1000, 0), rec(gzip.NewReader, 300, 0), rec(textBody, 50, 0)}
	now := []runtime.MemProfileRecord{rec(countedDecode, 1500, 1000), rec(gzip.NewReader, 900, 100), rec(textBody, 5000, 0), rec(runtime.GC, 7000, 0)}
	if got := pastBase(h.byStack(now), h.byStack(before)); got != 500 {
		t.Errorf("in use: %d bytes past the window's start, want 500: none by the root, which holds 500 less than it did, and 500 by a decoder", got)
	}
	h.allocated = true
	if got := pastBase(h.byStack(now), h.byStack(before)); got != 1100 {
		t.Errorf("allocated: %d bytes past the window's start, want 1100: 500 by the root, 600 by a decoder", got)
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

// beginBrotli opens a brotli decoder over wire and has it decode a byte, so it
// has read the first metablock's header and allocated its tables.
func beginBrotli(t *testing.T, wire []byte) *brotli.Reader {
	rd := brotli.NewReader(bytes.NewReader(wire))
	if _, err := rd.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	return rd
}

// code writes a Huffman code of n bits, most significant bit first (RFC
// 1951 §3.1.1).
func (w *brotliBits) code(n uint, c uint64) {
	for i := n; i > 0; i-- {
		w.bits(1, c>>(i-1)&1)
	}
}

// canonicalCodes is the canonical Huffman code of each symbol of lengths
// (RFC 1951 §3.2.2).
func canonicalCodes(lengths []int) []uint64 {
	var count [16]uint64
	for _, n := range lengths {
		if n > 0 {
			count[n]++
		}
	}
	var next [16]uint64
	for n, code := 1, uint64(0); n < 16; n++ {
		code = (code + count[n-1]) << 1
		next[n] = code
	}
	codes := make([]uint64, len(lengths))
	for s, n := range lengths {
		if n > 0 {
			codes[s] = next[n]
			next[n]++
		}
	}
	return codes
}

// mostLinksLengths is the code lengths of a complete code over n symbols
// whose codes over 9 bits take links 9-bit prefixes, with a 15-bit code
// among them, so flate's decoder links each to a table of 64 entries: the
// shorter codes (one per bit set in 512-links, for the symbols in short, in
// order), then two 10-bit codes for each prefix but the last, and 10, 11,
// 12, 13, 14, 15 and 15 bits for the last.
func mostLinksLengths(n, links int, short []int) []int {
	lengths := make([]int, n)
	var shortLens []int
	for b := 8; b >= 0; b-- {
		if (512-links)>>b&1 == 1 {
			shortLens = append(shortLens, 9-b)
		}
	}
	if len(shortLens) != len(short) {
		panic(fmt.Sprintf("%d links take %d shorter codes, not %d", links, len(shortLens), len(short)))
	}
	for i, s := range short {
		lengths[s] = shortLens[i]
	}
	long := []int{10, 11, 12, 13, 14, 15, 15}
	for range links - 1 {
		long = append(long, 10, 10)
	}
	for s := range lengths {
		if len(long) > 0 && lengths[s] == 0 && !slices.Contains(short, s) {
			lengths[s], long = long[0], long[1:]
		}
	}
	if len(long) > 0 {
		panic(fmt.Sprintf("%d symbols are too few for %d links", n, links))
	}
	return lengths
}

// mostLinksDeflate is a final DEFLATE block of n bytes of 'a' whose codes
// make flate's decoder hold the most link tables it can: its literal/length
// code takes 137 9-bit prefixes and its distance code 8, each with a 15-bit
// code (gzipLinkBytes). 'a' takes one bit.
func mostLinksDeflate(n int) []byte {
	lit := mostLinksLengths(286, 137, []int{'a', 256, 0, 1, 2, 3, 4})
	dist := mostLinksLengths(30, 8, []int{0, 1, 2, 3, 4, 5})
	var w brotliBits
	w.bits(1, 1)       // BFINAL
	w.bits(2, 2)       // BTYPE: dynamic codes
	w.bits(5, 286-257) // HLIT
	w.bits(5, 30-1)    // HDIST
	w.bits(4, 19-4)    // HCLEN
	for _, s := range []int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15} {
		// The code-length code: lengths 0 to 15 each take 4 bits, so each
		// one's code is its value; no repeat codes.
		w.bits(3, map[bool]uint64{true: 4}[s < 16])
	}
	for _, l := range append(append([]int{}, lit...), dist...) {
		w.code(4, uint64(l))
	}
	codes := canonicalCodes(lit)
	for range n {
		w.code(uint(lit['a']), codes['a'])
	}
	w.code(uint(lit[256]), codes[256])
	return w.buf
}

// mostStateGzipMember is a gzip member (RFC 1952) of n bytes of 'a' that
// makes a gzip.Reader hold the most it can beside its output: a header with
// an extra field of 65,535 bytes, and a name and a comment of 511 Latin-1
// bytes past ASCII each, and one block of mostLinksDeflate. commentEnd is
// the offset just past the comment's NUL.
func mostStateGzipMember(n int) (member []byte, commentEnd int) {
	var b bytes.Buffer
	b.Write([]byte{0x1f, 0x8b, 8, 4 | 8 | 16, 0, 0, 0, 0, 0, 255}) // deflate; FEXTRA, FNAME, FCOMMENT
	b.Write([]byte{0xff, 0xff})                                    // XLEN 65,535
	b.Write(bytes.Repeat([]byte{'x'}, 65_535))
	for range 2 { // the name, then the comment
		b.Write(bytes.Repeat([]byte{0xe9}, 511)) // 'é' in Latin-1
		b.WriteByte(0)
	}
	commentEnd = b.Len()
	b.Write(mostLinksDeflate(n))
	b.Write(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(bytes.Repeat([]byte{'a'}, n))))
	b.Write(binary.LittleEndian.AppendUint32(nil, uint32(n)))
	return b.Bytes(), commentEnd
}

// probedReader reads wire as a bytes.Reader does (a gzip.Reader reads it
// with no buffer of its own), and calls probe once it has read past each of
// marks, in order.
type probedReader struct {
	wire  []byte
	at    int
	marks []int
	probe func(mark int)
}

func (p *probedReader) passed() {
	for len(p.marks) > 0 && p.at >= p.marks[0] {
		m := p.marks[0]
		p.marks = p.marks[1:]
		p.probe(m)
	}
}

func (p *probedReader) Read(b []byte) (int, error) {
	if p.at == len(p.wire) {
		return 0, io.EOF
	}
	n := copy(b, p.wire[p.at:])
	p.at += n
	p.passed()
	return n, nil
}

func (p *probedReader) ReadByte() (byte, error) {
	if p.at == len(p.wire) {
		return 0, io.EOF
	}
	p.at++
	p.passed()
	return p.wire[p.at-1], nil
}

// inflate decodes src through a gzip.Reader, into buf, a read at a time:
// stackHeap counts its calls as a gzip decode's.
//
//go:noinline
func inflate(src io.Reader, buf []byte) (int, error) {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = zr.Close() }()
	for n := 0; ; {
		k, err := zr.Read(buf)
		n += k
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// TestAGzipDecodeReservesItsDecoderAtTheFormatsLimits pins
// gzipDecoderStateBytes against compress/gzip's and compress/flate's own, on
// a stream of two members that each make the decoder hold the most it can
// (mostStateGzipMember). What the decoder holds is read as it reads its
// input: as it ends the first member's block (its state, the link tables,
// the first header), as it ends the second member's comment (the second
// header, read while it keeps the first and the first block's tables), and
// as it ends the second block. Its most is as it ends that comment: then it
// makes the comment from the runes it converts it through, both live at
// once, before its next read; so the row adds what it allocates between the
// comment's NUL and that read (counted by allocation, which only grows) to
// what it holds at the NUL. The most is gzipDecoderStateBytes exactly: a
// version of the decoder that holds more, or less, fails here, and the
// constants must be recomputed from it.
func TestAGzipDecodeReservesItsDecoderAtTheFormatsLimits(t *testing.T) {
	const n = 100_000
	m1, _ := mostStateGzipMember(n)
	m2, commentEnd := mostStateGzipMember(n)
	wire := append(append([]byte{}, m1...), m2...)
	countEveryAllocation(t)
	decoder, made := newStackHeap(funcName(inflate)), newStackHeap(funcName(inflate))
	made.allocated = true
	comment := len(m1) + commentEnd // just past the second comment's NUL
	held := map[int]int64{}
	var madeAtComment, madeAtNextRead int64
	src := &probedReader{wire: wire, marks: []int{len(m1) - 8, comment, comment + 1, len(wire) - 8}}
	src.probe = func(mark int) {
		held[mark] = decoder.held()
		switch mark {
		case comment:
			madeAtComment = made.held()
		case comment + 1:
			madeAtNextRead = made.held()
		}
	}
	buf := make([]byte, 16<<10)
	decoder.open()
	made.open()
	got, err := inflate(src, buf)
	if err != nil || got != 2*n {
		t.Fatalf("decoded %d bytes, err %v; want %d", got, err, 2*n)
	}
	if len(held) != 4 {
		t.Fatalf("read the decoder at %d of its 4 marks", len(held))
	}
	between := madeAtNextRead - madeAtComment
	atComment := held[comment] + between
	most := max(held[len(m1)-8], atComment, held[len(wire)-8])
	if most != gzipDecoderStateBytes {
		t.Errorf("the decoder held %d bytes at the most, want gzipDecoderStateBytes %d exactly (it held %d, %d and %d at its marks, and allocated %d between the second comment's NUL and its next read): recompute it from this version of the decoder", most, gzipDecoderStateBytes, held[len(m1)-8], held[comment], held[len(wire)-8], between)
	}
	t.Logf("a %d-byte stream of two members: the decoder held %d, %d (+%d allocated before its next read) and %d bytes at its marks (gzipDecoderStateBytes %d)", len(wire), held[len(m1)-8], held[comment], between, held[len(wire)-8], gzipDecoderStateBytes)
}

// TestBrotliDecoderStateIsReservedAtTheFormatsLimits pins what a brotli
// decode reserves for its decoder (brotliReaderBytes and brotliTableBytes)
// against github.com/andybalholm/brotli's own, on a stream of about 1.3 KB
// that declares the most prefix codes the format allows: the tables it
// allocates are brotliTableBytes exactly, and what it holds once it has
// begun (as stackHeap counts beginBrotli's calls) is within the reservation.
// A version of the decoder that sizes its state or its tables otherwise fails
// here, and the constants must be recomputed from it.
func TestBrotliDecoderStateIsReservedAtTheFormatsLimits(t *testing.T) {
	const maxBlockTypes = 256 // RFC 7932 §6: NBLTYPESL, NBLTYPESI, NBLTYPESD
	wire := mostTablesBrotli(100_000)
	countEveryAllocation(t)
	decoder := newStackHeap(funcName(beginBrotli))
	decoder.open()
	rd := beginBrotli(t, wire)
	held := decoder.held()
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
// the growth, and gives the old one back only once the new one is made. A
// decode that asked for the growth alone would end with the same books (its
// output's capacity) and hold half as much again as it had reserved while it
// copied. One that gave the old buffer back before it made the new one would
// make the same calls, and hold both with the old one's room given back
// while it made and filled the new one: at each give-back the decode must
// already hold the new buffer whole (as stackHeap counts it). That is all the
// row sees. It counts what the decode holds at the give-back, so it cannot
// tell a give-back after the copy, which is what the code does, from one
// between the make and the copy: that one also holds the new buffer whole
// there, and it is a stated limit of the row. Its first call asks for the
// decoder's state, all of it: gzipDecoderStateBytes, or brotli's for the
// stream's window.
func TestADecodeAsksForEachGrownBufferWholeBeforeItGivesTheOldOneBack(t *testing.T) {
	body := textBody(600<<10, 11) // grows 64 KiB to 1 MiB, and ends short of it
	cases := []struct {
		encoded
		state int64 // what the decode reserves for its decoder
	}{
		{encoded{"gzip", "gzip", gzipOf(t, body), body}, gzipDecoderStateBytes},
		{encoded{"br", "br", brotliOf(t, body, 5, 18), body}, brotliReaderBytes + brotliTableBytes + brotliTableRegrowBytes + brotliRingBytes(18)},
	}
	countEveryAllocation(t)
	decode := newStackHeap(funcName(countedDecode))
	for _, c := range cases {
		var l reserveLedger
		var early []string
		decode.open()
		probe := func(n int64) bool {
			// A give-back of the buffer just outgrown: the call before it
			// asked for the new one, twice its size.
			if k := len(l.log); n < 0 && k > 0 && l.log[k-1] == -2*n {
				if held := decode.held(); held < -2*n {
					early = append(early, fmt.Sprintf("gave the %d-byte buffer back holding %d bytes, short of the %d-byte one it grew into", -n, held, -2*n))
				}
			}
			return l.reserve(n)
		}
		out, err := countedDecode(c.enc, c.wire, 5<<20, probe)
		if err != nil || !bytes.Equal(out, c.body) {
			t.Fatalf("%s: %d bytes, err %v; want the body", c.name, len(out), err)
		}
		for _, e := range early {
			t.Errorf("%s: %s", c.name, e)
		}
		state := c.state
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
