package pkg

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand/v2"
	"runtime"
	"testing"
	"weak"

	"github.com/andybalholm/brotli"
	"go.uber.org/zap"
)

// compressBody is n bytes of text that compresses: words drawn from a short
// list with numbers between them, so a writer's hash table finds matches
// and what it holds shapes the bytes it writes.
func compressBody(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 7))
	words := []string{"order", "token", "item", "price", "created_at", "status", "user", "id", "\"", "{", "}", ",", ":"}
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[r.IntN(len(words))])
		fmt.Fprintf(&b, "%d ", r.IntN(1000))
	}
	return b.Bytes()[:n]
}

// newWriterCompress is what Compress gave before it kept its writers: a new
// writer for each body.
func newWriterCompress(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Close() error
	}
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Compress keeps its writers between calls (gzipWriters, brotliWriters), and
// each body still compresses to the bytes a new writer gives it, byte for
// byte, whatever body its writer compressed before: every size here follows
// every other once (sizeWalk), on both sides of 1 MiB, from which a brotli
// writer hashes with its large table (andybalholm/brotli's chooseHasher;
// brotliLargeBody), and at it. The sizes are literal, not brotliLargeBody's:
// a writer kept across 1 MiB gives other bytes than a new one, so a wrong
// constant, or a brotli whose boundary moved, fails here.
func TestCompressWithAKeptWriterIsWhatANewWriterGives(t *testing.T) {
	logger := zap.NewNop()
	sizes := []int{0, 1, 777, 64 << 10, 512 << 10, 1<<20 - 1, 1 << 20, 2 << 20}
	for _, enc := range []string{"gzip", "br"} {
		bodies := make([][]byte, len(sizes))
		want := make([][]byte, len(sizes))
		for i, n := range sizes {
			bodies[i] = compressBody(n, uint64(i))
			want[i] = newWriterCompress(t, enc, bodies[i])
		}
		prev := -1
		for _, k := range sizeWalk(len(sizes)) {
			got, err := Compress(logger, enc, bodies[k])
			if err != nil {
				t.Fatalf("%s %d bytes: %v", enc, sizes[k], err)
			}
			if !bytes.Equal(got, want[k]) {
				t.Fatalf("%s: %d bytes compressed after %d bytes (-1: first) are %d bytes, not the %d a new writer gives", enc, sizes[k], sizeAt(sizes, prev), len(got), len(want[k]))
			}
			prev = k
		}
	}
}

// sizeWalk is a walk over 0..n-1 that has every ordered pair (a, b),
// a == b too, as consecutive steps exactly once: an Euler circuit of the
// complete directed graph with loops (Hierholzer's), n*n+1 steps.
func sizeWalk(n int) []int {
	next := make([]int, n)
	var walk []int
	var visit func(a int)
	visit = func(a int) {
		for next[a] < n {
			b := next[a]
			next[a]++
			visit(b)
		}
		walk = append(walk, a)
	}
	visit(0)
	return walk
}

func sizeAt(sizes []int, i int) int {
	if i < 0 {
		return -1
	}
	return sizes[i]
}

// sizeWalk has each ordered pair once, so the row above sees every size
// after every other.
func TestSizeWalkHasEveryOrderedPairOnce(t *testing.T) {
	for n := 1; n <= 8; n++ {
		w := sizeWalk(n)
		if len(w) != n*n+1 {
			t.Fatalf("n=%d: %d steps, want %d", n, len(w), n*n+1)
		}
		seen := map[[2]int]int{}
		for i := 1; i < len(w); i++ {
			seen[[2]int{w[i-1], w[i]}]++
		}
		for a := range n {
			for b := range n {
				if seen[[2]int{a, b}] != 1 {
					t.Fatalf("n=%d: pair (%d, %d) %d times", n, a, b, seen[[2]int{a, b}])
				}
			}
		}
	}
}

// Compress allocates no new writer state for a body when it has compressed
// one before: what a call allocates is about the body's compressed bytes,
// not a new writer's tables (a brotli writer's 2 MiB hash table, a gzip
// writer's flate state).
func TestCompressKeepsItsWritersState(t *testing.T) {
	if raceOn {
		t.Skip("under -race sync.Pool drops what it is given at random: what a call allocates is the plain run's to check")
	}
	logger := zap.NewNop()
	body := compressBody(4<<10, 1)
	for _, c := range []struct {
		enc   string
		bound uint64
	}{{"gzip", 64 << 10}, {"br", 64 << 10}} {
		if _, err := Compress(logger, c.enc, body); err != nil {
			t.Fatal(err)
		}
		const calls = 64
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range calls {
			if _, err := Compress(logger, c.enc, body); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		per := (after.TotalAlloc - before.TotalAlloc) / calls
		t.Logf("%s: %d bytes allocated a call", c.enc, per)
		if per > c.bound {
			t.Errorf("%s: Compress allocated %d bytes a call for a %d-byte body, past %d: it builds a new writer for each", c.enc, per, len(body), c.bound)
		}
	}
}

// newWriterFlush is what CompressWithoutEnd gives with a new writer.
func newWriterFlush(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Flush() error
	}
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A writer that CompressWithoutEnd flushed, and did not close, goes back to
// its pool: every later Compress and CompressWithoutEnd still gives a new
// writer's bytes, at every pair of sizes, both sides of brotli's 1 MiB pool
// class, in either order of the two.
func TestAFlushedWriterBackInItsPoolCompressesAsANewWriter(t *testing.T) {
	logger := zap.NewNop()
	sizes := []int{0, 1, 777, 64 << 10, 1<<20 - 1, 1 << 20, 2 << 20}
	for _, enc := range []string{"gzip", "br"} {
		bodies := make([][]byte, len(sizes))
		whole := make([][]byte, len(sizes))
		open := make([][]byte, len(sizes))
		for i, n := range sizes {
			bodies[i] = compressBody(n, uint64(100+i))
			whole[i] = newWriterCompress(t, enc, bodies[i])
			open[i] = newWriterFlush(t, enc, bodies[i])
		}
		walk := sizeWalk(len(sizes))
		for step, k := range walk {
			// Alternate: flushed, then closed, then flushed... and the reverse
			// on a second lap, so each pair is seen in each mode order.
			for lap := 0; lap < 2; lap++ {
				flushed := (step+lap)%2 == 0
				var got []byte
				var err error
				if flushed {
					got, err = CompressWithoutEnd(logger, enc, bodies[k])
				} else {
					got, err = Compress(logger, enc, bodies[k])
				}
				if err != nil {
					t.Fatal(err)
				}
				want := whole[k]
				if flushed {
					want = open[k]
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s step %d lap %d: %d bytes (flushed %v) gave %d bytes, a new writer %d", enc, step, lap, sizes[k], flushed, len(got), len(want))
				}
			}
		}
	}
}

// A compressor goes back to its pool pointing at no buffer (compress resets it
// to nil), so the pool keeps no call's compressed bytes reachable once the
// call drops them. Without that, a flushed writer still points at the buffer
// it wrote to, and so does a closed gzip one (gzip.Writer.Close keeps its
// destination): the pool would hold each call's bytes until the writer is
// reused or the pool is cleared. One GC moves a pool's writers to its victim
// cache, where they are still reachable: the bytes are collected by it only
// when no pooled writer points at them. So the row runs exactly one GC: a
// second would drop the victim cache and collect the bytes either way.
func TestACompressorBackInItsPoolHoldsNoCallsBytes(t *testing.T) {
	for _, encoding := range []string{"gzip", "br"} {
		for _, end := range []bool{false, true} {
			name := encoding + "/flushed"
			if end {
				name = encoding + "/closed"
			}
			t.Run(name, func(t *testing.T) {
				dropped := compressedAndDropped(t, encoding, end)
				runtime.GC()
				if dropped.Value() != nil {
					t.Fatal("the compressed bytes the call dropped are still reachable after a GC: the writer back in its pool points at them")
				}
			})
		}
	}
}

// compressedAndDropped compresses a body as Compress (end) or
// CompressWithoutEnd does and returns only a weak pointer to the bytes, so
// nothing of the call but its pooled writer can keep them reachable.
//
//go:noinline
func compressedAndDropped(t *testing.T, encoding string, end bool) weak.Pointer[byte] {
	t.Helper()
	fn := CompressWithoutEnd
	if end {
		fn = Compress
	}
	out, err := fn(zap.NewNop(), encoding, compressBody(4096, 3))
	if err != nil || len(out) == 0 {
		t.Fatalf("%s: compressed %d bytes (%v)", encoding, len(out), err)
	}
	return weak.Make(&out[0])
}
