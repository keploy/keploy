package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// learnedNames is the names LearnedElsewhere holds, in its order.
func learnedNames(states []models.MockState) []string {
	out := make([]string, 0, len(states))
	for _, st := range states {
		out = append(out, st.Name)
	}
	return out
}

// Record's full round's learning-only candidates are the other tests'
// recordings the set's calls have not consumed: never the open test's own
// nor the background's, and none once their owner consumed them. Unlike the
// pairing's, they hold under today's rules too (one rule for every set).
func TestLearnElsewhereCandidatesAreOtherTestsUnconsumedRecordings(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	equalNames(t, "the candidates under the background rules", mm.LearnElsewhereCandidates(models.HTTP), "other")

	// Today's rules: no background is named, and the open test's own is
	// still no candidate.
	mm.ServeExactBackground("", nil, nil)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	got := exactNames(mm.LearnElsewhereCandidates(models.HTTP))
	if !slices.Contains(got, "other") || slices.Contains(got, "own") {
		t.Fatalf("the candidates under today's rules are %v; want other's, never the open test's own", got)
	}

	// Consumed by its owner, a recording is no candidate.
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if err := mm.flagMockAsUsed(models.MockState{Name: "other", Kind: models.HTTP, Usage: models.Updated}, 0); err != nil {
		t.Fatal(err)
	}
	if c := mm.LearnElsewhereCandidates(models.HTTP); len(c) != 0 {
		t.Fatalf("a recording its owner consumed is a candidate: %v", exactNames(c))
	}
}

// The learning-only candidates exist only while record's full round's learn
// step runs under the proven switch: without Learn, once the learn step
// ended or while it is held, and with the switch off, there are none, and
// nothing is kept.
func TestLearnElsewhereIsOffOutsideTheLearnStep(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	clocks := map[string]models.ClockOffset{"claimed_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	off := func(why string) {
		t.Helper()
		if c := mm.LearnElsewhereCandidates(models.HTTP); c != nil {
			t.Fatalf("%s: candidates %v", why, exactNames(c))
		}
		if mm.LearnElsewhere(ms["other"], clocks) {
			t.Fatalf("%s: a call taught another test's recording", why)
		}
	}
	off("without Learn")
	mm.SetPairing(true)
	equalNames(t, "precondition: the candidates with Learn", mm.LearnElsewhereCandidates(models.HTTP), "other")
	mm.SetLearnEnded(true)
	off("the learn step ended")
	mm.SetLearnEnded(false)
	mm.SetLearnHeld(true)
	off("the learn step held")
	mm.SetLearnHeld(false)
	t.Setenv(provenswitch.Env, "")
	mm.SetPairing(true)
	off("the switch off")
	if got := mm.LearnedElsewhere(); len(got) != 0 {
		t.Fatalf("learning kept outside the learn step: %v", learnedNames(got))
	}
}

// What a call made in another window teaches a recording is no consumption:
// the consumed list (which attributes the set's calls to tests and judges
// them) never holds it, the recording stays its owner's to consume, and the
// set's LearnedElsewhere holds it, merged across calls. It is the set's: a
// new set starts with none.
func TestLearnElsewhereIsNoConsumption(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	if !mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"claimed_at": minute}) {
		t.Fatal("another test's unconsumed recording was taught nothing")
	}
	if !mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"dirty_at": {Res: time.Millisecond}}) {
		t.Fatal("a second call taught the recording nothing")
	}
	if mm.LearnElsewhere(ms["other"], nil) {
		t.Fatal("a call that taught no clock read was kept")
	}
	for _, name := range []string{"own", "w-boot", "pool"} {
		if mm.LearnElsewhere(ms[name], map[string]models.ClockOffset{"claimed_at": minute}) {
			t.Fatalf("%s, no other test's recording, was taught", name)
		}
	}
	if got := mm.GetConsumedMocks(); len(got) != 0 {
		t.Fatalf("learning only consumed %v", got)
	}
	if mm.consumedInSet("other") {
		t.Fatal("learning only marked the recording consumed")
	}
	got := mm.LearnedElsewhere()
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("learned elsewhere %v; want other alone", learnedNames(got))
	}
	if _, ok := got[0].Clocks["claimed_at"]; !ok {
		t.Fatalf("the first call's clock read was lost: %v", got[0].Clocks)
	}
	if _, ok := got[0].Clocks["dirty_at"]; !ok {
		t.Fatalf("the second call's clock read was lost: %v", got[0].Clocks)
	}
	// Its owner still consumes it.
	mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
	if p := pollFirst(t, mm, "other"); p != "other" {
		t.Fatalf("the recording's own test did not consume it: %s", p)
	}
	// A new set starts with nothing learned elsewhere.
	mm.dropPreviousSetConsumes()
	if got := mm.LearnedElsewhere(); len(got) != 0 {
		t.Fatalf("a new set holds the previous set's learning: %v", learnedNames(got))
	}
}
