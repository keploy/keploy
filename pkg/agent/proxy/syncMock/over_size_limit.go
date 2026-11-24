package manager

import (
	"slices"
	"sort"
	"strconv"
	"sync"
)

// maxOverSizeLimitCalls bounds how many calls one session's count of requests
// over keploy's size limit for a test case keeps apart (OverSizeTally). Past
// it, the requests of a call not seen yet are counted in the total and in
// OverSizeSkips.Other only: an app whose large calls carry an id in their
// path would otherwise grow the map for as long as the recording lasts.
const maxOverSizeLimitCalls = 256

// overSizeLimit is what the manager counts of the requests an ingress served
// and did not record as test cases because each was over keploy's size limit
// for a test case (NoteOverSizeLimit): how many over its life, and, by call,
// on each recording session's open tally. Zero value ready.
type overSizeLimit struct {
	mu      sync.Mutex
	total   uint64
	tallies []*OverSizeTally
}

// NoteOverSizeLimit counts a request an ingress served and did not record as
// a test case because its request or response was over keploy's size limit
// for a test case (conn.NoteOverSizeLimit says which, and tells the
// recording's client the request whole). call names it as the agent's log
// does: its method, its path's shape and its path tag. The app answered the
// request; the recording lacks it. Each open session's tally
// (OpenOverSizeTally) counts it, so the recording's summary reports the
// session's count and the calls with the most (routes/record.go), and a
// recording thinner than its traffic says why, rather than ending short
// without a word. It returns how many it has counted over the manager's
// life, this one included, for a caller that warns of the first and of every
// so many. Nil-safe: a nil manager counts nothing, and returns 0.
//
// It counts what no other count does: a request a pause of the memory guard
// cost is counted as that (NoteNotRecordedUnderPressure), never here too.
func (m *SyncMockManager) NoteOverSizeLimit(call string) uint64 {
	if m == nil {
		return 0
	}
	s := &m.overSize
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	for _, t := range s.tallies {
		t.note(call)
	}
	return s.total
}

// OverSizeLimitCount is how many requests NoteOverSizeLimit has counted over
// the manager's life. Nil-safe.
func (m *SyncMockManager) OverSizeLimitCount() uint64 {
	if m == nil {
		return 0
	}
	m.overSize.mu.Lock()
	defer m.overSize.mu.Unlock()
	return m.overSize.total
}

// OverSizeTally counts one recording session's own requests over keploy's
// size limit for a test case (NoteOverSizeLimit): those noted from its
// opening until its Close, in all and by call, whatever other tallies are
// open beside it. The manager serves every session of the agent's life, so a
// session's calls are kept apart on its own tally: one bound over the
// manager's life would, once that many calls had come, name no call of a
// later session. Guarded by the manager's overSize.mu.
type OverSizeTally struct {
	m      *SyncMockManager
	total  uint64
	byCall map[string]uint64
	other  uint64
}

// note counts one request of call on the tally. The caller holds overSize.mu.
func (t *OverSizeTally) note(call string) {
	t.total++
	if _, seen := t.byCall[call]; !seen && len(t.byCall) >= maxOverSizeLimitCalls {
		t.other++
		return
	}
	if t.byCall == nil {
		t.byCall = make(map[string]uint64)
	}
	t.byCall[call]++
}

// OpenOverSizeTally opens a tally of the requests over the size limit noted
// from now on (OverSizeTally). Close it when the session ends: an open tally
// costs every request noted a step, and it stays referenced until then. Nil
// for a nil manager.
func (m *SyncMockManager) OpenOverSizeTally() *OverSizeTally {
	if m == nil {
		return nil
	}
	t := &OverSizeTally{m: m}
	m.overSize.mu.Lock()
	m.overSize.tallies = append(m.overSize.tallies, t)
	m.overSize.mu.Unlock()
	return t
}

// Read returns a copy of what the tally has counted. Zero for a nil tally.
func (t *OverSizeTally) Read() OverSizeSkips {
	if t == nil {
		return OverSizeSkips{}
	}
	t.m.overSize.mu.Lock()
	defer t.m.overSize.mu.Unlock()
	out := OverSizeSkips{Total: t.total, Other: t.other}
	if len(t.byCall) > 0 {
		out.ByCall = make(map[string]uint64, len(t.byCall))
		for c, n := range t.byCall {
			out.ByCall[c] = n
		}
	}
	return out
}

// Close ends the tally: it counts nothing more, and Read keeps returning what
// it counted. Idempotent, and a no-op on a nil tally.
func (t *OverSizeTally) Close() {
	if t == nil {
		return
	}
	s := &t.m.overSize
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.tallies, t); i >= 0 {
		s.tallies = slices.Delete(s.tallies, i, i+1)
	}
}

// OpenOverSizeTallies reports how many tallies are open (OpenOverSizeTally):
// a session that ends leaves none of its own. Nil-safe.
func (m *SyncMockManager) OpenOverSizeTallies() int {
	if m == nil {
		return 0
	}
	m.overSize.mu.Lock()
	defer m.overSize.mu.Unlock()
	return len(m.overSize.tallies)
}

// OverSizeSkips is what a session's tally counted (OverSizeTally.Read).
type OverSizeSkips struct {
	// Total is the requests counted.
	Total uint64
	// ByCall is the requests counted for each call kept apart.
	ByCall map[string]uint64
	// Other is the requests counted for calls past the bound of those kept
	// apart (maxOverSizeLimitCalls).
	Other uint64
}

// Most names the calls with the most requests counted, at most n of them,
// most first, each as "<call> (<requests>)", and, when calls past them were
// counted (those beyond n, and those past the bound of the calls kept apart),
// last "<k> more requests of other calls": the list's counts add up to the
// total. Ties go by name, so the list is the same for the same counts.
func (s OverSizeSkips) Most(n int) []string {
	type call struct {
		name string
		reqs uint64
	}
	calls := make([]call, 0, len(s.ByCall))
	for c, r := range s.ByCall {
		calls = append(calls, call{c, r})
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].reqs != calls[j].reqs {
			return calls[i].reqs > calls[j].reqs
		}
		return calls[i].name < calls[j].name
	})
	rest := s.Other
	if len(calls) > n {
		for _, c := range calls[n:] {
			rest += c.reqs
		}
		calls = calls[:n]
	}
	out := make([]string, 0, len(calls)+1)
	for _, c := range calls {
		out = append(out, c.name+" ("+strconv.FormatUint(c.reqs, 10)+")")
	}
	if rest > 0 {
		out = append(out, strconv.FormatUint(rest, 10)+" more requests of other calls")
	}
	return out
}
