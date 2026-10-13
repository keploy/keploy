package mock

import (
	"context"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
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
// the recording the record commits carries it, and says per test what was
// learned (design §7.2 step 10).
func (m *mockService) persistLearned(ctx context.Context, set string, consumed []models.MockState, flows []FlowMocks) {
	p, ok := m.mockDB.(learnedPersister)
	if !ok {
		m.logger.Warn("keploy's mock store cannot write what the learn step learned; it is not kept", zap.String("set", set))
		return
	}
	states := make(map[string]models.MockState, len(consumed))
	for _, st := range consumed {
		states[st.Name] = st
	}
	added, err := p.PersistLearned(ctx, set, states)
	if err != nil {
		utils.LogError(m.logger, err, "failed to write what the learn step learned onto the mocks; it is not kept", zap.String("set", set))
		return
	}
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
}
