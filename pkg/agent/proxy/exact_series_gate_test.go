package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The shape of a sweep that looks up an app TestT deletes: TestT's own
// request looks the app up (c1, c2: the app's document as created), then
// TestT writes the delete's audit (del, TestT's own recording, at
// /audit), and the sweep's later lookups (d1, d2) answer that write's
// document. The judge places the lookups as two parts of one trailing
// series: the first closed by del, the second opened by it.
const (
	partOne = "lookup/TestT/1"
	partTwo = "lookup/TestT/1.2"
	auditTo = "/audit?app=a1"
)

// gatedSweep is a staged manager whose worker scope holds the two parts and
// whose TestT holds del. It returns del for the test's view.
func gatedSweep(t *testing.T) (*MockManager, *models.Mock) {
	t.Helper()
	del := httpCall("del", 3*time.Second, auditTo)
	mks := []*models.Mock{
		httpCall("c1", time.Second, sweepURL),
		httpCall("c2", 2*time.Second, sweepURL),
		del,
		httpCall("d1", 4*time.Second, sweepURL),
		httpCall("d2", 5*time.Second, sweepURL),
	}
	mm := stagedManager(t, nil, mks)
	mm.ServeExactWorker([]string{"c1", "c2", "d1", "d2"})
	mm.SetPoolTrails(map[string]string{"c1": "TestT", "c2": "TestT", "d1": "TestT", "d2": "TestT"})
	mm.SetPoolGates(map[string]models.SeriesGate{partOne: {ClosedBy: "del"}, partTwo: {OpenedBy: "del"}})
	mm.SetPoolSeries(map[string][]string{partOne: {"c1", "c2"}, partTwo: {"d1", "d2"}})
	return mm, del
}

// Once the run served TestT's write, the lookups that follow, in a later
// test's window and between tests, are answered the second part (the
// write's document), its last again past the recorded count, and never the
// first part's answer. Before it, TestT's own lookups are answered the
// first part.
func TestAPartOfASeriesAnswersOnlyAfterTheWriteThatOpensIt(t *testing.T) {
	mm, del := gatedSweep(t)
	mm.ServeExactTest("TestT", []*models.Mock{del})
	got := []string{callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false)}
	if want := []string{"c1", "c2"}; !slices.Equal(got, want) {
		t.Fatalf("before the write the lookups were answered %v, want %v", got, want)
	}
	if got := callURL(t, mm, auditTo, false); got != "del" {
		t.Fatalf("TestT's write was answered %q, want del", got)
	}
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("TestU", nil)
	got = nil
	for i := 0; i < 3; i++ {
		got = append(got, callURL(t, mm, sweepURL, false))
	}
	if want := []string{"d1", "d2", "d2"}; !slices.Equal(got, want) {
		t.Fatalf("after the write the sweep was answered %v, want %v", got, want)
	}
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, sweepURL, false); got != "d1" && got != "d2" {
		t.Fatalf("between tests after the write the sweep was answered %q, want a recording of the part the write opened", got)
	}
}

// A run where TestT's write never came is never answered the second part:
// TestT's lookups and the sweep's, in any window and between tests, are
// answered the first part, its last again.
func TestAPartWhoseWriteNeverCameIsNeverServed(t *testing.T) {
	mm, del := gatedSweep(t)
	mm.ServeExactTest("TestT", []*models.Mock{del})
	got := []string{callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false)}
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("TestU", nil)
	got = append(got, callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false))
	mm.ServeExactPool("k", nil)
	got = append(got, callURL(t, mm, sweepURL, false))
	for i, n := range got {
		if n != "c1" && n != "c2" {
			t.Fatalf("call %d of %v was answered %q with TestT's write never served, want only the first part", i, got, n)
		}
	}
	if got[0] != "c1" || got[1] != "c2" || got[3] != "c2" {
		t.Fatalf("the first part answered %v, want c1, c2, then its last again", got)
	}
}

// The run's served openers outlive a scope table sent again with the same
// gates (the same run), and not one with other gates (another set). A gate
// with an empty id, or one that names no recording, marks nothing.
func TestSeriesGatesKeepTheirServedOpenersAcrossTheSameTable(t *testing.T) {
	mm, del := gatedSweep(t)
	mm.ServeExactTest("TestT", []*models.Mock{del})
	if got := callURL(t, mm, auditTo, false); got != "del" {
		t.Fatalf("TestT's write was answered %q, want del", got)
	}
	mm.SetPoolGates(map[string]models.SeriesGate{partOne: {ClosedBy: "del"}, partTwo: {OpenedBy: "del"}, "": {OpenedBy: "x"}, "empty": {}})
	mm.SetPoolSeries(map[string][]string{partOne: {"c1", "c2"}, partTwo: {"d1", "d2"}})
	s := mm.exact.poolSeries.Load()
	if len(s.gates) != 2 {
		t.Fatalf("the gates kept %v, want the two that name a recording", s.gates)
	}
	if s.shut("d1") || !s.shut("c1") {
		t.Fatalf("the same table sent again forgot the served write: d1 shut %v, c1 shut %v", s.shut("d1"), s.shut("c1"))
	}
	mm.SetPoolGates(map[string]models.SeriesGate{partTwo: {OpenedBy: "del"}})
	mm.SetPoolSeries(map[string][]string{partOne: {"c1", "c2"}, partTwo: {"d1", "d2"}})
	s = mm.exact.poolSeries.Load()
	if !s.shut("d1") || s.shut("c1") {
		t.Fatalf("another table kept the last one's served write: d1 shut %v, c1 shut %v", s.shut("d1"), s.shut("c1"))
	}
	if (*seriesIndex)(nil).shut("d1") {
		t.Fatal("a nil index shut a recording")
	}
}

// A consume of a shut recording that a parser read before the write closed
// its part is refused, outside every window and between tests, so the
// parser reads again.
func TestAConsumeOfAShutPartIsRefused(t *testing.T) {
	mm, del := gatedSweep(t)
	mm.ServeExactPool("k", nil)
	mocks, _ := mm.GetSessionScopedMocks()
	var d1 *models.Mock
	for _, mk := range mocks {
		if mk.Name == "d1" {
			d1 = mk
		}
	}
	if d1 != nil {
		t.Fatal("a part no write opened was read")
	}
	mm.prepareOutside()
	for _, mk := range []*models.Mock{httpCall("d1", 4*time.Second, sweepURL)} {
		if v := mm.consumeOutside(mk, true); v != exactRefused {
			t.Fatalf("a consume of %s in a window before its write: %v, want refused", mk.Name, v)
		}
		if v, _ := mm.consumeBetween(mk); v != exactRefused {
			t.Fatalf("a consume of %s between tests before its write: %v, want refused", mk.Name, v)
		}
	}
	_ = del
}
