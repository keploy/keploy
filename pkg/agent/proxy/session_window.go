package proxy

import (
	"cmp"
	"math"
	"slices"
	"sort"
	"sync"
	"time"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// windowIndex finds a tree's entries recorded inside a time window without
// walking the tree.
//
// Lax mode promotes a recording's per-test MySQL data mocks into the session
// tier, so the session tree holds every test's traffic. A matcher that wants the
// current test's mocks used to walk the whole tree on every command, and the
// walk grew with each test the set held: a replay's total cost grew with the
// square of its length.
//
// The index belongs to one TreeDb and lives under its lock (TreeDb.win). It is
// built from the tree's own entries by the first windowed lookup on that tree,
// so a staging whose session tree nobody looks up by window (a replay without
// MySQL traffic, or a test that issues no query) never pays for it. A lookup
// holds the tree's lock from reading the index to resolving its IDs, so it
// returns the mocks the tree holds NOW: a point update keeps the mock's ID (it
// re-stamps the sort order), a deleted mock is skipped.
//
// An insert, or an update that stores a mock under another ID or at a request
// time the index does not file its ID under, drops the index under the write
// lock it takes to change the tree, and the next lookup builds it again.
// Whether an update keeps the index right is decided against what the index
// files, not against the mock the tree held before: a caller may have edited
// that mock in place, and then it no longer says where the index filed it.
//
// A lookup resolves IDs through the tree's ID index, which holds one entry per
// ID. When some entry cannot be resolved that way (an update moved a mock onto
// an ID another entry held, or one of two entries sharing an ID off it; no
// matcher does either), the index is built walk-only: lookups walk the tree
// until the next staging.
//
// Times are kept as Unix nanoseconds, which compare far faster than time.Time.
// Recorded request times are decoded, so they carry no monotonic reading and
// their order is their wall-clock order, the order recordedIn compares in.
type windowIndex struct {
	dated   []windowEntry // ascending by request time, then tree ID
	undated []int         // tree IDs of the entries with no request time: in every window
	// odd holds the tree IDs of the entries whose request time Unix
	// nanoseconds cannot hold (before 1677-09-21T00:12:43.145224192Z or after
	// 2262-04-11T23:47:16.854775807Z); every lookup checks them one by one.
	odd []int
	// walk is set when some entry of the tree cannot be resolved through the
	// tree's ID index, which holds one entry per ID (an update moved a mock
	// onto an ID another entry held, or one of two entries sharing an ID off
	// it): lookups walk the tree instead.
	walk bool
}

// byTimeThenID orders dated entries.
func byTimeThenID(a, b windowEntry) int {
	if c := cmp.Compare(a.ns, b.ns); c != 0 {
		return c
	}
	return cmp.Compare(a.id, b.id)
}

// files reports whether the index files tree ID id under request time t.
func (ix *windowIndex) files(id int, t time.Time) bool {
	if t.IsZero() {
		return slices.Contains(ix.undated, id)
	}
	ns, ok := unixNs(t)
	if !ok {
		return slices.Contains(ix.odd, id)
	}
	_, found := slices.BinarySearchFunc(ix.dated, windowEntry{ns, id}, byTimeThenID)
	return found
}

// windowEntry is one dated tree entry: its request time and its tree ID.
type windowEntry struct {
	ns int64
	id int
}

var (
	minNsTime = time.Unix(0, math.MinInt64)
	maxNsTime = time.Unix(0, math.MaxInt64)
)

// unixNs reports t as Unix nanoseconds, and whether they can hold it.
func unixNs(t time.Time) (int64, bool) {
	if t.Before(minNsTime) || t.After(maxNsTime) {
		return 0, false
	}
	return t.UnixNano(), true
}

// buildWindowIndexLocked indexes db's entries. db.mu must be held for writing.
func (db *TreeDb) buildWindowIndexLocked() *windowIndex {
	// Every ID index entry maps an ID to a live key with that ID: the tree's
	// changes remove the ID they remove. So the ID index has an entry for each
	// of the tree's entries exactly when they are as many, and otherwise some
	// entry cannot be resolved by its ID.
	if len(db.idIndex) != db.rbt.Size() {
		return &windowIndex{walk: true}
	}
	ix := &windowIndex{dated: make([]windowEntry, 0, db.rbt.Size())}
	it := db.rbt.Iterator()
	for it.Next() {
		key, ok := it.Key().(models.TestModeInfo)
		if !ok {
			continue
		}
		mk, ok := it.Value().(*models.Mock)
		if !ok || mk == nil {
			continue
		}
		at := mk.Spec.ReqTimestampMock
		if at.IsZero() {
			ix.undated = append(ix.undated, key.ID)
			continue
		}
		ns, ok := unixNs(at)
		if !ok {
			ix.odd = append(ix.odd, key.ID)
			continue
		}
		ix.dated = append(ix.dated, windowEntry{ns, key.ID})
	}
	// The tree walks in sort order, which a staged pool takes from its request
	// times, so this is usually one ordered pass. Lax mode appends the
	// per-test mocks it promotes after the reusable pool, two ordered runs.
	if !slices.IsSortedFunc(ix.dated, byTimeThenID) {
		slices.SortFunc(ix.dated, byTimeThenID)
	}
	return ix
}

// idsIn returns the tree IDs that can hold an entry recorded in [start, end]:
// the dated ones inside it, the undated ones and the odd ones.
func (ix *windowIndex) idsIn(start, end time.Time) []int {
	lo, hi := 0, len(ix.dated)
	if ns, ok := unixNs(start); ok {
		lo = sort.Search(len(ix.dated), func(i int) bool { return ix.dated[i].ns >= ns })
	} else if start.After(maxNsTime) {
		lo = len(ix.dated)
	}
	if ns, ok := unixNs(end); ok {
		hi = sort.Search(len(ix.dated), func(i int) bool { return ix.dated[i].ns > ns })
	} else if end.Before(minNsTime) {
		hi = 0
	}
	out := make([]int, 0, len(ix.undated)+len(ix.odd)+max(hi-lo, 0))
	for i := lo; i < hi; i++ {
		out = append(out, ix.dated[i].id)
	}
	out = append(out, ix.undated...)
	return append(out, ix.odd...)
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
	tree := m.unfiltered
	m.treesMu.RUnlock()

	session := tree.valuesInWindow(start, end)
	if v := m.exactView(); v != nil {
		// What GetSessionMocks shows under exact serving, narrowed alike.
		session = v.keep(session)
		if owned := v.unanswered(""); len(owned) > 0 {
			startup = append(owned, startup...)
		}
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

// RangeSessionMocksWithKey implements integrations.SessionKeyReader: the
// GetSessionMocks snapshot narrowed to the mocks ix files under key, in the
// same order, found through the startup and session trees' key indexes.
func (m *MockManager) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if v := m.exactView(); v != nil {
		// What GetSessionMocks shows under exact serving: the open test's
		// unanswered mocks first, then the staged ones the view shows. The
		// open test's are found through the view's own key index
		// (ownedKeyed), so a lookup reads only the mocks filed under key,
		// and computes no mock's keys after the view's first lookup.
		for _, mk := range unansweredAt(v, ownedKeyed(v, ix).byKey[key]) {
			if !fn(mk) {
				return nil
			}
		}
		inner := fn
		fn = func(mk *models.Mock) bool { return !v.shows(mk) || inner(mk) }
	}
	m.treesMu.RLock()
	startup, session := m.startup, m.unfiltered
	m.treesMu.RUnlock()
	// GetSessionMocks dedups by pointer only when the startup tier holds
	// anything, and then across both tiers.
	if startup == nil || startup.size() == 0 {
		session.rangeKeyed(ix, key, fn)
		return nil
	}
	var seen map[*models.Mock]struct{}
	stopped := false
	visit := func(mk *models.Mock) bool {
		if _, dup := seen[mk]; dup {
			return true
		}
		if seen == nil {
			seen = make(map[*models.Mock]struct{}, 8)
		}
		seen[mk] = struct{}{}
		if !fn(mk) {
			stopped = true
			return false
		}
		return true
	}
	startup.rangeKeyed(ix, key, visit)
	if !stopped {
		session.rangeKeyed(ix, key, visit)
	}
	return nil
}

// ownedIndex files the mocks an exact view owns (exactView.owned) under the
// keys a MockIndex gives them: byKey lists, for each key, the owned positions
// filed under it, ascending, so a walk of one key's list is unanswered's
// order narrowed to the key.
//
// A view's owned set is fixed once the view is made (exactView.add runs
// before it is installed), and a MockIndex's keys depend only on what a mock
// recorded and how replay classified it, so the index is built once per view
// and MockIndex. Under keploy mock replay one test can own a whole set: a
// lookup that computed every owned mock's keys walked thousands of mocks per
// command, more than a scan of the whole pool costs.
type ownedIndex struct {
	once  sync.Once
	byKey map[string][]int
}

// ownedIndexID names one view's index for one MockIndex. The view is held
// weakly: an index never keeps a replaced view, and the mocks it owns, alive.
type ownedIndexID struct {
	view weak.Pointer[exactView]
	ix   *integrations.MockIndex
}

// ownedIndexes holds the index of each live view and MockIndex. Entries of
// views the collector has freed are dropped when the map reaches
// ownedIndexCap entries, and when that leaves it full, every entry is
// dropped: an index is rebuilt on its view's next lookup, with the same
// result.
var ownedIndexes struct {
	mu sync.Mutex
	m  map[ownedIndexID]*ownedIndex
}

// ownedIndexCap bounds ownedIndexes. A manager installs one view at a time,
// and a parser declares its MockIndex once, so a replay holds a few entries.
const ownedIndexCap = 64

// ownedKeyed is v's owned index for ix, built on its first use.
func ownedKeyed(v *exactView, ix *integrations.MockIndex) *ownedIndex {
	id := ownedIndexID{weak.Make(v), ix}
	ownedIndexes.mu.Lock()
	oi := ownedIndexes.m[id]
	if oi == nil {
		if len(ownedIndexes.m) >= ownedIndexCap {
			for k := range ownedIndexes.m {
				if k.view.Value() == nil {
					delete(ownedIndexes.m, k)
				}
			}
			if len(ownedIndexes.m) >= ownedIndexCap {
				clear(ownedIndexes.m)
			}
		}
		if ownedIndexes.m == nil {
			ownedIndexes.m = map[ownedIndexID]*ownedIndex{}
		}
		oi = &ownedIndex{}
		ownedIndexes.m[id] = oi
	}
	ownedIndexes.mu.Unlock()
	oi.once.Do(func() {
		oi.byKey = map[string][]int{}
		// owned is fixed once the view is installed: read it without v.mu,
		// as unanswered's callers read the mocks it returns.
		for i, mk := range v.owned {
			for _, k := range ix.Keys(mk) {
				if l := oi.byKey[k]; len(l) == 0 || l[len(l)-1] != i {
					oi.byKey[k] = append(l, i)
				}
			}
		}
	})
	return oi
}

// unansweredAt is unanswered("") narrowed to the owned positions at
// (ascending): those of at that have not answered, in at's order.
func unansweredAt(v *exactView, at []int) []*models.Mock {
	if len(at) == 0 || len(v.owned) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(at))
	for _, i := range at {
		if i < len(v.owned) && !v.consumed[i] && !v.takenByOther(i) {
			out = append(out, v.owned[i])
		}
	}
	return out
}
