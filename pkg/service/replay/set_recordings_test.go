package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A call outside every test whose matcher found no recording to compare is
// told what its WHOLE set recorded of its shape, not what that matcher's pool
// held: the pool is one test's window and the session's, so an application's
// timer recorded only as it fired inside test-3 found none there when it
// fired between test-1 and test-2 on replay. It was told "no recording of
// this call in the set" with a step that records the set again whatever the
// application meant; the set has its recording, so the step holds only if the
// application changed the call on purpose. A call the set has truly no
// recording of is still told so, and one of a protocol keploy reads no shape
// of from a recording is told keploy cannot tell, never that there is none.
func TestRunTestSet_ACallOutsideEveryTestIsToldWhatItsWholeSetRecorded(t *testing.T) {
	tick := &models.Mock{Name: "mock-tick", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://clock:80/api/tick"}}}
	for _, c := range []struct {
		name     string
		call     models.UnmatchedCall
		recorded string
		facts    string
		ifMeant  bool
	}{
		{"recorded in another test's window", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tick", Destination: "clock:80"},
			models.RecordedShape, "the set has recordings of this call's shape, and it matches none of them", true},
		{"recorded nowhere in the set", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tock", Destination: "clock:80"},
			models.RecordedNone, "no recording of this call in the set", false},
		{"a protocol keploy reads no shape of", models.UnmatchedCall{Protocol: "Redis", Command: "GET", Destination: "cache:6379"},
			"", "no recording came close to this call, and keploy cannot tell whether the set has one of its shape", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 3, 0)
			h.mocks.filtered = []*models.Mock{tick}
			logs := observeSetLogs(h)
			call := c.call
			call.Kind, call.Between, call.MatchPhase = models.MissOutsideTest, true, models.MatchPhaseNoMocks
			h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) { return []models.UnmatchedCall{call}, nil })

			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("status %s; a call outside every test that no mock answered fails the set", status)
			}
			lines := outsideLines(logs)
			if len(lines) != 1 {
				t.Fatalf("%d outside_every_test lines; want one: %+v", len(lines), logs.All())
			}
			f := lines[0].ContextMap()
			next := "record the test set again in its place: " + recordAgainOf(h, "test-set-0")
			if c.ifMeant {
				next = "if the application changed this call on purpose, " + next
			}
			for k, want := range map[string]string{"recorded": c.recorded, "facts": c.facts, "next_step": next, "closest": ""} {
				if got := f[k]; got != want {
					t.Errorf("line field %s = %v; want %q", k, got, want)
				}
			}
			if got := h.report.report.UnclaimedCalls; len(got) != 1 || got[0].SetRecorded != c.recorded {
				t.Errorf("the report's call %+v; want it to say the set read %q", got, c.recorded)
			}
		})
	}
}

// setWalk is a mock store that walks the set's recordings (recorded), as a
// read of its mock file does, counting its walks; err fails each walk once it
// has visited them.
type setWalk struct {
	*prMockDB
	recorded func() []*models.Mock
	walks    *int
	err      error
}

func (w setWalk) WalkTestSetMocks(_ context.Context, _ string, visit func(m *models.Mock, perTest, session bool)) error {
	*w.walks++
	for _, m := range w.recorded() {
		visit(m, true, false)
	}
	return w.err
}

func setRecordingsReplayer(hooks TestHooks, walk setWalk) *Replayer {
	return &Replayer{logger: zap.NewNop(), mockDB: walk, hookImpl: hooks}
}

func tickRecording(url string) func() []*models.Mock {
	return func() []*models.Mock {
		return []*models.Mock{{Name: "mock-tick", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: url}}}}
	}
}

// The set's recordings are read once, and only for a call (each fails the run
// outside every test) that names no closest recording and whose phase
// compared none of its shape; every other call is left as it came, and a read
// that fails sets nothing. A call an earlier set made in its test, decided
// after that set ended, is read as one between tests is: it fails the run
// outside every test of the set that read it.
func TestWithSetRecordingsReadsOnlyForACallThatNeedsIt(t *testing.T) {
	tick := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tick", MatchPhase: models.MatchPhaseNoMocks}
	closest := tick
	closest.Between, closest.ClosestMock = true, "mock-tick"
	compared := tick
	compared.Between, compared.MatchPhase = true, models.MatchPhaseBody
	between := tick
	between.Between = true
	late := tick
	late.Late = true
	earlierSets := tick
	earlierSets.EarlierSet, earlierSets.EarlierSetTest = 1, "test-2"
	strict := between
	strict.MatchPhase = models.MatchPhaseStrict

	walks := 0
	r := setRecordingsReplayer(prHooks{}, setWalk{prMockDB: &prMockDB{}, recorded: tickRecording("http://clock:80/api/tick"), walks: &walks})
	got := r.withSetRecordings(context.Background(), "test-set-0", []models.UnmatchedCall{closest, compared, between, late, earlierSets, strict})
	for i, want := range []string{"", "", models.RecordedShape, models.RecordedShape, models.RecordedShape, ""} {
		if got[i].SetRecorded != want {
			t.Errorf("call %d: SetRecorded %q; want %q", i, got[i].SetRecorded, want)
		}
	}
	if walks != 1 {
		t.Errorf("the set was read %d times; want once", walks)
	}

	walks = 0
	if got := r.withSetRecordings(context.Background(), "test-set-0", []models.UnmatchedCall{closest, compared, strict}); walks != 0 || got[0].SetRecorded != "" {
		t.Errorf("the set was read %d times for calls none of which needs it", walks)
	}

	walks = 0
	failing := setRecordingsReplayer(prHooks{}, setWalk{prMockDB: &prMockDB{}, recorded: tickRecording("http://clock:80/api/tick"), walks: &walks, err: errPRMockReads})
	if got := failing.withSetRecordings(context.Background(), "test-set-0", []models.UnmatchedCall{between}); got[0].SetRecorded != "" {
		t.Errorf("a read that failed set %q; it sets nothing", got[0].SetRecorded)
	}
}

// setsWalk is a mock store that walks the recordings of the set it is asked
// for, and notes which sets it was asked for.
type setsWalk struct {
	*prMockDB
	recorded map[string][]*models.Mock
	asked    *[]string
}

func (w setsWalk) WalkTestSetMocks(_ context.Context, set string, visit func(m *models.Mock, perTest, session bool)) error {
	*w.asked = append(*w.asked, set)
	for _, m := range w.recorded[set] {
		visit(m, true, false)
	}
	return nil
}

// A call an earlier set made is told what THAT set recorded of its shape, read
// from that set's every recording, not the reading set's: the timer recorded
// in test-set-a, made between its tests and read in test-set-b, which has no
// recording of it, is told the set it was made in has its shape.
func TestAnEarlierSetsCallIsToldWhatThatSetRecorded(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	var asked []string
	r := &Replayer{logger: zap.New(core), mockDB: setsWalk{prMockDB: &prMockDB{}, asked: &asked,
		recorded: map[string][]*models.Mock{"test-set-a": tickRecording("http://clock:80/api/tick")()}}}
	for _, set := range []string{"test-set-a", "test-set-b"} {
		r.earlierSetCalls.begin(set)
	}
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tick", Kind: models.MissOutsideTest, EarlierSet: 1, MatchPhase: models.MatchPhaseNoMocks}
	if rest := r.takeEarlierSetCalls([]models.UnmatchedCall{call}); len(rest) != 0 {
		t.Fatalf("the read keeps %+v; the call is test-set-a's", rest)
	}
	if len(asked) != 1 || asked[0] != "test-set-a" {
		t.Fatalf("the sets read for the call: %v; want test-set-a alone", asked)
	}
	said := logs.FilterField(zap.String("kind", models.KindOutsideEveryTest)).FilterField(zap.String("test-set", "test-set-a")).All()
	if len(said) != 1 {
		t.Fatalf("the call was said %d times; want once: %+v", len(said), logs.All())
	}
	if got := said[0].ContextMap()["recorded"]; got != models.RecordedShape {
		t.Errorf("the line says recorded=%v; want %q, what test-set-a recorded", got, models.RecordedShape)
	}
}
