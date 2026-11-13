package mock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// manyUncharged is n misses charged to no test, each as a replay keeps one:
// a call's shape, its closest mock, the matcher's next step and the tests
// it fell between, as a Playwright project's round wrote them (an
// enterprise-ui project of 344 tests left 4459 such misses in one round).
func manyUncharged(n int) []UnchargedMiss {
	step := "keploy's learn step: " + strings.Repeat("the call's body changed at a place keploy does not learn; ", 12)
	out := make([]UnchargedMiss, n)
	for i := range out {
		out[i] = UnchargedMiss{UnmatchedCall: models.UnmatchedCall{
			Protocol:      "HTTP",
			ActualSummary: fmt.Sprintf("POST /query {query GetAllTestRunJobs%d}", i),
			Destination:   "localhost:8083",
			ClosestMock:   fmt.Sprintf("mock-%d", i),
			NextSteps:     step,
			WindowOf:      fmt.Sprintf("atg-features::projects/atg-tests/pages/ci-cd/page.spec.ts::CI/CD Page › Snippet State %d", i),
		}}
	}
	return out
}

// A round whose misses outside every test fill more than a megabyte writes a
// receipt keploy reads back whole: the receipt holds one entry per such miss,
// so it grows with the run's calls, as the per-test results do, and a reader
// that refused it would leave the whole round unjudged.
func TestReceiptOfManyUnchargedMissesReadsBack(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	misses := manyUncharged(5000)
	writeReceipt(zap.NewNop(), keployDir, Receipt{Set: "set", OnMiss: "fail", UnchargedMisses: misses})
	info, err := os.Stat(filepath.Join(keployDir, "set", ReceiptFile))
	if err != nil {
		t.Fatalf("no receipt written: %v", err)
	}
	if info.Size() <= 1<<20 {
		t.Fatalf("the receipt is %d bytes; the row needs one past a megabyte", info.Size())
	}
	got, err := ReadReceipt(keployDir, "set")
	if err != nil {
		t.Fatalf("ReadReceipt of a %d-byte receipt keploy wrote = %v, want it read", info.Size(), err)
	}
	if len(got.UnchargedMisses) != len(misses) || got.FailedBy != "" {
		t.Fatalf("read back %d uncharged misses, failedBy %q; want %d, none", len(got.UnchargedMisses), got.FailedBy, len(misses))
	}
}

// A receipt past the bound keploy reads is never written as is: keploy
// writes one it reads back, which says keploy did not complete the run and
// why, so a reader judges no round from a receipt it cannot read, and none
// from one that lost its lists. Every list goes, so what is left is a
// handful of scalars and fits.
func TestReceiptPastTheBoundSaysKeployFailed(t *testing.T) {
	defer func(was int64) { receiptCap = was }(receiptCap)
	receiptCap = 64 << 10
	got := writeAndRead(t, Receipt{Set: "set", OnMiss: "fail", Missed: 7, UnchargedMisses: manyUncharged(500),
		RequiredNotMade: []RequiredNotMade{{Series: "s"}}, RequiredNotDue: []RequiredNotMade{{Series: "d"}},
		Refusals: []models.RunRefusal{{Code: "c", Message: "m"}}, CannotCapture: models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1}},
		CaptureIncomplete: models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 2}}})
	if got.FailedBy != FailedByKeploy || !strings.HasPrefix(got.Error, "keploy's receipt of this run is ") || !strings.Contains(got.Error, "past the 65536 it reads back") {
		t.Fatalf("failedBy %q, error %q; want keploy, naming the bound", got.FailedBy, got.Error)
	}
	if n := len(got.UnchargedMisses) + len(got.RequiredNotMade) + len(got.RequiredNotDue) + len(got.Refusals) + len(got.CannotCapture) + len(got.CaptureIncomplete); n != 0 {
		t.Fatalf("the receipt kept %d list entries; want none", n)
	}
	if got.Missed != 7 || got.Set != "set" || got.OnMiss != "fail" {
		t.Fatalf("the receipt lost its counts: missed %d, set %q, onMiss %q", got.Missed, got.Set, got.OnMiss)
	}
}

// The bound holds the whole file, its head too: a body that fits only
// without the head is past it.
func TestReceiptPastTheBoundByItsHead(t *testing.T) {
	defer func(was int64) { receiptCap = was }(receiptCap)
	r := Receipt{Set: "set", OnMiss: "fail", UnchargedMisses: manyUncharged(20)}
	body, err := receiptBody(r)
	if err != nil {
		t.Fatal(err)
	}
	receiptCap = int64(len(receiptHead) + len(body) - 1)
	if got := writeAndRead(t, r); got.FailedBy != FailedByKeploy {
		t.Fatalf("failedBy %q of a receipt one byte past the bound; want keploy", got.FailedBy)
	}
}

// Why keploy could not complete the run stays, before why the receipt
// lists nothing.
func TestReceiptPastTheBoundKeepsTheRunsError(t *testing.T) {
	defer func(was int64) { receiptCap = was }(receiptCap)
	receiptCap = 16 << 10
	got := writeAndRead(t, Receipt{Set: "set", FailedBy: FailedByStrict, Error: "the agent stopped", UnchargedMisses: manyUncharged(100)})
	if got.FailedBy != FailedByKeploy || !strings.HasPrefix(got.Error, "the agent stopped; keploy's receipt of this run is ") {
		t.Fatalf("failedBy %q, error %q; want keploy, the run's error first", got.FailedBy, got.Error)
	}
}

// writeAndRead writes r as a replay's receipt and reads it back.
func writeAndRead(t *testing.T, r Receipt) *Receipt {
	t.Helper()
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, r.Set), 0o755); err != nil {
		t.Fatal(err)
	}
	writeReceipt(zap.NewNop(), keployDir, r)
	got, err := ReadReceipt(keployDir, r.Set)
	if err != nil || got == nil {
		t.Fatalf("ReadReceipt = %v, %v; want the receipt keploy wrote", got, err)
	}
	return got
}

// A receipt exactly at the bound is written as is.
func TestReceiptWithinTheBoundIsWrittenWhole(t *testing.T) {
	defer func(was int64) { receiptCap = was }(receiptCap)
	r := Receipt{Set: "set", OnMiss: "fail", UnchargedMisses: manyUncharged(20)}
	body, err := receiptBody(r)
	if err != nil {
		t.Fatal(err)
	}
	receiptCap = int64(len(receiptHead) + len(body))
	if got := writeAndRead(t, r); got.FailedBy != "" || len(got.UnchargedMisses) != 20 {
		t.Fatalf("failedBy %q, %d uncharged misses; want the receipt whole", got.FailedBy, len(got.UnchargedMisses))
	}
}
