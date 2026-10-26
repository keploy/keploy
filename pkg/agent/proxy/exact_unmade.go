package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// UnmadeWriteBefore implements integrations.UnmadeWriteReader: the first of
// the open test's own recordings of mk's kind that has not answered, that
// isWrite says is a write, and that was recorded before mk. For one of the
// test's own recordings, this view's copy, that has not answered, "before"
// is filed before it (recorded order). For any other recording (another
// test's, the background's, the start-up window's, one the test shares with
// the tests beside it at record: exactView.cand), "before" is a request
// time before mk's: every answer step is held to the writes of the open
// test the replay has not made, whoever recorded the answer. The writes are
// the test's own only: a shared recording is required of no test, and none
// is a write the test must make. A copy of one of the test's own that an
// earlier view served says none: its claim is refused, and the parser reads
// this view's copy again.
func (m *MockManager) UnmadeWriteBefore(mk *models.Mock, isWrite func(*models.Mock) bool) (*models.Mock, bool) {
	v := m.exactView()
	if v == nil || v.test == "" || mk == nil || isWrite == nil {
		return nil, false
	}
	i, own := v.index[mk.Name]
	own = own && !v.isCand(i)
	if own && mk.TestModeInfo.ServedIn != v.gen {
		return nil, false
	}
	at := mk.Spec.ReqTimestampMock
	v.mu.Lock()
	if own && v.consumed[i] {
		v.mu.Unlock()
		return nil, false
	}
	var unmade []*models.Mock
	for j, w := range v.owned {
		if own && j >= i {
			break
		}
		if v.isCand(j) || v.consumed[j] || w.Kind != mk.Kind || (!own && !w.Spec.ReqTimestampMock.Before(at)) {
			continue
		}
		unmade = append(unmade, w)
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
