package models

import (
	"slices"
	"time"
)

// DerivedBgCollectorConfig configures a bounded DerivedBgCollector.
type DerivedBgCollectorConfig struct {
	// MaxEntries caps total retained recordings across all shapes.
	MaxEntries int
	// MaxBytes caps total estimated decoded bytes across all retained
	// recordings. Zero disables the byte cap (useful when only lightweight
	// metadata entries are retained).
	MaxBytes int64
	// MaxPerShape caps retained samples per canonical request shape (keeping
	// the most recent samples of each shape).
	MaxPerShape int
	// MinPerShape is the minimum number of samples a multi-sample shape keeps
	// under global capacity pressure (e.g. learn.MinEpochReads+1 = 4 so
	// bare-epoch clock tracking keeps enough distinct observations). When
	// global eviction reaches a shape with <= MinPerShape samples, the entire
	// stale shape is dropped by recency instead of trimming it below
	// MinPerShape. Zero disables whole-shape protection.
	MinPerShape int
	// HeadReserve protects the earliest leading recordings (up to 1/4 of
	// MaxEntries) from being evicted by later high-cardinality traffic.
	HeadReserve int
	// HeadReserveBytes caps the protected leading prefix in bytes (up to 1/4
	// of MaxBytes when MaxBytes > 0).
	HeadReserveBytes int64
}

// DerivedBgItem is one retained item inside DerivedBgCollector.
type DerivedBgItem struct {
	Recording BackgroundRecording
	Entry     MockEntry
	Reason    string
	Shape     string
	At        time.Time
	Bytes     int64
}

// DerivedBgStats summarizes capacity evictions performed by a
// DerivedBgCollector so callers can log when recordings were dropped.
type DerivedBgStats struct {
	Kept            int
	DistinctShapes  int
	DroppedByCap    int
	DroppedShapes   int
	TrimmedPerShape int
}

type bgCollectorNode struct {
	item DerivedBgItem
	head bool

	prev, next   *bgCollectorNode
	sPrev, sNext *bgCollectorNode
}

type bgCollectorList struct {
	head, tail *bgCollectorNode
	count      int
}

func (l *bgCollectorList) pushBack(n *bgCollectorNode) {
	n.prev = l.tail
	n.next = nil
	if l.tail != nil {
		l.tail.next = n
	} else {
		l.head = n
	}
	l.tail = n
	l.count++
}

func (l *bgCollectorList) remove(n *bgCollectorNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
	l.count--
}

const (
	bgLRUTierNone uint8 = iota
	bgLRUTierUnderMin
	bgLRUTierAtMin
)

type bgCollectorShapeBucket struct {
	shape     string
	head      *bgCollectorNode
	tail      *bgCollectorNode
	count     int
	tailCount int

	// Intrusive LRU pointers inside DerivedBgCollector.underMinLRU or atMinLRU.
	inTailLRU bool
	lruTier   uint8
	lruPrev   *bgCollectorShapeBucket
	lruNext   *bgCollectorShapeBucket
}

func (b *bgCollectorShapeBucket) pushBack(n *bgCollectorNode) {
	n.sPrev = b.tail
	n.sNext = nil
	if b.tail != nil {
		b.tail.sNext = n
	} else {
		b.head = n
	}
	b.tail = n
	b.count++
	if !n.head {
		b.tailCount++
	}
}

func (b *bgCollectorShapeBucket) remove(n *bgCollectorNode) {
	if n.sPrev != nil {
		n.sPrev.sNext = n.sNext
	} else {
		b.head = n.sNext
	}
	if n.sNext != nil {
		n.sNext.sPrev = n.sPrev
	} else {
		b.tail = n.sPrev
	}
	n.sPrev, n.sNext = nil, nil
	b.count--
	if !n.head {
		b.tailCount--
	}
}

func (b *bgCollectorShapeBucket) firstNonHead() *bgCollectorNode {
	for cur := b.head; cur != nil; cur = cur.sNext {
		if !cur.head {
			return cur
		}
	}
	return nil
}

type bgShapeLRU struct {
	head, tail *bgCollectorShapeBucket
}

func (l *bgShapeLRU) remove(b *bgCollectorShapeBucket) {
	if !b.inTailLRU {
		return
	}
	if b.lruPrev != nil {
		b.lruPrev.lruNext = b.lruNext
	} else {
		l.head = b.lruNext
	}
	if b.lruNext != nil {
		b.lruNext.lruPrev = b.lruPrev
	} else {
		l.tail = b.lruPrev
	}
	b.lruPrev, b.lruNext = nil, nil
	b.inTailLRU = false
}

func (l *bgShapeLRU) moveToBack(b *bgCollectorShapeBucket) {
	if b.inTailLRU {
		if l.tail == b {
			return
		}
		l.remove(b)
	}
	b.lruPrev = l.tail
	b.lruNext = nil
	if l.tail != nil {
		l.tail.lruNext = b
	} else {
		l.head = b
	}
	l.tail = b
	b.inTailLRU = true
}

// DerivedBgCollector maintains a strictly bounded set of derived background
// recordings shared between the agent (Pass 1 in-RAM derivation) and the
// replayer (persisting window-derived startup:/worker: entries to
// mappings.yaml):
//   - deduplicates recurring background traffic by canonical request shape,
//     retaining at most MaxPerShape latest samples per shape;
//   - protects an optional leading boot/startup prefix (up to HeadReserve
//     entries and HeadReserveBytes, clamped to at most 1/4 of global caps);
//   - enforces global MaxEntries and MaxBytes ceilings by evicting the least-
//     recently-seen non-head shape first, dropping whole stale shapes whenever
//     their count is <= MinPerShape so active pollers are never cut below the
//     minimum samples needed for bare-epoch clock tracking.
type DerivedBgCollector struct {
	maxEntries       int
	maxBytes         int64
	maxPerShape      int
	minPerShape      int
	headReserve      int
	headReserveBytes int64

	headList    bgCollectorList
	tailList    bgCollectorList
	underMinLRU bgShapeLRU
	atMinLRU    bgShapeLRU
	headBytes   int64
	totalBytes  int64
	headSealed  bool
	byShape     map[string]*bgCollectorShapeBucket

	droppedByCap    int
	droppedShapes   int
	trimmedPerShape int
}

// NewDerivedBgCollector builds a bounded collector with cfg.
func NewDerivedBgCollector(cfg DerivedBgCollectorConfig) *DerivedBgCollector {
	if cfg.MaxEntries < 1 {
		cfg.MaxEntries = 1
	}
	if cfg.MaxPerShape < 1 {
		cfg.MaxPerShape = 1
	}
	if cfg.MinPerShape > cfg.MaxPerShape {
		cfg.MinPerShape = cfg.MaxPerShape
	}
	if cfg.MinPerShape < 0 {
		cfg.MinPerShape = 0
	}
	if cfg.HeadReserve < 0 {
		cfg.HeadReserve = 0
	}
	if capHead := cfg.MaxEntries / 4; cfg.HeadReserve > capHead {
		cfg.HeadReserve = capHead
	}
	if cfg.MaxBytes > 0 {
		if cfg.HeadReserveBytes < 0 {
			cfg.HeadReserveBytes = 0
		}
		if capHeadBytes := cfg.MaxBytes / 4; cfg.HeadReserveBytes > capHeadBytes {
			cfg.HeadReserveBytes = capHeadBytes
		}
	} else {
		cfg.HeadReserveBytes = 0
	}
	return &DerivedBgCollector{
		maxEntries:       cfg.MaxEntries,
		maxBytes:         cfg.MaxBytes,
		maxPerShape:      cfg.MaxPerShape,
		minPerShape:      cfg.MinPerShape,
		headReserve:      cfg.HeadReserve,
		headReserveBytes: cfg.HeadReserveBytes,
		byShape:          make(map[string]*bgCollectorShapeBucket),
	}
}

// TotalCount returns the total number of retained items across head and tail.
func (c *DerivedBgCollector) TotalCount() int {
	return c.headList.count + c.tailList.count
}

// HeadCount returns the number of retained items in the protected head prefix.
func (c *DerivedBgCollector) HeadCount() int {
	return c.headList.count
}

// TailCount returns the number of retained items in the recency tail.
func (c *DerivedBgCollector) TailCount() int {
	return c.tailList.count
}

// TotalBytes returns the estimated decoded bytes retained.
func (c *DerivedBgCollector) TotalBytes() int64 {
	return c.totalBytes
}

// ShapeCount returns the number of distinct shapes currently retained.
func (c *DerivedBgCollector) ShapeCount() int {
	return len(c.byShape)
}

// Stats returns eviction and retention counters for logging.
func (c *DerivedBgCollector) Stats() DerivedBgStats {
	return DerivedBgStats{
		Kept:            c.TotalCount(),
		DistinctShapes:  len(c.byShape),
		DroppedByCap:    c.droppedByCap,
		DroppedShapes:   c.droppedShapes,
		TrimmedPerShape: c.trimmedPerShape,
	}
}

func (c *DerivedBgCollector) removeFromTailLRU(b *bgCollectorShapeBucket) {
	if b == nil || !b.inTailLRU {
		return
	}
	switch b.lruTier {
	case bgLRUTierUnderMin:
		c.underMinLRU.remove(b)
	case bgLRUTierAtMin:
		c.atMinLRU.remove(b)
	}
	b.lruTier = bgLRUTierNone
}

func (c *DerivedBgCollector) placeInTailLRU(b *bgCollectorShapeBucket, touch bool) {
	if b == nil {
		return
	}
	if b.tailCount == 0 {
		c.removeFromTailLRU(b)
		return
	}
	target := bgLRUTierAtMin
	if c.minPerShape > 1 && b.count < c.minPerShape {
		target = bgLRUTierUnderMin
	}
	if b.lruTier == target && !touch {
		return
	}
	if b.lruTier != bgLRUTierNone && b.lruTier != target {
		c.removeFromTailLRU(b)
	}
	switch target {
	case bgLRUTierUnderMin:
		c.underMinLRU.moveToBack(b)
	case bgLRUTierAtMin:
		c.atMinLRU.moveToBack(b)
	}
	b.lruTier = target
}

func (c *DerivedBgCollector) evictNode(n *bgCollectorNode) {
	if n == nil {
		return
	}
	if n.head {
		c.headList.remove(n)
		c.headBytes -= n.item.Bytes
	} else {
		c.tailList.remove(n)
	}
	c.totalBytes -= n.item.Bytes
	if b := c.byShape[n.item.Shape]; b != nil {
		b.remove(n)
		if b.count == 0 {
			c.removeFromTailLRU(b)
			delete(c.byShape, n.item.Shape)
		} else {
			c.placeInTailLRU(b, false)
		}
	}
}

// evictOneTailByRecency evicts from the least-recently-seen non-head shape,
// preferring sparse shapes (< minPerShape, such as one-off ID walks) before
// multi-sample active pollers (>= minPerShape), and skipping excludeShape when
// another victim exists so a shape being populated never evicts its own prior
// samples while building up to minPerShape. When minPerShape > 0 and the victim
// shape holds <= minPerShape samples (counting any head-reserve samples too),
// the entire shape is evicted together so no shape is left crippled below
// minPerShape.
func (c *DerivedBgCollector) evictOneTailByRecency(excludeShape string) bool {
	pick := func(l *bgShapeLRU) *bgCollectorShapeBucket {
		for cur := l.head; cur != nil; cur = cur.lruNext {
			if cur.shape != excludeShape {
				return cur
			}
		}
		return nil
	}
	b := pick(&c.underMinLRU)
	if b == nil {
		b = pick(&c.atMinLRU)
	}
	if b == nil {
		b = c.underMinLRU.head
	}
	if b == nil {
		b = c.atMinLRU.head
	}
	if b == nil {
		return false
	}
	if c.minPerShape > 0 && b.count <= c.minPerShape {
		evicted := 0
		for b.head != nil {
			c.evictNode(b.head)
			evicted++
		}
		if evicted > 0 {
			c.droppedByCap += evicted
			c.droppedShapes++
			return true
		}
		c.removeFromTailLRU(b)
		return false
	}
	n := b.firstNonHead()
	if n == nil {
		c.removeFromTailLRU(b)
		return false
	}
	wasLast := b.count == 1
	c.evictNode(n)
	c.droppedByCap++
	if wasLast {
		c.droppedShapes++
	}
	return true
}

// Add inserts item into the collector under its canonical shape, enforcing the
// per-shape cap, head reserve, and global entry/byte caps.
func (c *DerivedBgCollector) Add(item DerivedBgItem) {
	size := item.Bytes
	if c.maxBytes > 0 && size > c.maxBytes {
		c.droppedByCap++
		return
	}
	shape := item.Shape
	if shape == "" && item.Recording.Mock != nil {
		shape = BackgroundShapeKey(item.Recording.Mock)
		item.Shape = shape
	}

	// Determine whether a same-shape eviction would occur, and pre-check
	// admissibility BEFORE mutating byShape, headList, or tailList so an
	// oversized outlier can never evict a protected head entry or drain
	// tailList only to be dropped itself.
	var sameShapeEvict *bgCollectorNode
	if b := c.byShape[shape]; b != nil && b.count >= c.maxPerShape {
		sameShapeEvict = b.head
	}

	projHeadCount := c.headList.count
	projHeadBytes := c.headBytes
	projTotalCount := c.TotalCount()
	projTotalBytes := c.totalBytes
	if sameShapeEvict != nil {
		projTotalCount--
		projTotalBytes -= sameShapeEvict.item.Bytes
		if sameShapeEvict.head {
			projHeadCount--
			projHeadBytes -= sameShapeEvict.item.Bytes
		}
	}

	admitHead := !c.headSealed &&
		c.headReserve > 0 &&
		projHeadCount < c.headReserve &&
		(c.maxBytes == 0 || projHeadBytes+size <= c.headReserveBytes) &&
		projTotalCount < c.maxEntries &&
		(c.maxBytes == 0 || projTotalBytes+size <= c.maxBytes)

	if !admitHead {
		if projHeadCount >= c.maxEntries || (c.maxBytes > 0 && projHeadBytes+size > c.maxBytes) {
			c.droppedByCap++
			return
		}
	}

	if sameShapeEvict != nil {
		c.evictNode(sameShapeEvict)
		c.trimmedPerShape++
	}

	node := &bgCollectorNode{item: item}
	if admitHead {
		node.head = true
		c.headList.pushBack(node)
		c.headBytes += size
	} else {
		c.headSealed = true
		for (c.TotalCount() >= c.maxEntries || (c.maxBytes > 0 && c.totalBytes+size > c.maxBytes)) && c.tailList.head != nil {
			if !c.evictOneTailByRecency(shape) {
				break
			}
		}
		c.tailList.pushBack(node)
	}

	c.totalBytes += size
	b := c.byShape[shape]
	if b == nil {
		b = &bgCollectorShapeBucket{shape: shape}
		c.byShape[shape] = b
	}
	b.pushBack(node)
	if !node.head {
		c.placeInTailLRU(b, true)
	} else if b.tailCount > 0 {
		c.placeInTailLRU(b, false)
	}
}

// Items returns all retained items ordered stably by recorded timestamp and name.
func (c *DerivedBgCollector) Items() []DerivedBgItem {
	n := c.TotalCount()
	if n == 0 {
		return nil
	}
	out := make([]DerivedBgItem, 0, n)
	for cur := c.headList.head; cur != nil; cur = cur.next {
		out = append(out, cur.item)
	}
	for cur := c.tailList.head; cur != nil; cur = cur.next {
		out = append(out, cur.item)
	}
	slices.SortStableFunc(out, func(a, b DerivedBgItem) int {
		if cmp := a.At.Compare(b.At); cmp != 0 {
			return cmp
		}
		na, nb := a.Entry.Name, b.Entry.Name
		if na == "" && a.Recording.Mock != nil {
			na = a.Recording.Mock.Name
		}
		if nb == "" && b.Recording.Mock != nil {
			nb = b.Recording.Mock.Name
		}
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
		return 0
	})
	return out
}

// Recordings returns the retained BackgroundRecordings ordered stably by
// recorded ReqTimestampMock.
func (c *DerivedBgCollector) Recordings() []BackgroundRecording {
	items := c.Items()
	if len(items) == 0 {
		return nil
	}
	out := make([]BackgroundRecording, len(items))
	for i, it := range items {
		out[i] = it.Recording
	}
	return out
}

// Entries returns the retained MockEntries ordered stably by recorded
// timestamp and name.
func (c *DerivedBgCollector) Entries() []MockEntry {
	items := c.Items()
	if len(items) == 0 {
		return nil
	}
	out := make([]MockEntry, len(items))
	for i, it := range items {
		out[i] = it.Entry
	}
	return out
}
