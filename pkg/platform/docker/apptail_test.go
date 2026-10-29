package docker

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
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

// A line a container prints reaches its tail only cut: the tail holds what
// keploy printed, never the line as it came, so a read with no redactor at
// all (one gone since) still finds no value. AppOutputTails, which a debug
// bundle reads, and ServiceOutputTail, which recentAppLogs reads, give the
// same cut text.
func TestTheTailHoldsOnlyTheCutLine(t *testing.T) {
	freshTails(t)
	keployLog.SetRedactor(composeCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })

	var out, errOut bytes.Buffer
	c := runnerOf("p", "app").upConsumer(&out, &errOut)
	c.Log("app-1", "DB_PASSWORD="+plantedComposeValue)
	c.Err("app-1", "dial with "+plantedComposeValue+" failed")

	if held := heldIn("p", "app-1"); bytes.Contains(held, []byte(plantedComposeValue)) || string(held) != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("tail holds %q; want the two cut lines alone", held)
	}
	if out.String() != "app-1  | DB_PASSWORD=***\n" || errOut.String() != "app-1  | dial with *** failed\n" {
		t.Fatalf("printed %q and %q; want the lines the tail holds", out.String(), errOut.String())
	}

	keployLog.SetRedactor(nil)
	if got := string(AppOutputTail("p", "app-1")); got != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("AppOutputTail = %q", got)
	}
	if got := string(ServiceOutputTail("p", "app")); got != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("ServiceOutputTail = %q", got)
	}
	tails, gone := AppOutputTails()
	if len(tails) != 1 || gone != 0 || tails[0].Project != "p" || tails[0].Name != "app-1" || tails[0].Service != "app" ||
		string(tails[0].Text) != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("AppOutputTails = %+v, %d", tails, gone)
	}
}

// A value registered after its line reached the tail is cut when the tail is
// read, by every reader: ServiceOutputTail (what recentAppLogs reads),
// AppOutputTail and AppOutputTails (what a debug bundle reads).
func TestATailReadCutsAValueRegisteredSince(t *testing.T) {
	freshTails(t)
	keployLog.SetRedactor(nil)
	c := runnerOf("p", "app").upConsumer(&bytes.Buffer{}, &bytes.Buffer{})
	c.Log("app-1", "token "+plantedComposeValue)
	if held := heldIn("p", "app-1"); string(held) != "token "+plantedComposeValue+"\n" {
		t.Fatalf("with no redactor the tail holds %q; want the line as it came", held)
	}

	keployLog.SetRedactor(composeCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	if got := string(ServiceOutputTail("p", "app")); got != "token ***\n" {
		t.Fatalf("ServiceOutputTail = %q, want the value cut on read", got)
	}
	if got := string(AppOutputTail("p", "app-1")); got != "token ***\n" {
		t.Fatalf("AppOutputTail = %q, want the value cut on read", got)
	}
	tails, _ := AppOutputTails()
	if len(tails) != 1 || string(tails[0].Text) != "token ***\n" {
		t.Fatalf("AppOutputTails = %+v, want the value cut on read", tails)
	}
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

// runnerOf is a runner of project with the services named, enough for what
// its Up's consumer does with a line.
func runnerOf(project string, services ...string) *ComposeRunner {
	svcs := composetypes.Services{}
	for _, name := range services {
		svcs[name] = composetypes.ServiceConfig{Name: name}
	}
	return &ComposeRunner{project: &composetypes.Project{Name: project, Services: svcs}}
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

// serviceOf reads compose's container naming backwards from the project.
func TestServiceOfReadsComposeNamesBackwards(t *testing.T) {
	r := &ComposeRunner{project: &composetypes.Project{Services: composetypes.Services{
		"app":   {Name: "app"},
		"app-1": {Name: "app-1"},
		"db":    {Name: "db", ContainerName: "orders-db"},
	}}}
	for name, want := range map[string]string{
		"app-1":     "app",
		"app-12":    "app",
		"app-1-1":   "app-1",
		"orders-db": "db",
		"db-1":      "",
		"app-":      "",
		"app-x":     "",
		"other-1":   "",
	} {
		if got := r.serviceOf(name); got != want {
			t.Errorf("serviceOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// Each Up of a project starts a new run of its tails, as keploy's one Up per
// test set on one runner does: what the tails hold is that Up's output alone,
// as `docker logs` of that Up's container was. A set whose app printed
// nothing reads nothing, not the set before; a set whose app printed one
// panic line reads that line alone; a line an earlier Up's attach stream
// still delivers is printed but not kept. Another project's tails are its
// own: an Up of one project leaves them, and no read of one project answers
// with them.
func TestEachUpStartsANewRunOfTheProjectsTails(t *testing.T) {
	freshTails(t)
	r := runnerOf("p", "app", "db")
	other := runnerOf("q", "app")

	otherUp := other.upConsumer(io.Discard, io.Discard)
	otherUp.Log("app-1", "q's app line")
	set1 := r.upConsumer(io.Discard, io.Discard)
	for i := 0; i < 30; i++ {
		set1.Log("app-1", fmt.Sprintf("set-1 line %d", i))
	}
	set1.Log("db-1", "set-1 db ready")
	if got := string(ServiceOutputTail("p", "app")); strings.Count(got, "set-1 line") != 30 {
		t.Fatalf("set 1's app tail = %q; want its 30 lines", got)
	}

	var late bytes.Buffer
	set1.out = &late
	set2 := r.upConsumer(io.Discard, io.Discard)
	set1.Log("app-1", "set-1 late line")
	if late.String() != "app-1  | set-1 late line\n" {
		t.Fatalf("an earlier Up's late line printed %q; want it printed as it came", late.String())
	}
	if got := ServiceOutputTail("p", "app"); got != nil {
		t.Fatalf("set 2's app printed nothing, but its tail = %q", got)
	}
	if got := AppOutputTail("p", "db-1"); got != nil {
		t.Fatalf("set 2's db printed nothing, but its tail = %q", got)
	}

	set2.Err("app-1", "set-2: panic: cannot reach the db")
	if got := string(ServiceOutputTail("p", "app")); got != "set-2: panic: cannot reach the db\n" {
		t.Fatalf("set 2 printed one line; its app tail = %q", got)
	}
	if got := string(ServiceOutputTail("q", "app")); got != "q's app line\n" {
		t.Fatalf("project q's tail = %q after p's Ups; want its own line kept", got)
	}
	tails, _ := AppOutputTails()
	if len(tails) != 2 || tails[0].Project != "q" || tails[1].Project != "p" || string(tails[1].Text) != "set-2: panic: cannot reach the db\n" {
		t.Fatalf("AppOutputTails = %+v; want q's tail and set 2's app tail", tails)
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
	s.record("keep", keep, "app-1", "app", "held")
	for i := 0; i < appTailMaxRuns+100; i++ {
		s.startRun(fmt.Sprintf("p%d", i))
	}
	if len(s.runs) > appTailMaxRuns {
		t.Fatalf("remembers %d runs, over %d", len(s.runs), appTailMaxRuns)
	}
	if s.runs["p0"] != nil {
		t.Fatalf("the oldest run of a project that holds no tail is still remembered")
	}
	s.record("keep", keep, "app-1", "app", "still kept")
	if got := string(s.m[tailKey{"keep", "app-1"}].buf); got != "held\nstill kept\n" {
		t.Fatalf("the project that holds a tail lost its run: tail %q", got)
	}
}
