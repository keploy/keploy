package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// UnmadeWriteBefore implements integrations.UnmadeWriteReader: the first of
// the open test's own recordings of mk's kind that has not answered, that
// gates says is a write that can change what mk reads (the parser's: a
// write elsewhere holds no read), and that was recorded before mk. For one of the
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
func (m *MockManager) UnmadeWriteBefore(mk *models.Mock, gates func(*models.Mock) bool) (*models.Mock, bool) {
	v := m.exactView()
	if v == nil || v.test == "" || mk == nil || gates == nil {
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
		if gates(w) {
			return w, true
		}
	}
	return nil, false
}

var _ integrations.UnmadeWriteReader = (*MockManager)(nil)

// UnmadeWriteBefore is the manager's (integrations.UnmadeWriteReader) while
// the staging is still h's call's: a write of another staging's test says
// nothing of what this call's test has made.
func (h *heldScope) UnmadeWriteBefore(mk *models.Mock, gates func(*models.Mock) bool) (*models.Mock, bool) {
	w, ok := h.MockManager.UnmadeWriteBefore(mk, gates)
	if h.moved() {
		return nil, false
	}
	return w, ok
}

// UnmadeWriteBefore forwards the store's unmade write before mk
// (integrations.UnmadeWriteReader), none when it keeps no such reader, so a
// parser on a scoped view answers a read before a write the replay has not
// made as it would on the bare manager.
func (s *scopedMockDb) UnmadeWriteBefore(mk *models.Mock, gates func(*models.Mock) bool) (*models.Mock, bool) {
	if ur, ok := s.MockMemDb.(integrations.UnmadeWriteReader); ok {
		return ur.UnmadeWriteBefore(mk, gates)
	}
	return nil, false
}

var _ integrations.UnmadeWriteReader = (*scopedMockDb)(nil)

// StandIn implements integrations.OwnRecordingStandIn: mk, the copy of one
// of the open test's own recordings that this view served, is filed as one
// a read answered from before an unmade write stood in for (exactView.stood).
// A copy an earlier view served (a previous attempt's, or a test's that
// ended: the view in force is another), and any other recording (another
// test's, the background's, a shared one), file nothing. One that answered
// already, or that a call takes after, is settled as answered by that call
// (standInsLocked).
func (m *MockManager) StandIn(mk *models.Mock) {
	v := m.exactView()
	if v == nil || mk == nil {
		return
	}
	i, own := v.index[mk.Name]
	if !own || v.isCand(i) || mk.TestModeInfo.ServedIn != v.gen {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stood == nil {
		v.stood = make([]bool, len(v.owned))
	}
	v.stood[i] = true
}

var _ integrations.OwnRecordingStandIn = (*MockManager)(nil)

// standInsLocked, with v.mu held as v closes, files as answered each of its
// test's own recordings a read stood in for (StandIn) that no call took
// after, in recorded order after those that answered: the read was the
// recording's call, made before a write the recording followed. It returns
// them, for the manager to flag as used (noteExactStoodIn).
func (v *exactView) standInsLocked() []*models.Mock {
	var out []*models.Mock
	for i, stood := range v.stood {
		if stood && !v.consumed[i] {
			v.consumed[i] = true
			v.order = append(v.order, i)
			out = append(out, v.owned[i])
		}
	}
	return out
}

// noteExactStoodIn flags owned, a recording of a test that ended with a read
// having stood in for it and no call having taken it (standInsLocked), as
// used: the test's mock accounting counts it as the call it recorded. It
// answered nothing: nothing it learned is flagged (no noise, variables or
// binds), and it is no call of the set answered earlier (noteAnswered).
func (m *MockManager) noteExactStoodIn(owned *models.Mock) {
	if err := m.flagMockAsUsed(exactUsedState(owned), 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
}

// StandIn forwards to the store's (integrations.OwnRecordingStandIn), when
// it keeps one.
func (s *scopedMockDb) StandIn(mk *models.Mock) {
	if si, ok := s.MockMemDb.(integrations.OwnRecordingStandIn); ok {
		si.StandIn(mk)
	}
}

var _ integrations.OwnRecordingStandIn = (*scopedMockDb)(nil)
