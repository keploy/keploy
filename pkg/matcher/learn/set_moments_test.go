package learn

import (
	"fmt"
	"testing"
	"time"
)

// setMoments keeps each recording's and text's momentIndex once for the set
// on its budget, so repeated holdsMoment / formsName queries across Decide
// calls do not re-run b64Read + scanInstants or reload the recording's payload.
// When the set's moment cap or budget is exhausted, or a recording is unread,
// holdsMoment falls back to per-call reading and still reports unread cleanly.
func TestSetMomentsCachesAcrossCallsAndRespectsBudget(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	fs := filtersOf(dc.set)

	const n = 20
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	raws := make([]Raw, n)
	for i := range n {
		stamp := ts.AddDate(0, i, 0).Format(time.RFC3339)
		m := getAnswer(fmt.Sprintf("mock-mom-%d", i), "http://api/t", `{"at":"`+stamp+`"}`, ts.Add(-time.Duration(n-i)*time.Second))
		raws[i] = dc.LazyRawOf(m, false, nil)
	}
	wantAt := instantSpans(ts.AddDate(0, 5, 0).Format(time.RFC3339))[0].moment()

	// Call 1 populates the set's moment indices once per recording.
	x1 := newReaderIndex(dc)
	for i, r := range raws {
		held, _, read := x1.textOf(r).holdsMoment(wantAt)
		if !read {
			t.Fatalf("call 1 rec %d: unread", i)
		}
		if held != (i == 5) {
			t.Fatalf("call 1 rec %d: held=%v, want %v", i, held, i == 5)
		}
	}
	x1.release()
	if fs.momentBuilds != n {
		t.Fatalf("after call 1: momentBuilds = %d, want %d", fs.momentBuilds, n)
	}
	loadsAfter1 := dc.Loads()

	// Calls 2..5 (fresh Reader each time) answer with zero extra builds and zero reloads.
	for call := 2; call <= 5; call++ {
		x := newReaderIndex(dc)
		for i, r := range raws {
			held, _, read := x.textOf(r).holdsMoment(wantAt)
			if !read || held != (i == 5) {
				t.Fatalf("call %d rec %d: held=%v read=%v", call, i, held, read)
			}
		}
		x.release()
	}
	if fs.momentBuilds != n {
		t.Fatalf("after 5 calls: momentBuilds = %d, want %d (rebuilt)", fs.momentBuilds, n)
	}
	if got := dc.Loads() - loadsAfter1; got != 0 {
		t.Fatalf("calls 2..5 reloaded recordings %d times, want 0", got)
	}

	// Unread recording keeps no entry and reports unread.
	xu := newReaderIndex(dc)
	_, _, read := xu.textOf(Raw{mock: "mock-unread"}).holdsMoment(wantAt)
	if read || !xu.unread() {
		t.Fatalf("unread recording: read=%v unread=%v; want false, true", read, xu.unread())
	}
	xu.release()

	// When momentsUsed is at the cap, a new recording falls back to per-call read.
	origUsed := fs.momentsUsed
	fs.momentsUsed = readerBudgetBytes(dc.set)
	defer func() { fs.momentsUsed = origUsed }()
	extraStamp := ts.Add(99 * time.Minute).Format(time.RFC3339)
	extraMock := getAnswer("mock-mom-extra", "http://api/t", `{"at":"`+extraStamp+`"}`, ts)
	extraRaw := dc.LazyRawOf(extraMock, false, nil)
	extraWant := instantSpans(extraStamp)[0].moment()
	xc := newReaderIndex(dc)
	held, _, read := xc.textOf(extraRaw).holdsMoment(extraWant)
	xc.release()
	if !read || !held {
		t.Fatalf("past moments cap: held=%v read=%v; want true, true", held, read)
	}
}

func TestPlaceListIndicesOfMatchesMocks(t *testing.T) {
	earlier := []Earlier{
		{Mock: "mock-a", Seen: []Seen{{Place: "p1"}, {Place: "p2"}}},
		{Mock: "mock-b", Seen: []Seen{{Place: "p3"}}},
		{Mock: "mock-a", Seen: []Seen{{Place: "p4"}}},
	}
	pl := &placeList{
		in:     Input{Earlier: earlier},
		ex:     []exPlaces{{e: 0, off: 0}, {e: 1, off: 2}, {e: 2, off: 3}},
		leaves: 4,
		rest:   []evPlace{{mock: "mock-c"}, {mock: "mock-b"}},
	}
	cand := map[string]bool{"mock-a": true, "mock-c": true}
	want := map[int]bool{}
	for i, m := range pl.mocks() {
		if cand[m] {
			want[i] = true
		}
	}
	got := map[int]bool{}
	for i := range pl.indicesOf(cand) {
		got[i] = true
	}
	if len(got) != len(want) {
		t.Fatalf("indicesOf = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i] {
			t.Fatalf("indicesOf missing %d: got %v, want %v", i, got, want)
		}
	}
}
