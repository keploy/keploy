package proxy

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
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
	equalNames(t, "the candidates under the background rules", mm.LearnElsewhereCandidates(models.HTTP, nil), "other")

	// Today's rules: no background is named, and the open test's own is
	// still no candidate.
	mm.ServeExactBackground("", nil, nil)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	got := exactNames(mm.LearnElsewhereCandidates(models.HTTP, nil))
	if !slices.Contains(got, "other") || slices.Contains(got, "own") {
		t.Fatalf("the candidates under today's rules are %v; want other's, never the open test's own", got)
	}

	// Consumed by its owner, a recording is no candidate.
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if err := mm.flagMockAsUsed(models.MockState{Name: "other", Kind: models.HTTP, Usage: models.Updated}, 0); err != nil {
		t.Fatal(err)
	}
	if c := mm.LearnElsewhereCandidates(models.HTTP, nil); len(c) != 0 {
		t.Fatalf("a recording its owner consumed is a candidate: %v", exactNames(c))
	}
}

// A call's learning-only candidates are read by its request keys: of the
// recordings the trees hold and those kept on disk, only those filed under
// one of the call's keys, and one on disk is decoded only then. A full round
// parks thousands of other tests' recordings on disk, and every miss of the
// learn step asks: read by kind alone, each miss decoded every one of them.
// Without keys, or of a kind whose parser declares none, every recording of
// the kind is a candidate, as before.
func TestLearnElsewhereCandidatesDecodeOnlyRecordingsUnderTheCallsKeys(t *testing.T) {
	undoHTTP := models.RegisterRequestKeys(models.HTTP, func(m *models.Mock) []string {
		if m.Spec.HTTPReq == nil {
			return nil
		}
		return []string{m.Spec.HTTPReq.URL}
	})
	t.Cleanup(undoHTTP)
	undoMongo := models.RegisterRequestKeys(models.Mongo, nil)
	t.Cleanup(undoMongo)
	mm, ms := s12Set(t)
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	timer := exactMock("timer-on-disk", 500*time.Millisecond, models.LifetimePerTest)
	timer.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "/other"}
	parked := []*models.Mock{timer}
	for i := 0; i < 50; i++ {
		mk := exactMock(fmt.Sprintf("parked-%02d", i), time.Duration(600+i)*time.Millisecond, models.LifetimePerTest)
		mk.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("/parked-%02d", i)}
		parked = append(parked, mk)
	}
	mongo := exactMock("mongo-on-disk", 700*time.Millisecond, models.LifetimePerTest)
	mongo.Kind = models.Mongo
	parked = append(parked, mongo)
	for _, mk := range parked {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	loaded := map[string]int{}
	counted := func(visit func(string, func() (*models.Mock, error)) bool) func(string, func() (*models.Mock, error)) bool {
		return func(name string, load func() (*models.Mock, error)) bool {
			return visit(name, func() (*models.Mock, error) { loaded[name]++; return load() })
		}
	}
	mm.SetRecordingSource(func(kind models.Kind, keys []string, visit func(string, func() (*models.Mock, error)) bool) error {
		return disk.RangeKeys(kind, keys, counted(visit))
	})
	mm.SetRecordedOfKind(func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		return disk.RangeKind(kind, counted(visit))
	})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})

	equalNames(t, "the candidates under the call's key", mm.LearnElsewhereCandidates(models.HTTP, []string{"/other"}),
		"timer-on-disk", "other")
	for name, n := range loaded {
		if name != "timer-on-disk" || n != 1 {
			t.Fatalf("the call's keys file timer-on-disk alone, yet the read decoded %v", loaded)
		}
	}
	clear(loaded)
	if c := mm.LearnElsewhereCandidates(models.HTTP, []string{"/nothing-filed"}); len(c) != 0 || len(loaded) != 0 {
		t.Fatalf("a key nothing is filed under gave %v and decoded %v", exactNames(c), loaded)
	}

	// Without keys, every recording of the kind, decoded.
	all := exactNames(mm.LearnElsewhereCandidates(models.HTTP, nil))
	for _, mk := range append([]*models.Mock{ms["other"]}, parked[:len(parked)-1]...) {
		if !slices.Contains(all, mk.Name) {
			t.Fatalf("without keys the candidates are %v; want other and every one parked of the kind", all)
		}
	}
	if slices.Contains(all, "own") || slices.Contains(all, "mongo-on-disk") {
		t.Fatalf("without keys the candidates are %v: the open test's own, or another kind's", all)
	}
	// A kind whose parser declares no keys is read whole, keys or not.
	equalNames(t, "a kind with no request keys", mm.LearnElsewhereCandidates(models.Mongo, []string{"/other"}), "mongo-on-disk")
}

// The learning-only candidates exist only while record's full round's learn
// step runs under the proven switch: without Learn, once the learn step
// ended or while it is held, in a staging's view, and with the switch off,
// there are none, and nothing is kept.
func TestLearnElsewhereIsOffOutsideTheLearnStep(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	clocks := map[string]models.ClockOffset{"claimed_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	off := func(why string) {
		t.Helper()
		if c := mm.LearnElsewhereCandidates(models.HTTP, nil); c != nil {
			t.Fatalf("%s: candidates %v", why, exactNames(c))
		}
		if mm.LearnElsewhere(ms["other"], clocks) {
			t.Fatalf("%s: a call taught another test's recording", why)
		}
	}
	off("without Learn")
	mm.SetPairing(true)
	equalNames(t, "precondition: the candidates with Learn", mm.LearnElsewhereCandidates(models.HTTP, nil), "other")
	mm.SetLearnEnded(true)
	off("the learn step ended")
	mm.SetLearnEnded(false)
	mm.SetLearnHeld(true)
	off("the learn step held")
	mm.SetLearnHeld(false)
	// A view the next set's staging installs before it names the set's
	// background: the trees may still hold the last set's recordings, whose
	// names may be this set's.
	mm.StageExactBackground(models.RecordedWithS12Routing)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	off("a staging view")
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	equalNames(t, "precondition: the candidates once the set's background is named", mm.LearnElsewhereCandidates(models.HTTP, nil), "other")
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

// What a call made in another window teaches a recording is that window's:
// each state names the test open when such a call came, once per window
// with the last instant it taught, and "" for a call between tests. A call
// whose round moved since it was read (a test's begin or end between the
// read and its learning) names none: keploy no longer knows its window, so
// whatever undoes a window's learning reads it as any window's.
func TestLearnElsewhereNamesTheWindowThatTaughtIt(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	minute := map[string]models.ClockOffset{"claimed_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	windows := func() []models.TeachingWindow {
		t.Helper()
		got := mm.LearnedElsewhere()
		if len(got) != 1 || got[0].Name != "other" {
			t.Fatalf("learned elsewhere %v; want other alone", learnedNames(got))
		}
		return got[0].TaughtIn
	}

	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if !mm.LearnElsewhere(ms["other"], minute) {
		t.Fatal("a call in T1's window taught other nothing")
	}
	first := windows()
	if len(first) != 1 || first[0].Test != "T1" || first[0].At.IsZero() {
		t.Fatalf("taught in %+v; want T1's window, with its instant", first)
	}
	time.Sleep(time.Millisecond)
	if !mm.LearnElsewhere(ms["other"], minute) {
		t.Fatal("a second call in T1's window taught other nothing")
	}
	if again := windows(); len(again) != 1 || again[0].Test != "T1" || !again[0].At.After(first[0].At) {
		t.Fatalf("taught in %+v after a second call in T1's window; want T1's window once, at the later instant", again)
	}

	mm.ServeExactPool("", nil)
	if !mm.LearnElsewhere(ms["other"], minute) {
		t.Fatal("a call between tests taught other nothing")
	}
	if got := windows(); len(got) != 2 || got[1].Test != "" {
		t.Fatalf("taught in %+v; want T1's window, then between tests", got)
	}

	// A call read in T3's window whose learning lands after T4 began.
	mm.ServeExactTest("T3", nil)
	call := mm.MarkCallRound()
	held, _ := mm.HoldToRound(call)
	mm.ServeExactTest("T4", nil)
	if !held.(integrations.LearnElsewhere).LearnElsewhere(ms["other"], minute) {
		t.Fatal("a call whose round moved taught other nothing")
	}
	for _, w := range windows() {
		if w.Test == "T3" || w.Test == "T4" {
			t.Fatalf("taught in %+v; a call whose round moved since its read named %s", windows(), w.Test)
		}
	}
	// Read and learned in one window, it names it.
	call = mm.MarkCallRound()
	held, _ = mm.HoldToRound(call)
	if !held.(integrations.LearnElsewhere).LearnElsewhere(ms["other"], minute) {
		t.Fatal("a call in T4's window taught other nothing")
	}
	if got := windows(); len(got) != 3 || got[2].Test != "T4" {
		t.Fatalf("taught in %+v; want T4's window last", got)
	}
}
