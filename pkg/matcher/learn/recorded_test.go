package learn

import (
	"bytes"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
)

// recordingOf is a recording of the set as a store decodes it.
func recordingOf(name string) *models.Mock {
	at := time.Unix(100, 0).UTC()
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		HTTPReq: &models.HTTPReq{URL: "http://svc/" + name}}}
}

// decodeOf counts the decodes of the recording named name in *n.
func decodeOf(name string, n *int) func() (*models.Mock, error) {
	return func() (*models.Mock, error) {
		*n++
		return recordingOf(name), nil
	}
}

// A set keeps a recording it has room for decoded: each read after the
// first decodes none, whatever the number of reads, and hands a recording
// equal to the one decode gives, one of the reader's own, so that a field a
// reader sets is not what the next reader reads. The set's next set keeps
// none, and decodes it again.
func TestOutcomeCostASetDecodesARecordingOnceWhereItHasRoom(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	var n int
	var got []*models.Mock
	for range 5 {
		m, err := tr.KeepRecording("a", 1<<10, decodeOf("a", &n))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if n != 1 || tr.RecordingDecodes() != 1 {
		t.Errorf("five reads decoded %d (counted %d); want 1", n, tr.RecordingDecodes())
	}
	for i, m := range got {
		if !reflect.DeepEqual(m, recordingOf("a")) {
			t.Errorf("read %d gave %+v; want the recording as decoded", i, m)
		}
		for _, o := range got[:i] {
			if m == o {
				t.Errorf("read %d shares its recording with an earlier read", i)
			}
		}
	}
	got[0].Name, got[0].Spec.HTTPReq = "edited", nil
	if m, _ := tr.KeepRecording("a", 1<<10, decodeOf("a", &n)); !reflect.DeepEqual(m, recordingOf("a")) {
		t.Errorf("after a reader set its fields the set handed %+v; want the recording as decoded", m)
	}
	was := tr.derived
	tr.NewSet()
	if used := was.usedBytes(); used != 0 {
		t.Errorf("the set over still keeps %d bytes; want none, let go as the next set began", used)
	}
	if _, _ = tr.KeepRecording("a", 1<<10, decodeOf("a", &n)); n != 2 || tr.RecordingDecodes() != 1 {
		t.Errorf("the next set decoded %d in all (its count %d); want the recording decoded again, once", n, tr.RecordingDecodes())
	}
}

// Past the set's budget a recording is decoded at each read, as with no
// set, and the same recording is handed: a recording larger than the whole
// budget is kept by none, and one the other parts of the set leave no room
// for is kept by none and lets none of them go. A decode that fails is
// kept by none, and its error handed on.
func TestOutcomeCostARecordingPastTheSetsBudgetIsDecodedAtEachRead(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 64 << 10
	tr := NewTracker()
	tr.NewSet()
	var n int
	measured := recordingMeasures.Load()
	for range 3 {
		m, err := tr.KeepRecording("big", 64<<10, decodeOf("big", &n))
		if err != nil || !reflect.DeepEqual(m, recordingOf("big")) {
			t.Fatalf("a recording past the budget read as %+v, %v; want the recording as decoded", m, err)
		}
	}
	if d := recordingMeasures.Load() - measured; n != 3 || d != 0 {
		t.Errorf("three reads of a recording past the budget decoded %d and measured %d; want 3, none", n, d)
	}
	// The other parts take all but 4 KiB of the budget: a recording of
	// 8 KiB is decoded at each read, and the other parts keep theirs, and
	// so does the recording of 1 KiB kept before: one with no room lets
	// none go.
	n = 0
	if _, err := tr.KeepRecording("small", 1<<10, decodeOf("small", &n)); err != nil || n != 1 {
		t.Fatalf("a small recording: %v, %d decodes; want 1", err, n)
	}
	s := tr.derived
	s.mu.Lock()
	s.held.keep("other", partStamps, learnBudget-4<<10-slotBytes-int64(len("other")), func(*heldParts) {})
	used := s.held.used
	s.mu.Unlock()
	if used == 0 {
		t.Fatal("the row's other part was not kept")
	}
	measured = recordingMeasures.Load()
	for range 3 {
		if m, err := tr.KeepRecording("mid", 8<<10, decodeOf("mid", &n)); err != nil || !reflect.DeepEqual(m, recordingOf("mid")) {
			t.Fatalf("a recording with no room read as %+v, %v; want the recording as decoded", m, err)
		}
	}
	// Decoded at each read, as with no set, and measured at none: its
	// size has no room before it is measured (recRoom).
	if d := recordingMeasures.Load() - measured; d != 0 {
		t.Errorf("a recording with no room was measured %d times in 3 reads; want none", d)
	}
	s.mu.Lock()
	heldNow, recs := s.held.used, s.recs.used
	s.mu.Unlock()
	if _, err := tr.KeepRecording("small", 1<<10, decodeOf("small", &n)); err != nil || n != 4 || heldNow != used || recs == 0 {
		t.Errorf("with no room: %d decodes of a kept recording and 3 reads of one with none, the other parts %d bytes (were %d), recordings %d bytes; want 4, %d, the kept one's", n, heldNow, used, recs, used)
	}
	n = 3
	fail := errors.New("no record")
	for range 2 {
		if _, err := tr.KeepRecording("bad", 1<<10, func() (*models.Mock, error) { n++; return nil, fail }); !errors.Is(err, fail) {
			t.Errorf("a failed decode read as %v; want its error", err)
		}
	}
	if n != 5 {
		t.Errorf("two reads of a recording that failed to decode decoded %d more; want 2", n-3)
	}
}

// The recordings keep only the room the set's budget has spare: where it
// is full they let go of the one read least recently for the next, and the
// first part of the set's other tiers that needs room has them let go
// before any of its own.
func TestOutcomeCostTheSetsRecordingsKeepOnlyTheRoomItsBudgetHasSpare(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 16 << 10
	tr := NewTracker()
	tr.NewSet()
	var n int
	read := func(name string) {
		t.Helper()
		if _, err := tr.KeepRecording(name, 6<<10, decodeOf(name, &n)); err != nil {
			t.Fatal(err)
		}
	}
	read("a")
	read("b")
	read("c") // no room for three: a, read least recently, goes
	if n != 3 {
		t.Fatalf("three recordings decoded %d; want 3", n)
	}
	read("c")
	read("b")
	if n != 3 {
		t.Errorf("the two kept read again decoded %d more; want none", n-3)
	}
	read("a")
	if n != 4 {
		t.Errorf("the recording let go read again decoded %d more; want 1", n-3)
	}
	// The words tier needs room: the recordings go first.
	s := tr.derived
	s.mu.Lock()
	s.words.keep("w", partExtra, 8<<10, func(*wordParts) {})
	words, recs, used := s.words.used, s.recs.used, s.budget.used
	usedAll := s.usedBytes
	s.mu.Unlock()
	if words == 0 || used > learnBudget || recs > learnBudget-words {
		t.Errorf("a word part of 8 KiB beside the recordings: words %d, recordings %d, set %d of %d; want the words kept and the recordings within what is left", words, recs, used, learnBudget)
	}
	if got := usedAll(); got != used {
		t.Errorf("the set counts %d bytes in its tiers; want %d, the budget's", got, used)
	}
}

// Where another tier holds part of the set's budget, the recordings keep
// within the room it leaves, though they are within their own cap (the
// set's budget, noCap): the next recording lets go of the one read least
// recently, and the set never holds past its budget.
func TestOutcomeCostTheSetsRecordingsLetGoForTheRoomTheOtherTiersLeave(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 16 << 10
	tr := NewTracker()
	tr.NewSet()
	s := tr.derived
	s.mu.Lock()
	s.held.keep("other", partStamps, 6<<10, func(*heldParts) {})
	held := s.held.used
	s.mu.Unlock()
	var n int
	for _, name := range []string{"a", "b", "c", "b", "a"} {
		if _, err := tr.KeepRecording(name, 4<<10, decodeOf(name, &n)); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		used, heldNow, recCap := s.budget.used, s.held.used, s.recs.cap
		s.mu.Unlock()
		if used > learnBudget || heldNow != held || recCap != learnBudget {
			t.Fatalf("after %s: the set holds %d of %d, the other tier %d (was %d), the recordings' cap %d; want the set within its budget, the other tier's part kept, the cap the budget", name, used, learnBudget, heldNow, held, recCap)
		}
	}
	// a, b, c decoded; c lets a go; b read again kept; a decoded again.
	if n != 4 {
		t.Errorf("five reads of three recordings, two kept at a time beside the other tier, decoded %d; want 4", n)
	}
}

// A row's cap on the set's recordings (recordingBudget) bounds them within
// the set's budget: a recording past the cap is decoded at each read and
// lets none of those kept go. A recording kept twice (two readers decoded
// it at once) is counted once. A tracker of no set decodes at each read,
// and so does a set for a decode that gives no recording and no error: it
// keeps none and counts no decode.
func TestOutcomeCostTheSetsRecordingsKeepWithinTheirCap(t *testing.T) {
	defer func(b int64) { recordingBudget = b }(recordingBudget)
	recordingBudget = 8 << 10
	tr := NewTracker()
	tr.NewSet()
	var n int
	measured := recordingMeasures.Load()
	for _, name := range []string{"a", "big", "big", "a"} {
		size := int64(1 << 10)
		if name == "big" {
			size = 10 << 10
		}
		if _, err := tr.KeepRecording(name, size, decodeOf(name, &n)); err != nil {
			t.Fatal(err)
		}
	}
	// Measured once, the one kept: one past the cap is not measured.
	if d := recordingMeasures.Load() - measured; n != 3 || d != 1 {
		t.Errorf("a kept and one past the cap read twice each decoded %d and measured %d; want 3, 1", n, d)
	}
	s := tr.derived
	s.mu.Lock()
	before := s.recs.used
	s.keepSpare("a", 1<<10, recordingOf("a"))
	after, all := s.recs.used, s.budget.used
	s.mu.Unlock()
	if after != before || all != before {
		t.Errorf("a recording kept again: the recordings count %d, the set %d; want %d, as kept once", after, all, before)
	}
	empty := 0
	for range 2 {
		if m, err := tr.KeepRecording("empty", 1<<10, func() (*models.Mock, error) { empty++; return nil, nil }); m != nil || err != nil {
			t.Errorf("a decode of no recording and no error read as %+v, %v; want neither", m, err)
		}
	}
	if decodes := tr.RecordingDecodes(); empty != 2 || decodes != 3 {
		t.Errorf("two reads of a decode of no recording decoded %d, the set counts %d decodes; want 2, and the 3 before", empty, decodes)
	}
	for _, none := range []*Tracker{nil, {}} {
		n = 0
		for range 2 {
			var m *models.Mock
			var err error
			if crashed := func() (crashed any) {
				defer func() { crashed = recover() }()
				m, err = none.KeepRecording("a", 1<<10, decodeOf("a", &n))
				return nil
			}(); crashed != nil {
				t.Errorf("a tracker of no set panicked at a read: %v; want the recording as decoded", crashed)
				continue
			}
			if err != nil || !reflect.DeepEqual(m, recordingOf("a")) {
				t.Errorf("a tracker of no set read %+v, %v; want the recording as decoded", m, err)
			}
		}
		if n != 2 || none.RecordingDecodes() != 0 {
			t.Errorf("a tracker of no set decoded %d of two reads, counted %d; want 2, 0", n, none.RecordingDecodes())
		}
	}
}

// A kept recording counts on the set's budget at least the heap it holds
// decoded, whatever the size its reader knew of it (its record's): a
// recording of thousands of headers holds about eleven times its record,
// and counted by its record it kept that much more than the budget said.
// Nor does it count much more than it holds, which would leave the set
// room for fewer recordings than its budget has: at most three times (a
// map's entries are counted at the most any fill of its table costs), and
// 1 KiB.
func TestOutcomeCostAKeptRecordingCountsTheHeapItHoldsDecoded(t *testing.T) {
	headers := func() *models.Mock {
		m := recordingOf("headers")
		h := func() map[string]string {
			h := map[string]string{}
			for i := range 4000 {
				h["h"+strconv.Itoa(i)] = "v" + strconv.Itoa(i)
			}
			return h
		}
		m.Spec.HTTPReq.Header = h()
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Header: h()}
		return m
	}
	body := func() *models.Mock {
		m := recordingOf("body")
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("x", 1<<20)}
		return m
	}
	small := func() *models.Mock { return recordingOf("small") }
	// A Mongo answer's message is an interface: one holding a value, not a
	// pointer, holds it boxed on the heap.
	boxed := func() *models.Mock {
		m := recordingOf("boxed")
		m.Spec.MongoResponses = make([]models.MongoResponse, 0, 1000)
		for i := range cap(m.Spec.MongoResponses) {
			m.Spec.MongoResponses = append(m.Spec.MongoResponses, models.MongoResponse{Message: models.MongoOpMessage{FlagBits: i, Sections: []string{"s" + strconv.Itoa(i)}}})
		}
		return m
	}
	for _, c := range []struct {
		name   string
		make   func() *models.Mock
		copies int
	}{{"4000 headers", headers, 8}, {"a 1 MiB body", body, 8}, {"a small recording", small, 4096}, {"1000 answers holding their message by value", boxed, 64}} {
		// Copies of it live at the second read, a few MiB of them: the
		// runtime's own work moves the heap by a few KiB between two
		// reads, well under 1/64 of that.
		var a, b runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&a)
		ms := make([]*models.Mock, 0, c.copies)
		for range cap(ms) {
			ms = append(ms, c.make())
		}
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&b)
		heap := (int64(b.HeapAlloc) - int64(a.HeapAlloc) - int64(unsafe.Sizeof(ms[0]))*int64(cap(ms))) / int64(len(ms))
		runtime.KeepAlive(ms)
		tr := NewTracker()
		tr.NewSet()
		if _, err := tr.KeepRecording(c.name, 1<<10, func() (*models.Mock, error) { return c.make(), nil }); err != nil {
			t.Fatal(err)
		}
		s := tr.derived
		s.mu.Lock()
		counted := s.recs.used - slotBytes - int64(len(c.name))
		s.mu.Unlock()
		t.Logf("%s: holds %d bytes decoded, counted %d", c.name, heap, counted)
		// Within 1/64 of the heap read is the heap it holds.
		if counted < heap-heap/64 || counted > 3*heap+1<<10 {
			t.Errorf("%s: kept, counted %d bytes on the set's budget; want at least the %d it holds decoded (less 1/64), at most three times that and 1 KiB", c.name, counted, heap)
		}
	}
	// A map or a pointer the recording reaches twice holds its heap once,
	// and is counted once.
	shared, apart := headers(), headers()
	shared.Spec.HTTPResp.Header = shared.Spec.HTTPReq.Header
	if a, b := recordingHeap(shared), recordingHeap(apart); a >= b {
		t.Errorf("a recording whose request and answer share one header map counts %d bytes; want less than one with a map each (%d)", a, b)
	}
	responses := func(h1, h2 *models.MongoHeader) *models.Mock {
		return &models.Mock{Spec: models.MockSpec{MongoResponses: []models.MongoResponse{{Header: h1}, {Header: h2}}}}
	}
	h := &models.MongoHeader{Length: 1}
	if a, b := recordingHeap(responses(h, h)), recordingHeap(responses(h, &models.MongoHeader{Length: 1})); a >= b {
		t.Errorf("two answers that share one header count %d bytes; want less than two with a header each (%d)", a, b)
	}
	// An interface holding a value holds it boxed, as one holding a
	// pointer to it holds what the pointer names.
	message := func(msg any) *models.Mock {
		return &models.Mock{Spec: models.MockSpec{MongoResponses: []models.MongoResponse{{Message: msg}}}}
	}
	if a, b := recordingHeap(message(models.MongoOpMessage{FlagBits: 1, Sections: []string{"a"}})), recordingHeap(message(&models.MongoOpMessage{FlagBits: 1, Sections: []string{"a"}})); a != b {
		t.Errorf("an answer holding its message by value counts %d bytes; want the %d of one holding a pointer to it", a, b)
	}
}

// Two first reads of one recording at once both decode it, and the set
// keeps the first kept: the second, finding it kept, does not measure what
// it decoded, so a recording is measured once for the set however many
// readers decode it at once.
func TestOutcomeCostARecordingTwoReadersDecodeAtOnceIsMeasuredOnce(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	measured := recordingMeasures.Load()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := tr.KeepRecording("a", 1<<10, func() (*models.Mock, error) {
			close(started)
			<-release
			return recordingOf("a"), nil
		})
		done <- err
	}()
	<-started
	var n int
	_, err := tr.KeepRecording("a", 1<<10, decodeOf("a", &n))
	close(release)
	if err := errors.Join(err, <-done); err != nil {
		t.Fatal(err)
	}
	if d, decodes := recordingMeasures.Load()-measured, tr.RecordingDecodes(); n != 1 || decodes != 2 || d != 1 {
		t.Errorf("two first reads at once decoded %d (the set counts %d) and measured %d; want the second read's decode, 2 and 1", n, decodes, d)
	}
	s := tr.derived
	s.mu.Lock()
	kept := len(s.recs.by)
	s.mu.Unlock()
	if kept != 1 {
		t.Errorf("two first reads of one recording at once kept %d recordings; want 1", kept)
	}
}

// A recording whose least size (its record's, as the store gives it) has
// room in the set but whose measured heap has none is measured once, at
// its first read, and refused: the set remembers its key, on its budget, so
// each later read decodes it, as with no set, and measures it no more. A
// key of another generation of its store names it anew and is measured
// once more; the set's next set remembers none.
func TestOutcomeCostARecordingTheSetRefusedIsMeasuredOnce(t *testing.T) {
	headers := func() *models.Mock {
		m := recordingOf("headers")
		h := map[string]string{}
		for i := range 4000 {
			h["h"+strconv.Itoa(i)] = "v" + strconv.Itoa(i)
		}
		m.Spec.HTTPReq.Header = h
		return m
	}
	const least = 2*54219 + 1<<10 // DiskMocks.recordedAt's, of its 54,219-byte record
	heap := recordingHeap(headers())
	if heap <= 2*least {
		t.Fatalf("4000 headers measure %d bytes; want well past their least size, %d", heap, least)
	}
	tr := NewTracker()
	tr.NewSet()
	s := tr.derived
	s.mu.Lock()
	s.budget.limit = s.budget.used + (least+heap)/2
	s.mu.Unlock()
	var n int
	decode := func() (*models.Mock, error) { n++; return headers(), nil }
	measured := recordingMeasures.Load()
	const reads = 20
	for range reads {
		m, err := tr.KeepRecording("1:0:0", least, decode)
		if err != nil || !reflect.DeepEqual(m, headers()) {
			t.Fatalf("a refused recording read as %v (error %v); want it as decoded", m != nil, err)
		}
	}
	s.mu.Lock()
	kept, refused, used, all := len(s.recs.by), len(s.refused.by), s.budget.used, s.usedBytes
	s.mu.Unlock()
	if d := recordingMeasures.Load() - measured; n != reads || d != 1 || kept != 0 {
		t.Errorf("%d reads of a recording whose heap has no room decoded %d, measured %d, kept %d; want %d, 1, 0", reads, n, d, kept, reads)
	}
	if want := int64(slotBytes + len("1:0:0")); refused != 1 || used != want {
		t.Errorf("the set remembers %d refused recordings and counts %d bytes on its budget; want 1, its slot's %d", refused, used, want)
	}
	if u := all(); u != used {
		t.Errorf("the set's tiers count %d bytes, its budget %d; want the same", u, used)
	}
	measured = recordingMeasures.Load()
	if _, err := tr.KeepRecording("1:1:0", least, decode); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.KeepRecording("1:1:0", least, decode); err != nil {
		t.Fatal(err)
	}
	if d := recordingMeasures.Load() - measured; d != 1 {
		t.Errorf("a key of the store's next generation, read twice, measured %d; want 1", d)
	}
	tr.NewSet()
	if u := s.usedBytes(); u != 0 {
		t.Errorf("the set over keeps %d bytes; want none", u)
	}
	next := tr.derived
	next.mu.Lock()
	next.budget.limit = next.budget.used + (least+heap)/2
	next.mu.Unlock()
	measured = recordingMeasures.Load()
	if _, err := tr.KeepRecording("1:0:0", least, decode); err != nil {
		t.Fatal(err)
	}
	if d := recordingMeasures.Load() - measured; d != 1 {
		t.Errorf("the next set's first read of a recording the set before refused measured %d; want 1", d)
	}
}

// A slice of bools or numbers is counted by its array, its capacity's,
// not its length's, and its elements are not visited one by one: a
// Postgres copy-out of 1 MiB took several times its decode to walk byte by
// byte, for no byte of its count.
func TestOutcomeCostARecordingsByteSlicesAreCountedByTheirArrays(t *testing.T) {
	m := recordingOf("copy")
	data := make([][]byte, 0, 65)
	for i := range 64 {
		data = append(data, bytes.Repeat([]byte{byte(i)}, 16<<10))
	}
	data = append(data, make([]byte, 1<<20, 2<<20))
	m.Spec.PostgresV3 = &models.PostgresV3Spec{Type: "query", Query: &models.PostgresV3QuerySpec{Response: &models.PostgresV3Response{CopyOut: &models.PostgresV3CopyOutPayload{Data: data}}}}
	walked := recordingWalks.Load()
	heap := recordingHeap(m)
	if d := recordingWalks.Load() - walked; d > 1000 {
		t.Errorf("a recording of 3 MiB in 65 byte slices visited %d values to measure; want at most 1000", d)
	}
	if want := int64(64*16<<10 + 2<<20); heap < want {
		t.Errorf("a recording of 64 slices of 16 KiB and one of 1 MiB in 2 MiB counts %d bytes; want at least their arrays' %d", heap, want)
	}
}
