package learn

import (
	"errors"
	"reflect"
	"testing"
	"time"

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
	for range 3 {
		m, err := tr.KeepRecording("big", 64<<10, decodeOf("big", &n))
		if err != nil || !reflect.DeepEqual(m, recordingOf("big")) {
			t.Fatalf("a recording past the budget read as %+v, %v; want the recording as decoded", m, err)
		}
	}
	if n != 3 {
		t.Errorf("three reads of a recording past the budget decoded %d; want 3", n)
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
	for range 3 {
		if m, err := tr.KeepRecording("mid", 8<<10, decodeOf("mid", &n)); err != nil || !reflect.DeepEqual(m, recordingOf("mid")) {
			t.Fatalf("a recording with no room read as %+v, %v; want the recording as decoded", m, err)
		}
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

// A row's cap on the set's recordings (recordingBudget) bounds them within
// the set's budget: a recording past the cap is decoded at each read and
// lets none of those kept go. A recording kept twice (two readers decoded
// it at once) is counted once. A tracker of no set decodes at each read.
func TestOutcomeCostTheSetsRecordingsKeepWithinTheirCap(t *testing.T) {
	defer func(b int64) { recordingBudget = b }(recordingBudget)
	recordingBudget = 8 << 10
	tr := NewTracker()
	tr.NewSet()
	var n int
	for _, name := range []string{"a", "big", "big", "a"} {
		size := int64(1 << 10)
		if name == "big" {
			size = 10 << 10
		}
		if _, err := tr.KeepRecording(name, size, decodeOf(name, &n)); err != nil {
			t.Fatal(err)
		}
	}
	if n != 3 {
		t.Errorf("a kept and one past the cap read twice each decoded %d; want 3", n)
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
	for _, none := range []*Tracker{nil, {}} {
		n = 0
		for range 2 {
			if m, err := none.KeepRecording("a", 1<<10, decodeOf("a", &n)); err != nil || !reflect.DeepEqual(m, recordingOf("a")) {
				t.Errorf("a tracker of no set read %+v, %v; want the recording as decoded", m, err)
			}
		}
		if n != 2 || none.RecordingDecodes() != 0 {
			t.Errorf("a tracker of no set decoded %d of two reads, counted %d; want 2, 0", n, none.RecordingDecodes())
		}
	}
}
