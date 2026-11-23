package learn

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// setMoments keeps each recording's and text's momentIndex once for the set
// on its budget (with entry overhead counted against readerBudgetBytes), so
// repeated holdsMoment / formsName queries across Decide calls do not re-run
// b64Read + scanInstants or reload recording payloads. On fallback (cap or set
// budget full, or unread lazy loader), t.held hands the payload to indexOnce so
// each question reads the recording at most once.
func TestSetMomentsCachesAcrossCallsAndRespectsBudget(t *testing.T) {
	t.Run("recording hits, formsName, and zero reloads across calls", func(t *testing.T) {
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

		x1 := newReaderIndex(dc)
		for i, r := range raws {
			held, _, read := x1.textOf(r).holdsMoment(wantAt)
			if !read || held != (i == 5) {
				t.Fatalf("call 1 rec %d: held=%v read=%v, want held=%v", i, held, read, i == 5)
			}
		}
		x1.release()
		if fs.momentBuilds != n || fs.momentsUsed <= 0 || fs.momentsUsed > readerBudgetBytes(dc.set) {
			t.Fatalf("after call 1: builds=%d used=%d cap=%d", fs.momentBuilds, fs.momentsUsed, readerBudgetBytes(dc.set))
		}
		loadsAfter1 := dc.Loads()

		for call := 2; call <= 5; call++ {
			x := newReaderIndex(dc)
			for i, r := range raws {
				rt := x.textOf(r)
				held, _, read := rt.holdsMoment(wantAt)
				if !read || held != (i == 5) {
					t.Fatalf("call %d rec %d: held=%v read=%v", call, i, held, read)
				}
				named, nread := rt.formsName(func(h moment) bool {
					ok, _ := byMoment(h, wantAt)
					return ok
				})
				if !nread || named != (i == 5) {
					t.Fatalf("call %d rec %d formsName: named=%v read=%v", call, i, named, nread)
				}
			}
			x.release()
		}
		if fs.momentBuilds != n {
			t.Fatalf("after 5 calls: momentBuilds = %d, want %d", fs.momentBuilds, n)
		}
		if got := dc.Loads() - loadsAfter1; got != 0 {
			t.Fatalf("calls 2..5 reloaded recordings %d times, want 0", got)
		}
	})

	t.Run("failing lazy loader reads once and marks unread", func(t *testing.T) {
		origBody := bodyBudget
		bodyBudget = 0
		defer func() { bodyBudget = origBody }()

		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		fs := filtersOf(dc.set)
		ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		m := getAnswer("mock-fail-lazy", "http://api/t", `{"at":"2026-05-01T10:00:00Z"}`, ts)
		raw := dc.LazyRawOf(m, false, func() (*models.Mock, error) {
			return nil, errors.New("disk failure")
		})
		wantAt := instantSpans("2026-05-01T10:00:00Z")[0].moment()
		loads := dc.Loads()
		x := newReaderIndex(dc)
		_, _, read := x.textOf(raw).holdsMoment(wantAt)
		x.release()
		if read || !x.unread() {
			t.Fatalf("failing lazy raw: read=%v unread=%v; want false, true", read, x.unread())
		}
		if got := dc.Loads() - loads; got != 1 {
			t.Fatalf("failing lazy raw loaded %d times for one holdsMoment; want 1", got)
		}
		if len(fs.momentOf) != 0 {
			t.Fatalf("failing lazy raw published %d entries in momentOf; want 0", len(fs.momentOf))
		}
	})

	t.Run("bodyBudget=0 fallback reads once per question for holdsMoment and formsName", func(t *testing.T) {
		origBody := bodyBudget
		bodyBudget = 0
		defer func() { bodyBudget = origBody }()

		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		fs := filtersOf(dc.set)
		ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		stamp := ts.Format(time.RFC3339)
		m := getAnswer("mock-fallback-one-read", "http://api/t", `{"at":"`+stamp+`"}`, ts)
		raw := dc.LazyRawOf(m, false, nil)
		wantAt := instantSpans(stamp)[0].moment()

		// Leave room for minN (the pre-load check) but not minN+valN so
		// setMoments actually loads the payload, fails the post-build
		// momentsUsed+totalN > room check, and hands the payload off via t.held.
		minN := momentsEntryBytes + momentsIDSlotBytes + allocBytes(len(raw.id), false) + momentsIDGroupBytes
		fs.momentsUsed = readerBudgetBytes(dc.set) - minN

		loads := dc.Loads()
		x := newReaderIndex(dc)
		held, _, read := x.textOf(raw).holdsMoment(wantAt)
		x.release()
		if !read || !held {
			t.Fatalf("fallback holdsMoment: held=%v read=%v; want true, true", held, read)
		}
		if got := dc.Loads() - loads; got != 1 {
			t.Fatalf("fallback holdsMoment with bodyBudget=0 loaded %d times; want 1", got)
		}
		if len(fs.momentOf) != 0 {
			t.Fatalf("post-build cap fallback published %d entries; want 0", len(fs.momentOf))
		}

		// Also verify fallback when set budget itself is exhausted (setClaim fails).
		fs.momentsUsed = 0
		dc.set.mu.Lock()
		dc.set.budget.used = dc.set.budget.limit
		dc.set.mu.Unlock()
		loads = dc.Loads()
		x2 := newReaderIndex(dc)
		named, nread := x2.textOf(raw).formsName(func(h moment) bool {
			ok, _ := byMoment(h, wantAt)
			return ok
		})
		x2.release()
		if !nread || !named {
			t.Fatalf("fallback formsName: named=%v read=%v; want true, true", named, nread)
		}
		if got := dc.Loads() - loads; got != 1 {
			t.Fatalf("fallback formsName with exhausted set budget loaded %d times; want 1", got)
		}
	})

	t.Run("zero-instant handed texts are capped by readerBudgetBytes", func(t *testing.T) {
		origBudget := readerBudget
		readerBudget = 1024
		defer func() { readerBudget = origBudget }()

		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		fs := filtersOf(dc.set)
		wantAt := instantSpans("2026-05-01T10:00:00Z")[0].moment()

		x := newReaderIndex(dc)
		for i := range 200 {
			s := fmt.Sprintf("plain-leaf-no-instant-%d", i)
			held, _, read := x.text(s).holdsMoment(wantAt)
			if !read || held {
				t.Fatalf("plain leaf %d: held=%v read=%v; want false, true", i, held, read)
			}
		}
		x.release()
		if fs.momentsUsed > readerBudget {
			t.Fatalf("zero-instant texts exceeded readerBudget: momentsUsed=%d > %d", fs.momentsUsed, readerBudget)
		}
		if len(fs.momentTexts) >= 200 {
			t.Fatalf("zero-instant texts bypassed cap: %d entries in momentTexts", len(fs.momentTexts))
		}
		// Repeated queries on a cached text hit momentTexts without rebuilding.
		buildsBefore := fs.momentBuilds
		x2 := newReaderIndex(dc)
		held, _, read := x2.text("plain-leaf-no-instant-0").holdsMoment(wantAt)
		x2.release()
		if !read || held || fs.momentBuilds != buildsBefore {
			t.Fatalf("cached handed text: held=%v read=%v builds=%d->%d", held, read, buildsBefore, fs.momentBuilds)
		}
	})

	t.Run("concurrent callers on same recording and text under race", func(t *testing.T) {
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		stamp := ts.Format(time.RFC3339)
		m := getAnswer("mock-race-mom", "http://api/t", `{"at":"`+stamp+`"}`, ts)
		raw := dc.LazyRawOf(m, false, nil)
		wantAt := instantSpans(stamp)[0].moment()

		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := newReaderIndex(dc)
				defer x.release()
				if held, _, read := x.textOf(raw).holdsMoment(wantAt); !read || !held {
					t.Errorf("concurrent raw holdsMoment: held=%v read=%v", held, read)
				}
				if held, _, read := x.text(`{"at":"` + stamp + `"}`).holdsMoment(wantAt); !read || !held {
					t.Errorf("concurrent text holdsMoment: held=%v read=%v", held, read)
				}
			}()
		}
		wg.Wait()
		fs := filtersOf(dc.set)
		fs.mu.Lock()
		gotIDs, gotTexts, gotUsed := len(fs.momentOf), len(fs.momentTexts), fs.momentsUsed
		fs.mu.Unlock()
		if gotIDs != 1 || gotTexts != 1 {
			t.Fatalf("post-race cache entries: momentOf=%d momentTexts=%d; want 1, 1", gotIDs, gotTexts)
		}
		if gotUsed <= 0 {
			t.Fatalf("post-race momentsUsed=%d; want > 0", gotUsed)
		}
		dc.set.mu.Lock()
		budgetUsed := dc.set.budget.used
		dc.set.mu.Unlock()
		if budgetUsed < gotUsed {
			t.Fatalf("post-race set budget.used=%d < momentsUsed=%d (refund accounting mismatch)", budgetUsed, gotUsed)
		}
	})
}

func TestPlaceListIndicesOfMatchesMocksAndIgnoresFalse(t *testing.T) {
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
	cand := map[string]bool{"mock-a": true, "mock-b": false, "mock-c": true}
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
