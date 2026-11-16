package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A given place (here the test case's request) stands first in both runs'
// orders, in one order, so a copy by position there takes the same place in
// both runs: it is no end of a copy by order. An insert's
// eventDetails.duration_ms went from "4" to "18" while start-up answers in
// flight together had held 4 and only the test case's request held 18, as
// "2025-06-18" in both runs: the word is chance, set aside and made anew,
// the order not asked, whether the start-ups were in flight together or
// given at one instant. One more answer holding round F's word in round F
// is an end, and the untold order refuses the word. A request holding the
// recorded word, beside answers given at one instant that hold round F's,
// asks no order either: the word is refused for what those answers show,
// not for their order. Where a candidate was not given before the call in
// both runs (round F never served it, or keploy cannot place it), a given
// place may have been the last place the application had in one run alone,
// so its ends count and the order is asked: an R end the test case's request
// faces (a start-up F never served), an F end it faces (an answer holding
// round F's word that F never served), and a candidate whose answer time
// keploy cannot read. keploy test and keploy mock alike.
func TestOutcomeAGivenPlaceIsNoEndOfACopyByOrder(t *testing.T) {
	const path = "body.eventDetails.duration_ms"
	request := func(v string) []Earlier {
		return []Earlier{{Mock: givenPlace("post-client-v1-mcp-1"), Given: true, At: oAt.Add(-2 * time.Second), Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.params.protocolVersion", Noise: "body.params.protocolVersion", Type: String, Value: v}, Live: v},
		}}}
	}
	// aloft: mock-2's request was recorded before mock-1 was answered.
	aloft := func(first Raw) []Raw {
		return []Raw{first, oAnswer("mock-2", 499*time.Millisecond+500*time.Microsecond, "4").ServedAs(2).ServedAsRecorded()}
	}
	served := oAnswer("mock-1", 500*time.Millisecond, "4").ServedAs(1).ServedAsRecorded()
	together := []Raw{
		oAnswer("mock-29", 300*time.Millisecond, "4").ServedAs(1).ServedAsRecorded(),
		oAnswer("mock-88", 300*time.Millisecond, "4").ServedAs(2).ServedAsRecorded(),
	}
	// fOnly held r in the recording and round F's f in round F.
	fOnly := func(name, r, f string, n int) Raw {
		return oAnswer(name, 200*time.Millisecond, r).ServedAs(n).ServedLive(oAnswer(name, 200*time.Millisecond, f))
	}
	fTogether := []Raw{fOnly("mock-72", "7", "18", 3), fOnly("mock-73", "8", "18", 4)}
	// untimed: its answer stamped before its own request, so keploy reads
	// no answer time and cannot place it.
	untimedMock := oPgMock("mock-90", 300*time.Millisecond, "SELECT next_slot FROM mock-90", []models.PostgresV3Cells{{{Value: "4"}}}, "SELECT 1")
	untimedMock.Spec.ResTimestampMock = oAt.Add(-400 * time.Millisecond)
	untimed := RawOf(untimedMock, false)
	type row struct {
		what    string
		earlier []Earlier
		raw     []Raw
		fresh   bool
		says    string
		lacks   string
		passes  int64
	}
	rows := []row{
		{"the request holds round F's word in both runs, start-ups in flight together the recorded one", request("2025-06-18"), aloft(served), true, "set aside", "order", 1},
		{"the same, the start-ups given at one instant", request("2025-06-18"), together, true, "set aside", "order", 1},
		{"control: one more answer holds round F's word in round F", request("2025-06-18"), append(aloft(served), fOnly("mock-72", "7", "18", 3)), false, "were in flight together", "", 2},
		{"the request holds the recorded word, answers given at one instant round F's", request("v-4"), fTogether, false, "at mock-72 before this call (both values hold it", "at one instant", 1},
		{"a start-up held the recorded word and round F never served it", request("2025-06-18"), aloft(oAnswer("mock-1", 500*time.Millisecond, "4")), false, "were in flight together", "", 2},
		{"the request holds the recorded word, an answer holding round F's never served", request("v-4"), append(fTogether[:1:1], oAnswer("mock-74", 250*time.Millisecond, "18")), false, "another order: the 2nd earlier answer was mock-74", "", 2},
		{"a candidate whose answer time keploy cannot read", request("2025-06-18"), append(aloft(served), untimed.ServedAs(3).ServedAsRecorded()), false, "no answer time", "", 2},
	}
	for _, mode := range []struct {
		what   string
		drifts func() *DriftCache
	}{{"keploy test", oDrifts}, {"keploy mock", oClientDrifts}} {
		for _, r := range rows {
			in := oInput(path, "4", "18", r.earlier, r.raw)
			in.Drifts = mode.drifts()
			before := wordPasses.Load()
			ds := Decide(in)
			passes := wordPasses.Load() - before
			d := oWord(t, ds, "4", "18")
			what := mode.what + ", " + r.what
			if r.fresh {
				if d.Outcome != OutcomeFresh || !strings.Contains(d.Line, r.says) || strings.Contains(d.Line, r.lacks) {
					t.Errorf("%s: %s, line %q; want fresh, its line saying %q and naming no %q", what, oDescribe([]Decision{d}), d.Line, r.says, r.lacks)
				}
			} else {
				oRefused(t, what, d, RefusalUnknown, r.says)
				if r.lacks != "" && d.Refusal != nil && strings.Contains(d.Refusal.Why, r.lacks) {
					t.Errorf("%s: refused %q; want no %q (the order not asked)", what, d.Refusal.Why, r.lacks)
				}
			}
			if passes != r.passes*int64(len(ds)) {
				t.Errorf("%s: %d decisions of %d words; want %d each", what, passes, len(ds), r.passes)
			}
		}
	}
}
