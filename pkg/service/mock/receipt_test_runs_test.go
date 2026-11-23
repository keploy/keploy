package mock

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestTheTestsReceiptListsEveryWindowInTheOrderItBegan pins
// TestsReceipt.Windows: every named window, a test run twice once per run,
// sorted by when it began, an open window with no end, and no window
// without a test.
func TestTheTestsReceiptListsEveryWindowInTheOrderItBegan(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.FixedZone("IST", 19800))
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	got := testWindowsOf([]models.ScopeWindow{
		{Name: "TestB", Start: at(10), End: at(12)},
		{Name: "", Start: at(1), End: at(2)},
		{Name: "TestA", Start: at(0), End: at(3)},
		{Name: "TestA", Start: at(20), End: at(21)},
		{Name: "TestC", Start: at(30)},
	})
	want := []TestWindow{
		{Test: "TestA", Began: at(0).UTC(), Ended: at(3).UTC()},
		{Test: "TestB", Began: at(10).UTC(), Ended: at(12).UTC()},
		{Test: "TestA", Began: at(20).UTC(), Ended: at(21).UTC()},
		{Test: "TestC", Began: at(30).UTC()},
	}
	if len(got) != len(want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Test != want[i].Test || !got[i].Began.Equal(want[i].Began) || !got[i].Ended.Equal(want[i].Ended) {
			t.Fatalf("window %d = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
		if got[i].Began.Location() != time.UTC {
			t.Fatalf("window %d began in %v, want UTC", i, got[i].Began.Location())
		}
	}
	if !got[3].Ended.IsZero() {
		t.Fatalf("an open window has no end, got %v", got[3].Ended)
	}
	if testWindowsOf(nil) != nil {
		t.Fatalf("no windows lists none")
	}
}
