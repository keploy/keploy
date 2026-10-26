package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// UnmadeWriteBefore implements integrations.UnmadeWriteReader: when mk is
// one of the open test's own recordings, this view's copy, that has not
// answered, the first of the test's own recordings of mk's kind filed before
// it (recorded order) that has not answered either and that isWrite says is
// a write. A recording the test shares with the tests beside it at record
// (exactView.cand) is none of the test's own: it is required of no test, and
// one chosen keeps its choice. The view files those after all of the test's
// own, so none is ever filed before one of them.
func (m *MockManager) UnmadeWriteBefore(mk *models.Mock, isWrite func(*models.Mock) bool) (*models.Mock, bool) {
	v := m.exactView()
	if v == nil || v.test == "" || mk == nil || isWrite == nil {
		return nil, false
	}
	i, ok := v.index[mk.Name]
	if !ok || v.isCand(i) || mk.TestModeInfo.ServedIn != v.gen {
		return nil, false
	}
	v.mu.Lock()
	if v.consumed[i] {
		v.mu.Unlock()
		return nil, false
	}
	var unmade []*models.Mock
	for j := 0; j < i; j++ {
		if !v.consumed[j] && v.owned[j].Kind == mk.Kind {
			unmade = append(unmade, v.owned[j])
		}
	}
	v.mu.Unlock()
	for _, w := range unmade {
		if isWrite(w) {
			return w, true
		}
	}
	return nil, false
}

var _ integrations.UnmadeWriteReader = (*MockManager)(nil)

// UnmadeWriteBefore is the manager's (integrations.UnmadeWriteReader) while
// the staging is still h's call's: a write of another staging's test says
// nothing of what this call's test has made.
func (h *heldScope) UnmadeWriteBefore(mk *models.Mock, isWrite func(*models.Mock) bool) (*models.Mock, bool) {
	w, ok := h.MockManager.UnmadeWriteBefore(mk, isWrite)
	if h.moved() {
		return nil, false
	}
	return w, ok
}

// UnmadeWriteBefore forwards the store's unmade write before mk
// (integrations.UnmadeWriteReader), none when it keeps no such reader, so a
// parser on a scoped view answers a read before a write the replay has not
// made as it would on the bare manager.
func (s *scopedMockDb) UnmadeWriteBefore(mk *models.Mock, isWrite func(*models.Mock) bool) (*models.Mock, bool) {
	if ur, ok := s.MockMemDb.(integrations.UnmadeWriteReader); ok {
		return ur.UnmadeWriteBefore(mk, isWrite)
	}
	return nil, false
}

var _ integrations.UnmadeWriteReader = (*scopedMockDb)(nil)
