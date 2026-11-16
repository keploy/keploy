package replay

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/localfile"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A call a test set's application makes outside every test (as it starts,
// before the set's first test, or after its last) that no mock answers is
// answered by keploy itself: a DNS lookup gets keploy's synthetic answer, a
// connection is refused or closed. A call outside every test is served, not
// required: it fails nothing that passed without it (with the proven switch
// on, a call that counts against a test still fails the set:
// before_set_calls.go, unclaimed_calls.go). It was named nowhere: the agent
// dropped those made before the set's first test with the switch off, and a
// DNS lookup's with it on, and with the switch off no one read those made
// after the set's last test (readAfterLastTest), so a start-up lookup made
// only in a replay (a configuration that
// differs from the recording's) could be neither seen nor corrected. The
// set's report names each now (TestReport.OutsideTestMisses), by its shape,
// the log says each with what to do, and the set's keploy test misses file
// (localfile.TestMissesFile, local and git-ignored) names each whole. It is
// keploy test's own: keploy mock replay's (localfile.MissesFile) is never
// written or removed by a keploy test run.

// readAfterLastTest is, with the proven switch off, the set's end read of the
// calls its application made after its last test that no mock answered
// (unclaimedCalls, which makes it under the switch, marked as the set's end
// read): with the switch off none of them fails the set, so they are read
// for its report alone, which names each (outsideTestMisses), as made after
// its last test. They were read by no one: cut at the next set's staging,
// said only by their shape in a line that keeps no record of them, or, after
// the run's last set, never read. The application is still running then: a
// call it makes after this read is cut at the next set's staging, as before.
// A read that cannot vouch for every such call (one cut short by the run's
// stop included) is said at WARN; the set's verdict is what it would be
// without the read, whatever it returns.
func (r *Replayer) readAfterLastTest(ctx context.Context, testSetID string) {
	if _, _, err := r.unclaimedCalls(ctx); err != nil {
		r.logger.Warn("keploy could not read every call the application made after the test-set's last test that no mock answered: its report may not name each (outsideTestMisses); they fail nothing, so the test-set's verdict stands",
			zap.String("test-set", testSetID), zap.Error(err))
	}
}

// outsideLogMax is how many outside-test misses a set's log names, one line
// each; the report names every one.
const outsideLogMax = 20

// outsideWriter is keploy test, as a set's misses file names it.
var outsideWriter = localfile.Writer{
	File:      localfile.TestMissesFile,
	Command:   "keploy test",
	Holds:     "each call the application made outside every test in the last run of this set that no recorded mock answered and that failed nothing",
	Unwritten: "could not write the calls the application made outside every test whole; the log names each by its shape",
}

// outsideTestMisses is the set's outside-test misses
// (TestReport.OutsideTestMisses), as its report names them, by their shape.
// It writes them whole, each with the same next step, in the set's misses
// file (localfile.WriteMisses: in place of the last run's, and only where it
// is git-ignored; a set with none removes the last run's), and says each in
// the log (logOutsideTestMisses), with the file's path.
func (r *Replayer) outsideTestMisses(testSetID string) []models.OutsideTestMiss {
	calls := r.setCalls.outside(provenswitch.On())
	whole := make([]models.UnmatchedCall, 0, len(calls))
	for _, c := range calls {
		c.NextSteps = models.OutsideTestNextStep(c)
		if c.Whole != nil {
			w := *c.Whole
			w.NextStep = c.NextSteps
			c.Whole = &w
		}
		whole = append(whole, c)
	}
	file := localfile.WriteMisses(r.logger, r.config.Path, testSetID, outsideWriter, whole)
	misses := models.OutsideTestMissesOf(calls)
	r.logOutsideTestMisses(testSetID, misses, file)
	return misses
}

// logOutsideTestMisses says, at WARN, each call of misses, the set's
// outside-test misses (TestReport.OutsideTestMisses), by its shape beside its
// tags, as the report names it, with how many times it was made and its next
// step: up to outsideLogMax lines, then how many more the report names. file
// is the set's misses file, which names each whole: a last line points to
// it ("" when none was written).
func (r *Replayer) logOutsideTestMisses(testSetID string, misses []models.OutsideTestMiss, file string) {
	for i, m := range misses {
		if i == outsideLogMax {
			r.logger.Warn("the application made more calls outside every test that no mock answered; the test-set's report names each (outsideTestMisses)",
				zap.String("test-set", testSetID), zap.Int("more", len(misses)-outsideLogMax))
			break
		}
		r.logger.Warn("the application made a call outside every test that no mock answered; it fails nothing (a call outside every test is served, not required), and the test-set's report names it (outsideTestMisses)",
			zap.String("test-set", testSetID),
			zap.String("protocol", m.Protocol),
			zap.String("call", m.Call),
			utils.PathTagField(m.PathTag),
			zap.String("destination", m.Destination),
			zap.String("when", m.When),
			zap.Int("count", m.Count),
			zap.String("next_step", m.NextStep))
	}
	if file != "" {
		r.logger.Info("the test-set's misses file names each call the application made outside every test whole, beside its path tag; it is local and git-ignored",
			zap.String("test-set", testSetID), zap.String("misses_file", file))
	}
}
