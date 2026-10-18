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

// DiskMocks parks per-test mocks on the agent's local disk and loads only the
// mocks a given test needs (its request-time window) on demand, so resident RAM
// is O(config/startup + one window) instead of the whole delivered pool.
type DiskMocks struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	offset   int64
	entries  []diskEntry          // sorted by reqTsNano after Finalize
	byName   map[string]diskEntry // name -> entry, by value so it survives sorting entries
	finalize bool
	logger   *zap.Logger
	closed   bool

	// carry indexes the carry-over entries (models.IsCarryOver at Add: a
	// RegisterCarryOver kind's, a StagingRelease need's), sorted with
	// entries, so the lookahead load reads only them. Empty while nothing is
	// registered.
	carry []diskEntry

	// staged indexes the entries a models.StagingWithStartup need declares
	// (models.IsStagedAtStaging at Add), sorted with entries, so the set's
	// staging call reads only them beside the startup band (LoadStaged).
	// Empty while no kind declares one.
	staged []diskEntry

	// destinations is, for each recorded destination a mock on disk reached
	// (destinationKey), the entry of the first one recorded
	// (LoadDestinations). One entry per destination, resident.
	destinations map[string]diskEntry

	// statements is the statement catalog: for each statement a mock on disk
	// records (its kind and models.StatementKey), the entry of the first one
	// recorded (LoadStatements). One entry per statement, resident.
	statements map[string]diskEntry
}

// diskEntry locates one on-disk mock: 48 B, held in entries and byName and in
// each index that lists the mock (carry, staged, destinations, statements).
// For spilled HTTP/Mongo mocks the response is a separate blob at
// respOff/respLen; respOff -1 = inline.
type diskEntry struct {
	reqTsNano int64
	// resTsNano is the response time, for the in-flight load (LoadInFlight).
	resTsNano int64
	off       int64
	length    int
	respOff   int64
	respLen   int
}

// responseSpillMinBytes: only elide HTTP bodies at least this big; smaller ones
// stay inline (a serve-time disk read isn't worth it). Mongo always spills.
const responseSpillMinBytes = 8 * 1024

// spilledResponse is the elided response payload, stored apart from the mock so
// the resident window holds only matchers. Exactly one field is populated.
type spilledResponse struct {
	HTTP  *models.HTTPResp
	Mongo []models.MongoResponse
}

// NewDiskMocks creates the store over a fresh temp file; caller must Close it.
func NewDiskMocks(logger *zap.Logger) (*DiskMocks, error) {
	f, err := os.CreateTemp("", "keploy-diskmocks-*.gob")
	if err != nil {
		return nil, fmt.Errorf("disk mocks: create temp file: %w", err)
	}
	path := f.Name()
	if os.Remove(path) == nil {
		path = ""
	}
	return &DiskMocks{
		f:      f,
		path:   path,
		byName: make(map[string]diskEntry),
		logger: logger,
	}, nil
}

// EligibleForDisk reports whether a mock goes to disk: only per-test mocks with
// valid timestamps. Reusable/config and missing-timestamp mocks stay resident
// (the filter routes them without a window check). Call DeriveLifetime first.
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

// EligibleForResponseSpill reports whether a disk-eligible mock has a big
// HTTP/Mongo response worth eliding from the window (matching never reads it).
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

// Add writes a mock to disk and indexes it; caller drops its resident ref after.
// Spill-eligible mocks write their response as a separate blob and encode the
// record with the response elided. Called from the single-threaded ingest loop.
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

	// Fresh encoder per record → each is self-contained and decodable via ReadAt.
	var buf bytes.Buffer
	if spill {
		// Elide the response for encoding, then restore (caller drops m anyway).
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
	e := diskEntry{reqTsNano: m.Spec.ReqTimestampMock.UnixNano(), resTsNano: m.Spec.ResTimestampMock.UnixNano(),
		off: off, length: n, respOff: respOff, respLen: respLen}
	d.byName[m.Name] = e
	d.entries = append(d.entries, e)
	if models.IsCarryOver(m) {
		d.carry = append(d.carry, e)
	}
	if models.IsStagedAtStaging(m) {
		d.staged = append(d.staged, e)
	}
	if k := destinationKey(m); k != "" {
		if first, ok := d.destinations[k]; !ok || e.reqTsNano < first.reqTsNano {
			if d.destinations == nil {
				d.destinations = make(map[string]diskEntry)
			}
			d.destinations[k] = e
		}
	}
	if k := models.StatementKey(m); k != "" {
		key := string(m.Kind) + "\x00" + k
		if first, ok := d.statements[key]; !ok || e.reqTsNano < first.reqTsNano {
			if d.statements == nil {
				d.statements = make(map[string]diskEntry)
			}
			d.statements[key] = e
		}
	}
	d.finalize = false
	return nil
}

// writeLocked appends b at the running offset and advances it. Caller holds mu.
func (d *DiskMocks) writeLocked(b []byte) (int64, int, error) {
	off := d.offset
	n, err := d.f.WriteAt(b, off)
	if err != nil {
		return 0, 0, err
	}
	d.offset += int64(n)
	return off, n, nil
}

// Finalize sorts the index by request timestamp for binary search. Idempotent.
func (d *DiskMocks) Finalize() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalize {
		return
	}
	sort.Slice(d.entries, func(i, j int) bool { return d.entries[i].reqTsNano < d.entries[j].reqTsNano })
	sort.Slice(d.carry, func(i, j int) bool { return d.carry[i].reqTsNano < d.carry[j].reqTsNano })
	sort.Slice(d.staged, func(i, j int) bool { return d.staged[i].reqTsNano < d.staged[j].reqTsNano })
	d.finalize = true
}

// readAt decodes the mock at an entry. The file is immutable after Finalize, so
// ReadAt is concurrency-safe. Elided responses get a lazy serve-time loader.
func (d *DiskMocks) readAt(e diskEntry) (*models.Mock, error) {
	buf := make([]byte, e.length)
	if _, err := d.f.ReadAt(buf, e.off); err != nil {
		return nil, fmt.Errorf("disk mocks: read at %d: %w", e.off, err)
	}
	var m models.Mock
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&m); err != nil {
		return nil, fmt.Errorf("disk mocks: decode at %d: %w", e.off, err)
	}
	if e.respOff >= 0 {
		respOff, respLen := e.respOff, e.respLen
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			return d.loadResponse(respOff, respLen)
		})
	}
	return &m, nil
}

// loadResponse reads and decodes an elided response blob on demand at serve.
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

// LoadWindow returns mocks with request timestamp in [start,end] (inclusive,
// matching SetMocksWithWindow).
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
	// An inverted window selects nothing: the scan below runs from the first
	// entry >= lo while entries are <= hi, so hi < lo yields an empty band and
	// the test replays with no per-test mocks at all — a whole test's worth of
	// hard misses, reported as if the recording were empty.
	//
	// It reaches here from a recorded pair whose response timestamp precedes
	// its request (clock skew on the recording host, most often a container
	// clock stepping). Normalising to the degenerate [lo, lo] keeps the mock
	// whose request time anchors the window instead of dropping everything,
	// and says so once rather than failing silently.
	if hi < lo {
		d.logger.Info("test window is inverted (end before start); clamping to the start instant "+
			"so the window selects the anchoring mock rather than nothing — the recording's "+
			"response timestamp precedes its request, usually a clock step on the recording host",
			zap.Time("start", start), zap.Time("end", end))
		hi = lo
	}
	i := sort.Search(len(d.entries), func(k int) bool { return d.entries[k].reqTsNano >= lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.entries) && d.entries[i].reqTsNano <= hi; i++ {
		sel = append(sel, d.entries[i])
	}
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadInFlight returns the mocks in flight at at that were sent after from:
// request time in (from, at), response time at or after at
// (models.SentInGapAnsweredBy), that the in-flight rule applies to
// (models.InFlightEligible). For a test window starting at at, with from the
// end of the gap before it (models.WindowSchedule.GapBefore), they are the
// calls in flight when the test began. A mock with no response time is not
// one. It scans the entries by request time, from from to at, and decodes
// only those answered at or after at.
func (d *DiskMocks) LoadInFlight(from, at time.Time) ([]*models.Mock, error) {
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
	lo, hi := from.UnixNano(), at.UnixNano()
	i := sort.Search(len(d.entries), func(k int) bool { return d.entries[k].reqTsNano > lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.entries) && d.entries[i].reqTsNano < hi; i++ {
		if e := d.entries[i]; e.resTsNano >= hi && e.resTsNano > 0 {
			sel = append(sel, e)
		}
	}
	d.mu.Unlock()
	decoded, err := d.decodeAll(sel)
	if err != nil {
		return nil, err
	}
	out := decoded[:0]
	for _, m := range decoded {
		if models.InFlightEligible(m) && models.SentInGapAnsweredBy(m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, from, at) {
			out = append(out, m)
		}
	}
	return out, nil
}

// LoadCarryOver returns the carry-over mocks (models.IsCarryOver) with request
// timestamp in [from, to] (inclusive), for the agent's lookahead load of the
// carry-over tier. It reads only those entries, whatever else the range holds.
func (d *DiskMocks) LoadCarryOver(from, to time.Time) ([]*models.Mock, error) {
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
	lo, hi := from.UnixNano(), to.UnixNano()
	i := sort.Search(len(d.carry), func(k int) bool { return d.carry[k].reqTsNano >= lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.carry) && d.carry[i].reqTsNano <= hi; i++ {
		sel = append(sel, d.carry[i])
	}
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadStaged returns, in recorded order, the mocks a
// models.StagingWithStartup need declares with request timestamp in
// [from, to] (inclusive): what the set's staging call stages beside the
// startup band (LoadBefore), from the band's cutoff to the end of the
// staging's window. It reads only those entries.
func (d *DiskMocks) LoadStaged(from, to time.Time) ([]*models.Mock, error) {
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
	lo, hi := from.UnixNano(), to.UnixNano()
	i := sort.Search(len(d.staged), func(k int) bool { return d.staged[k].reqTsNano >= lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.staged) && d.staged[i].reqTsNano <= hi; i++ {
		sel = append(sel, d.staged[i])
	}
	d.mu.Unlock()
	return d.decodeAll(sel)
}

// LoadBefore returns mocks with request timestamp strictly before t — the
// startup band routed to the startup tree; loaded once, kept resident by caller.
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

// LoadStatements returns the statement catalog: for each statement the mocks
// on disk record (models.StatementKey), the first one recorded, in recorded
// order. A client may prepare a statement at app start that its recordings hold
// only inside tests, which the startup band (LoadBefore) does not reach: the
// agent reads these once, when the set is stored, and seeds them on the proxy
// describe-only, so a parser can answer the prepare and never serves them.
func (d *DiskMocks) LoadStatements() ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	sel := make([]diskEntry, 0, len(d.statements))
	for _, e := range d.statements {
		sel = append(sel, e)
	}
	d.mu.Unlock()
	sort.Slice(sel, func(i, j int) bool {
		if sel[i].reqTsNano != sel[j].reqTsNano {
			return sel[i].reqTsNano < sel[j].reqTsNano
		}
		return sel[i].off < sel[j].off
	})
	return d.decodeAll(sel)
}

// LoadDestinations returns, in recorded order, the first mock on disk of
// each recorded destination (destinationKey): what the recording says of its
// destinations, bounded by how many it reached, for the proxy's destination
// rules to read before any of them is staged (Proxy.SeedRecordedDestinations).
func (d *DiskMocks) LoadDestinations() ([]*models.Mock, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("disk mocks: store closed")
	}
	sel := make([]diskEntry, 0, len(d.destinations))
	for _, e := range d.destinations {
		sel = append(sel, e)
	}
	d.mu.Unlock()
	sort.Slice(sel, func(i, j int) bool {
		if sel[i].reqTsNano != sel[j].reqTsNano {
			return sel[i].reqTsNano < sel[j].reqTsNano
		}
		return sel[i].off < sel[j].off
	})
	return d.decodeAll(sel)
}

// LoadByNames returns mocks by name (mapping-based replay ignores the window).
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

// LoadAll returns every on-disk mock (lax mode needs the full per-test set).
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

// EarliestReqTs returns the earliest on-disk request timestamp (zero if empty);
// folded into the replay freeze anchor since boot mocks often live on disk.
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

// Len returns the on-disk mock count (diagnostics/tests).
func (d *DiskMocks) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// SpilledResponses returns how many on-disk mocks keep their response apart
// (EligibleForResponseSpill), to be loaded only when served (diagnostics/tests).
func (d *DiskMocks) SpilledResponses() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, e := range d.entries {
		if e.respOff >= 0 {
			n++
		}
	}
	return n
}

// DiskBytes returns total bytes written to the file (diagnostics/tests).
func (d *DiskMocks) DiskBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offset
}

// Close closes and removes the temp file. Idempotent; later ops then fail.
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
		if err := removeIfNamed(d.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func removeIfNamed(path string) error {
	if path == "" {
		return nil
	}
	return os.Remove(path)
}
