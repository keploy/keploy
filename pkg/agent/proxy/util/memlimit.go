package util

import (
	"errors"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.uber.org/zap"
)

// ErrMemoryLimitExceeded is a sentinel error returned when proxy memory
// recording is paused. Kept for backward compatibility with the
// integrations module.
//
// Deprecated: Use ErrRecordingPausedDueToMemoryPressure and
// memoryguard.IsRecordingPaused instead.
var ErrMemoryLimitExceeded = errors.New("proxy memory limit exceeded, recording dropped")

// MemoryLimiter is a backward-compatibility shim.
//
// The actual memory limit logic now lives in the memoryguard package,
// which monitors container cgroup usage and sets a global pause flag.
// This type is retained so that the integrations module (which still
// references *util.MemoryLimiter) continues to compile while it is
// migrated to call memoryguard.IsRecordingPaused() directly.
//
// All TryAcquire calls return false when the memory guard has signalled
// that recording is paused, and true otherwise.  Release is a no-op.
//
// Deprecated: Use memoryguard.IsRecordingPaused().
type MemoryLimiter struct {
	_ struct{} // zero-size; all state comes from the global memory guard
}

// NewMemoryLimiter returns a new MemoryLimiter.
// The limit and logger parameters are accepted for API compatibility but
// are otherwise unused; the real limit is managed by memoryguard.Start.
//
// Deprecated: Use memoryguard.Start and memoryguard.IsRecordingPaused.
func NewMemoryLimiter(_ int64, _ *zap.Logger) *MemoryLimiter {
	return &MemoryLimiter{}
}

// TryAcquire returns false when the global memory guard is paused,
// true otherwise.  The byte count n is ignored.
func (ml *MemoryLimiter) TryAcquire(_ int64) bool {
	if ml == nil {
		return true
	}
	return !memoryguard.IsRecordingPaused()
}

// Release is a no-op; accounting is handled by memoryguard.
func (ml *MemoryLimiter) Release(_ int64) {}

// IsExceeded reports whether the memory guard is currently paused.
func (ml *MemoryLimiter) IsExceeded() bool {
	if ml == nil {
		return false
	}
	return memoryguard.IsRecordingPaused()
}
