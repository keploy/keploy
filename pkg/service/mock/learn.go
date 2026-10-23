package mock

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learning reports whether this replay runs the learn step (design v14 §7.2,
// row L; package matcher/learn): `keploy mock record`'s full round, which the
// record marks in memory (config.Test.Learn), under the proven switch.
func (m *mockService) learning() bool {
	return provenswitch.On() && m.config.Test.Learn
}

// learningPool reports whether the learn step may pair pool mocks too: only
// a full record, which replaces the pool (config.Test.LearnPool).
func (m *mockService) learningPool() bool {
	return m.learning() && m.config.Test.LearnPool
}

// keepLearned writes what record's full round learned onto the set's mocks
// (persistLearned), where the agent said its learn step reads the session's
// tests as client tests (clientTestsRead). Otherwise it keeps none of it, and
// says so: an agent older than this keploy learns a value the test harness
// gave the application as noise where it changed, and the recording keeps
// comparing every field. consumedErr is why the agent's consumed mocks could
// not be read: then what was learned cannot be read either.
func (m *mockService) keepLearned(ctx context.Context, set string, consumed []models.MockState, consumedErr error, flows []FlowMocks, expected map[string][]models.MockEntry) (map[string][]string, []string, error) {
	if consumedErr != nil {
		return nil, nil, fmt.Errorf("what the learn step learned could not be read from keploy's agent: %w", consumedErr)
	}
	if !m.clientTestsRead() {
		m.logger.Warn("keploy kept nothing its agent learned in record's full round: the agent did not say it reads keploy mock's tests as a test harness's client tests, as an agent older than this keploy does not, so a value the harness gave the application may have been learned as noise; the recording keeps comparing every field",
			zap.String("mock-set", set),
			zap.String("next_step", "run the keploy agent of this keploy version"))
		return nil, nil, nil
	}
	return m.persistLearned(ctx, set, consumed, flows, expected)
}

// clientTestsRead reports whether the agent said, answering the session's
// MockOutgoing, that its learn step reads the session's tests as client
// tests (pkg.ClientTestsReader, models.ClientTestsReadHeader). An
// instrumentation that cannot say read none so.
func (m *mockService) clientTestsRead() bool {
	r, ok := m.instrumentation.(pkg.ClientTestsReader)
	return ok && r.AgentReadsClientTests()
}

// learnedPersister writes what the learn step learned onto a set's mocks and
// says what it added, by mock (the yaml mock store, PersistLearned).
type learnedPersister interface {
	PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error)
}

// persistLearned writes what the learn step learned onto the set's mocks, so
// the recording the record commits carries it, says per test what was
// learned (design §7.2 step 10), and returns it by test, each field as
// "<mock> <what mockdb's PersistLearned added>", and what it wrote onto mocks
// no test consumed (the set's start-up and pool), sorted. With config.Test.LearnTests set it writes only
// onto the mocks of those tests (and onto the pool's with LearnPool): a test
// the record did not record again keeps its recording as it is. expected is
// each test's own mocks, by its mapping.
func (m *mockService) persistLearned(ctx context.Context, set string, consumed []models.MockState, flows []FlowMocks, expected map[string][]models.MockEntry) (map[string][]string, []string, error) {
	p, ok := m.mockDB.(learnedPersister)
	if !ok {
		return nil, nil, errors.New("keploy's mock store cannot write what the learn step learned")
	}
	owner := map[string]string{}
	for test, entries := range expected {
		for _, e := range entries {
			owner[e.Name] = test
		}
	}
	states := make(map[string]models.MockState, len(consumed))
	for _, st := range consumed {
		if !m.learnsOnto(owner[st.Name]) {
			continue
		}
		// A variable is bound at an earlier call of the same test, which the
		// record made too; one bound at a recording it did not make cannot
		// be written whole.
		for target := range st.Binds {
			if !m.learnsOnto(owner[target]) {
				return nil, nil, fmt.Errorf("the learn step learned a variable of %s bound at %s, a recording this record did not make", st.Name, target)
			}
		}
		states[st.Name] = st
	}
	added, err := p.PersistLearned(ctx, set, states)
	if err != nil {
		return nil, nil, fmt.Errorf("writing what the learn step learned onto keploy/%s: %w", set, err)
	}
	byTest := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range flows {
		var fields []string
		for _, st := range f.Consumed {
			for _, a := range added[st.Name] {
				fields = append(fields, st.Name+" "+a)
			}
			if len(added[st.Name]) > 0 {
				seen[st.Name] = true
			}
		}
		if len(fields) > 0 {
			byTest[f.Flow] = fields
			m.logger.Info("learned fields that changed between record and replay", zap.String("test", f.Flow), zap.Strings("fields", fields))
		}
	}
	var rest []string
	for name, as := range added {
		if seen[name] {
			continue
		}
		for _, a := range as {
			rest = append(rest, name+" "+a)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		m.logger.Info("learned fields that changed between record and replay, on mocks no test consumed", zap.Strings("fields", rest))
	}
	return byTest, rest, nil
}

// learnsOnto reports whether what the learn step learned is kept on a mock
// whose test (by mapping) is test, "" for the pool's. A test's mapping is
// by its top-level ID (a Go subtest's mocks map to its test), and an ID is
// matched whole: a "/" in it is no subtest separator in every framework (a
// test named after a route).
func (m *mockService) learnsOnto(test string) bool {
	switch {
	case m.config.Test.LearnTests == nil:
		return true
	case test == "":
		return m.config.Test.LearnPool
	}
	return m.config.Test.LearnTests[test]
}
