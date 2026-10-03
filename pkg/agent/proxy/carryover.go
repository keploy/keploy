package proxy

import (
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// carryOverCapBytes bounds what the carry-over tier holds: 16 MiB, 1.6% of the
// agent's 1 GiB limit. The worst case measured (every publish of a set left
// unconsumed on a production recording) is 13.15 MiB. Above it, carry-over stops
// in recorded order: the earliest-recorded mocks are kept. A var so tests can
// lower it.
var carryOverCapBytes = 16 << 20

// carryKey identifies one mock in the carry-over tier. Names are unique inside
// a test set, but the recorded request time is part of the key for the same
// reason sameMock compares it: the manager does not enforce unique names.
type carryKey struct {
	name string
	req  int64
}

func carryKeyOf(m *models.Mock) carryKey {
	return carryKey{name: m.Name, req: m.Spec.ReqTimestampMock.UnixNano()}
}

// carryOverPool is the carry-over tier: per-test mocks of RegisterCarryOver
// kinds that are reachable outside their own window — ahead of it by up to
// models.CarryOverLookahead, and after it until consumed. Held in recorded
// order. Disjoint from the per-test tree: a mock the current window holds is
// served from there, not from here.
//
// mu is a LEAF: nothing is acquired under it.
type carryOverPool struct {
	mu    sync.Mutex
	byKey map[carryKey]carryEntry
	// ordered holds the same mocks in recorded order, kept sorted on every
	// insert and removal (binary search + shift), so a read is a copy.
	ordered []*models.Mock
	bytes   int
	// capWarned latches the one cap Warn per staging epoch; leftOut counts
	// the mocks the cap kept out since then.
	capWarned bool
	leftOut   int
}

// carryEntry keeps the size charged at insert, so the release credits exactly
// what was charged.
type carryEntry struct {
	mk   *models.Mock
	size int
}

func newCarryOverPool() *carryOverPool {
	return &carryOverPool{byKey: make(map[carryKey]carryEntry)}
}

func recordedBefore(a, b *models.Mock) bool {
	ta, tb := a.Spec.ReqTimestampMock, b.Spec.ReqTimestampMock
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Name < b.Name
}

// reset empties the pool and re-arms the cap Warn (a new staging epoch, or a
// replay pass that went back to an earlier window).
func (p *carryOverPool) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byKey = make(map[carryKey]carryEntry)
	p.ordered = nil
	p.bytes = 0
	p.capWarned = false
	p.leftOut = 0
}

// fileResult is what one filing did, for the revision bump and the cap log.
type fileResult struct {
	kinds         map[models.Kind]struct{}
	leftOut       int // kept out by the cap in this filing
	firstCapHit   bool
	heldCount     int
	heldBytes     int
	leftOutTotal  int
	removedByTree int
}

// file drops the entries the new per-test tree now holds, then adds the
// candidates in recorded order until the cap. Candidates already held, or held
// by the per-test tree, are skipped.
func (p *carryOverPool) file(candidates []*models.Mock, inTree map[carryKey]struct{}) fileResult {
	res := fileResult{kinds: map[models.Kind]struct{}{}}
	sort.SliceStable(candidates, func(i, j int) bool { return recordedBefore(candidates[i], candidates[j]) })

	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.byKey {
		if _, now := inTree[k]; now {
			p.removeLocked(k, e)
			res.kinds[e.mk.Kind] = struct{}{}
			res.removedByTree++
		}
	}
	for _, mk := range candidates {
		k := carryKeyOf(mk)
		if _, held := p.byKey[k]; held {
			continue
		}
		if _, now := inTree[k]; now {
			continue
		}
		size := approxMockBytes(mk)
		if p.bytes+size > carryOverCapBytes {
			res.leftOut++
			continue
		}
		// Matchers can reach it from here on (see models.Mock.pooled). Every
		// candidate today comes from a staging's own slices, which marked it
		// already; this keeps a future caller's mock from being stamped.
		mk.MarkPooled()
		p.byKey[k] = carryEntry{mk: mk, size: size}
		p.bytes += size
		i := sort.Search(len(p.ordered), func(i int) bool { return recordedBefore(mk, p.ordered[i]) })
		p.ordered = append(p.ordered, nil)
		copy(p.ordered[i+1:], p.ordered[i:])
		p.ordered[i] = mk
		res.kinds[mk.Kind] = struct{}{}
	}
	if res.leftOut > 0 {
		p.leftOut += res.leftOut
		if !p.capWarned {
			p.capWarned = true
			res.firstCapHit = true
		}
	}
	res.heldCount, res.heldBytes, res.leftOutTotal = len(p.byKey), p.bytes, p.leftOut
	return res
}

// take removes the entry for want when it is the mock the caller means, and
// returns the held mock.
func (p *carryOverPool) take(want models.Mock) (*models.Mock, bool) {
	if want.Name == "" {
		return nil, false
	}
	k := carryKey{name: want.Name, req: want.Spec.ReqTimestampMock.UnixNano()}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byKey[k]
	if !ok || e.mk.Kind != want.Kind {
		return nil, false
	}
	p.removeLocked(k, e)
	return e.mk, true
}

// removeLocked drops one held entry from both indexes. Caller holds mu.
func (p *carryOverPool) removeLocked(k carryKey, e carryEntry) {
	delete(p.byKey, k)
	p.bytes -= e.size
	i := sort.Search(len(p.ordered), func(i int) bool { return !recordedBefore(p.ordered[i], e.mk) })
	for ; i < len(p.ordered); i++ {
		if p.ordered[i] == e.mk {
			p.ordered = append(p.ordered[:i], p.ordered[i+1:]...)
			return
		}
	}
}

// snapshot returns the held mocks of kind ("" = all) in recorded order.
func (p *carryOverPool) snapshot(kind models.Kind) []*models.Mock {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*models.Mock, 0, len(p.ordered))
	for _, mk := range p.ordered {
		if kind == "" || mk.Kind == kind {
			out = append(out, mk)
		}
	}
	return out
}

func (p *carryOverPool) size() (count, bytes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey), p.bytes
}

// approxMockBytes estimates the heap a held mock pins, for the carry-over cap.
// It counts the payload-bearing fields — the generic request/response frames
// that broker and cache kinds keep their bytes in, HTTP bodies and headers,
// and the metadata map — plus a fixed overhead for the struct itself. It is an
// estimate for a budget, not an exact size.
func approxMockBytes(m *models.Mock) int {
	if m == nil {
		return 0
	}
	const overhead = 512
	n := overhead + len(m.Name)
	for k, v := range m.Spec.Metadata {
		n += len(k) + len(v) + 32
	}
	for _, payloads := range [2][]models.Payload{m.Spec.GenericRequests, m.Spec.GenericResponses} {
		for _, p := range payloads {
			for _, b := range p.Message {
				n += len(b.Type) + len(b.Data) + 32
			}
		}
	}
	if r := m.Spec.HTTPReq; r != nil {
		n += len(r.URL) + len(r.Body)
		for k, v := range r.Header {
			n += len(k) + len(v) + 32
		}
	}
	if r := m.Spec.HTTPResp; r != nil {
		n += len(r.Body)
		for k, v := range r.Header {
			n += len(k) + len(v) + 32
		}
	}
	return n
}

// carryReachable reports whether a registered mock recorded at req is
// reachable once the current window starts at start: the release window of
// req - CarryOverLookahead has started. With no schedule, everything is.
func carryReachable(sched *models.WindowSchedule, req, start time.Time) bool {
	return sched.Released(req.Add(-models.CarryOverLookahead), start)
}
