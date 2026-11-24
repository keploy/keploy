package proxy

import (
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

const (
	defaultCarryOverCapBytes = 16 << 20
	// carryOverCapEnv optionally raises the carry-over tier cap (in MiB) when
	// a recording holds more than 16 MiB of unconsumed carry-over mocks.
	carryOverCapEnv = "KEPLOY_CARRY_OVER_CAP_MIB"
)

// carryOverCapBytes bounds what the carry-over tier holds: 16 MiB, 1.6% of the
// agent's 1 GiB limit. The worst case measured (every publish of a set left
// unconsumed on a production recording) is 13.15 MiB. Above it, carry-over stops
// in recorded order: the earliest-recorded mocks are kept. A var so tests can
// lower it.
var carryOverCapBytes = defaultCarryOverCapBytes

func effectiveCarryOverCapBytes() int {
	if carryOverCapBytes != defaultCarryOverCapBytes {
		return carryOverCapBytes
	}
	if raw := strings.TrimSpace(os.Getenv(carryOverCapEnv)); raw != "" {
		if mib, err := strconv.Atoi(raw); err == nil && mib > 0 {
			return mib << 20
		}
	}
	return carryOverCapBytes
}

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

// carryOverPool is the carry-over tier: carry-over mocks (models.IsCarryOver:
// RegisterCarryOver, a StagingRelease need) that are reachable outside their
// own window — ahead of it by up to
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
	// leftOut counts the mocks the cap kept out in the current staging epoch
	// (logged at Debug; the run-wide total and single Warn live on MockManager).
	leftOut int
	// evicted records the names of carry-over mocks kept out by the byte cap
	// so later miss reports can attribute an exact evicted mock ID.
	evicted map[carryKey]string
}

const maxEvictedCarryMeta = 16384

// carryOverMockSig computes the canonical call signature for an evicted mock,
// preferring explicit operation/commandType metadata (and topic when present,
// so broker kinds such as Pulsar never collapse distinct topics onto the bare
// op token) so it matches runtime ActualSummary values.
func carryOverMockSig(mk *models.Mock) string {
	if mk == nil {
		return ""
	}
	if mk.Spec.HTTPReq != nil {
		return normalizeCarryOverSig(string(mk.Kind), string(mk.Spec.HTTPReq.Method)+" "+mk.Spec.HTTPReq.URL)
	}
	topic := strings.TrimSpace(mk.Spec.Metadata["topic"])
	if cmd := strings.TrimSpace(mk.Spec.Metadata["commandType"]); cmd != "" {
		if topic != "" {
			return normalizeCarryOverSig(string(mk.Kind), cmd+" "+topic)
		}
		return normalizeCarryOverSig(string(mk.Kind), cmd)
	}
	if op := strings.TrimSpace(mk.Spec.Metadata["operation"]); op != "" {
		if topic != "" {
			return normalizeCarryOverSig(string(mk.Kind), op+" "+topic)
		}
		return normalizeCarryOverSig(string(mk.Kind), op)
	}
	return normalizeCarryOverSig(string(mk.Kind), models.MockSummaryFromSpec(mk))
}

// normalizeCarryOverSig produces the same canonical call signature for both
// recorded mock summaries (MockSummaryFromSpec, which uses full URLs for HTTP
// and "<Kind> <op>" for broker/DB kinds) and runtime mismatch summaries
// (ActualSummary, which uses "METHOD /path" for HTTP and "<op> <topic/target>"
// for broker/DB kinds). For Pulsar, the op alone (e.g. "send") is never
// sufficient: the topic token must be present too.
func normalizeCarryOverSig(kind, summary string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ""
	}
	if strings.EqualFold(kind, string(models.HTTP)) || strings.EqualFold(kind, "HTTP") {
		parts := strings.Fields(s)
		if len(parts) < 2 {
			return ""
		}
		method := strings.ToUpper(parts[0])
		rawTarget := parts[1]
		if strings.Contains(rawTarget, "://") {
			if u, err := url.Parse(rawTarget); err == nil {
				rawTarget = u.Path
			}
		}
		if i := strings.IndexByte(rawTarget, '?'); i >= 0 {
			rawTarget = rawTarget[:i]
		}
		if rawTarget == "" {
			rawTarget = "/"
		}
		return method + " " + rawTarget
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	if kind != "" && len(fields) > 1 && strings.EqualFold(fields[0], strings.TrimSpace(kind)) {
		fields = fields[1:]
	}
	if strings.Contains(strings.ToLower(kind), "pulsar") {
		if len(fields) < 2 {
			return ""
		}
		return strings.ToLower(fields[0]) + " " + fields[1]
	}
	if len(fields) >= 2 {
		return strings.ToLower(fields[0]) + " " + fields[1]
	}
	return strings.ToLower(fields[0])
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

// recordedBefore orders mocks as they were recorded: by request time, then
// by the number mockdb gave each as it wrote it
// (models.CompareRecordedNumbers), then by name.
func recordedBefore(a, b *models.Mock) bool {
	ta, tb := a.Spec.ReqTimestampMock, b.Spec.ReqTimestampMock
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	if c := models.CompareRecordedNumbers(a.Name, b.Name); c != 0 {
		return c < 0
	}
	return a.Name < b.Name
}

// reset empties the pool for a new staging epoch or a replay pass that went
// back to an earlier window. The run-wide overflow counter and once-per-run
// Warn live on MockManager and are NOT reset here.
func (p *carryOverPool) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byKey = make(map[carryKey]carryEntry)
	p.ordered = nil
	p.bytes = 0
	p.leftOut = 0
	p.evicted = nil
}

// fileResult is what one filing did, for the revision bump and the cap log.
type fileResult struct {
	kinds         map[models.Kind]struct{}
	leftOut       int // kept out by the cap in this filing
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
			delete(p.evicted, k)
			res.kinds[e.mk.Kind] = struct{}{}
			res.removedByTree++
		}
	}
	for _, mk := range candidates {
		k := carryKeyOf(mk)
		if _, held := p.byKey[k]; held {
			delete(p.evicted, k)
			continue
		}
		if _, now := inTree[k]; now {
			delete(p.evicted, k)
			continue
		}
		capBytes := effectiveCarryOverCapBytes()
		size := approxMockBytes(mk)
		if p.bytes+size > capBytes {
			res.leftOut++
			if p.evicted == nil {
				p.evicted = make(map[carryKey]string)
			}
			if len(p.evicted) < maxEvictedCarryMeta {
				p.evicted[k] = mk.Name
			}
			continue
		}
		delete(p.evicted, k)
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

// matchEvicted returns the exact evicted mock ID from the carry-over tier when
// r.ClosestMock is present in the eviction record and r is not a payload diff.
//
// Eviction is NEVER claimed without an exact evicted mock ID (no signature
// fallback), and a payload/body/variable diff against a test's own candidate
// mock is never attributed to carry-over eviction.
func (p *carryOverPool) matchEvicted(r *models.MockMismatchReport) string {
	if r == nil || r.ClosestMock == "" {
		return ""
	}
	// A payload/body/variable diff against a compared candidate mock is never
	// blamed on carry-over eviction, even if a mock with the same name was
	// evicted earlier.
	if len(r.FieldDiffs) > 0 || len(r.Differs) > 0 {
		return ""
	}
	switch r.MatchPhase {
	case models.MatchPhaseBody, models.MatchPhaseStrict, models.MatchPhaseVariable:
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.evicted) == 0 {
		return ""
	}
	for _, name := range p.evicted {
		if name == r.ClosestMock {
			return name
		}
	}
	return ""
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
// reachable once the current window starts at start
// (models.WindowSchedule.CarryOverReachable, the rule the replayer's mock
// prune holds a carry-over mock by too).
func carryReachable(sched *models.WindowSchedule, req, start time.Time) bool {
	return sched.CarryOverReachable(req, start)
}
