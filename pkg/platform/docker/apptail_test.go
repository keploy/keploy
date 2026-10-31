package docker

import (
	"bytes"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	keployLog "go.keploy.io/server/v3/utils/log"
)

// freshTails empties the process's tails for one test, and again after it.
func freshTails(t *testing.T) {
	t.Helper()
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
}

// heldBytes is what the tail of name holds as stored, before any read cuts it
// again.
func heldBytes(name string) []byte {
	appOutput.mu.Lock()
	defer appOutput.mu.Unlock()
	if t := appOutput.m[tailKey{name: name}]; t != nil {
		return append([]byte(nil), t.buf...)
	}
	return nil
}

// One container that prints far more than its share holds at most its share,
// starts at a whole line, ends with the last line it printed, and counts
// every byte it let go.
func TestOneContainersTailIsBoundedByBytes(t *testing.T) {
	freshTails(t)
	var written int64
	line := strings.Repeat("x", 300)
	for i := 0; written < 64<<20; i++ {
		l := fmt.Sprintf("%08d %s", i, line)
		RecordAppOutput("", "app-1", "app", l)
		written += int64(len(l) + 1)
	}
	held := heldBytes("app-1")
	if len(held) > appTailMaxContainerBytes {
		t.Fatalf("holds %d bytes, over the %d share", len(held), appTailMaxContainerBytes)
	}
	if len(held) < appTailMaxContainerBytes/2 {
		t.Fatalf("holds %d bytes; trimming dropped far more than it had to", len(held))
	}
	if !bytes.HasSuffix(held, []byte(line+"\n")) {
		t.Fatalf("does not end with the last line printed")
	}
	if len(held) == 0 || !bytes.Equal(held[9:9+len(line)], []byte(line)) {
		t.Fatalf("does not start at a whole line: %q", held[:20])
	}
	tails, _ := AppOutputTails()
	if got := tails[0].Dropped + int64(len(held)); got != written {
		t.Fatalf("dropped %d + held %d = %d, want the %d written", tails[0].Dropped, len(held), got, written)
	}
	if capacity := heldCap(); capacity > appTailMaxContainerBytes {
		t.Fatalf("buffer capacity %d is over the %d share", capacity, appTailMaxContainerBytes)
	}
}

// A line longer than its container's share keeps its end, as the first line
// the tail holds.
func TestALineLongerThanTheShareKeepsItsEnd(t *testing.T) {
	freshTails(t)
	long := strings.Repeat("a", appTailMaxContainerBytes) + "THE-END"
	RecordAppOutput("", "app-1", "app", long)
	held := heldBytes("app-1")
	if len(held) > appTailMaxContainerBytes || !bytes.HasSuffix(held, []byte("THE-END\n")) {
		t.Fatalf("holds %d bytes ending %q", len(held), held[len(held)-10:])
	}
}

// heldIn is what the tail of project's container name holds as stored.
func heldIn(project, name string) []byte {
	appOutput.mu.Lock()
	defer appOutput.mu.Unlock()
	if t := appOutput.m[tailKey{project, name}]; t != nil {
		return append([]byte(nil), t.buf...)
	}
	return nil
}

// heldCap is the buffer capacity every tail takes together.
func heldCap() int {
	appOutput.mu.Lock()
	defer appOutput.mu.Unlock()
	total := 0
	for _, t := range appOutput.m {
		total += cap(t.buf)
	}
	return total
}

// heldAndDropped is what the tail of name holds as stored, and the bytes it
// counts as dropped.
func heldAndDropped(name string) ([]byte, int64) {
	appOutput.mu.Lock()
	defer appOutput.mu.Unlock()
	if t := appOutput.m[tailKey{name: name}]; t != nil {
		return append([]byte(nil), t.buf...), t.dropped
	}
	return nil, 0
}

// The newest line is never the one trimming throws away, however long it is.
// After earlier output, a line longer than the share keeps its last share
// bytes, and a line longer than three quarters of the share (where a trim
// cuts to) but within it is kept whole. Every byte let go is counted.
func TestALongLineAfterEarlierOutputKeepsItsEnd(t *testing.T) {
	share := appTailMaxContainerBytes
	for _, c := range []struct {
		name    string
		earlier string
		long    string
		want    func(long string) string
	}{
		{
			name:    "longer than the share, after one short line",
			earlier: "starting",
			long:    strings.Repeat("a", share) + "THE-END",
			want:    func(l string) string { return (l + "\n")[len(l)+1-share:] },
		},
		{
			name:    "80% of the share, after half a share",
			earlier: strings.Repeat("b", share/2),
			long:    strings.Repeat("c", share*8/10) + "LAST",
			want:    func(l string) string { return l + "\n" },
		},
		{
			name:    "the share less its line end, after one short line",
			earlier: "starting",
			long:    strings.Repeat("d", share-1-len("PANIC")) + "PANIC",
			want:    func(l string) string { return l + "\n" },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			freshTails(t)
			RecordAppOutput("", "app-1", "app", c.earlier)
			RecordAppOutput("", "app-1", "app", c.long)
			held, dropped := heldAndDropped("app-1")
			if want := c.want(c.long); string(held) != want {
				t.Fatalf("holds %d bytes ending %q; want the newest line's last %d bytes", len(held), held[max(0, len(held)-12):], len(want))
			}
			if written := int64(len(c.earlier) + 1 + len(c.long) + 1); dropped+int64(len(held)) != written {
				t.Fatalf("dropped %d + held %d != the %d written", dropped, len(held), written)
			}
		})
	}
}

// When a new container joins and every share shrinks, a container whose last
// line is longer than its new share keeps that line's end (its last share
// bytes), and one whose last line still fits its new share keeps it whole.
// With the share at its floor (64 KiB, from 256 containers), a 50 KiB line
// printed after 20 KiB of output is kept whole.
func TestANewContainerShrinkingTheShareKeepsALongLastLinesEnd(t *testing.T) {
	freshTails(t)
	// Four containers hold 4 MiB each; the fifth shrinks the share to 16/5 MiB.
	over := strings.Repeat("e", 3584<<10) + "OVER-END"  // 3.5 MiB: over 16/5 MiB
	within := strings.Repeat("f", 3<<20) + "WITHIN-END" // 3 MiB after 512 KiB: over 3/4 of 16/5 MiB, within it
	RecordAppOutput("", "over-1", "over", "booting")
	RecordAppOutput("", "over-1", "over", over)
	RecordAppOutput("", "within-1", "within", strings.Repeat("h", 512<<10))
	RecordAppOutput("", "within-1", "within", within)
	RecordAppOutput("", "c-1", "c", "x")
	RecordAppOutput("", "d-1", "d", "x")
	if held := heldBytes("over-1"); string(held) != "booting\n"+over+"\n" {
		t.Fatalf("before the share shrank: over-1 holds %d bytes, want both lines whole", len(held))
	}
	RecordAppOutput("", "e-1", "e", "x")

	share := appTailShare(5)
	held, dropped := heldAndDropped("over-1")
	if want := (over + "\n")[len(over)+1-share:]; string(held) != want {
		t.Fatalf("over-1 holds %d bytes ending %q; want its last line's last %d bytes", len(held), held[max(0, len(held)-12):], share)
	}
	if written := int64(len("booting\n") + len(over) + 1); dropped+int64(len(held)) != written {
		t.Fatalf("over-1: dropped %d + held %d != the %d written", dropped, len(held), written)
	}
	if held := heldBytes("within-1"); string(held) != within+"\n" {
		t.Fatalf("within-1 holds %d bytes ending %q; want its last line whole", len(held), held[max(0, len(held)-12):])
	}

	freshTails(t)
	for i := 0; i < appTailMaxContainers-1; i++ {
		RecordAppOutput("", fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), "hello")
	}
	trace := strings.Repeat("g", 50<<10) + "PANIC"
	RecordAppOutput("", "app-1", "app", strings.Repeat("i", 20<<10))
	RecordAppOutput("", "app-1", "app", trace)
	if held := heldBytes("app-1"); string(held) != trace+"\n" {
		t.Fatalf("at the share's floor, the app's 50 KiB newest line: holds %d bytes ending %q", len(held), held[max(0, len(held)-12):])
	}
}

// Many containers together hold at most the budget: each gets a smaller share
// as more print, and past appTailMaxContainers the one that printed least
// recently goes whole. A container that printed early, crashed and went
// quiet stays while fewer than that many newer ones print.
func TestAllContainersTogetherAreBoundedByBytes(t *testing.T) {
	freshTails(t)
	chunk := strings.Repeat("y", 1000)
	RecordAppOutput("", "crashed-1", "crashed", "panic: the reason it died")
	for i := 0; i < appTailMaxContainers-1; i++ {
		for j := 0; j < 200; j++ {
			RecordAppOutput("", fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), chunk)
		}
	}
	if got := string(ServiceOutputTail("", "crashed")); got != "panic: the reason it died\n" {
		t.Fatalf("the quiet crashed container's tail = %q; want it kept", got)
	}

	for i := appTailMaxContainers - 1; i < appTailMaxContainers+43; i++ {
		for j := 0; j < 200; j++ {
			RecordAppOutput("", fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), chunk)
		}
	}
	tails, gone := AppOutputTails()
	if len(tails) > appTailMaxContainers {
		t.Fatalf("holds %d containers, over %d", len(tails), appTailMaxContainers)
	}
	if gone != 44 {
		t.Fatalf("dropped %d containers whole, want 44", gone)
	}
	var total int
	for _, tl := range tails {
		if len(tl.Text) > appTailShare(len(tails)) {
			t.Fatalf("%s holds %d bytes, over its share %d", tl.Name, len(tl.Text), appTailShare(len(tails)))
		}
		total += len(tl.Text)
	}
	if total > appTailBudget {
		t.Fatalf("holds %d bytes together, over the %d budget", total, appTailBudget)
	}
	if capacity := heldCap(); capacity > appTailBudget {
		t.Fatalf("the buffers take %d bytes together, over the %d budget", capacity, appTailBudget)
	}
	if ServiceOutputTail("", "crashed") != nil || ServiceOutputTail("", "svc0") != nil {
		t.Fatalf("the least recently written containers were not the ones dropped")
	}
	if ServiceOutputTail("", fmt.Sprintf("svc%d", appTailMaxContainers+42)) == nil {
		t.Fatalf("the newest container was dropped")
	}
}

// ServiceOutputTail answers with the service's container that printed last.
func TestServiceOutputTailIsTheContainerThatPrintedLast(t *testing.T) {
	freshTails(t)
	RecordAppOutput("", "app-1", "app", "old replica")
	RecordAppOutput("", "app-2", "app", "new replica")
	if got := string(ServiceOutputTail("", "app")); got != "new replica\n" {
		t.Fatalf("ServiceOutputTail = %q", got)
	}
	if ServiceOutputTail("", "") != nil || ServiceOutputTail("", "db") != nil {
		t.Fatalf("a service with no tail answered one")
	}
}

// RecordAppOutput cuts the line itself: whoever records a line, the tail
// holds it cut, so the guarantee does not rest on its caller cutting first.
func TestRecordAppOutputHoldsOnlyTheCutLine(t *testing.T) {
	freshTails(t)
	keployLog.SetRedactor(composeCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	RecordAppOutput("", "app-1", "app", "DB_PASSWORD="+plantedComposeValue)
	if held := heldBytes("app-1"); string(held) != "DB_PASSWORD=***\n" {
		t.Fatalf("tail holds %q; want the cut line alone", held)
	}
}

// The memory the tails take, not only the text they hold, stays within the
// budget, whatever they were sent: here a container's buffer grown to the
// largest share, then containers that each grow a buffer to the share of
// their time and then hold one short line (the trim keeps a buffer sized from
// what it keeps), while each new container shrinks every share (a buffer
// larger than its new share moves to one that fits).
func TestTheTailsBuffersTogetherStayWithinTheBudget(t *testing.T) {
	freshTails(t)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for written := 0; written < appTailMaxContainerBytes; written += 1001 {
		RecordAppOutput("", "big-1", "big", strings.Repeat("z", 1000))
	}
	for i := 2; i <= appTailMaxContainers; i++ {
		name := fmt.Sprintf("c%d-1", i)
		RecordAppOutput("", name, "", strings.Repeat("a", appTailShare(i)-1002))
		RecordAppOutput("", name, "", strings.Repeat("b", 2000))
	}
	if capacity := heldCap(); capacity > appTailBudget {
		t.Fatalf("the buffers take %d bytes together (%.1f MiB), over the %d budget", capacity, float64(capacity)/(1<<20), appTailBudget)
	}
	appOutput.mu.Lock()
	share := appTailShare(len(appOutput.m))
	for k, tl := range appOutput.m {
		if cap(tl.buf) > share || cap(tl.buf) > 2*len(tl.buf) {
			appOutput.mu.Unlock()
			t.Fatalf("%s's buffer takes %d bytes for %d of text; want at most its %d share and twice its text",
				k.name, cap(tl.buf), len(tl.buf), share)
		}
	}
	appOutput.mu.Unlock()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > appTailBudget+4<<20 {
		t.Fatalf("the heap grew %.1f MiB holding the tails; want at most the %d MiB budget and 4 MiB besides",
			float64(grew)/(1<<20), appTailBudget>>20)
	}
}

// The runs remembered, one per project brought up, are bounded: past the
// bound the oldest of the projects that hold no tail is forgotten, and a
// project that still holds one keeps its run and records on.
func TestTheRunsRememberedAreBounded(t *testing.T) {
	s := newAppTails()
	keep := s.startRun("keep")
	s.record("keep", keep, "app-1", "app", "held", 0)
	for i := 0; i < appTailMaxRuns+100; i++ {
		s.startRun(fmt.Sprintf("p%d", i))
	}
	if len(s.runs) > appTailMaxRuns {
		t.Fatalf("remembers %d runs, over %d", len(s.runs), appTailMaxRuns)
	}
	if s.runs["p0"] != nil {
		t.Fatalf("the oldest run of a project that holds no tail is still remembered")
	}
	s.record("keep", keep, "app-1", "app", "still kept", 0)
	if got := string(s.m[tailKey{"keep", "app-1"}].buf); got != "held\nstill kept\n" {
		t.Fatalf("the project that holds a tail lost its run: tail %q", got)
	}
}

// tailPattern is n bytes that differ from their neighbours, from position
// from of one stream, so a byte held out of place shows.
func tailPattern(from, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte((from + i) % 251)
	}
	return p
}

// add weighs a line against the room the buffer has left before it sums any
// length, and sizes each buffer it grows by hand: twice the old one held to
// the limit, or just what the text needs when that is more. Each case is at
// an edge of one of those choices, from a state addLocked reaches (the line
// under the limit, the buffer within it), with sizes past 256 so append's own
// growth would show; the last is outside that and only keeps the text.
func TestAppTailAddSizesEachBufferFromTheRoomLeft(t *testing.T) {
	const limit = 4096
	for _, c := range []struct {
		name                 string
		held, capacity, line int
		wantCap              int // 0: any buffer that holds the text
	}{
		{"an empty line to an empty tail", 0, 0, 0, 1},
		{"a line that fills the free room", 1000, 1024, 23, 1024},
		{"one byte past the free room doubles the buffer", 1000, 1024, 24, 2048},
		{"a doubled buffer past the limit is held to it", 3000, 3000, 100, limit},
		{"a line the doubled buffer cannot hold grows it to just what the text needs", 300, 300, 1000, 1301},
		{"a line the doubled buffer cannot hold, past free room", 300, 400, 1000, 1301},
		{"a text of just the limit", 2000, 2000, 2095, limit},
		{"a buffer twice which is one under the limit", 2047, 2047, 0, 4094},
		{"a buffer twice which is the limit", 2048, 2048, 0, limit},
		{"a buffer twice which is one past the limit", 2049, 2049, 0, limit},
		{"a buffer of the limit grows past it to just what the text needs", limit, limit, 0, limit + 1},
		{"the longest line to a full buffer", limit, limit, limit - 1, 2 * limit},
		{"a line of the limit, which addLocked never passes", 0, 0, limit, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			buf := make([]byte, c.held, c.capacity)
			copy(buf, tailPattern(0, c.held))
			tl := &appTail{buf: buf}
			tl.add(string(tailPattern(c.held, c.line)), limit)
			want := append(tailPattern(0, c.held+c.line), '\n')
			if !bytes.Equal(tl.buf, want) {
				t.Fatalf("holds %d bytes that are not the %d of the text and the line", len(tl.buf), len(want))
			}
			if c.wantCap != 0 && cap(tl.buf) != c.wantCap {
				t.Fatalf("a %d-byte buffer holds the %d bytes; want %d", cap(tl.buf), len(tl.buf), c.wantCap)
			}
		})
	}
}

// keepFrom sizes the buffer it moves the kept text to from that text alone:
// room to grow to twice it, held to the limit, but never less than the text.
func TestAppTailKeepFromSizesItsBufferFromWhatItKeeps(t *testing.T) {
	const limit = 4096
	for _, c := range []struct {
		name       string
		held, from int
		wantCap    int
	}{
		{"nothing kept", 100, 100, 0},
		{"room to twice the text", 1000, 0, 2000},
		{"room to twice the text, from past its start", 3000, 1000, 4000},
		{"twice the text one under the limit", 2047, 0, 4094},
		{"twice the text the limit", 2048, 0, limit},
		{"twice the text one past the limit", 2049, 0, limit},
		{"a text of the limit", limit, 0, limit},
		{"a text past the limit keeps room for itself", limit + 1, 0, limit + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl := &appTail{buf: tailPattern(0, c.held)}
			tl.keepFrom(c.from, limit)
			if !bytes.Equal(tl.buf, tailPattern(c.from, c.held-c.from)) {
				t.Fatalf("keeps %d bytes that are not the text's last %d", len(tl.buf), c.held-c.from)
			}
			if cap(tl.buf) != c.wantCap {
				t.Fatalf("a %d-byte buffer keeps the %d bytes; want %d", cap(tl.buf), len(tl.buf), c.wantCap)
			}
		})
	}
}

// twiceHeldTo is twice x held to the limit, for an even and an odd limit, at
// each side of half of it, past it, and for the largest int, where 2*x would
// overflow.
func TestTwiceHeldToIsTwiceXHeldToTheLimit(t *testing.T) {
	for _, c := range []struct{ x, limit, want int }{
		{0, 0, 0},
		{0, 4096, 0},
		{1, 4096, 2},
		{2047, 4096, 4094},
		{2048, 4096, 4096},
		{2049, 4096, 4096},
		{2047, 4095, 4094},
		{2048, 4095, 4095},
		{4096, 4096, 4096},
		{5000, 4096, 4096},
		{math.MaxInt/2 + 1, 4096, 4096},
		{math.MaxInt, 4096, 4096},
	} {
		if got := twiceHeldTo(c.x, c.limit); got != c.want {
			t.Errorf("twiceHeldTo(%d, %d) = %d; want %d", c.x, c.limit, got, c.want)
		}
	}
}
