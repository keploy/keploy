package manager

import (
	"maps"
	"slices"
	"strconv"
	"strings"
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
// falls back to a bounded RAM tail behind the spill up to queueBound, and past
// queueBound preserves SyncMockManager's existing fail-closed drop and
// test-case leave-out path.
type OutQueueSpill interface {
	// NewLane creates an independent FIFO spill for one output channel lane,
	// or returns nil when spilling is disabled.
	NewLane() OutQueueSpillLane

	// RAMLimit returns the process-wide queued byte threshold at which the
	// output queue begins spilling instead of growing in RAM. A quarter of
	// queueBound is always reserved for spilled-item bookkeeping, so values
	// <= 0 or above queueBound - queueBound/4 are clamped to
	// queueBound - queueBound/4.
	RAMLimit() int64
}

// OutQueueSpillLane is one outLane's FIFO overflow store. All methods except
// those called on a lane returned by Detach are serialized by the lane's own
// spill mutex (outside SyncMockManager.outQueueMu so disk I/O and encryption
// do not block RAM queue operations or OutQueued). Put and Peek must be
// bounded and must never wait on the output channel consumer.
type OutQueueSpillLane interface {
	// Put appends (mock, owner, size) to the tail of the lane's spill FIFO.
	// Returns false if spill capacity is exhausted or unavailable so the
	// caller falls back to the RAM tail (up to queueBound) or
	// MockDroppedOutputFull.
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

// spillMetaReserveDivisor reserves 1/4 of queueBound for spilled-item
// bookkeeping so RAM head entries never starve spill metadata when
// RAMLimit <= 0 or RAMLimit >= queueBound.
const spillMetaReserveDivisor int64 = 4

// spillRAMCap clamps ramLimit so spilled-item bookkeeping always retains
// bound/spillMetaReserveDivisor headroom inside bound.
func spillRAMCap(bound, ramLimit int64) int64 {
	maxHead := bound - bound/spillMetaReserveDivisor
	if maxHead < 0 {
		maxHead = 0
	}
	if ramLimit <= 0 || ramLimit > maxHead {
		return maxHead
	}
	return ramLimit
}

// spillItemMetaBaseBytes covers the compact spillItemMeta struct (~136 B) plus
// slice growth slack per spilled mock. Metadata maps are interned per lane
// (spillInternedMap) so repeating pod metadata costs ~0 B per additional mock.
const (
	spillItemMetaBaseBytes int64 = 160
	spillItemDoneBytes     int64 = 32
	spillItemAsyncBase     int64 = 64
	spillMapHeaderBytes    int64 = 256
	// spillMapEntryBytes uses mockSize's 2*16+mapEntryOverhead (48 B) plus
	// 16 B bucket-load-factor slack (Go maps keep <=6.5/8 occupancy) so even
	// unique 50+ key maps are bounded conservatively.
	spillMapEntryBytes   int64 = 2*16 + mapEntryOverhead + 16
	maxSpillInternedMaps       = 256
)

const (
	spillFlagStartup  uint8 = 1 << 0
	spillFlagUndeliv  uint8 = 1 << 1
	spillFlagReusable uint8 = 1 << 2
)

// spillInternedMap holds one immutable cloned metadata map shared by all
// spilled items on a lane that carry identical key/value pairs. Guarded by
// lane.spillMu.
type spillInternedMap struct {
	m      map[string]string
	bytes  int64
	refs   int64
	key    string
	pooled bool
}

// spillItemMeta retains only lightweight attribution and fail-closed metadata
// for each spilled mock (never payload bytes) so a corrupt spill read can
// still attribute drops (ConnectionID, Metadata) and open the exact same
// per-test or shared orphan leave-out spans as a RAM drop.
type spillItemMeta struct {
	owner     string
	done      func(delivered bool, epoch uint64)
	kind      models.Kind
	name      string
	connID    string
	md        *spillInternedMap
	reqTime   time.Time
	async     *models.AsyncMeta
	lifetime  models.Lifetime
	flags     uint8
	metaBytes int64 // per-item bytes charged (excludes shared md.bytes)
}

func spillMetadataMapBytes(md map[string]string) int64 {
	if len(md) == 0 {
		return 0
	}
	n := spillMapHeaderBytes
	for k, v := range md {
		n += int64(len(k)+len(v)) + spillMapEntryBytes
	}
	return n
}

func spillMetadataKey(md map[string]string) string {
	keys := make([]string, 0, len(md))
	total := 0
	for k, v := range md {
		keys = append(keys, k)
		total += len(k) + len(v) + 16
	}
	slices.Sort(keys)
	var sb strings.Builder
	sb.Grow(total)
	var numBuf [20]byte
	for _, k := range keys {
		v := md[k]
		sb.Write(strconv.AppendInt(numBuf[:0], int64(len(k)), 10))
		sb.WriteByte(':')
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.Write(strconv.AppendInt(numBuf[:0], int64(len(v)), 10))
		sb.WriteByte(':')
		sb.WriteString(v)
		sb.WriteByte(';')
	}
	return sb.String()
}

// spillItemBaseRamBytes computes the per-item RAM charge for a spilled mock
// without cloning metadata maps (safe to call under outQueueMu).
func spillItemBaseRamBytes(mock *models.Mock, owner string, done func(delivered bool, epoch uint64)) int64 {
	n := spillItemMetaBaseBytes + int64(len(owner))
	if done != nil {
		n += spillItemDoneBytes
	}
	if mock != nil {
		n += int64(len(mock.Name) + len(mock.ConnectionID) + len(mock.Kind))
		if mock.Spec.Async != nil {
			n += spillItemAsyncBase + int64(len(mock.Spec.Async.Lane)+len(mock.Spec.Async.AnchorAfter))
		}
	}
	return n
}

func (im spillItemMeta) isStartup() bool { return im.flags&spillFlagStartup != 0 }
func (im spillItemMeta) undeliv() bool   { return im.flags&spillFlagUndeliv != 0 }
func (im spillItemMeta) reusable() bool  { return im.flags&spillFlagReusable != 0 }

func buildSpillItemMeta(mock *models.Mock, owner string, done func(delivered bool, epoch uint64), md *spillInternedMap, itemBytes int64) spillItemMeta {
	meta := spillItemMeta{
		owner:     owner,
		done:      done,
		md:        md,
		metaBytes: itemBytes,
	}
	if mock != nil {
		meta.kind = mock.Kind
		meta.name = mock.Name
		meta.connID = mock.ConnectionID
		meta.reqTime = mock.Spec.ReqTimestampMock
		meta.lifetime = mock.TestModeInfo.Lifetime
		if mock.TestModeInfo.IsStartup {
			meta.flags |= spillFlagStartup
		}
		if mock.IsUndelivered() {
			meta.flags |= spillFlagUndeliv
		}
		if attributedReusable(mock) {
			meta.flags |= spillFlagReusable
		}
		if mock.Spec.Async != nil {
			a := *mock.Spec.Async
			meta.async = &a
		}
	}
	return meta
}

func (im spillItemMeta) stubMock() *models.Mock {
	var md map[string]string
	if im.md != nil && len(im.md.m) > 0 {
		md = maps.Clone(im.md.m)
	}
	var async *models.AsyncMeta
	if im.async != nil {
		a := *im.async
		async = &a
	}
	m := &models.Mock{
		Kind:         im.kind,
		Name:         im.name,
		ConnectionID: im.connID,
		Spec: models.MockSpec{
			Metadata:         md,
			ReqTimestampMock: im.reqTime,
			Async:            async,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:  im.lifetime,
			IsStartup: im.isStartup(),
		},
	}
	if im.undeliv() {
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
