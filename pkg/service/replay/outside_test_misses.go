package replay

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A call a test set's application makes outside every test (as it starts,
// before the set's first test) that no mock answers is answered by keploy
// itself: a DNS lookup gets keploy's synthetic answer, a connection is
// refused or closed. A call outside every test is served, not required: it
// fails nothing that passed without it (with the proven switch on, a call
// that counts against a test still fails the set: before_set_calls.go,
// unclaimed_calls.go). It was named nowhere: the agent dropped those made
// before the set's first test with the switch off, and a DNS lookup's with it
// on, so a start-up lookup made only in a replay (a configuration that
// differs from the recording's) could be neither seen nor corrected. The
// set's report names each now (TestReport.OutsideTestMisses), by its shape,
// and the log says each with what to do.

// outsideLogMax is how many outside-test misses a set's log names, one line
// each; the report names every one.
const outsideLogMax = 20

// logOutsideTestMisses says, at WARN, each call of misses, the set's
// outside-test misses (TestReport.OutsideTestMisses), by its shape beside its
// tags, as the report names it, with how many times it was made and its next
// step: up to outsideLogMax lines, then how many more the report names.
func (r *Replayer) logOutsideTestMisses(testSetID string, misses []models.OutsideTestMiss) {
	for i, m := range misses {
		if i == outsideLogMax {
			r.logger.Warn("the application made more calls outside every test that no mock answered; the test-set's report names each (outsideTestMisses)",
				zap.String("test-set", testSetID), zap.Int("more", len(misses)-outsideLogMax))
			return
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
}
