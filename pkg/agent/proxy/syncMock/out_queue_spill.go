package manager

import (
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// OutQueueSpill is an optional spill factory for SyncMockManager's output
// queue (outLane). When installed via SetOutQueueSpill or
// SetDefaultOutQueueSpill, mocks that would otherwise overflow the in-memory
// output queue (MockDroppedOutputFull) are spilled to a per-lane FIFO and
// streamed back one item at a time by drainOutQueue in exact FIFO order.
//
// Implementations live outside OSS (for example, the encrypted disk spill in
// enterprise). Returning false from Put (spill disabled, disk full, or broken)
// preserves SyncMockManager's existing fail-closed drop and test-case leave-out
// path.
type OutQueueSpill interface {
	// NewLane creates an independent FIFO spill for one output channel lane,
	// or returns nil when spilling is disabled.
	NewLane() OutQueueSpillLane

	// RAMLimit returns the process-wide queued byte threshold at which the
	// output queue begins spilling instead of growing in RAM. A value <= 0
	// uses the standard queueBound; values above queueBound are clamped to it.
	RAMLimit() int64
}

// OutQueueSpillLane is one outLane's FIFO overflow store. All methods except
// those called on a lane returned by Detach run under
// SyncMockManager.outQueueMu.
type OutQueueSpillLane interface {
	// Put appends (mock, owner, size) to the tail of the lane's spill FIFO.
	// Returns false if spill capacity is exhausted or unavailable so the
	// caller falls back to MockDroppedOutputFull.
	Put(mock *models.Mock, owner string, size int64) bool

	// Len reports how many mocks are currently held in this spill lane.
	Len() int

	// Peek returns the oldest spilled mock without removing it. Returns
	// (nil, "", 0, nil) when Len() == 0. A non-nil error indicates an
	// unreadable/corrupt spill; the caller discards the lane and fails closed
	// for every unread mock.
	Peek() (mock *models.Mock, owner string, size int64, err error)

	// Pop removes the oldest spilled mock previously returned by Peek.
	Pop()

	// Detach moves all currently spilled items to a standalone lane for
	// lock-free draining (e.g. giveUpOutQueue) and leaves this lane empty and
	// closed to further Puts. Returns nil when empty.
	Detach() OutQueueSpillLane

	// Discard releases all resources held by this lane and returns how many
	// unread mocks were discarded.
	Discard() int
}

type spillItemMeta struct {
	owner     string
	done      func(delivered bool, epoch uint64)
	kind      models.Kind
	name      string
	mockType  string
	reqTime   time.Time
	lifetime  models.Lifetime
	isStartup bool
	undeliv   bool
}

func newSpillItemMeta(mock *models.Mock, owner string, done func(delivered bool, epoch uint64)) spillItemMeta {
	meta := spillItemMeta{owner: owner, done: done}
	if mock != nil {
		meta.kind = mock.Kind
		meta.name = mock.Name
		if mock.Spec.Metadata != nil {
			meta.mockType = mock.Spec.Metadata["type"]
		}
		meta.reqTime = mock.Spec.ReqTimestampMock
		meta.lifetime = mock.TestModeInfo.Lifetime
		meta.isStartup = mock.TestModeInfo.IsStartup
		meta.undeliv = mock.IsUndelivered()
	}
	return meta
}

func (im spillItemMeta) stubMock() *models.Mock {
	var md map[string]string
	if im.mockType != "" {
		md = map[string]string{"type": im.mockType}
	}
	m := &models.Mock{
		Kind: im.kind,
		Name: im.name,
		Spec: models.MockSpec{
			Metadata:         md,
			ReqTimestampMock: im.reqTime,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:  im.lifetime,
			IsStartup: im.isStartup,
		},
	}
	if im.undeliv {
		m.MarkUndelivered()
	}
	return m
}

var defaultOutQueueSpill atomic.Pointer[OutQueueSpill]

// SetDefaultOutQueueSpill installs the process-wide default OutQueueSpill used
// by SyncMockManager instances that have not configured a per-instance spill
// via SetOutQueueSpill (including syncMock.Get() and syncMock.New()). Passing
// nil clears the default.
func SetDefaultOutQueueSpill(s OutQueueSpill) {
	if s == nil {
		defaultOutQueueSpill.Store(nil)
		return
	}
	defaultOutQueueSpill.Store(&s)
}

func getDefaultOutQueueSpill() OutQueueSpill {
	if p := defaultOutQueueSpill.Load(); p != nil {
		return *p
	}
	return nil
}

// SetOutQueueSpill installs an OutQueueSpill for this SyncMockManager
// instance, overriding any process-wide default. Passing nil falls back to the
// process-wide default (if any).
func (m *SyncMockManager) SetOutQueueSpill(s OutQueueSpill) {
	if m == nil {
		return
	}
	if s == nil {
		m.outSpill.Store(nil)
		return
	}
	m.outSpill.Store(&s)
}

func (m *SyncMockManager) activeOutSpill() OutQueueSpill {
	if m != nil {
		if p := m.outSpill.Load(); p != nil {
			return *p
		}
	}
	return getDefaultOutQueueSpill()
}
