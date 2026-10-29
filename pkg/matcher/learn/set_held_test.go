package learn

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldRow is the row's set: n recordings, each a row of its own words
// (words of them) and an instant, and a call whose clock read and word
// changed, so the echo reads every earlier payload's instants and the word
// rule their words.
type heldRow struct {
	raw      func() []Raw
	classify func(dc *DriftCache) []Learned
	// run is classify of a dc whose application reads its clock already
	// (appClock): handlers sharing dc run it at once.
	run func(dc *DriftCache) []Learned
}

func newHeldRow(n, words int) heldRow {
	at := eRecAt
	mocks := fillers(n, at)
	for i, m := range mocks {
		w := make([]string, words)
		for j := range w {
			w[j] = fmt.Sprintf("w%dx%d", i, j)
		}
		row := m.Spec.PostgresV3.Query.Response.Rows[0]
		row[0].Value = strings.Join(w, " ")
		row[1].Value = at.Add(-time.Duration(n-i) * time.Second).Format(time.RFC3339Nano)
	}
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
	}
	const clock, word = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:55:53.9Z"}, word: {Path: word, Type: String, Value: "a1b2c3d4"}}}
	live := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:56:54.1Z"}, word: {Path: word, Type: String, Value: "z9y8x7w6"}}}
	run := func(dc *DriftCache) []Learned {
		return ClassifyFollowing(dc, []string{clock, word}, live, rec, at, nil, raw, nil, nil, nil)
	}
	return heldRow{raw: func() []Raw { return raw }, run: run, classify: func(dc *DriftCache) []Learned {
		dc.appClock = true
		return run(dc)
	}}
}

// countSpans counts the reads of a filler payload's instants (instantSpans)
// until the row ends.
func countSpans(t *testing.T) *atomic.Int64 {
	spans := instantSpans
	t.Cleanup(func() { instantSpans = spans })
	var read atomic.Int64
	instantSpans = func(s string) []instant {
		if strings.Contains(s, "filler") {
			read.Add(1)
		}
		return spans(s)
	}
	return &read
}

// Past the bytes a set keeps of its payloads' instants (heldBudget), a
// recording's are kept by none, and a set that let the recording read least
// recently go to keep the next was read anew on every call (the learn step
// reads every earlier recording in the same order). It now keeps the
// recordings it kept and reads the rest through the test's cache of texts,
// as with no set: a call never reads a payload's instants more often than a
// cache of no tracker's does, once per test, and learns what it learns.
func TestASetPastItsHeldBudgetReadsTheRestOncePerTest(t *testing.T) {
	defer func(b int64) { heldBudget = b }(heldBudget)
	row := newHeldRow(60, 40)
	want := row.classify(noSet(NewDriftCache()))
	read := countSpans(t)

	heldBudget = 16 << 10
	tr := NewTracker()
	tr.NewSet()
	for test := 1; test <= 2; test++ {
		tr.Begin()
		read.Store(0)
		first := row.classify(tr.Drifts())
		if read.Load() == 0 {
			t.Fatalf("test %d: the first call read no payload's instants: the row reads nothing", test)
		}
		if held := tr.derived.heldBytes(); held > heldBudget {
			t.Errorf("test %d: the set holds %d bytes of instants and filters, past its budget %d", test, held, heldBudget)
		}
		read.Store(0)
		second := row.classify(tr.Drifts())
		if n := read.Load(); n != 0 {
			t.Errorf("test %d: a second call read earlier payloads' instants %d times again, want 0: past its budget the set read the payloads it did not keep on every call", test, n)
		}
		for i, got := range [][]Learned{first, second} {
			if !reflect.DeepEqual(got, want) {
				t.Errorf("test %d, call %d: learned %v, want %v (a cache of no tracker's)", test, i+1, got, want)
			}
		}
		tr.End()
	}
}

// The agent's handlers of one set run the learn step at once (a pooled
// Mongo connection each), and each reads the same earlier recordings: a
// recording that none has read yet was read by every handler that reached
// it, a dozen at once. One handler now reads it while the others wait for
// what it keeps.
func TestHandlersOfOneSetReadAPayloadOnce(t *testing.T) {
	const handlers = 12
	row := newHeldRow(1, 4)
	x := row.raw()[0]
	want := noSet(NewDriftCache()).payloadMoments(x)
	if len(want) == 0 {
		t.Fatal("the row's payload holds no instant")
	}
	spans := instantSpans
	defer func() { instantSpans = spans }()
	var read atomic.Int64
	gate := make(chan struct{})
	text := x.payload().text
	instantSpans = func(s string) []instant {
		if s == text && read.Add(1) == 1 {
			<-gate
		}
		return spans(s)
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	got := make([][]moment, handlers)
	var wg sync.WaitGroup
	start := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = dc.payloadMoments(x)
		}()
	}
	start(0)
	for read.Load() == 0 {
		runtime.Gosched()
	}
	for i := 1; i < handlers; i++ {
		start(i)
	}
	waiting := func() int {
		tr.derived.mu.Lock()
		defer tr.derived.mu.Unlock()
		return tr.derived.waits
	}
	// Until every other handler waits for the first one's read, or one
	// reads the payload itself.
	for waiting() < handlers-1 && read.Load() == 1 {
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()
	tr.End()
	if n := read.Load(); n != 1 {
		t.Errorf("%d handlers read the payload's instants %d times, want once", handlers, n)
	}
	for i, m := range got {
		if !reflect.DeepEqual(m, want) {
			t.Errorf("handler %d: instants %v, want %v", i, m, want)
		}
	}
}

// A set the size of api-server's round under keploy mock record (14 813
// Mongo recordings) keeps every recording's instants and word filters, read
// once for the set, within a bound proportional to its recordings: about a
// recording's slot, 64 bytes an instant, and its filters of a fixed size,
// far under the set's budget. The heap the learn step keeps after a call
// that read every one of them is that, the words the set keeps, and the
// test's cache of texts.
func TestASetOfApiServersSizeHoldsItsInstantsWithinItsBound(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 14 813 recordings")
	}
	const n, words = 14813, 60
	// A recording's slot and id, its instants and their places (two
	// fields' worth), and its filters for words words.
	const perRecording = slotBytes + 64 + 6*headerBytes + 8*momentBytes + 2*placeBytes + 2*filterMaxBytes
	row := newHeldRow(n, words)
	read := countSpans(t)
	heap := func() int64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	before := heap()
	row.classify(dc)
	kept := heap() - before
	runtime.KeepAlive(dc)
	held := tr.derived.heldBytes()
	t.Logf("%d recordings: the set holds %d KiB of instants and filters (%d B each); the heap grew %d MiB", n, held>>10, held/n, kept>>20)
	if limit := tr.derived.budget.limit; held > n*perRecording || held > limit {
		t.Errorf("the set holds %d bytes of %d recordings' instants and filters, past %d (%d each) or its budget %d", held, n, n*perRecording, perRecording, limit)
	}
	if bound := 2*held + 2*tr.derived.keptBytes() + textBudget + 16<<20; kept > bound {
		t.Errorf("the heap grew %d MiB over a call reading %d recordings, past %d MiB (twice what the set holds and the words it keeps, the test's texts' and 16 MiB)", kept>>20, n, bound>>20)
	}
	read.Store(0)
	row.classify(dc)
	if r := read.Load(); r != 0 {
		t.Errorf("a second call read earlier payloads' instants %d times again, want 0", r)
	}
	tr.End()
}
