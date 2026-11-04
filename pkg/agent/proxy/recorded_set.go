package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// RangeRecordedSet implements integrations.RecordedSetRanger: the recordings
// of kind its trees hold (per-test, session, startup), then those the agent
// keeps on disk (SetRecordedOfKind), each once by name, whatever a read
// shows of them (a test's window, a consume, exact serving's view). With no
// on-disk source set, whole is false and nothing is visited: the agent sets
// one as it stages a set it keeps on disk (Agent.stageMocks), and the trees
// of a set it keeps resident may hold only the open test's window.
func (m *MockManager) RangeRecordedSet(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) (bool, error) {
	if m.closed.Load() {
		return false, nil
	}
	m.exact.ofKind.mu.Lock()
	src := m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
	if src == nil {
		return false, nil
	}
	seen := map[string]bool{}
	for _, mk := range m.stagedOfKind(kind) {
		seen[mk.Name] = true
		if !visit(mk.Name, func() (*models.Mock, error) { return mk, nil }) {
			return true, nil
		}
	}
	err := src(kind, func(name string, load func() (*models.Mock, error)) bool {
		if seen[name] {
			return true
		}
		seen[name] = true
		return visit(name, load)
	})
	return err == nil, err
}

// RangeRecordedSet is the manager's while the staging is still h's call's
// after the range: the set the call was marked in. Once it moved, nothing is
// visited and whole is false, so the call is never decided from the
// recordings of a set the replay staged after its mark.
func (h *heldScope) RangeRecordedSet(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) (bool, error) {
	type seen struct {
		name string
		load func() (*models.Mock, error)
	}
	var all []seen
	whole, err := h.MockManager.RangeRecordedSet(kind, func(name string, load func() (*models.Mock, error)) bool {
		all = append(all, seen{name, load})
		return true
	})
	// The manager tells no whole set whenever its source failed.
	if !whole || h.moved() {
		return false, err
	}
	for _, s := range all {
		if !visit(s.name, s.load) {
			break
		}
	}
	return true, nil
}

// RangeRecordedSet forwards the wrapped store's: a recording of the set is
// what decides another's place in it, and the view's reads, not this, say
// what answers the worker's calls.
func (s *scopedMockDb) RangeRecordedSet(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) (bool, error) {
	return integrations.RangeRecordedSet(s.MockMemDb, kind, visit)
}

var (
	_ integrations.RecordedSetRanger = (*MockManager)(nil)
	_ integrations.RecordedSetRanger = (*heldScope)(nil)
	_ integrations.RecordedSetRanger = (*scopedMockDb)(nil)
)
