package mock

import (
	"context"
	"errors"
	"fmt"
	"sort"

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

// learnedPersister writes what the learn step learned onto a set's mocks and
// says what it added, by mock (the yaml mock store, PersistLearned).
type learnedPersister interface {
	PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error)
}

// persistLearned writes what the learn step learned onto the set's mocks, so
// the recording the record commits carries it, says per test what was
// learned (design §7.2 step 10), and returns it by test, each field as
// "<mock> <path> (<kind>)". With config.Test.LearnTests set it writes only
// onto the mocks of those tests (and onto the pool's with LearnPool): a test
// the record did not record again keeps its recording as it is. expected is
// each test's own mocks, by its mapping.
func (m *mockService) persistLearned(ctx context.Context, set string, consumed []models.MockState, flows []FlowMocks, expected map[string][]models.MockEntry) (map[string][]string, error) {
	p, ok := m.mockDB.(learnedPersister)
	if !ok {
		return nil, errors.New("keploy's mock store cannot write what the learn step learned")
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
		states[st.Name] = st
	}
	added, err := p.PersistLearned(ctx, set, states)
	if err != nil {
		return nil, fmt.Errorf("writing what the learn step learned onto keploy/%s: %w", set, err)
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
	return byTest, nil
}

// learnsOnto reports whether what the learn step learned is kept on a mock
// whose test (by mapping) is test, "" for the pool's.
func (m *mockService) learnsOnto(test string) bool {
	switch {
	case m.config.Test.LearnTests == nil:
		return true
	case test == "":
		return m.config.Test.LearnPool
	}
	return m.config.Test.LearnTests[test]
}
