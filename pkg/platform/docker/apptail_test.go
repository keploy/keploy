package docker

import (
	"bytes"
	"fmt"
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
	if t := appOutput.m[name]; t != nil {
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
	c := composeLogConsumer{out: &out, err: &errOut, serviceOf: func(string) string { return "app" }}
	c.Log("app-1", "DB_PASSWORD="+plantedComposeValue)
	c.Err("app-1", "dial with "+plantedComposeValue+" failed")

	if held := heldBytes("app-1"); bytes.Contains(held, []byte(plantedComposeValue)) || string(held) != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("tail holds %q; want the two cut lines alone", held)
	}

	keployLog.SetRedactor(nil)
	if got := string(AppOutputTail("app-1")); got != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("AppOutputTail = %q", got)
	}
	if got := string(ServiceOutputTail("app")); got != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("ServiceOutputTail = %q", got)
	}
	tails, gone := AppOutputTails()
	if len(tails) != 1 || gone != 0 || tails[0].Name != "app-1" || tails[0].Service != "app" ||
		string(tails[0].Text) != "DB_PASSWORD=***\ndial with *** failed\n" {
		t.Fatalf("AppOutputTails = %+v, %d", tails, gone)
	}
}

// A value registered after its line reached the tail is cut when the tail is
// read.
func TestATailReadCutsAValueRegisteredSince(t *testing.T) {
	freshTails(t)
	keployLog.SetRedactor(nil)
	c := composeLogConsumer{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	c.Log("app-1", "token "+plantedComposeValue)

	keployLog.SetRedactor(composeCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	if got := string(AppOutputTail("app-1")); got != "token ***\n" {
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
		RecordAppOutput("app-1", "app", l)
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
	appOutput.mu.Lock()
	capacity := cap(appOutput.m["app-1"].buf)
	appOutput.mu.Unlock()
	if capacity > 2*appTailMaxContainerBytes+len(line)+16 {
		t.Fatalf("buffer capacity %d grows past twice the share", capacity)
	}
}

// A line longer than its container's share keeps its end, as the first line
// the tail holds.
func TestALineLongerThanTheShareKeepsItsEnd(t *testing.T) {
	freshTails(t)
	long := strings.Repeat("a", appTailMaxContainerBytes) + "THE-END"
	RecordAppOutput("app-1", "app", long)
	held := heldBytes("app-1")
	if len(held) > appTailMaxContainerBytes || !bytes.HasSuffix(held, []byte("THE-END\n")) {
		t.Fatalf("holds %d bytes ending %q", len(held), held[len(held)-10:])
	}
}

// heldAndDropped is what the tail of name holds as stored, and the bytes it
// counts as dropped.
func heldAndDropped(name string) ([]byte, int64) {
	appOutput.mu.Lock()
	defer appOutput.mu.Unlock()
	if t := appOutput.m[name]; t != nil {
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
			RecordAppOutput("app-1", "app", c.earlier)
			RecordAppOutput("app-1", "app", c.long)
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
	RecordAppOutput("over-1", "over", "booting")
	RecordAppOutput("over-1", "over", over)
	RecordAppOutput("within-1", "within", strings.Repeat("h", 512<<10))
	RecordAppOutput("within-1", "within", within)
	RecordAppOutput("c-1", "c", "x")
	RecordAppOutput("d-1", "d", "x")
	if held := heldBytes("over-1"); string(held) != "booting\n"+over+"\n" {
		t.Fatalf("before the share shrank: over-1 holds %d bytes, want both lines whole", len(held))
	}
	RecordAppOutput("e-1", "e", "x")

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
		RecordAppOutput(fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), "hello")
	}
	trace := strings.Repeat("g", 50<<10) + "PANIC"
	RecordAppOutput("app-1", "app", strings.Repeat("i", 20<<10))
	RecordAppOutput("app-1", "app", trace)
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
	RecordAppOutput("crashed-1", "crashed", "panic: the reason it died")
	for i := 0; i < appTailMaxContainers-1; i++ {
		for j := 0; j < 200; j++ {
			RecordAppOutput(fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), chunk)
		}
	}
	if got := string(ServiceOutputTail("crashed")); got != "panic: the reason it died\n" {
		t.Fatalf("the quiet crashed container's tail = %q; want it kept", got)
	}

	for i := appTailMaxContainers - 1; i < appTailMaxContainers+43; i++ {
		for j := 0; j < 200; j++ {
			RecordAppOutput(fmt.Sprintf("svc%d-1", i), fmt.Sprintf("svc%d", i), chunk)
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
	if ServiceOutputTail("crashed") != nil || ServiceOutputTail("svc0") != nil {
		t.Fatalf("the least recently written containers were not the ones dropped")
	}
	if ServiceOutputTail(fmt.Sprintf("svc%d", appTailMaxContainers+42)) == nil {
		t.Fatalf("the newest container was dropped")
	}
}

// ServiceOutputTail answers with the service's container that printed last.
func TestServiceOutputTailIsTheContainerThatPrintedLast(t *testing.T) {
	freshTails(t)
	RecordAppOutput("app-1", "app", "old replica")
	RecordAppOutput("app-2", "app", "new replica")
	if got := string(ServiceOutputTail("app")); got != "new replica\n" {
		t.Fatalf("ServiceOutputTail = %q", got)
	}
	if ServiceOutputTail("") != nil || ServiceOutputTail("db") != nil {
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
