package learn

import (
	"strings"
	"testing"
	"time"
)

// A changed word whose every candidate place is a start-up answer that held
// the recorded word, given in an order keploy cannot tell (answers given at
// one instant, or a serve round F did not number), where no candidate holds
// round F's word in round F: no copy by position can give round F's word,
// so the order cannot matter and marks nothing. Each place is unchanged
// while the word changed, so the word is chance there, set aside and made
// anew (an insert's eventDetails.duration_ms "4" went to "51" while
// start-up Mongo answers given together had held 4). Where one candidate
// holds round F's word in round F, the order counts and the word is
// refused as unknown, naming it; a 32-bit word at such places is a
// conflict, decided once. keploy test and keploy mock alike.
func TestOutcomeAnUntoldOrderCountsOnlyWithBothEndsOfACopy(t *testing.T) {
	const path = "body.eventDetails.duration_ms"
	type row struct {
		what      string
		raw       []Raw
		rec, live string
		want      WordOutcome
		says      string
		passes    int64
	}
	together := func(rec string) []Raw {
		return []Raw{
			oAnswer("mock-29", 300*time.Millisecond, rec).ServedAs(1).ServedAsRecorded(),
			oAnswer("mock-88", 300*time.Millisecond, rec).ServedAs(2).ServedAsRecorded(),
		}
	}
	unnumbered := func(rec string) []Raw {
		return []Raw{
			oAnswer("mock-20", 400*time.Millisecond, rec).ServedAs(1).ServedAsRecorded(),
			oAnswer("mock-173", 300*time.Millisecond, rec).WithServed(true).ServedAsRecorded(),
		}
	}
	// fOnly holds another word in the recording and round F's in round F.
	fOnly := func(f string) Raw {
		return oAnswer("mock-72", 200*time.Millisecond, "7").ServedAs(3).ServedLive(oAnswer("mock-72", 200*time.Millisecond, f))
	}
	rows := []row{
		{"start-ups given at one instant, none holds round F's word", together("4"), "4", "51", OutcomeFresh, "set aside", 1},
		{"start-ups, one serve not numbered, none holds round F's word", unnumbered("9"), "9", "268", OutcomeFresh, "set aside", 1},
		{"control: given at one instant, one more holds round F's word in F", append(together("4"), fOnly("51")), "4", "51", OutcomeRefused, "at one instant", 0},
		{"control: one serve not numbered, one more holds round F's word in F", append(unnumbered("9"), fOnly("268")), "9", "268", OutcomeRefused, "does not know where record's full round served mock-173", 0},
		{"a 32-bit word at start-ups given at one instant", together("A7K29QXM"), "A7K29QXM", "Q8M31WPN", OutcomeRefused, `"A7K29QXM" stood whole at mock-29`, 1},
	}
	for _, mode := range []struct {
		what   string
		drifts func() *DriftCache
	}{{"keploy test", oDrifts}, {"keploy mock", oClientDrifts}} {
		for _, r := range rows {
			in := oInput(path, r.rec, r.live, nil, r.raw)
			in.Drifts = mode.drifts()
			before := wordPasses.Load()
			ds := Decide(in)
			passes := wordPasses.Load() - before
			d := oWord(t, ds, r.rec, r.live)
			what := mode.what + ", " + r.what
			switch {
			case r.want == OutcomeFresh:
				if d.Outcome != OutcomeFresh || !strings.Contains(d.Line, r.says) || strings.Contains(d.Line, "order") {
					t.Errorf("%s: %s, line %q; want fresh, its line saying %q and naming no order", what, oDescribe([]Decision{d}), d.Line, r.says)
				}
			case r.passes == 0:
				oRefused(t, what, d, RefusalUnknown, r.says)
			default:
				oRefused(t, what, d, RefusalConflict, r.says)
			}
			if r.passes != 0 && passes != r.passes*int64(len(ds)) {
				t.Errorf("%s: %d decisions of %d words; want %d each (the order not asked)", what, passes, len(ds), r.passes)
			}
		}
	}
}
