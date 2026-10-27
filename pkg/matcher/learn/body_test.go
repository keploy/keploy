package learn

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// lazyOf is each of mocks as dc.LazyRawOf reads it, served in order and as
// recorded, as the word rule's rows read RawOf's.
func lazyOf(dc *DriftCache, mocks []*models.Mock) []Raw {
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = dc.LazyRawOf(m, true, nil).ServedAs(i + 1).ServedAsRecorded()
	}
	return raw
}

// A lazy Raw (LazyRawOf) reads its recording's payload only where a rule
// reads it, from a store that keeps the set's payloads within a budget. What
// keploy learns from lazy Raws is what it learns from Raws read whole
// (RawOf), with every payload kept and with none kept (each read again on
// its next use), in each test of a set: a rule reading a field past the
// accessor would read an empty payload and learn otherwise.
func TestALazyRecordingLearnsWhatOneReadWholeLearns(t *testing.T) {
	at := eRecAt
	const p = "Req.query.bind[0]"
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	for _, c := range []struct{ name, rec, live, holds string }{
		{"one word", "a1b2c3d4", "z9y8x7w6", "id a1b2c3d4 kept"},
		{"runs whole", "q7-r8s9", "k5-m4n3", "ref q7-r8s9 kept"},
		{"runs apart", "q7-r8s9", "k5-m4n3", "ref q7 then r8s9"},
		{"held nowhere", "a1b2c3d4", "z9y8x7w6", "nothing of it"},
	} {
		src := &models.Mock{Name: "src", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at.Add(-time.Second / 2),
			ResTimestampMock: at.Add(-time.Second/2 + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT note FROM src",
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: c.holds}}}, CommandComplete: "SELECT 1"},
			}},
		}}
		mocks := append(fillers(10, at.Add(-time.Second)), src)
		rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.rec}}}
		live := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.live}}}
		eager := make([]Raw, len(mocks))
		for i, m := range mocks {
			eager[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
		}
		dc := NewDriftCache()
		dc.appClock = true
		want := ClassifyFollowing(dc, []string{p}, live, rec, at, nil, eager, nil, nil, nil)
		for _, budget := range []int64{64 << 20, 0} {
			bodyBudget = budget
			tr := NewTracker()
			tr.NewSet()
			for test := 1; test <= 2; test++ {
				tr.Begin()
				dc := tr.Drifts()
				dc.appClock = true
				got := ClassifyFollowing(dc, []string{p}, live, rec, at, nil, lazyOf(dc, mocks), nil, nil, nil)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s, budget %d, test %d: lazily learned %v, want %v (read whole)", c.name, budget, test, got, want)
				}
				tr.End()
			}
		}
	}
}

// A call reads the payloads of the recordings before it, and keploy keeps
// them for the set: what it keeps stays within the store's budget whatever
// the set's size, the least recently read let go first and read again
// where a rule next reads it, as it was. Making a lazy Raw reads nothing,
// and whether one was read at all is known past its payload's eviction.
func TestTheSetsPayloadsAreKeptWithinABudget(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 256 << 10
	at := eRecAt
	mocks := make([]*models.Mock, 300)
	for i := range mocks {
		mocks[i] = getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/"+fmt.Sprint(i), `{"note":"`+strings.Repeat(fmt.Sprintf("w%d ", i), 1500)+`"}`, at.Add(-time.Duration(300-i)*time.Second))
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = dc.LazyRawOf(m, false, nil)
	}
	if n := dc.Loads(); n != 0 {
		t.Fatalf("making %d lazy Raws loaded %d payloads, want 0", len(raw), n)
	}
	var total int64
	for i, x := range raw {
		got, want := x.payload(), rawOfNow(mocks[i], false)
		total += got.size()
		if got.text != want.rText || !reflect.DeepEqual(got.fields, want.rFields) || !reflect.DeepEqual(got.stamps, want.rStamps) || !got.read {
			t.Fatalf("%s: the lazy payload is not the one RawOf reads", mocks[i].Name)
		}
	}
	s := dc.set.bodies
	if total <= 4*s.budget {
		t.Fatalf("the row's payloads are %d bytes, not past the budget %d: it pins nothing", total, s.budget)
	}
	if s.used > s.budget {
		t.Errorf("the store keeps %d bytes of payloads, over its budget %d (%d read)", s.used, s.budget, total)
	}
	if n := dc.Loads(); n != len(mocks) {
		t.Errorf("reading each payload once loaded %d, want %d", n, len(mocks))
	}
	if !raw[0].isRead() || dc.Loads() != len(mocks) {
		t.Errorf("asking whether a payload let go was read loaded it again (%d loads)", dc.Loads())
	}
	if got := raw[0].payload(); got.text != rawOfNow(mocks[0], false).rText || dc.Loads() != len(mocks)+1 {
		t.Errorf("a payload let go is read again as it was, once (%d loads, want %d)", dc.Loads(), len(mocks)+1)
	}
	// A payload larger than the whole budget is read, and not kept.
	big := getAnswer("mock-big", "http://api/items/big", `{"note":"`+strings.Repeat("big ", int(s.budget))+`"}`, at.Add(-time.Second/2))
	if got := dc.LazyRawOf(big, false, nil).payload(); !got.read || got.size() <= s.budget {
		t.Fatalf("the payload past the budget was not read whole (%d bytes)", got.size())
	}
	if _, kept := s.by[MockID(big)]; kept || s.used > s.budget {
		t.Errorf("a payload larger than the budget is kept (%v), the store at %d bytes of %d", kept, s.used, s.budget)
	}
	tr.End()
	// The next set starts with an empty store.
	tr.NewSet()
	if n := tr.Drifts().Loads(); n != 0 {
		t.Errorf("a new set's store has %d loads, want 0", n)
	}
}

// A recording whose answer keploy cannot read back (an answer the agent
// parked on disk that is gone) is an unread earlier place: a call whose
// value may have come from there is refused, naming the recording and
// why, never learned.
func TestALazyRecordingThatCannotBeReadIsRefusedNamingIt(t *testing.T) {
	at := eRecAt
	const p = "Req.query.bind[0]"
	mocks := fillers(3, at.Add(-time.Second))
	rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: "a1b2c3d4"}}}
	live := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: "z9y8x7w6"}}}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	dc.appClock = true
	raw := lazyOf(dc, mocks)
	raw[1] = dc.LazyRawOf(mocks[1], true, func() (*models.Mock, error) {
		return nil, errors.New("the parked answer is gone")
	}).ServedAs(2).ServedAsRecorded()
	got := ClassifyFollowing(dc, []string{p}, live, rec, at, nil, raw, nil, nil, nil)
	if len(got) != 1 || got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, "could not read back filler-1") || !strings.Contains(got[0].Why, "the parked answer is gone") {
		t.Fatalf("learned %+v, want it refused naming filler-1 and why", got)
	}
	if raw[1].isRead() {
		t.Errorf("a recording that could not be loaded reads as read")
	}
}
