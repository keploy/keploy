package proxy

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// DiskMocks keeps heavy per-test mocks on the agent's LOCAL DISK instead of in
// RAM, and loads only the mocks a given test needs (its time window) on demand.
//
// Why: the replay agent used to hold the ENTIRE delivered pool resident
// (ClientMockStorage) for the whole replay, using the per-test time window only
// as a MATCHING filter (MockManager.SetMocksWithWindow) — never as a RESIDENCY
// filter. On a large pool (notably the new-release historical smart-set) that
// resident set OOM-kills the rpl-pod agent.
//
// DiskMocks makes the window a residency filter: at ingest, window-eligible
// per-test mocks are gob-encoded to a per-session temp file and indexed by
// request timestamp; the resident pointer is dropped. At UpdateMockParams the
// agent loads only what the filter would keep — the current window plus the
// startup band — via a binary search. Resident RAM becomes O(config/startup +
// one window) regardless of pool size, for every mock kind.
//
// Reusable mocks (session/connection/config) and the rare ineligible per-test
// mocks (missing/invalid timestamps) are NOT put on disk — the caller keeps them
// resident so the existing filter routing (lifetime-first, missing-timestamp,
// lax promotion) is reproduced exactly on the loaded superset.
type DiskMocks struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	offset   int64
	entries  []diskEntry          // sorted by reqTsNano after Finalize
	byName   map[string]diskEntry // mock name -> entry (by value, sort-immune; for mapping-based replay)
	finalize bool
	logger   *zap.Logger
	closed   bool
}

// diskEntry locates one on-disk mock. ~56 B resident per mock — the only
// per-mock cost that stays in RAM; the payload lives in the temp file.
//
// For big-body HTTP/Mongo per-test mocks the response payload is written as a
// SEPARATE on-disk blob (respOff/respLen) and elided from the request-side
// record, so LoadWindow reconstructs only matchers — never the bodies. The
// body is hydrated on demand at serve time (Mock.HydrateResponse). respOff is
// -1 when the response is inline (small responses, non-eligible kinds).
type diskEntry struct {
	reqTsNano int64
	off       int64
	length    int
	respOff   int64 // -1 => response is inline in the mock record
	respLen   int
}

// responseSpillMinBytes: only elide HTTP response bodies at least this large.
// Small responses stay inline (holding them resident in the window is cheap and
// avoids a serve-time disk read); eliding them would add hydrate latency with
// no memory benefit. Mongo responses always spill — they are the big-doc case
// that drives the serve-time transient. Targets the payload bloat, not counts.
const responseSpillMinBytes = 8 * 1024

// spilledResponse is the response payload written to disk apart from the
// (small) request-side mock, so the resident window holds only matchers.
// Kind-agnostic: exactly one field is populated. The interface-typed Mongo
// messages round-trip through gob the same way the whole-mock record does.
type spilledResponse struct {
	HTTP  *models.HTTPResp
	Mongo []models.MongoResponse
}

// NewDiskMocks creates an on-disk mock store backed by a fresh temp file. The
// caller owns its lifecycle and must Close it when the pool generation is
// retired (next StoreMocksStream / agent shutdown).
func NewDiskMocks(logger *zap.Logger) (*DiskMocks, error) {
	f, err := os.CreateTemp("", "keploy-diskmocks-*.gob")
	if err != nil {
		return nil, fmt.Errorf("disk mocks: create temp file: %w", err)
	}
	return &DiskMocks{
		f:      f,
		path:   f.Name(),
		byName: make(map[string]diskEntry),
		logger: logger,
	}, nil
}

// EligibleForDisk reports whether a mock should be kept on disk instead of RAM.
// Only window-eligible per-test mocks qualify: reusable mocks
// (session/connection/config) must stay resident (matched across the whole
// session, never window-filtered), and per-test mocks with missing/inconsistent
// timestamps are routed by the filter without a window check, so they stay
// resident too. Call DeriveLifetime on the mock before this.
func EligibleForDisk(m *models.Mock) bool {
	if m == nil || m.TestModeInfo.Lifetime != models.LifetimePerTest {
		return false
	}
	req := m.Spec.ReqTimestampMock
	res := m.Spec.ResTimestampMock
	if req.IsZero() || res.IsZero() || res.Before(req) {
		return false
	}
	return true
}

// EligibleForResponseSpill reports whether a disk-eligible per-test mock carries
// a large HTTP/Mongo response worth eliding from the resident window and
// hydrating at serve time. Matching never reads responses, so keeping a big body
// resident for the whole test window is pure waste — this is what drives the
// serve-time transient spike on fan-out tests (one window spanning many big
// docs). Other kinds (and small HTTP bodies) keep their response inline.
func EligibleForResponseSpill(m *models.Mock) bool {
	if !EligibleForDisk(m) {
		return false
	}
	switch m.Kind {
	case models.HTTP:
		return m.Spec.HTTPResp != nil && len(m.Spec.HTTPResp.Body) >= responseSpillMinBytes
	case models.Mongo:
		return len(m.Spec.MongoResponses) > 0
	}
	return false
}

// Add writes an eligible mock to disk and records its location. The caller must
// drop its resident reference to the mock afterwards. Called from the single-
// threaded StoreMocksStream decode loop.
//
// For response-spill-eligible mocks the response payload is encoded and written
// as a separate blob, and the mock record is encoded with its response elided —
// so LoadWindow reconstructs matchers only. The response hydrates on demand at
// serve (see readAt / Mock.HydrateResponse).
func (d *DiskMocks) Add(m *models.Mock) error {
	spill := EligibleForResponseSpill(m)

	var respBlob []byte
	if spill {
		var rb bytes.Buffer
		sr := spilledResponse{HTTP: m.Spec.HTTPResp, Mongo: m.Spec.MongoResponses}
		if err := gob.NewEncoder(&rb).Encode(&sr); err != nil {
			return fmt.Errorf("disk mocks: encode response %q: %w", m.Name, err)
		}
		respBlob = rb.Bytes()
	}

	var buf bytes.Buffer
	// Fresh encoder per record → each record is self-contained (full type
	// definitions) so it can be decoded in isolation via ReadAt; a shared
	// encoder emits type defs only once and later records fail to decode alone.
	if spill {
		// Encode with the response elided; restore afterwards so the caller's
		// mock is untouched (it is dropped by the caller regardless).
		httpResp, mongoResp := m.Spec.HTTPResp, m.Spec.MongoResponses
		m.Spec.HTTPResp, m.Spec.MongoResponses = nil, nil
		err := gob.NewEncoder(&buf).Encode(m)
		m.Spec.HTTPResp, m.Spec.MongoResponses = httpResp, mongoResp
		if err != nil {
			return fmt.Errorf("disk mocks: encode %q: %w", m.Name, err)
		}
	} else if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("disk mocks: encode %q: %w", m.Name, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("disk mocks: store closed")
	}
	respOff := int64(-1)
	respLen := 0
	if spill {
		off, n, err := d.writeLocked(respBlob)
		if err != nil {
			return fmt.Errorf("disk mocks: write response %q: %w", m.Name, err)
		}
		respOff, respLen = off, n
	}
	off, n, err := d.writeLocked(buf.Bytes())
	if err != nil {
		return fmt.Errorf("disk mocks: write %q: %w", m.Name, err)
	}
	e := diskEntry{reqTsNano: m.Spec.ReqTimestampMock.UnixNano(), off: off, length: n, respOff: respOff, respLen: respLen}
	d.byName[m.Name] = e
	d.entries = append(d.entries, e)
	d.finalize = false
	return nil
}

// writeLocked appends b to the temp file at the running offset and advances it.
// Caller holds d.mu.
func (d *DiskMocks) writeLocked(b []byte) (int64, int, error) {
	off := d.offset
	n, err := d.f.WriteAt(b, off)
	if err != nil {
		return 0, 0, err
	}
	d.offset += int64(n)
	return off, n, nil
}

// Finalize sorts the index by request timestamp so LoadWindow/LoadBefore can
// binary-search. Idempotent; call once after the ingest loop.
func (d *DiskMocks) Finalize() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalize {
		return
	}
	sort.Slice(d.entries, func(i, j int) bool { return d.entries[i].reqTsNano < d.entries[j].reqTsNano })
	// byName holds entries by value, so it is unaffected by sorting d.entries.
	d.finalize = true
}

// readAt decodes the mock at a given entry. The file is append-only and
// immutable after Finalize, so ReadAt is safe for concurrent readers.
func (d *DiskMocks) readAt(e diskEntry) (*models.Mock, error) {
	buf := make([]byte, e.length)
	if _, err := d.f.ReadAt(buf, e.off); err != nil {
		return nil, fmt.Errorf("disk mocks: read at %d: %w", e.off, err)
	}
	var m models.Mock
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&m); err != nil {
		return nil, fmt.Errorf("disk mocks: decode at %d: %w", e.off, err)
	}
	// The response body was elided from this record — install a lazy loader so
	// it hydrates from disk only when the mock is actually served. The window
	// therefore holds matchers only, not the (potentially large) bodies.
	if e.respOff >= 0 {
		respOff, respLen := e.respOff, e.respLen
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			return d.loadResponse(respOff, respLen)
		})
	}
	return &m, nil
}

// loadResponse reads and decodes an elided response payload. The temp file is
// append-only and immutable after Finalize, so ReadAt is safe for concurrent
// readers; a closed store returns a clean error rather than mis-serving.
func (d *DiskMocks) loadResponse(off int64, length int) (*models.HTTPResp, []models.MongoResponse, error) {
	d.mu.Lock()
	closed := d.closed
	f := d.f
	d.mu.Unlock()
	if closed {
		return nil, nil, fmt.Errorf("disk mocks: store closed")
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, nil, fmt.Errorf("disk mocks: read response at %d: %w", off, err)
	}
	var sr spilledResponse
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&sr); err != nil {
		return nil, nil, fmt.Errorf("disk mocks: decode response at %d: %w", off, err)
	}
	return sr.HTTP, sr.Mongo, nil
}

// LoadWindow returns on-disk mocks whose request timestamp is in [start,end],
// matching SetMocksWithWindow's inclusive bounds (!Before(start) && !After(end)).
func (d *DiskMocks) LoadWindow(start, end time.Time) ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	if !d.finalize {
		d.mu.Unlock()
		d.Finalize()
		d.mu.Lock()
	}
	lo := start.UnixNano()
	hi := end.UnixNano()
	i := sort.Search(len(d.entries), func(k int) bool { return d.entries[k].reqTsNano >= lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.entries) && d.entries[i].reqTsNano <= hi; i++ {
		sel = append(sel, d.entries[i])
	}
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadBefore returns on-disk mocks with request timestamp strictly before t —
// the startup band (req < firstWindowStart) that SetMocksWithWindow routes to
// the startup tree. Loaded once and kept resident by the caller.
func (d *DiskMocks) LoadBefore(t time.Time) ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	if !d.finalize {
		d.mu.Unlock()
		d.Finalize()
		d.mu.Lock()
	}
	cut := t.UnixNano()
	i := sort.Search(len(d.entries), func(k int) bool { return d.entries[k].reqTsNano >= cut })
	sel := append([]diskEntry(nil), d.entries[:i]...)
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadByNames returns the on-disk mocks named in names (mapping-based replay,
// which selects by name and ignores the window). Missing names are skipped.
func (d *DiskMocks) LoadByNames(names []string) ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	sel := make([]diskEntry, 0, len(names))
	for _, n := range names {
		if e, ok := d.byName[n]; ok {
			sel = append(sel, e)
		}
	}
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadAll returns every on-disk mock (lax timestamp mode, which promotes
// out-of-window per-test mocks to the session pool and therefore needs the full
// per-test set). Lax mode is not the OOM path; correctness over the RAM bound.
func (d *DiskMocks) LoadAll() ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	sel := append([]diskEntry(nil), d.entries...)
	d.mu.Unlock()
	return d.decodeAll(sel)
}

func (d *DiskMocks) decodeAll(sel []diskEntry) ([]*models.Mock, error) {
	out := make([]*models.Mock, 0, len(sel))
	for _, e := range sel {
		m, err := d.readAt(e)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// EarliestReqTs returns the earliest request timestamp across on-disk mocks, or
// the zero time if none. Used so finalizeClientMocks seeds the freeze anchor
// from the full pool (on-disk per-test mocks often carry the earliest, app-boot,
// timestamps) rather than only the resident slices.
func (d *DiskMocks) EarliestReqTs() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) == 0 {
		return time.Time{}
	}
	if !d.finalize {
		min := d.entries[0].reqTsNano
		for _, e := range d.entries[1:] {
			if e.reqTsNano < min {
				min = e.reqTsNano
			}
		}
		return time.Unix(0, min)
	}
	return time.Unix(0, d.entries[0].reqTsNano)
}

// Len returns the number of on-disk mocks (diagnostics/tests).
func (d *DiskMocks) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// DiskBytes returns the total bytes written to the store's file (diagnostics/tests).
func (d *DiskMocks) DiskBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offset
}

// Close releases the store: closes and removes the temp file. Idempotent. After
// Close, all operations fail rather than silently mis-serve.
func (d *DiskMocks) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	var firstErr error
	if d.f != nil {
		if err := d.f.Close(); err != nil {
			firstErr = err
		}
		if err := os.Remove(d.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
