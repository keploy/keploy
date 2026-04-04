package util

import (
	"errors"
	"sync/atomic"

	"go.uber.org/zap"
)

// ErrMemoryLimitExceeded is a sentinel error returned by ReadBuffConn
// when the MemoryLimiter's TryAcquire call fails (cumulative buffered
// bytes exceed the configured limit). Parsers should catch this via
// errors.Is and fall back to passthrough mode (continue forwarding
// traffic without decoding or recording). Custom-read parsers can
// also check IsExceeded() directly.
var ErrMemoryLimitExceeded = errors.New("proxy memory limit exceeded, recording dropped")

// MemoryLimiter tracks total buffered bytes across all proxy
// connections and provides non-blocking memory gating for record mode.
//
// When the configured limit is exceeded, parsers are expected to stop
// decoding/recording and fall back to transparent passthrough. This
// ensures zero latency impact on the user's application — forwarding
// always continues, only recording is dropped.
//
// All methods are nil-receiver safe: a nil MemoryLimiter behaves as
// unlimited (all TryAcquire calls succeed, IsExceeded returns false).
// This means callers never need to nil-check before calling.
type MemoryLimiter struct {
	used     atomic.Int64
	limit    int64
	logger   *zap.Logger
	exceeded atomic.Bool
}

// NewMemoryLimiter creates a limiter with the given byte limit.
// A limit of 0 means unlimited (the limiter is effectively disabled).
func NewMemoryLimiter(limit int64, logger *zap.Logger) *MemoryLimiter {
	if limit <= 0 {
		return nil // nil is the "unlimited" sentinel
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &MemoryLimiter{
		limit:  limit,
		logger: logger,
	}
}

// TryAcquire attempts to reserve n bytes. Returns true if the
// reservation succeeded (usage was under the limit), false if the
// limit has been reached. This method never blocks.
func (ml *MemoryLimiter) TryAcquire(n int64) bool {
	if ml == nil {
		return true
	}
	newUsed := ml.used.Add(n)
	if newUsed > ml.limit {
		// Roll back — we went over.
		ml.used.Add(-n)
		if !ml.exceeded.Swap(true) {
			// First time exceeding: log once.
			ml.logger.Debug("proxy memory limit reached, parsers will drop recording; increase record.maxBufferMemoryMB or set to 0 to disable",
				zap.Int64("limit_bytes", ml.limit),
				zap.Int64("current_bytes", newUsed-n))
		}
		return false
	}
	return true
}

// Release returns n bytes to the available pool.
func (ml *MemoryLimiter) Release(n int64) {
	if ml == nil {
		return
	}
	newUsed := ml.used.Add(-n)
	if newUsed < 0 {
		// Guard against underflow from double-release bugs.
		ml.used.Store(0)
		newUsed = 0
	}
	// Clear the exceeded flag when usage drops below 90% of limit
	// (hysteresis to prevent thrashing).
	if ml.exceeded.Load() && newUsed < ml.limit*9/10 {
		ml.exceeded.Store(false)
		ml.logger.Debug("proxy memory usage below threshold, recording can resume",
			zap.Int64("current_bytes", newUsed),
			zap.Int64("limit_bytes", ml.limit))
	}
}

// IsExceeded returns true if the memory limit is currently exceeded.
// This is a fast atomic check suitable for hot-path polling.
func (ml *MemoryLimiter) IsExceeded() bool {
	if ml == nil {
		return false
	}
	return ml.exceeded.Load()
}

// Usage returns the current number of tracked bytes.
func (ml *MemoryLimiter) Usage() int64 {
	if ml == nil {
		return 0
	}
	return ml.used.Load()
}

// Limit returns the configured byte limit.
func (ml *MemoryLimiter) Limit() int64 {
	if ml == nil {
		return 0
	}
	return ml.limit
}
