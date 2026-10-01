package proxy

import (
	"sort"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// sessionWindowIndex finds the session-tree mocks recorded inside a time window
// without walking the tree.
//
// Lax mode promotes a recording's per-test MySQL data mocks into the session
// tier, so the session tree holds every test's traffic. A matcher that wants the
// current test's mocks used to walk the whole tree on every command, and the
// walk grew with each test the set held: a replay's total cost grew with the
// square of its length.
//
// The index is built once per tree, by buildTier as it inserts: each
// entry's tree ID in request-time order, and the entries with no request time,
// which are in every window. A lookup resolves the IDs through the
// tree, so it returns the mocks the tree holds NOW: a point update keeps the
// mock's ID (it re-stamps the sort order), a deleted mock is skipped.
//
// The only thing it cannot follow is a point update that changes a mock's ID or
// request time, which nothing in the replay path does. stale records that one
// happened, and GetSessionMocksInWindow then walks the tree as before.
type sessionWindowIndex struct {
	at      []time.Time // recorded request times, ascending
	ids     []int       // ids[i] is the tree ID of the mock recorded at at[i]
	undated []int       // tree IDs of the mocks with no request time
	stale   atomic.Bool
}

// windowEntry is one mock as buildTier inserts it: its tree ID and its
// recorded request time. It is captured at insertion rather than read back from
// the mock, because the same *Mock listed twice is inserted under two IDs.
type windowEntry struct {
	at time.Time
	id int
}

func newSessionWindowIndex(entries []windowEntry) *sessionWindowIndex {
	ix := &sessionWindowIndex{}
	dated := make([]windowEntry, 0, len(entries))
	for _, e := range entries {
		if e.at.IsZero() {
			ix.undated = append(ix.undated, e.id)
			continue
		}
		dated = append(dated, e)
	}
	// The staging path sorts the reusable pool by request time, so in strict
	// mode this is a check, not a sort. In lax mode the per-test mocks it
	// promotes are appended after that pool, and the entries are sorted here.
	less := func(i, j int) bool {
		if dated[i].at.Equal(dated[j].at) {
			return dated[i].id < dated[j].id
		}
		return dated[i].at.Before(dated[j].at)
	}
	if !sort.SliceIsSorted(dated, less) {
		sort.Slice(dated, less)
	}
	ix.at = make([]time.Time, len(dated))
	ix.ids = make([]int, len(dated))
	for i, e := range dated {
		ix.at[i], ix.ids[i] = e.at, e.id
	}
	return ix
}

// idsIn returns the tree IDs of the mocks recorded in [start, end] and of the
// undated ones.
func (ix *sessionWindowIndex) idsIn(start, end time.Time) []int {
	lo := sort.Search(len(ix.at), func(i int) bool { return !ix.at[i].Before(start) })
	hi := sort.Search(len(ix.at), func(i int) bool { return ix.at[i].After(end) })
	out := make([]int, 0, len(ix.undated)+max(hi-lo, 0))
	if hi > lo {
		out = append(out, ix.ids[lo:hi]...)
	}
	return append(out, ix.undated...)
}

// recordedIn reports whether mk belongs to the window [start, end]: its request
// time lies inside it, or it has none.
func recordedIn(mk *models.Mock, start, end time.Time) bool {
	at := mk.Spec.ReqTimestampMock
	return at.IsZero() || (!at.Before(start) && !at.After(end))
}

// GetSessionMocksInWindow implements integrations.SessionWindowReader: the
// GetSessionMocks snapshot narrowed to the mocks recorded in [start, end] or
// undated, in the same order, found through the session tree's window index.
func (m *MockManager) GetSessionMocksInWindow(start, end time.Time) ([]*models.Mock, error) {
	startup, err := m.GetStartupMocks()
	if err != nil {
		return nil, err
	}
	m.treesMu.RLock()
	tree, ix := m.unfiltered, m.unfilteredWindows
	m.treesMu.RUnlock()

	var session []*models.Mock
	if ix == nil || ix.stale.Load() {
		tree.rangeValues(func(v interface{}) bool {
			if mk, ok := v.(*models.Mock); ok && mk != nil && recordedIn(mk, start, end) {
				session = append(session, mk)
			}
			return true
		})
	} else {
		session = tree.valuesInTreeOrder(ix.idsIn(start, end), func(mk *models.Mock) bool {
			return recordedIn(mk, start, end)
		})
	}

	// The same union GetSessionMocks builds, over the narrowed halves: the
	// predicate is per mock, so filtering before or after the union keeps the
	// same mocks in the same order.
	if len(startup) == 0 {
		return session, nil
	}
	out := make([]*models.Mock, 0, len(session)+8)
	seen := make(map[*models.Mock]struct{}, len(session)+8)
	for _, list := range [][]*models.Mock{startup, session} {
		for _, mk := range list {
			if mk == nil || !recordedIn(mk, start, end) {
				continue
			}
			if _, dup := seen[mk]; dup {
				continue
			}
			seen[mk] = struct{}{}
			out = append(out, mk)
		}
	}
	return out, nil
}
