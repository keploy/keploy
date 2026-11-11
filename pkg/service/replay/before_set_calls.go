package replay

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The calls a test set's application made before its first test that no mock
// answered (F2, under the proven switch). A call made while no test is open is
// a pool call (design v14 §5). Before the set's first test the application is
// starting, and none of its calls is a test's: the agent dropped the misses
// made then with a warning, so a set whose application made, as it started, a
// call no mock answers (a write it makes at boot that changed since the
// recording) passed. The agent now hands them to the set's first test's read
// marked as the set's (models.UnmatchedCall.BeforeSet, an outside_test miss);
// the replay keeps them out of that test's verdict and fails the set with
// them at its end, naming each, as it does a call made after the set's last
// test (unclaimed_calls.go).
//
// A call a parser held past its own window for a later test's recording that
// none answered (models.UnmatchedCall.Held) is the set's too: its miss is
// filed at the set's end read, or at its connection's end, which may fall in
// any test's window. Whichever read takes it, it fails the set, named as
// held, and charges no test: the test open when its connection ended did not
// make it.
//
// A call the application made between two tests of the set, while no test's
// window was open and no test's view served (models.UnmatchedCall.Between),
// is the set's too: the next test's read carries it, and it was that test's
// failure, though the test framework opens each test and that test did not
// make it. It fails the set, outside every test, named with the tests it was
// made between, and charges no test. So is a call the application made in a
// test that keploy decided only after that test's end was read
// (models.UnmatchedCall.Late): a later test's read takes it, though that test
// did not make it. It fails the set, outside every test, named under the test
// it was made in (MadeIn), and charges no test. A miss of the set's background
// (models.UnmatchedCall.Background: the same request as a background
// recording) counts against no set (dataPlaneCalls).

// setCalls are the calls of the set RunTestSet is running that no mock
// answered and that are the set's, no test's (setsCall): a read hands them
// over with a test's own, and they are kept here, out of that test's verdict,
// for the set's end to fail the set with. Reset as each set begins.
type setCalls struct {
	mu    sync.Mutex
	calls []models.UnmatchedCall
	// last is the test whose end was read last in the set (takeOf; "" after
	// the set's end read): the test a call made between two tests was made
	// after.
	last string
}

// setsCall reports whether c is the set's, no test's: made before the set's
// first test (models.UnmatchedCall.BeforeSet), held by a parser for a later
// test's recording that none answered (models.UnmatchedCall.Held), or made
// between two tests (models.UnmatchedCall.Between), or made in an earlier
// test and decided after its end was read (models.UnmatchedCall.Late).
func setsCall(c models.UnmatchedCall) bool { return c.BeforeSet || c.Held || c.Between || c.Late }

// reset forgets the previous set's calls.
func (s *setCalls) reset() {
	s.mu.Lock()
	s.calls = nil
	s.last = ""
	s.mu.Unlock()
}

// take keeps the calls of read, a read that is no test's end (the set's end),
// that are the set's (setsCall), and returns the others.
func (s *setCalls) take(read []models.UnmatchedCall) []models.UnmatchedCall {
	return s.takeOf(read, "")
}

// takeOf keeps the calls of read, the read of test's end ("" for the set's
// end), that are the set's (setsCall), and returns the others: the test's
// own. A call made between two tests is named with the test read before it
// (MadeAfter) and test (MadeBefore); so is a call made in an earlier test
// (Late), and named under the test read before it when no mark named the
// test it was made in (MadeIn).
func (s *setCalls) takeOf(read []models.UnmatchedCall, test string) []models.UnmatchedCall {
	var rest []models.UnmatchedCall
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range read {
		if setsCall(c) {
			if c.Between || c.Late {
				c.MadeAfter, c.MadeBefore = s.last, test
			}
			if c.Late && c.MadeIn == "" {
				c.MadeIn = s.last
			}
			s.calls = append(s.calls, c)
			continue
		}
		rest = append(rest, c)
	}
	s.last = test
	if len(rest) == len(read) {
		return read
	}
	return rest
}

// failing is the set's calls that count against it (dataPlaneCalls), in the
// order they were read.
func (s *setCalls) failing() []models.UnmatchedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return dataPlaneCalls(s.calls)
}

// answers is the set's calls that were answered (models.UnmatchedCall.Answer,
// proven switch), in the order they were read: the set's answers, which fail
// nothing (TestReport.SetAnswers).
func (s *setCalls) answers() []models.UnmatchedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, answers := splitAnswers(s.calls)
	return answers
}

// setCallsWhy says why the set's calls fail it, naming each: those made
// before the set's first test, and those a parser held for a later test's
// recording that none answered (models.UnmatchedCall.Held), wherever they
// were made.
func setCallsWhy(calls []models.UnmatchedCall) string {
	var before, held, between, late []string
	for _, c := range calls {
		switch {
		case c.Held:
			held = append(held, describeUnclaimed(c))
		case c.Between:
			between = append(between, describeUnclaimed(c)+" ("+betweenWhere(c)+")")
		case c.Late:
			late = append(late, describeUnclaimed(c)+" ("+lateWhere(c)+")")
		default:
			before = append(before, describeUnclaimed(c))
		}
	}
	var why []string
	if len(before) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) before the set's first test that no mock answered: %s",
			len(before), strings.Join(before, "; ")))
	}
	if len(between) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) between two tests that no mock answered (%s): %s",
			len(between), models.KindOutsideEveryTest, strings.Join(between, "; ")))
	}
	if len(late) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) in a test that no mock answered and keploy decided only after that test ended, so no test is charged (%s): %s",
			len(late), models.KindOutsideEveryTest, strings.Join(late, "; ")))
	}
	if len(held) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) that keploy held for a later test's recording, which no test of the set answered: %s",
			len(held), strings.Join(held, "; ")))
	}
	return strings.Join(why, "; ")
}

// notOutsideEveryTest is the calls of calls the replay does not fail the set
// with as calls outside every test (models.UnmatchedCall.OutsideEveryTest),
// in order: made before the set's first test, or held for a later test's
// recording.
func notOutsideEveryTest(calls []models.UnmatchedCall) []models.UnmatchedCall {
	var rest []models.UnmatchedCall
	for _, c := range calls {
		if !c.OutsideEveryTest() {
			rest = append(rest, c)
		}
	}
	return rest
}

// joinWhy is why and more, each a reason the set fails for, joined as the
// set's reason joins them.
func joinWhy(why, more string) string {
	if why == "" {
		return more
	}
	return why + "; " + more
}

// failedOutsideOnly reports whether the replay failed a set it ended status,
// with the calls unclaimed charged to the set, for its calls outside every
// test alone (models.TestReport.OutsideOnly): the set is FAILED, one of
// unclaimed is such a call, whyWithout (the set's reason with those calls
// left out) is empty, and without them the set ends as its tests' verdicts
// end it (endsAsTests: those calls turned a PASSED set FAILED, or a test of
// it failed).
func failedOutsideOnly(status models.TestSetStatus, unclaimed []models.UnmatchedCall, endsAsTests bool, whyWithout string) bool {
	return status == models.TestSetStatusFailed && endsAsTests && whyWithout == "" &&
		slices.ContainsFunc(unclaimed, models.UnmatchedCall.OutsideEveryTest)
}

// betweenWhere says between which tests c, a call made Between two, was
// made.
func betweenWhere(c models.UnmatchedCall) string {
	if c.MadeAfter == "" {
		return "made before test " + c.MadeBefore + " began"
	}
	return "made after test " + c.MadeAfter + " ended and before test " + c.MadeBefore + " began"
}

// lateWhere says where c, a call made in a test and decided after its end
// (Late), was made, and which test's read took it.
func lateWhere(c models.UnmatchedCall) string {
	where := "made in a test of the set"
	if c.MadeIn != "" {
		where = "made in test " + c.MadeIn
	}
	where += ", decided after that test ended"
	if c.MadeBefore != "" {
		where += ", read at test " + c.MadeBefore + "'s end"
	}
	return where
}

// withSetRecordings is calls, each a call that fails the run outside every
// test (a set's own, or one an earlier set made), with what their set's
// recordings hold of the shape of each that names no closest recording and
// whose match phase compared no recording of its shape
// (models.UnmatchedCall.SetRecorded). Such a call's matcher read one test's
// window and the session's, less what was spent: a call whose shape the set
// recorded only in another test's window found none there. So the set's
// every recording is read (eachSetMock: each test's and the session's, as
// recorded, one at a time), once, and only when such a call is there. A read
// that fails sets nothing: no call then says the set has none.
func (r *Replayer) withSetRecordings(ctx context.Context, testSetID string, calls []models.UnmatchedCall) []models.UnmatchedCall {
	needs := func(c models.UnmatchedCall) bool {
		return c.ClosestMock == "" && c.ShapeRecordings() != models.RecordedShape
	}
	if !slices.ContainsFunc(calls, needs) {
		return calls
	}
	seen := map[models.RecordedCall]bool{}
	var recorded []models.RecordedCall
	err := r.eachSetMock(ctx, testSetID, true, func(m *models.Mock, _ bool) {
		if m == nil || (m.Kind != models.HTTP && m.Kind != models.Mongo) {
			return
		}
		if rc := models.RecordedCallOf(m); !seen[rc] {
			seen[rc] = true
			recorded = append(recorded, rc)
		}
	})
	if err != nil {
		r.logger.Debug("keploy could not read the test set's recordings, so a call outside every test does not say whether the set has one of its shape",
			zap.String("test-set", testSetID), zap.Error(err))
		return calls
	}
	out := slices.Clone(calls)
	for i := range out {
		if needs(out[i]) {
			out[i].SetRecorded = models.SetShapeRecordings(out[i], recorded)
		}
	}
	return out
}

// betweenFacts is what keploy knows of the recording closest to c, a call
// made Between two tests: its name and the fields c differs from it in, each
// in its shape (models.FieldShapes, never a value); with none named, what
// keploy knows of the set's recordings of its shape
// (UnmatchedCall.ShapeRecordings): that it has some and c matches none, that
// it has none, or, when keploy cannot tell, only that none came close.
func betweenFacts(c models.UnmatchedCall) string {
	differs := models.FieldShapes(c.Differs)
	switch {
	case c.ClosestMock != "" && len(differs) > 0:
		return "the closest recording is " + c.ClosestMock + ", which it differs from in " + strings.Join(differs, ", ")
	case c.ClosestMock != "":
		return "the closest recording is " + c.ClosestMock + ", which it does not match"
	}
	switch c.ShapeRecordings() {
	case models.RecordedShape:
		return "the set has recordings of this call's shape, and it matches none of them"
	case models.RecordedNone:
		return "no recording of this call in the set"
	}
	return "no recording came close to this call, and keploy cannot tell whether the set has one of its shape"
}

// betweenNextStep is the one next step for c, a call outside every test,
// chosen from its facts (betweenFacts): the set is recorded again with no
// condition only when keploy knows it has no recording of c's shape
// (models.RecordedNone); with one that came close, recordings of its shape,
// or a phase that says neither, it is recorded again if the application
// changed the call on purpose. set is the test set whose call it is, ""
// for an earlier set's call in a run that ran no such set, and record the
// command that records set again in its place (recordAgainCommand), with
// where it moves the set's recordings. record has no command when keploy
// cannot print one: for set "", the set that made the call
// is recorded again with the command that recorded it, which keploy cannot
// name; for a set the hooks say is recorded again with no command the user
// runs (SetRecordAgainer), it is their step in words (record.Say); for a set
// whose name keploy record would not record under, set is recorded again
// under its own name with the command that recorded it.
func betweenNextStep(c models.UnmatchedCall, set string, record utils.RecordAgain) string {
	if record.Command == "" && set == "" {
		return "record the earlier test set that made this call again, with the command that recorded it; keploy cannot name that set, and recording this set again does not hold the call"
	}
	again := "record the test set again in its place: " + record.Text()
	switch {
	case record.Command == "" && record.Say != "":
		again = record.Say
	case record.Command == "":
		again = "record the test set " + set + " again under its own name, with the command that recorded it; keploy cannot print a command that records it in its place"
	}
	if c.ClosestMock != "" || c.ShapeRecordings() != models.RecordedNone {
		return "if the application changed this call on purpose, " + again
	}
	return again
}

// recordAgainCommand is how testSetID is recorded again in its place
// (utils.RecordSetAgain): the command moves the set's folder out of the
// run's keploy folder, into the run's own folder of keploy-replaced beside it
// (named by the run's id, so nothing is removed or overwritten), then keploy
// record records under the set's name with the application's command and the
// keploy folder and keploy.yml the run read, each word quoted for a POSIX
// shell. No command when keploy cannot name the set's folder, or when no set
// is running (no run id to name the folder by). For a set the run names, the
// hooks' answer stands in place of that command when they give one
// (SetRecordAgainer): a set kept where the replay runs is recorded again
// there, never by a command for the folder the replay read.
func (r *Replayer) recordAgainCommand(testSetID string) utils.RecordAgain {
	if a, ok := r.hookImpl.(SetRecordAgainer); ok && testSetID != "" {
		if record, says := a.RecordSetAgain(testSetID); says {
			return record
		}
	}
	if r.config == nil {
		return utils.RecordAgain{}
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return utils.RecordSetAgain(r.config.Path, r.config.ConfigPath, cwd, testSetID, r.setRunID, r.config.Command)
}

// logSetCalls says, at ERROR, each of the set's calls that fails it. A call
// made between two tests is said in the documented form of a call outside
// every test (keploy's docs, "outside every test"): kind, the call's shape,
// between which tests, what keploy knows of its closest recording and the
// one next step, each a field of the line. So is a call made in a test and
// decided after its end (Late), with the test it was made in (made_in); a
// held one is said as held. Hooks that say calls outside every test
// themselves (OutsideCallsLogger) are handed those first, all at once, and
// the replay says none of them when the hooks said them.
func (r *Replayer) logSetCalls(testSetID string, calls []models.UnmatchedCall) {
	said := false
	if sayer, ok := r.hookImpl.(OutsideCallsLogger); ok {
		outside := slices.DeleteFunc(slices.Clone(calls), func(c models.UnmatchedCall) bool { return !c.OutsideEveryTest() })
		if len(outside) > 0 {
			said = sayer.LogOutsideCalls(testSetID, outside, r.recordAgainCommand(testSetID))
		}
	}
	for _, c := range calls {
		if c.OutsideEveryTest() && said {
			continue
		}
		if c.OutsideEveryTest() {
			msg := "test-set failed: the application made a call between two tests that no mock answered; it is outside every test, so it fails the set and no test (" + provenswitch.Env + "=1)"
			if !c.Between {
				msg = "test-set failed: the application made a call in a test that no mock answered, and keploy decided it only after that test ended; it is charged to no test, so it fails the set and no test (" + provenswitch.Env + "=1)"
			}
			r.logger.Error(msg, r.outsideFields(testSetID, c.MadeIn, c)...)
			continue
		}
		msg := "test-set failed: the application made a call before its first test that no mock answered (KEPLOY_PROVEN=1)"
		next := "a call the application makes before the set's first test (as it starts, or while it waits for the set) is no test's; it changed or is new since the recording: fix the application, or re-record if the change is intended"
		if c.Held {
			msg = "test-set failed: keploy held a call of the application for a later test's recording, and no test of the set answered it (KEPLOY_PROVEN=1)"
			next = "a call held for a later test (a consumer's poll whose recorded answer came in a later test) found no recording there: the application's calls changed since the recording; fix the application, or re-record if the change is intended"
		}
		r.logger.Error(msg,
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.Named()),
			zap.String("destination", c.Destination),
			zap.String("diff", c.Diff),
			zap.String("next_step", next))
	}
}

// outsideFields is c, a call outside every test, in its documented form
// (keploy's docs, "outside every test"), as the fields of its ERROR line:
// kind, the test set whose call it is (testSetID), the call's shape, the test
// it was made in (madeIn) or between which tests, what keploy knows of its
// closest recording, what its match phase says of the set's recordings of
// its shape (recorded: models.RecordedShape, models.RecordedNone, or "") and
// the one next step.
func (r *Replayer) outsideFields(testSetID, madeIn string, c models.UnmatchedCall) []zap.Field {
	return []zap.Field{
		zap.String("kind", models.KindOutsideEveryTest),
		zap.String("test-set", testSetID),
		zap.String("protocol", c.Protocol),
		zap.String("call", c.Sanitized().ActualSummary),
		zap.String("destination", c.Destination),
		zap.String("made_in", madeIn),
		zap.String("after", c.MadeAfter),
		zap.String("before", c.MadeBefore),
		zap.Time("at", c.At),
		zap.String("closest", c.ClosestMock),
		zap.Strings("differs", models.FieldShapes(c.Differs)),
		zap.String("recorded", c.ShapeRecordings()),
		zap.String("facts", betweenFacts(c)),
		zap.String("next_step", betweenNextStep(c, testSetID, r.recordAgainCommand(testSetID))),
	}
}
