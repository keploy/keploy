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
	"go.uber.org/zap/zaptest/observer"
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
		t.Fatalf("the call's clock read was lost: %v", got[0].Clocks)
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

// One pairing per recording per round: a recording a call made in another
// window taught is taught no more in the round, whatever a later call would
// teach it, so a timer that ticked more often than the recordings it left
// teaches each of them once, the first tie of each tick onto the next one
// (the parser's pairing reads a refused recording as spent and pairs again).
// What the first call taught is kept whole. A new set teaches it anew.
func TestLearnElsewhereTeachesARecordingOncePerRound(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	if !mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"claimed_at": minute}) {
		t.Fatal("another test's unconsumed recording was taught nothing")
	}
	mm.ServeExactTest("T3", nil)
	if mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"dirty_at": {Res: time.Millisecond}}) {
		t.Fatal("a second call of the round taught a recording a call of the round taught already")
	}
	got := mm.LearnedElsewhere()
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("learned elsewhere %v; want other alone", learnedNames(got))
	}
	if c, ok := got[0].Clocks["claimed_at"]; !ok || c != minute || len(got[0].Clocks) != 1 {
		t.Fatalf("other holds %v; want the first call's claimed_at alone", got[0].Clocks)
	}
	if w := got[0].TaughtIn; len(w) != 1 || w[0].Test != "T1" {
		t.Fatalf("taught in %+v; want the first call's window, T1, alone", w)
	}
	// A new set's round teaches its recordings anew.
	mm.dropPreviousSetConsumes()
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if !mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"dirty_at": {Res: time.Millisecond}}) {
		t.Fatal("a new set's round could not teach a recording the previous set's taught")
	}
}

// One pairing per recording per round, and its own one wins,
// the other way round from the record's (the record voids a teaching its
// own pairing followed): a recording its own test's call consumed first is
// taught nothing by a later call of its shape in another window, and the
// set holds no such teaching of it.
func TestARecordingItsOwnTestConsumedFirstIsTaughtNothingAcrossWindows(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
	if p := pollFirst(t, mm, "other"); p != "other" {
		t.Fatalf("the recording's own test did not consume it: %s", p)
	}
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	if mm.LearnElsewhere(ms["other"], map[string]models.ClockOffset{"claimed_at": minute}) {
		t.Fatal("a call in T1's window taught a recording its own test's call consumed first")
	}
	if got := mm.LearnedElsewhere(); len(got) != 0 {
		t.Fatalf("learned elsewhere %v; want none", learnedNames(got))
	}
}

// An idle answer of the background (a worker's "nothing yet") is taught the
// clock reads of a poll's call wherever the call lands, as the parser
// carries them to it: without that it answers no later poll whose clock
// read moved. Any other background recording, and an idle answer of the
// open test's own, is still taught nothing; the teaching is no consumption,
// names its window, and comes once per round, as any recording's.
func TestLearnElsewhereKeepsWhatAPollTaughtAnIdleBackgroundRecording(t *testing.T) {
	mm, ms := s12Set(t)
	t.Cleanup(models.RegisterIdleAnswer(models.HTTP, func(m *models.Mock) bool { return m.Name == "w-later" || m.Name == "own" }))
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	clocks := map[string]models.ClockOffset{"dirty_at": minute}
	if !mm.LearnElsewhere(ms["w-later"], clocks) {
		t.Fatal("an idle answer of the worker scope was taught nothing")
	}
	for _, name := range []string{"w-boot", "pool", "own"} {
		if mm.LearnElsewhere(ms[name], clocks) {
			t.Fatalf("%s, no idle answer of the background, was taught", name)
		}
	}
	got := mm.LearnedElsewhere()
	if len(got) != 1 || got[0].Name != "w-later" || got[0].Clocks["dirty_at"] != minute {
		t.Fatalf("learned elsewhere %v (%v); want w-later alone, with the poll's dirty_at", learnedNames(got), got)
	}
	if w := got[0].TaughtIn; len(w) != 1 || w[0].Test != "T1" {
		t.Fatalf("taught in %+v; want the poll's window, T1", w)
	}
	if c := mm.GetConsumedMocks(); len(c) != 0 || mm.consumedInSet("w-later") {
		t.Fatalf("teaching an idle answer consumed %v", c)
	}
	if mm.LearnElsewhere(ms["w-later"], map[string]models.ClockOffset{"dirty_at": {Res: time.Millisecond}}) {
		t.Fatal("a second poll of the round taught the idle answer again")
	}
}

// An idle answer a call of the set consumed was paired in its own window: a
// poll's call in another window teaches it nothing, as any consumed
// recording, so both records never void what the agent said it kept. An
// idle answer no call consumed is still taught.
func TestLearnElsewhereTeachesNoIdleAnswerACallConsumed(t *testing.T) {
	mm, ms := s12Set(t)
	t.Cleanup(models.RegisterIdleAnswer(models.HTTP, func(m *models.Mock) bool { return m.Name == "w-later" || m.Name == "w-boot" }))
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactWorker([]string{"w-later", "w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if p := pollFirst(t, mm, "w-later"); p != "w-later" || !mm.consumedInSet("w-later") {
		t.Fatalf("the poll did not consume w-later: %s", p)
	}
	minute := map[string]models.ClockOffset{"dirty_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	if mm.LearnElsewhere(ms["w-later"], minute) {
		t.Fatal("a call in another window taught an idle answer a call of the set consumed")
	}
	if !mm.LearnElsewhere(ms["w-boot"], minute) {
		t.Fatal("an idle answer no call consumed was taught nothing")
	}
	if got := mm.LearnedElsewhere(); len(got) != 1 || got[0].Name != "w-boot" {
		t.Fatalf("learned elsewhere %v; want w-boot alone", learnedNames(got))
	}
	// Between tests (the pool's view) too: the check needs no test open.
	mm.ServeExactPool("", nil)
	if mm.LearnElsewhere(ms["w-later"], minute) {
		t.Fatal("a call between tests taught an idle answer a call of the set consumed")
	}
	if got := mm.LearnedElsewhere(); len(got) != 1 || got[0].Name != "w-boot" {
		t.Fatalf("learned elsewhere %v between tests; want w-boot alone", learnedNames(got))
	}
}

// What a call made in another window teaches a recording is that window's:
// each state names the test open when the call came, with the instant it
// taught, and "" for a call between tests. A call whose round moved since
// it was read (a test's begin or end between the read and its learning)
// names none: keploy no longer knows its window, so whatever undoes a
// window's learning reads it as any window's.
func TestLearnElsewhereNamesTheWindowThatTaughtIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ms := map[string]*models.Mock{"own": httpCall("own", 0, "/own")}
	all := []*models.Mock{ms["own"]}
	for i, name := range []string{"o1", "o2", "o3", "o4"} {
		ms[name] = httpCall(name, time.Duration(i+1)*time.Second, "/"+name)
		all = append(all, ms[name])
	}
	mm := stagedManager(t, nil, all)
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
	mm.SetPairing(true)
	minute := map[string]models.ClockOffset{"claimed_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	windowOf := func(name string) []models.TeachingWindow {
		t.Helper()
		for _, st := range mm.LearnedElsewhere() {
			if st.Name == name {
				return st.TaughtIn
			}
		}
		t.Fatalf("learned elsewhere %v; want %s among them", learnedNames(mm.LearnedElsewhere()), name)
		return nil
	}

	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if !mm.LearnElsewhere(ms["o1"], minute) {
		t.Fatal("a call in T1's window taught o1 nothing")
	}
	if got := windowOf("o1"); len(got) != 1 || got[0].Test != "T1" || got[0].At.IsZero() {
		t.Fatalf("taught in %+v; want T1's window, with its instant", got)
	}

	mm.ServeExactPool("", nil)
	if !mm.LearnElsewhere(ms["o2"], minute) {
		t.Fatal("a call between tests taught o2 nothing")
	}
	if got := windowOf("o2"); len(got) != 1 || got[0].Test != "" {
		t.Fatalf("taught in %+v; want between tests", got)
	}

	// A call read in T3's window whose learning lands after T4 began.
	mm.ServeExactTest("T3", nil)
	call := mm.MarkCallRound()
	held, _ := mm.HoldToRound(call)
	mm.ServeExactTest("T4", nil)
	if !held.(integrations.LearnElsewhere).LearnElsewhere(ms["o3"], minute) {
		t.Fatal("a call whose round moved taught o3 nothing")
	}
	if got := windowOf("o3"); len(got) != 1 || got[0].Test != "" {
		t.Fatalf("taught in %+v; a call whose round moved since its read must name no window", got)
	}
	// Read and learned in one window, it names it.
	call = mm.MarkCallRound()
	held, _ = mm.HoldToRound(call)
	if !held.(integrations.LearnElsewhere).LearnElsewhere(ms["o4"], minute) {
		t.Fatal("a call in T4's window taught o4 nothing")
	}
	if got := windowOf("o4"); len(got) != 1 || got[0].Test != "T4" {
		t.Fatalf("taught in %+v; want T4's window", got)
	}
}

// The set's count of learning-only pairings is said by operation, at info,
// when the round's learning is read: each operation's calls and what came
// of them, the set's whole. A read with nothing counted since says nothing
// again; a later count is said whole at the next read. The next set's
// staging says what no read said, and starts its own count.
func TestLearnElsewhereCountIsSaidByOperationWhenTheRoundsLearningIsRead(t *testing.T) {
	mm, _ := s12Set(t)
	core, logs := observer.New(zap.InfoLevel)
	mm.logger = zap.New(core)
	said := func() []string {
		var out []string
		for _, e := range logs.TakeAll() {
			if e.Message != elsewhereCountLine {
				continue
			}
			c := e.ContextMap()
			out = append(out, fmt.Sprint(c["kind"], " | ", c["operation"], " | ", c["calls"], " | ", c["outcomes"]))
		}
		return out
	}
	const tick, scan = "findAndModify ptw_endpoints in keploy", "find loadTestRun in keploy"
	mm.LearnElsewhereCounted(models.Mongo, tick, "learned its clock reads")
	mm.LearnElsewhereCounted(models.Mongo, tick, "one call more than recorded")
	mm.LearnElsewhereCounted(models.Mongo, tick, "learned its clock reads")
	mm.LearnElsewhereCounted(models.Mongo, scan, "changed beyond a read of the clock")
	mm.LearnedElsewhere()
	want := []string{
		"Mongo | " + scan + " | 1 | [changed beyond a read of the clock: 1]",
		"Mongo | " + tick + " | 3 | [learned its clock reads: 2 one call more than recorded: 1]",
	}
	if got := said(); !slices.Equal(got, want) {
		t.Fatalf("said %q; want %q", got, want)
	}
	mm.LearnedElsewhere()
	if got := said(); len(got) != 0 {
		t.Fatalf("a read with nothing counted since said %q again", got)
	}
	mm.LearnElsewhereCounted(models.Mongo, scan, "changed beyond a read of the clock")
	mm.LearnedElsewhere()
	want[0] = "Mongo | " + scan + " | 2 | [changed beyond a read of the clock: 2]"
	if got := said(); !slices.Equal(got, want) {
		t.Fatalf("after a later count, said %q; want the set's whole %q", got, want)
	}
	mm.LearnElsewhereCounted(models.Mongo, scan, "read no clock")
	mm.dropPreviousSetConsumes()
	want[0] = "Mongo | " + scan + " | 3 | [changed beyond a read of the clock: 2 read no clock: 1]"
	if got := said(); !slices.Equal(got, want) {
		t.Fatalf("the next set's staging said %q; want what no read said, %q", got, want)
	}
	mm.LearnedElsewhere()
	if got := said(); len(got) != 0 || mm.elsewhereCounts != nil {
		t.Fatalf("a new set read %q, counting %v; want nothing of the previous set's", got, mm.elsewhereCounts)
	}
}
