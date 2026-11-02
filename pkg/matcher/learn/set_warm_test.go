package learn

import (
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// warmReadersOf has a call's warm read with n readers for the row
// (warmReaders).
func warmReadersOf(t *testing.T, n int) {
	t.Helper()
	was := warmReaders
	warmReaders = func() int { return n }
	t.Cleanup(func() { warmReaders = was })
}

// A call reads the recordings before it with warmReaders readers at once:
// while one reader is held in its payload's read, another reads every other
// payload, each once, and the call learns what one reader's read learns.
// Counted, not timed: the held read is let go only once the others are
// read; a call that read them one at a time would never get past it.
func TestOneHandlersReadersReadTheRecordingsBeforeItsCallAtOnce(t *testing.T) {
	const n = 24
	row := newHeldRow(n, 4)
	raw := row.raw()
	plain := NewTracker()
	plain.NewSet()
	plain.Begin()
	want := row.classify(plain.Drifts())
	plain.End()

	warmReadersOf(t, 2)
	counts, reading, begun, release := heldUntil(t, raw, raw[0].payload().text)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	s := dc.set
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		dc.warm(raw)
	}()
	<-reading
	for {
		if _, called, _ := readsOf(s); called == n-1 {
			break
		}
		select {
		case <-ended:
			t.Fatal("the call's warm ended while a payload's read was held: want it to wait for that read")
		default:
		}
		runtime.Gosched()
	}
	release()
	<-ended
	if staged, called, waits := readsOf(s); staged != 0 || called != n || waits != 0 {
		t.Errorf("the call's two readers read %d of its %d payloads (the staging %d) and waited %d times: want each read by the call, no wait", called, n, staged, waits)
	}
	if b := begun.Load(); b != 1 {
		t.Errorf("the held payload was read %d times: want once", b)
	}
	if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
		t.Errorf("the call's readers decoded %d of the set's %d payloads, %d times in all: want each once", payloads, n, decodes)
	}
	if got := row.classify(dc); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the set read by two readers learned\n%v\nwhere one reader's read learned\n%v", got, want)
	}
}

// A panic in a reader's read of a call's warm is the call's own, raised on
// the handler's goroutine once the readers end, where the parser's recover
// (integrations.RecoverCtx) ends the one connection: never a panic on a
// goroutine of its own, which would end the agent. The other reader reads
// on, each payload once.
func TestAPanicInOneHandlersWarmReaderIsTheHandlersOwn(t *testing.T) {
	const n = 8
	row := newHeldRow(n, 4)
	raw := row.raw()
	warmReadersOf(t, 2)
	first := raw[0].payload().text
	counts := countDecodes(t, raw, func(text string) {
		if text == first {
			panic("a payload's read failed")
		}
	})
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	got := func() (r any) {
		defer func() { r = recover() }()
		dc.warm(raw)
		return nil
	}()
	if got != "a payload's read failed" {
		t.Fatalf("the call's warm, a reader's read panicking, gave %v on the call's goroutine: want that panic", got)
	}
	if _, called, _ := readsOf(dc.set); called != n {
		t.Errorf("the call's readers ended %d of its %d payloads' reads: want every one (the panicking one too)", called, n)
	}
	if payloads, decodes := decodesOf(counts); payloads != n-1 || decodes != n-1 {
		t.Errorf("the other reader decoded %d of the %d payloads that read whole, %d times: want each once", payloads, n-1, decodes)
	}
}

// A call's warm reads each payload's answer stamps (stampTexts) with the
// payload in hand, the stamps a drifted value's search reads of every
// payload before it searches one (stampsEchoed): with the set's payloads
// past its store's budget, let go as they are read, the search then loads
// none of them back for the stamps, and reads the stamps a payload holds.
// Before, it loaded every payload let go once more for its stamps alone.
func TestACallsWarmReadsEachPayloadsStampsWithThePayload(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 256 << 10
	at := time.Date(2026, 10, 24, 9, 55, 53, 0, time.UTC)
	mocks := make([]*models.Mock, 300)
	for i := range mocks {
		mocks[i] = getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/"+fmt.Sprint(i), `{"note":"`+strings.Repeat(fmt.Sprintf("w%d ", i), 1500)+`"}`, at.Add(-time.Duration(300-i)*time.Second))
		mocks[i].Spec.HTTPResp.Header["Date"] = mocks[i].Spec.ResTimestampMock.Format(http.TimeFormat)
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = dc.LazyRawOf(m, false, nil)
	}
	dc.warm(raw)
	loads := dc.Loads()
	if loads != len(mocks) {
		t.Fatalf("the call's warm loaded %d payloads: want each of the %d once", loads, len(mocks))
	}
	if kept := len(dc.set.bodies.kept.by); kept*4 > len(mocks) {
		t.Fatalf("the store keeps %d of the %d payloads: the row needs most of them let go", kept, len(mocks))
	}
	stamped := 0
	for i, x := range raw {
		r := rawOfNow(mocks[i], false)
		var want []string
		for _, f := range r.rStamps {
			want = append(want, r.rText[f.start:f.end])
		}
		if got := dc.stampTexts(x); !slices.Equal(got, want) {
			t.Fatalf("%s: the stamps read are %q, its payload holds %q", mocks[i].Name, got, want)
		}
		if len(want) > 0 {
			stamped++
		}
	}
	if stamped != len(mocks) {
		t.Fatalf("%d of the %d answers hold a stamp: the row needs each to", stamped, len(mocks))
	}
	if n := dc.Loads(); n != loads {
		t.Errorf("reading the stamps of %d payloads the call's warm read loaded %d of them again: want none", len(mocks), n-loads)
	}
}

// A read whose cost (readWeight) is all of the set's readInFlight runs
// alone: while one reader is held in such a payload's read, the other
// waits for room (inFlightWaiting) and begins none, and once the held read
// is let go every payload is read, each once. Counted, not timed: the row
// sees the other reader waiting.
func TestAHandlersReadCostingAllTheSetsReadsInFlightRunsAlone(t *testing.T) {
	const n = 6
	row := newHeldRow(n, 4)
	warmReadersOf(t, 2)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	s := dc.set
	raw := make([]Raw, n)
	for i, m := range row.mocks {
		m.SetStoredSize(readInFlight / readCost)
		raw[i] = dc.LazyRawOf(m, true, nil)
	}
	if w := readWeight(raw[0]); w != readInFlight {
		t.Fatalf("a recording of %d bytes costs %d of the %d in flight: want all of them", readInFlight/readCost, w, readInFlight)
	}
	waiting := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inFlightWaiting
	}
	var atHold int64
	held := raw[0].payload().text
	counts, reading, begun, release := heldUntil(t, raw, held)
	decodeHeld := decodeForms
	decodeForms = func(text string) decoded {
		if text == held {
			_, atHold = decodesOf(counts)
		}
		return decodeHeld(text)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		dc.warm(raw)
	}()
	<-reading
	for waiting() == 0 {
		if _, d := decodesOf(counts); d != atHold {
			t.Fatalf("%d payloads' reads began while one costing all the set's reads in flight was held: want none", d-atHold)
		}
		select {
		case <-ended:
			t.Fatal("the call's warm ended while a payload's read was held: want it to wait for that read")
		default:
		}
		runtime.Gosched()
	}
	if _, d := decodesOf(counts); d != atHold {
		t.Fatalf("%d payloads' reads began while one costing all the set's reads in flight was held: want none", d-atHold)
	}
	release()
	<-ended
	if b := begun.Load(); b != 1 {
		t.Errorf("the held payload was read %d times: want once", b)
	}
	if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
		t.Errorf("the call's readers decoded %d of the set's %d payloads, %d times in all: want each once", payloads, n, decodes)
	}
}

// What a read reserves of the set's readInFlight (readWeight) is readCost
// for each byte of its recording, known before its payload is read: a read
// Raw's text; the agent store's count of a lazy one's (StoredSize); the
// bytes of the spec of one no store counted whose answer is in memory
// (specBytes); and all of readInFlight, a read alone, for one whose answer
// is parked and no store counted, or one that costs more.
func TestAReadReservesWhatItsRecordingsBytesCost(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	small := newHeldRow(1, 4).mocks[0]
	read := rawOfNow(small, true)
	counted := newHeldRow(1, 4).mocks[0]
	counted.SetStoredSize(1000)
	parked := newHeldRow(1, 4).mocks[0]
	parked.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, nil, nil })
	big := newHeldRow(1, 4).mocks[0]
	big.SetStoredSize(readInFlight)
	inMemory := newHeldRow(1, 4).mocks[0]
	spec := specBytes(reflect.ValueOf(inMemory.Spec), 0)
	for _, c := range []struct {
		name string
		x    Raw
		want int64
	}{
		{"a read Raw, by its text", read, readCost * int64(len(read.rText))},
		{"a lazy one the store counted", dc.LazyRawOf(counted, true, nil), readCost * 1000},
		{"a lazy one in memory no store counted, by its spec", dc.LazyRawOf(inMemory, true, nil), readCost * spec},
		{"a lazy one parked no store counted, alone", dc.LazyRawOf(parked, true, nil), readInFlight},
		{"one costing more than readInFlight, alone", dc.LazyRawOf(big, true, nil), readInFlight},
	} {
		if got := readWeight(c.x); got != c.want {
			t.Errorf("%s reserves %d: want %d", c.name, got, c.want)
		}
	}
	if spec <= 0 || readCost*spec >= readInFlight {
		t.Errorf("a small in-memory recording's spec counts %d bytes: want some, under what runs alone", spec)
	}

	type inner struct {
		S string
		b []byte
	}
	type shape struct {
		Text  string
		Bytes []byte
		Ptr   *inner
		Nil   *inner
		List  []inner
		Map   map[string]string
		Any   any
		At    time.Time
		Num   int
		Arr   [2]string
	}
	v := shape{Text: "abc", Bytes: []byte("defg"), Ptr: &inner{S: "hi", b: []byte("j")}, List: []inner{{S: "kl"}, {b: []byte("mno")}},
		Map: map[string]string{"pq": "rst"}, Any: "uvwx", At: time.Now(), Num: 7, Arr: [2]string{"y", "zz"}}
	// 3 + 4 + (2 + 1) + 0 + (2 + 0) + (0 + 3) + (2 + 3) + 4 + 8 (the time) + 8 (the number) + (1 + 2)
	if got, want := specBytes(reflect.ValueOf(v), 0), int64(3+4+3+2+3+5+4+8+8+3); got != want {
		t.Errorf("specBytes counts %d bytes of the shape: want %d", got, want)
	}
	type loop struct{ Next *loop }
	l := &loop{}
	l.Next = l
	if got := specBytes(reflect.ValueOf(l), 0); got != 0 {
		t.Errorf("specBytes of a value that holds itself counts %d: want 0, no deeper than its limit", got)
	}
}
