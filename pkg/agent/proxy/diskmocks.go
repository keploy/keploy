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
}

// diskEntry locates one on-disk mock (~56 B resident). For spilled HTTP/Mongo
// mocks the response is a separate blob at respOff/respLen; respOff -1 = inline.
type diskEntry struct {
	reqTsNano int64
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
	return &DiskMocks{
		f:      f,
		path:   f.Name(),
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
	e := diskEntry{reqTsNano: m.Spec.ReqTimestampMock.UnixNano(), off: off, length: n, respOff: respOff, respLen: respLen}
	d.byName[m.Name] = e
	d.entries = append(d.entries, e)
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
	i := sort.Search(len(d.entries), func(k int) bool { return d.entries[k].reqTsNano >= lo })
	sel := make([]diskEntry, 0)
	for ; i < len(d.entries) && d.entries[i].reqTsNano <= hi; i++ {
		sel = append(sel, d.entries[i])
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
		if err := os.Remove(d.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
