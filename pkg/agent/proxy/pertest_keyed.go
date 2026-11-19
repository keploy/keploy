package proxy

import (
	"slices"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// RangePerTestMocksWithKey is GetPerTestMocksInWindow narrowed to the mocks
// ix files under key, handed to fn one at a time
// (integrations.PerTestKeyRanger). Under exact serving the open test's
// unanswered mocks under key are read past the answered ones by their links
// (rangeUnansweredWithKey), so a read that stops at the key's next one
// visits only it and copies none of the test's other mocks; the tier is read
// only when fn has not stopped by then, as rangeExactPerTest reads it.
// Otherwise the read is walked.
func (m *MockManager) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	return m.rangePerTestMocksWithKeyIn(m.exactView(), ix, key, fn)
}

// rangePerTestMocksWithKeyIn is RangePerTestMocksWithKey read in v: the view
// in force, or the view a call held to its mark was read in
// (heldScope.readView); nil with exact serving off.
func (m *MockManager) rangePerTestMocksWithKeyIn(v *exactView, ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	// What is not the open test's own is filed by Keys alone, as the
	// trees' key indexes file the staged mocks.
	under := func(mk *models.Mock) bool { return mk != nil && slices.Contains(distinctKeys(ix, mk), key) }
	if v == nil {
		mocks, err := m.getPerTestMocksInWindowIn(nil)
		if err != nil {
			return err
		}
		for _, mk := range mocks {
			if under(mk) && !fn(mk) {
				break
			}
		}
		return nil
	}
	m.swapMu.RLock()
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	m.swapMu.RUnlock()
	if !v.rangeUnansweredWithKey(ix, key, fn) {
		return nil
	}
	for _, mk := range v.keepPerTest(v.shownOf(tree, ""), m.exactFromStartup(v, "")) {
		if under(mk) && !fn(mk) {
			return nil
		}
	}
	return nil
}

// RangePerTestMocksWithKey is the manager's keyed per-test walk
// (integrations.PerTestKeyRanger), each mock checked after it is read, as
// RangePerTestMocksInWindow checks it: the walk stops once the staging moved,
// or the call's view was replaced (viewGone).
func (h *heldScope) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if h.moved() {
		return nil
	}
	return h.rangePerTestMocksWithKeyIn(h.readView(), ix, key, func(mk *models.Mock) bool {
		return !h.moved() && !h.viewGone() && (h.hides(mk) || fn(mk))
	})
}

// RangePerTestMocksWithKey forwards the keyed per-test walk of the wrapped
// store through the same name filter as GetPerTestMocksInWindow, as
// RangePerTestMocksInWindow does. A store without the walk, or a live view
// (which orders the read by start), is read whole and narrowed by Keys.
func (s *scopedMockDb) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	r, ok := s.MockMemDb.(integrations.PerTestKeyRanger)
	if ok && s.passThrough() {
		return r.RangePerTestMocksWithKey(ix, key, fn)
	}
	if !ok || s.live {
		all, err := s.GetPerTestMocksInWindow()
		if err != nil {
			return err
		}
		for _, mk := range all {
			if mk != nil && slices.Contains(ix.Keys(mk), key) && !fn(mk) {
				break
			}
		}
		return nil
	}
	v := s.current()
	if v.refused != "" {
		return v.refusal()
	}
	gone := s.consumed(v)
	return r.RangePerTestMocksWithKey(ix, key, func(mk *models.Mock) bool {
		return !visibleIn(v, mk) || (gone != nil && gone(mk.Name)) || fn(mk)
	})
}
