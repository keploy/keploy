package manager

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// readThroughCache tracks canonical read-only keys rescued from sampled-out,
// static-deduped, or out-of-window requests during a recording session.
//
// Invariants:
//   - Only read-only, response-bearing mocks (models.ReadThroughKey) may enter.
//   - Never mutates the caller's original *models.Mock (always DeepCopies before
//     tagging and renaming) so loss accounting and drop diagnostics stay exact.
//   - Deduplicated by canonical read key: 10,000 identical reads store 1 entry.
//   - Bounded by both entry count (maxEntries) and total bytes (maxBytes) so
//     high-cardinality sampled-out reads cannot OOM the agent or bloat mocks.yaml.
//   - Guarded by SyncMockManager.mu.
type readThroughCache struct {
	seen       map[string]string
	conflicted map[string]struct{}
	bytes      int64
	maxEntries int
	maxBytes   int64
	pending    []*models.Mock

	rescued uint64
	deduped uint64
	capped  uint64
}

func (c *readThroughCache) limits() (int, int64) {
	maxEntries := c.maxEntries
	if maxEntries <= 0 {
		maxEntries = models.DefaultReadThroughMaxEntries
	}
	maxBytes := c.maxBytes
	if maxBytes <= 0 {
		maxBytes = models.DefaultReadThroughMaxBytes
	}
	return maxEntries, maxBytes
}

func (c *readThroughCache) reset() {
	c.seen = nil
	c.conflicted = nil
	c.bytes = 0
	c.pending = nil
	c.rescued = 0
	c.deduped = 0
	c.capped = 0
}

// offer inspects mk, a per-test mock about to be discarded because no kept
// test window owns it. If mk is an eligible read-only mock whose canonical key
// has not yet been seen and fits within the entry/byte bounds, offer returns
// a tagged DeepCopy carrying a deterministic read-through name without ever
// mutating mk. If a later recording for an already-seen canonical key carries
// a semantically different response (e.g., two separate servers sharing port
// and schema, or a cache value that changed during recording), offer emits
// the conflicting copy once under a "-conflict" suffix so replay fails closed
// on that key rather than silently serving the first response.
func (c *readThroughCache) offer(mk *models.Mock, logger *zap.Logger) (*models.Mock, bool) {
	if mk == nil {
		return nil, false
	}
	key, ok := models.ReadThroughKey(mk)
	if !ok {
		return nil, false
	}
	digest := models.ReadThroughResponseDigest(mk)
	if prevDigest, exists := c.seen[key]; exists {
		if _, poisoned := c.conflicted[key]; poisoned || prevDigest == digest {
			c.deduped++
			return nil, false
		}
		if c.conflicted == nil {
			c.conflicted = make(map[string]struct{})
		}
		c.conflicted[key] = struct{}{}
		cloned := mk.DeepCopy()
		if !models.MarkReadThrough(cloned) {
			return nil, false
		}
		cloned.Name = models.ReadThroughConflictMockName(key)
		if logger != nil {
			logger.Warn("syncMock: conflicting responses captured for the same read-through key; marking key fail-closed for replay",
				zap.String("kind", string(mk.Kind)),
				zap.String("mockName", cloned.Name),
				zap.String("next_step", "separate distinct servers onto distinct ports/databases or avoid relying on read-through for mutable responses"))
		}
		return cloned, true
	}
	maxEntries, maxBytes := c.limits()
	sz := mockSize(mk)
	if len(c.seen) >= maxEntries || c.bytes+sz > maxBytes {
		c.capped++
		if c.capped == 1 && logger != nil {
			logger.Warn("syncMock: session read-through cache reached capacity; further unique sampled-out reads will be dropped",
				zap.Int("entries", len(c.seen)),
				zap.Int("max_entries", maxEntries),
				zap.Int64("bytes", c.bytes),
				zap.Int64("max_bytes", maxBytes),
				zap.String("next_step", "split recording into shorter sessions if more distinct cache-warming reads must be retained"))
		}
		return nil, false
	}
	cloned := mk.DeepCopy()
	if !models.MarkReadThrough(cloned) {
		return nil, false
	}
	if c.seen == nil {
		c.seen = make(map[string]string, 64)
	}
	c.seen[key] = digest
	c.bytes += sz
	c.rescued++
	cloned.Name = models.ReadThroughMockName(key)
	return cloned, true
}

// setReadThroughLimits overrides the entry and byte caps for the session
// read-through cache (used by tests). Zero or negative values restore the
// defaults (models.DefaultReadThroughMaxEntries / models.DefaultReadThroughMaxBytes).
func (m *SyncMockManager) setReadThroughLimits(maxEntries int, maxBytes int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.readThrough.maxEntries = maxEntries
	m.readThrough.maxBytes = maxBytes
	m.mu.Unlock()
}

// readThroughStats returns the current session read-through cache counters
// (rescued, deduped, capped, unique entries, retained bytes).
func (m *SyncMockManager) readThroughStats() (rescued, deduped, capped uint64, entries int, bytes int64) {
	if m == nil {
		return 0, 0, 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readThrough.rescued, m.readThrough.deduped, m.readThrough.capped, len(m.readThrough.seen), m.readThrough.bytes
}

// offerReadThroughLocked rescues a tagged copy of mk into the deduplicated
// session read-through tier without mutating mk or interfering with drop/cut
// accounting. Caller holds m.mu.
func (m *SyncMockManager) offerReadThroughLocked(mk *models.Mock, outChanBound bool, mocksToSend *[]ownedMock) bool {
	if !outChanBound || mk == nil {
		return false
	}
	cloned, ok := m.readThrough.offer(mk, m.dropLogger())
	if !ok {
		return false
	}
	*mocksToSend = append(*mocksToSend, ownedMock{mock: cloned})
	return true
}

// queueReadThroughLocked queues a tagged copy of mk from a context that does
// not directly hold a mocksToSend slice (e.g. dropHeldLocked); the next drain
// (drainReadThroughPendingLocked) emits it on outChan. Never mutates mk.
// Caller holds m.mu.
func (m *SyncMockManager) queueReadThroughLocked(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	cloned, ok := m.readThrough.offer(mk, m.dropLogger())
	if !ok {
		return false
	}
	m.readThrough.pending = append(m.readThrough.pending, cloned)
	return true
}

// drainReadThroughPendingLocked moves any queued read-through mocks into
// mocksToSend when outChan is bound. Caller holds m.mu.
func (m *SyncMockManager) drainReadThroughPendingLocked(outChanBound bool, mocksToSend *[]ownedMock) {
	if !outChanBound || len(m.readThrough.pending) == 0 {
		return
	}
	for _, mk := range m.readThrough.pending {
		*mocksToSend = append(*mocksToSend, ownedMock{mock: mk})
	}
	clear(m.readThrough.pending)
	m.readThrough.pending = m.readThrough.pending[:0]
}

// flushUnownedReadThrough rescues copies of remaining unowned read-only mocks
// still sitting in m.buffer (or queued in readThrough.pending) at graceful
// recording stop (CloseOutChan), before the output channel is sealed.
//
// Respects open in-flight windows (openFromLocked) and leaves m.buffer intact
// so in-flight requests waiting for a keep decision still observe their own
// pending mocks via bufferPendingInLocked.
func (m *SyncMockManager) flushUnownedReadThrough() {
	var mocksToSend []ownedMock
	m.mu.Lock()
	outChanBound, _ := m.outChanStatus()
	if !outChanBound {
		m.mu.Unlock()
		return
	}
	m.drainReadThroughPendingLocked(outChanBound, &mocksToSend)
	openFrom, open := m.openFromLocked()
	for i := 0; i < len(m.buffer); i++ {
		mk := m.buffer[i]
		if mk == nil {
			continue
		}
		if open && !mk.Spec.ReqTimestampMock.IsZero() && !mk.Spec.ReqTimestampMock.Before(openFrom) {
			continue
		}
		m.offerReadThroughLocked(mk, outChanBound, &mocksToSend)
	}
	m.noteTaken(mocksToSend)
	rescued, deduped, capped, entries, retainedBytes := m.readThrough.rescued, m.readThrough.deduped, m.readThrough.capped, len(m.readThrough.seen), m.readThrough.bytes
	logger := m.dropLogger()
	m.mu.Unlock()
	if logger != nil && (rescued > 0 || deduped > 0 || capped > 0) {
		logger.Debug("syncMock: session read-through cache summary",
			zap.Uint64("rescued", rescued),
			zap.Uint64("deduped", deduped),
			zap.Uint64("capped", capped),
			zap.Int("entries", entries),
			zap.Int64("bytes", retainedBytes))
	}
	m.sendTaken(mocksToSend)
}
