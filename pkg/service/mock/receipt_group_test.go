package mock

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// polled is the i-th of a page's polls of query q that no test was charged
// with, as the proxy hands it at a run's end: every occurrence lands at its
// own moment, in its own window, from its own renderer process and worker,
// a repeat of the same spent recording at its own gaps.
func polled(q string, i int) models.UnmatchedCall {
	at := time.Date(2026, 11, 12, 21, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
	return models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /query",
		Operation:     "query " + q,
		Destination:   "localhost:8083",
		ClosestMock:   "mock-611",
		NextSteps:     "keploy's learn step: it is the call a recorded call of the test answered already",
		At:            at,
		WindowOf:      fmt.Sprintf("atg-features::pages/page-%d.spec.ts::test", i%344),
		Instance:      uint32(i % 3),
		Source:        &models.Origin{PID: uint32(1000 + i), Comm: "chrome-headless", Exe: "/ms-playwright/chrome", Cmdline: fmt.Sprintf("chrome --renderer-client-id=%d", i)},
		Repeat: &models.RepeatNote{Mock: "mock-611", Test: "atg-features::pages/schema.spec.ts::test", RecordedAt: time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC),
			ReplayGapSeconds: float64(i), ReplayGapFromStart: i%2 == 0, RecordGapSeconds: float64(2 * i), RecordGapKnown: i%2 == 1,
			RecordedLast: fmt.Sprintf("mock-%d", 700+i)},
	}
}

// A call the run made again is one entry with its count, not one per call:
// a page that polls four queries between tests over a whole project leaves
// four entries, each with when, where and by which process it was first
// made, in the order the run first made each.
func TestReceiptListsACallMadeAgainOnceWithItsCount(t *testing.T) {
	queries := []string{"GetAllSandboxTestJobs", "GetAllTestRunJobs", "GetAllLoadTestJobs", "GetAllGenJobs"}
	var misses []models.UnmatchedCall
	for i := range 4459 {
		misses = append(misses, polled(queries[i%len(queries)], i))
	}
	got := unchargedMisses(misses)
	if len(got) != len(queries) {
		t.Fatalf("%d entries; want one per query, %d", len(got), len(queries))
	}
	if n := UnchargedCalls(got); n != 4459 {
		t.Fatalf("the entries count %d calls; want all 4459", n)
	}
	for i, u := range got {
		first := polled(queries[i], i)
		if u.Operation != first.Operation || !u.At.Equal(first.At) || u.WindowOf != first.WindowOf {
			t.Fatalf("entry %d is %q at %v in %q; want its first call, %q at %v in %q",
				i, u.Operation, u.At, u.WindowOf, first.Operation, first.At, first.WindowOf)
		}
		if u.Process == nil || u.Process.PID != first.Source.PID || u.Process.Cmdline != first.Source.Cmdline {
			t.Fatalf("entry %d names process %+v; want its first call's, pid %d", i, u.Process, first.Source.PID)
		}
		if u.Repeat == nil || u.Repeat.ReplayGapSeconds != first.Repeat.ReplayGapSeconds || u.Repeat.RecordedLast != first.Repeat.RecordedLast {
			t.Fatalf("entry %d repeat %+v; want its first call's gaps", i, u.Repeat)
		}
		if want := 4459/4 + map[bool]int{true: 1}[i < 4459%4]; u.Count != want {
			t.Fatalf("entry %d counts %d; want %d", i, u.Count, want)
		}
	}
	// It reads back as written: the count is the receipt's, not only the run's.
	back := writeAndRead(t, Receipt{Set: "set", OnMiss: "fail", UnchargedMisses: got})
	if len(back.UnchargedMisses) != len(queries) || UnchargedCalls(back.UnchargedMisses) != 4459 {
		t.Fatalf("read back %d entries counting %d calls; want 4, 4459", len(back.UnchargedMisses), UnchargedCalls(back.UnchargedMisses))
	}
}

// Calls that differ in what evidence keeps of them stay entries of their own:
// another call, another closest recording, another recording repeated, another
// test trailed, another reason, another kind of process or none.
func TestReceiptKeepsDistinctUnchargedCallsApart(t *testing.T) {
	base := polled("GetAllGenJobs", 0)
	vary := []func(*models.UnmatchedCall){
		func(m *models.UnmatchedCall) { m.Operation = "query GetAllTestRunJobs" },
		func(m *models.UnmatchedCall) { m.ActualSummary = "POST /graphql" },
		func(m *models.UnmatchedCall) { m.ClosestMock = "mock-612" },
		func(m *models.UnmatchedCall) { m.Repeat.Mock = "mock-613" },
		func(m *models.UnmatchedCall) { m.TrailsOf = "atg-features::pages/cleanup.spec.ts::test" },
		func(m *models.UnmatchedCall) { m.Reason = models.MissRequestChanged },
		func(m *models.UnmatchedCall) { m.Source.Exe = "/usr/bin/node" },
		func(m *models.UnmatchedCall) { m.Source = nil },
	}
	misses := []models.UnmatchedCall{base}
	for i, v := range vary {
		m := polled("GetAllGenJobs", i+1)
		v(&m)
		misses = append(misses, m)
	}
	got := unchargedMisses(misses)
	if len(got) != len(misses) {
		t.Fatalf("%d entries of %d distinct calls; want one each", len(got), len(misses))
	}
	for i, u := range got {
		if u.Count != 1 {
			t.Fatalf("entry %d counts %d; want 1", i, u.Count)
		}
	}
}

// A call a test was charged with, and one that counts against no test, are no
// entry and no count of another's.
func TestReceiptCountsOnlyUnchargedCalls(t *testing.T) {
	charged, background := polled("GetAllGenJobs", 1), polled("GetAllGenJobs", 2)
	charged.ChargedTo, background.Background = "atg-features::pages/page-1.spec.ts::test", true
	got := unchargedMisses([]models.UnmatchedCall{charged, polled("GetAllGenJobs", 0), background})
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("entries %+v; want the one uncharged call, counted once", got)
	}
}

// A receipt that predates the count lists each call once: an entry without
// one is one call.
func TestReceiptEntryWithoutACountIsOneCall(t *testing.T) {
	ms := []UnchargedMiss{{}, {Count: 3}, {}}
	if n := UnchargedCalls(ms); n != 5 {
		t.Fatalf("UnchargedCalls = %d; want 5", n)
	}
}

// Past the bound, the receipt says how many calls its entries counted, and how
// many distinct calls those were.
func TestReceiptPastTheBoundCountsTheCallsItLost(t *testing.T) {
	defer func(was int64) { receiptCap = was }(receiptCap)
	receiptCap = 16 << 10
	misses := manyUncharged(100)
	misses[0].Count = 41
	got := writeAndRead(t, Receipt{Set: "set", OnMiss: "fail", UnchargedMisses: misses})
	if !strings.Contains(got.Error, "none of the run's 140 misses charged to no test (100 distinct calls)") {
		t.Fatalf("error %q; want it to count 140 calls in 100 entries", got.Error)
	}
}

// A call keploy cannot key (a time JSON does not write) is an entry of its
// own, never another's count.
func TestReceiptCallWithoutAKeyIsItsOwnEntry(t *testing.T) {
	a, b := polled("GetAllGenJobs", 0), polled("GetAllGenJobs", 4)
	a.Repeat.RecordedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Repeat.RecordedAt = a.Repeat.RecordedAt
	if got := unchargedMisses([]models.UnmatchedCall{a, b}); len(got) != 2 {
		t.Fatalf("%d entries; want each unkeyed call its own", len(got))
	}
}
