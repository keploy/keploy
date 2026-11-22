package mock

import "go.keploy.io/server/v3/pkg/models"

// consumedReads is what the agent consumed over a run, as every read of it
// the run made reads it, one entry per recording: the reads of the wait for
// the series the run owes (readOwedSeries), then the run's end's read
// (reportOutcome), or the account a docker compose agent left as it stopped
// (ComposeOutcomeReader). A read hands back only what was served since the
// read before (the agent's GetConsumedMocks drains it), so a recording served
// on both sides of a read is in both reads; it is still one recording
// consumed. Every consumer of the run's consumed reads this one set: the
// receipt's count and the metered one (ReplayOutcome.Consumed), each test's
// line, and what the learn step learned.
//
// The entry is the one the agent keeps of a recording served twice within a
// read (its MockManager's flagMockAsUsed): at the place and with the first use
// (Timestamp) of its first read, so it is put under the test it first
// answered in, and its latest state otherwise. That state carries what every
// earlier pairing of the session learned on it (the agent merges each into
// its never-drained consumed of the session), so what the learn step learned
// on it is what the latest read says, as a read of the run's whole consumed
// would say. It learned after the learn step ended when its latest read says
// so: within one read, any of its consumes that did is not hidden by a later
// one that did not, and an earlier read's is, as the agent ORs it within one
// drain only (flagMockAsUsed: another drain's consumers were not served
// through it), so the two keep one rule.
type consumedReads struct {
	states []models.MockState
	at     map[string]int
}

// add adds what one read handed back.
func (c *consumedReads) add(read []models.MockState) {
	inRead := make(map[string]bool, len(read))
	for _, st := range read {
		i, seen := c.at[st.Name]
		if !seen {
			if c.at == nil {
				c.at = make(map[string]int)
			}
			c.at[st.Name] = len(c.states)
			c.states = append(c.states, st)
			inRead[st.Name] = true
			continue
		}
		first := c.states[i]
		st.Timestamp = first.Timestamp
		if inRead[st.Name] {
			st.LearnedAfterEnd = st.LearnedAfterEnd || first.LearnedAfterEnd
		}
		inRead[st.Name] = true
		c.states[i] = st
	}
}

// list is the run's consumed, each recording once, in the order its reads
// first listed them: nil when no read handed back any.
func (c *consumedReads) list() []models.MockState {
	return c.states
}
