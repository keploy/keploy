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

// A reader reads the parts of a recording it reads in turn (a call's warm
// and the staging's, warmOne; the staging's read of what a call's read
// left, stageRest) from one load of its payload, held until its last part
// is read (Raw.holding). Here the set's store keeps no payload, as a store
// past its budget lets go of one while other readers load theirs, so a
// reader reading from the store found the payload gone at each part after
// its first and loaded it again: one more decode of the whole recording
// each time (the stamps row saw it once in some runs under load). Counted:
// each reader loads each payload once, whatever the readers' count, and
// reads the parts a store that keeps every payload gives.
func TestAReaderReadsARecordingsPartsFromOneLoadOfItsPayload(t *testing.T) {
	const n = 6
	mocks := stampedAnswers(n, eRecAt)
	for _, m := range mocks {
		// A stamp: an HTTP-date of when its answer was recorded.
		m.Spec.HTTPResp.Header["Date"] = m.Spec.ResTimestampMock.Format(http.TimeFormat)
	}
	type parts struct {
		moments []moment
		keys    []instantKey
		stamps  []string
		places  []keyedPlace
		fields  [][]moment
		forms   [][]moment
	}
	partsOf := func(dc *DriftCache, raw []Raw) []parts {
		out := make([]parts, len(raw))
		for i, x := range raw {
			p := &out[i]
			p.moments, p.stamps = dc.payloadMoments(x), dc.stampTexts(x)
			p.keys, _ = dc.searchedKeys(x)
			p.places, _ = dc.setPlaces(x)
			p.fields, _ = dc.fieldMoments(x)
			p.forms, _ = dc.fieldForms(x)
		}
		return out
	}
	lazy := func(dc *DriftCache) []Raw {
		raw := make([]Raw, n)
		for i, m := range mocks {
			raw[i] = dc.LazyRawOf(m, false, nil)
		}
		return raw
	}
	keptAll := func() []parts {
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		raw := lazy(dc)
		if err := dc.warmSet(srcOf(mocks), 1); err != nil {
			t.Fatal(err)
		}
		return partsOf(dc, raw)
	}()
	if len(keptAll[2].stamps) == 0 || len(keptAll[2].moments) == 0 || len(keptAll[2].places) == 0 {
		t.Fatalf("a payload's parts are %+v: the row needs stamps, instants and places in each", keptAll[2])
	}
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	for _, readers := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d readers", readers), func(t *testing.T) {
			warmReadersOf(t, readers)
			tr := NewTracker()
			tr.NewSet()
			tr.Begin()
			defer tr.End()
			dc := tr.Drifts()
			raw := lazy(dc)
			dc.warm(raw)
			if got := dc.Loads(); got != n {
				t.Fatalf("a call's warm with %d readers loaded %d payloads of %d recordings, its set's store keeping none: want each once", readers, got, n)
			}
			if err := dc.warmSet(srcOf(mocks), readers); err != nil {
				t.Fatal(err)
			}
			if staged, called, _ := readsOf(dc.set); staged != 0 || called != n || dc.set.finished != n {
				t.Fatalf("the call read %d, the staging %d and read the rest of %d: want the call all %d, the staging the rest of each", called, staged, dc.set.finished, n)
			}
			if got := dc.Loads() - n; got != n {
				t.Fatalf("the staging's read of what the call's read left loaded %d payloads of %d recordings: want each once", got, n)
			}
			if got := partsOf(dc, raw); !reflect.DeepEqual(got, keptAll) {
				t.Errorf("the parts read from one load of each payload are %+v: a store that keeps every payload gives %+v", got, keptAll)
			}

			st := NewTracker()
			st.NewSet()
			st.Begin()
			defer st.End()
			sc := st.Drifts()
			if err := sc.warmSet(srcOf(mocks), readers); err != nil {
				t.Fatal(err)
			}
			if staged, _, _ := readsOf(sc.set); staged != n {
				t.Fatalf("the staging read %d of %d payloads: want all", staged, n)
			}
			if got := sc.Loads(); got != n {
				t.Fatalf("the staging with %d readers loaded %d payloads of %d recordings, its set's store keeping none: want each once", readers, got, n)
			}
			if got := partsOf(sc, lazy(sc)); !reflect.DeepEqual(got, keptAll) {
				t.Errorf("the staging's parts read from one load of each payload are %+v: a store that keeps every payload gives %+v", got, keptAll)
			}
		})
	}
}

// The payload a reader holds for a recording's parts (Raw.holding) is its
// copy's alone: the Raws a call gives its warm hold none once it ends, so
// what the set's store lets go of is let go for them too. A warm that held
// it in the Raw it was given would keep every payload it read alive for as
// long as the call keeps its Raws, past the set's budget. Counted: with a
// store that keeps no payload, each of the call's Raws read after its warm
// loads its payload again.
func TestTheRawsACallWarmsHoldNoPayloadOnceItsWarmEnds(t *testing.T) {
	const n = 4
	mocks := stampedAnswers(n, eRecAt)
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	for _, readers := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d readers", readers), func(t *testing.T) {
			warmReadersOf(t, readers)
			tr := NewTracker()
			tr.NewSet()
			tr.Begin()
			defer tr.End()
			dc := tr.Drifts()
			raw := make([]Raw, n)
			for i, m := range mocks {
				raw[i] = dc.LazyRawOf(m, false, nil)
			}
			dc.warm(raw)
			if _, called, _ := readsOf(dc.set); called != n || dc.Loads() != n {
				t.Fatalf("the call's warm read %d of %d payloads, loading %d: want each once", called, n, dc.Loads())
			}
			for i, x := range raw {
				if x.src.held != nil {
					t.Errorf("the call's Raw of recording %d holds a payload after its warm: want the reader's copy alone to", i)
				}
				if len(x.payload().text) == 0 {
					t.Fatalf("recording %d read no payload", i)
				}
			}
			if got := dc.Loads() - n; got != n {
				t.Errorf("reading the call's %d Raws after its warm loaded %d payloads, its set's store keeping none: want each again", n, got)
			}
		})
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
	if w := readWeight(raw[0], readCost); w != readInFlight {
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

// The set's staging (WarmSet) reserves each read's cost from the same
// readInFlight as the calls: while one of its readers is held in a payload
// costing all of it, the staging waits for room (inFlightWaiting) and hands
// its other reader none, and once the held read is let go it reads every
// payload, each once, and gives back all it reserved. The staging visits
// its recordings in order and reserves a read before a reader has it, so
// the held payload, the first, holds all of readInFlight before any other
// is handed out: no other payload is read while it is held, whatever the
// readers' order. Counted, not timed: the row sees the staging waiting.
func TestAStagingReadCostingAllTheSetsReadsInFlightRunsAlone(t *testing.T) {
	const n = 6
	row := newHeldRow(n, 4)
	was := stageReaders
	stageReaders = func() int { return 2 }
	t.Cleanup(func() { stageReaders = was })
	for _, m := range row.mocks {
		m.SetStoredSize(readInFlight / stageCost)
	}
	raw := row.raw()
	tr := NewTracker()
	tr.NewSet()
	s := tr.Drifts().set
	if w := readWeight(tr.Drifts().LazyRawOf(row.mocks[0], false, withAnswer(row.mocks[0])), stageCost); w != readInFlight {
		t.Fatalf("a recording of %d bytes costs the staging %d of the %d in flight: want all of them", readInFlight/stageCost, w, readInFlight)
	}
	waiting := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inFlightWaiting
	}
	counts, reading, begun, release := heldUntil(t, raw, raw[0].payload().text)
	done := tr.WarmSet(srcOf(row.mocks))
	<-reading
	for waiting() == 0 {
		if _, d := decodesOf(counts); d != 0 {
			t.Fatalf("%d payloads were read while the staging read one costing all the set's reads in flight: want none", d)
		}
		select {
		case <-done:
			t.Fatal("the set's staging ended while a payload's read was held: want it to wait for that read")
		default:
		}
		runtime.Gosched()
	}
	if _, d := decodesOf(counts); d != 0 {
		t.Fatalf("%d payloads were read while the staging read one costing all the set's reads in flight: want none", d)
	}
	release()
	use := <-done
	if b := begun.Load(); b != 1 {
		t.Errorf("the held payload was read %d times: want once", b)
	}
	if use.Readers != 2 || use.Staged != n || use.Called != 0 {
		t.Errorf("the set's staging reported %+v: want 2 readers, all %d payloads read by it, none by a call", use, n)
	}
	if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
		t.Errorf("the staging's readers decoded %d of the set's %d payloads, %d times in all: want each once", payloads, n, decodes)
	}
	if !s.inFlight.TryAcquire(readInFlight) {
		t.Errorf("the staging kept some of the set's reads in flight after it ended: want all given back")
	}
}

// What a read reserves of the set's readInFlight (readWeight) is readBase
// and its cost (a call's readCost, the staging's stageCost) for each byte
// of its recording, known before its payload is read: a read
// Raw's text; the agent store's count of a lazy one's (StoredSize); the
// bytes of the spec of one no store counted whose answer is in memory
// (specBytes); and all of readInFlight, a read alone, for one whose answer
// is parked and no store counted, or one that costs more. A nil recording
// counts as not known.
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
	for _, cost := range []int64{readCost, stageCost} {
		for _, c := range []struct {
			name string
			x    Raw
			want int64
		}{
			{"a read Raw, by its text", read, readBase + cost*int64(len(read.rText))},
			{"a lazy one the store counted", dc.LazyRawOf(counted, true, nil), readBase + cost*1000},
			{"a lazy one in memory no store counted, by its spec", dc.LazyRawOf(inMemory, true, nil), readBase + cost*spec},
			{"a lazy one parked no store counted, alone", dc.LazyRawOf(parked, true, nil), readInFlight},
			{"one costing more than readInFlight, alone", dc.LazyRawOf(big, true, nil), readInFlight},
		} {
			if got := readWeight(c.x, cost); got != c.want {
				t.Errorf("%s reserves %d at %d a byte: want %d", c.name, got, cost, c.want)
			}
		}
	}
	if n := recordingBytes(nil); n != -1 {
		t.Errorf("a nil recording counts %d bytes: want -1, not known", n)
	}
	if spec <= 0 || readBase+stageCost*spec >= readInFlight {
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

// What a read reserves of the set's readInFlight, at literal sizes: 8 MiB
// in flight at once; a call's read of a recording of 4 KiB the agent's
// store counted reserves 1 216 KiB (64 KiB and 288 bytes for each of its
// bytes), so six run at once, the staging's 3 136 KiB (64 KiB and 768 a
// byte), two at once; of 16 KiB, a call's 4 672 KiB, one at a time, the
// staging's all of it; of 1 MiB or 40 KiB, all of it on either path, a
// read alone, as the bound row's are read. Literal, so that a cost or a
// cap changed in the code fails here and not only in the rows that follow
// the constants. And each path is seen to reserve its own: while its one
// reader is held in a 4 KiB recording's read, what is left of readInFlight
// is all but that read's literal cost, not a byte more or less.
func TestAReadReservesItsRecordingsLiteralCost(t *testing.T) {
	if readInFlight != 8<<20 {
		t.Fatalf("the set's readers have %d bytes in flight at once: want 8 MiB", readInFlight)
	}
	const callOf4KiB, stagedOf4KiB = 1216 << 10, 3136 << 10
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	for _, c := range []struct {
		stored, call, staged int64
	}{
		{1 << 20, 8 << 20, 8 << 20},
		{40 << 10, 8 << 20, 8 << 20},
		{16 << 10, 4672 << 10, 8 << 20},
		{4 << 10, callOf4KiB, stagedOf4KiB},
		{1000, 353536, 833536},
	} {
		m := newHeldRow(1, 4).mocks[0]
		m.SetStoredSize(c.stored)
		x := dc.LazyRawOf(m, true, nil)
		if got := readWeight(x, readCost); got != c.call {
			t.Errorf("a call's read of a recording of %d bytes the store counted reserves %d of the set's reads in flight: want %d", c.stored, got, c.call)
		}
		if got := readWeight(x, stageCost); got != c.staged {
			t.Errorf("the staging's read of a recording of %d bytes the store counted reserves %d of the set's reads in flight: want %d", c.stored, got, c.staged)
		}
	}

	row := newHeldRow(2, 4)
	for _, m := range row.mocks {
		m.SetStoredSize(4 << 10)
	}
	raw := row.raw()
	leftWhileHeld := func(t *testing.T, s *setDerived, who string, want int64) {
		t.Helper()
		if !s.inFlight.TryAcquire(readInFlight - want) {
			t.Errorf("%s reserved more than %d of the set's reads in flight for its held read of 4 KiB", who, want)
			return
		}
		if s.inFlight.TryAcquire(1) {
			s.inFlight.Release(1)
			t.Errorf("%s reserved less than %d of the set's reads in flight for its held read of 4 KiB", who, want)
		}
		s.inFlight.Release(readInFlight - want)
	}
	t.Run("a call's warm", func(t *testing.T) {
		warmReadersOf(t, 1)
		_, reading, _, release := heldUntil(t, raw, raw[0].payload().text)
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		call := []Raw{dc.LazyRawOf(row.mocks[0], true, nil), dc.LazyRawOf(row.mocks[1], true, nil)}
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			dc.warm(call)
		}()
		<-reading
		leftWhileHeld(t, dc.set, "a call's warm", callOf4KiB)
		release()
		<-ended
	})
	t.Run("the staging", func(t *testing.T) {
		oneStageReader(t)
		_, reading, _, release := heldUntil(t, raw, raw[0].payload().text)
		tr := NewTracker()
		tr.NewSet()
		s := tr.derived
		staged := tr.WarmSet(srcOf(row.mocks))
		<-reading
		leftWhileHeld(t, s, "the staging", stagedOf4KiB)
		release()
		if use := <-staged; use.Staged != len(row.mocks) || use.Err != nil {
			t.Errorf("the set's staging reported %+v: want both recordings read by it", use)
		}
	})
}

// What one read of a payload allocates (its load, bodyOf, and the parts
// its reader reads of it), with its recording counted by the agent's store
// (models.Mock.StoredSize), is at most the cost its reader reserves for it
// before it reads: readBase and, for each byte, readCost on a call's warm
// (warmOne: the parts every call's rules read) and stageCost on the
// staging's, whether it reads the whole payload (warmOne staged: those
// parts and stagedParts) or the rest of one a call read first (stageRest).
// A collection running through a read keeps all it allocated until the
// next one, so the reservation is what bounds the readers' memory in
// flight whatever their count. The set's store keeps no payload here, so
// each read loads its own, as past the set's budget. Read on payloads from
// the sparsest in instants (the HTTP bound row's words) to the densest (a
// list of dates, each a candidate read in several layouts): at 16 bytes,
// 256 and 1 KiB, where readBase counts most; at 16 KiB and 31 KiB, under
// the sizes read alone, where the cost decides how many reads run at once;
// and a call's at 1 MiB. A read that came to cost more, or a cost set
// lower, fails here; the per-byte figures are logged.
func TestAReadAllocatesNoMoreThanItsCost(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 55, 53, 0, time.UTC)
	list := func(size int, open, close string, item func(i int) string) string {
		var b strings.Builder
		b.WriteString(open)
		for i := 0; b.Len() < size; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(item(i))
		}
		b.WriteString(close)
		return b.String()
	}
	words := func(size int) string {
		var b strings.Builder
		b.WriteString(`{"blob":"`)
		for i := 0; b.Len() < size; i++ {
			fmt.Fprintf(&b, "w7x%d ", i)
		}
		b.WriteString(`"}`)
		return b.String()
	}
	quoted := func(layout string, step time.Duration) func(i int) string {
		return func(i int) string { return `"` + at.Add(time.Duration(i)*step).Format(layout) + `"` }
	}
	shapes := []struct {
		name string
		body func(size int) string
	}{
		{"words", words},
		{"items with an instant each", func(n int) string {
			return list(n, `{"items":[`, `]}`, func(i int) string {
				return fmt.Sprintf(`{"id":"it-%d","n":%d,"made":"%s"}`, i, i, at.Add(time.Duration(i)*time.Second).Format(time.RFC3339))
			})
		}},
		{"uuids", func(n int) string {
			return list(n, "[", "]", func(i int) string { return fmt.Sprintf(`"%08x-1234-4abc-8def-%012x"`, i, i*7) })
		}},
		{"RFC 3339 instants", func(n int) string { return list(n, "[", "]", quoted(time.RFC3339, time.Second)) }},
		{"HTTP-dates", func(n int) string { return list(n, "[", "]", quoted(time.RFC1123, time.Second)) }},
		{"epoch seconds", func(n int) string { return list(n, "[", "]", func(i int) string { return fmt.Sprint(1792656120 + i) }) }},
		{"epoch milliseconds", func(n int) string {
			return list(n, "[", "]", func(i int) string { return fmt.Sprint(int64(1792656120000) + int64(i)) })
		}},
		{"times of day", func(n int) string { return list(n, "[", "]", quoted("15:04:05", time.Second)) }},
		{"dates", func(n int) string { return list(n, "[", "]", quoted("2006-01-02", 24*time.Hour)) }},
	}
	warmReadersOf(t, 1)
	oneStageReader(t)
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	// read is one read of body's recording on path, and what it allocated.
	read := func(path, body string) int64 {
		m := getAnswer("big-0", "http://api/blobs/0", body, eRecAt)
		m.SetStoredSize(int64(len(body)))
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		x := dc.LazyRawOf(m, true, nil)
		loads := 1
		if path == "rest" {
			dc.warm([]Raw{x})
			loads = 2
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if path == "call" {
			dc.warm([]Raw{x})
		} else if err := dc.warmSet(srcOf([]*models.Mock{m}), 1); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		staged, called, _ := readsOf(dc.set)
		if ok := map[string]bool{"call": staged == 0 && called == 1, "staged": staged == 1 && called == 0,
			"rest": staged == 0 && called == 1 && dc.set.finished == 1}[path]; !ok || dc.Loads() != loads {
			t.Fatalf("%s read of %d bytes: the staging read %d, a call %d, the rest of %d, loading %d: want the one read, %d loads", path, len(body), staged, called, dc.set.finished, dc.Loads(), loads)
		}
		return int64(after.TotalAlloc - before.TotalAlloc)
	}
	// A first read sets up what every read after it shares (its tables).
	read("staged", shapes[len(shapes)-1].body(1<<10))
	for _, size := range []int{16, 256, 1 << 10, 16 << 10, 31 << 10, 1 << 20} {
		for _, sh := range shapes {
			body := sh.body(size)
			for _, c := range []struct {
				path string
				cost int64
			}{{"call", readCost}, {"staged", stageCost}, {"rest", stageCost}} {
				if size == 1<<20 && c.path != "call" {
					continue
				}
				began := time.Now()
				alloc := read(c.path, body)
				t.Logf("%s, %s read of %d bytes: allocated %d, %.1f a byte past readBase (its cost: %d a byte) in %v", sh.name, c.path, len(body), alloc, float64(alloc-readBase)/float64(len(body)), c.cost, time.Since(began).Round(time.Millisecond))
				if want := readBase + c.cost*int64(len(body)); alloc > want {
					t.Errorf("%s: a %s read of a %d-byte recording allocated %d bytes: past the %d it costs (readBase and %d a byte)", sh.name, c.path, len(body), alloc, want, c.cost)
				}
			}
		}
	}
}
