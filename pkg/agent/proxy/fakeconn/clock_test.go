package fakeconn

import (
	"strings"
	"testing"
	"time"
	"unsafe"
)

// chunkWithoutClock is Chunk as it was before ReadClock: a chunk must cost no
// more memory for its clock, which sits in the padding after Dir.
type chunkWithoutClock struct {
	Dir       Direction
	ConnSeq   uint32
	Bytes     []byte
	ReadAt    time.Time
	WrittenAt time.Time
	SeqNo     uint64
}

// A queued chunk is 88 bytes on a 64-bit platform (the relay's tee budgets
// by it), and its clock adds nothing on any platform.
func TestChunkSize(t *testing.T) {
	if got, want := unsafe.Sizeof(Chunk{}), unsafe.Sizeof(chunkWithoutClock{}); got != want {
		t.Fatalf("a Chunk is %d bytes, %d without its clock; ReadClock must sit in Dir's padding", got, want)
	}
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if got := unsafe.Sizeof(Chunk{}); got != 88 {
			t.Fatalf("a Chunk is %d bytes on a 64-bit platform; want 88", got)
		}
	}
	if off := unsafe.Offsetof(Chunk{}.ReadClock); off != 1 {
		t.Fatalf("ReadClock is at offset %d; want 1, right after Dir", off)
	}
}

// resetClockIDs forgets this run's clock names, for the test's life.
func resetClockIDs(t *testing.T) {
	t.Helper()
	forget := func() {
		clockIDsMu.Lock()
		defer clockIDsMu.Unlock()
		for i := range clockIDs {
			clockIDs[i].Store(nil)
		}
	}
	forget()
	t.Cleanup(forget)
}

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one saying %q", want)
		}
		if s, _ := r.(string); !strings.Contains(s, want) {
			t.Fatalf("panic %v; want one saying %q", r, want)
		}
	}()
	f()
}

// A clock is named once per run: unnamed it is "", named it keeps its id, the
// same id again is no change, and another id panics. ClockUnset is never
// named, and no id is empty.
func TestClockIDsAreNamedOncePerRun(t *testing.T) {
	resetClockIDs(t)
	for _, c := range []Clock{ClockUnset, ClockProxy, ClockKtime, Clock(200)} {
		if id := ClockID(c); id != "" {
			t.Fatalf("%s is named %q before any SetClockID", c, id)
		}
	}
	SetClockID(ClockProxy, "proxy:run-1")
	SetClockID(ClockKtime, "ktime:boot-1.anchor-1")
	SetClockID(ClockProxy, "proxy:run-1") // the same id again
	if got := ClockID(ClockProxy); got != "proxy:run-1" {
		t.Fatalf("ClockProxy is %q; want proxy:run-1", got)
	}
	if got := ClockID(ClockKtime); got != "ktime:boot-1.anchor-1" {
		t.Fatalf("ClockKtime is %q; want ktime:boot-1.anchor-1", got)
	}
	if got := ClockID(ClockUnset); got != "" {
		t.Fatalf("ClockUnset is named %q", got)
	}
	mustPanic(t, "already", func() { SetClockID(ClockProxy, "proxy:run-2") })
	if got := ClockID(ClockProxy); got != "proxy:run-1" {
		t.Fatalf("a refused rename left ClockProxy %q", got)
	}
	mustPanic(t, "cannot be named", func() { SetClockID(ClockUnset, "x") })
	mustPanic(t, "cannot be named", func() { SetClockID(Clock(200), "x") })
	mustPanic(t, "an empty id", func() { SetClockID(ClockKtime, "") })
}

// A chunk's clock is unset unless its producer sets one: the zero chunk is
// ClockUnset. Each clock has a short name.
func TestAZeroChunksClockIsUnset(t *testing.T) {
	if (Chunk{}).ReadClock != ClockUnset {
		t.Fatal("a zero chunk's clock is set")
	}
	for c, want := range map[Clock]string{ClockUnset: "unset", ClockProxy: "proxy", ClockKtime: "ktime", Clock(9): "clock(9)"} {
		if got := c.String(); got != want {
			t.Errorf("Clock(%d).String() = %q, want %q", c, got, want)
		}
	}
}
