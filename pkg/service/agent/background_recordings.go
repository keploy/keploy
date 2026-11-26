package agent

import (
	"container/heap"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Bounds for derived background recordings (when mappings.yaml is absent and
// DeriveBackgroundFromWindows is set). In a long continuous recording with
// late test windows, tens or hundreds of thousands of outside-window mocks
// may live in DiskMocks; retaining them all decoded in MockManager.background
// defeats DiskMocks and OOM-kills the replay sidecar.
var (
	// derivedBgMaxEntries caps total derived background recordings retained
	// in RAM across all kinds.
	derivedBgMaxEntries = 512
	// derivedBgMaxBytes caps total estimated decoded RAM retained for derived
	// background recordings across all kinds.
	derivedBgMaxBytes int64 = 8 << 20 // 8 MiB
	// derivedBgMaxPerShape caps representatives retained per canonical
	// request shape (>= learn.MinEpochReads+1 so bare-epoch clock tracking
	// retains enough samples with non-zero From).
	derivedBgMaxPerShape = 8
	// derivedBgHeadReserve protects the earliest distinct boot/startup
	// recordings at the start of the recording from being evicted by later
	// high-cardinality outside-window traffic.
	derivedBgHeadReserve      = 64
	derivedBgHeadReserveBytes = int64(1 << 20) // 1 MiB
)

// seedBackgroundRecordings hands the proxy, as a set is staged, the set's
// background recordings params names (models.MockFilterParams.BackgroundMocks)
// with their recorded spans (backgroundRecordingsOf), read from every mock
// the agent holds for the set: resident ones and those kept on disk, served
// or not. A staging call that names none clears the previous set's. A set
// whose recordings cannot be read is handed none: every miss stays its
// test's, as before.
func (a *Agent) seedBackgroundRecordings(params models.MockFilterParams, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) {
	seeder, ok := a.Proxy.(coreAgent.BackgroundRecordingsSeeder)
	if !ok {
		return
	}
	var recs []models.BackgroundRecording
	var err error
	if len(params.BackgroundMocks) > 0 {
		recs, err = backgroundRecordingsOf(params.BackgroundMocks, params.StartupMocks, resident, disk)
	} else if params.DeriveBackgroundFromWindows && len(params.RecordedWindows) > 0 && !params.ExactStaging {
		recs, err = derivedBackgroundRecordingsOf(params.RecordedWindows, resident, disk)
	}
	if err != nil {
		a.logger.Warn("could not read the test set's background recordings; every call that misses stays its test's miss",
			zap.Int("background", len(params.BackgroundMocks)), zap.Int("windows", len(params.RecordedWindows)), zap.Error(err))
		recs = nil
	}
	seeder.SeedBackgroundRecordings(recs)
}

// derivedBackgroundRecordingsOf derives outside-window background recordings
// from windows in a single bounded streaming pass over resident and disk
// mocks, using the same worker-scope rules as models.DeriveWindowMappings
// (models.WindowClassifier) and the same recorded-span start rule as
// backgroundRecordingsOf (prevCallTracker).
//
// Each on-disk mock is decoded at most once and dropped immediately unless
// admitted into a bounded collector capped by canonical request shape
// (derivedBgMaxPerShape), total entry count (derivedBgMaxEntries), and
// estimated decoded bytes (derivedBgMaxBytes), while preserving both the
// leading boot/startup prefix and the most recent outside-window recordings
// closest to the test windows. Derived recordings are returned with
// Startup=false because DeriveWindowMappings places all outside-window
// recordings in Worker.
func derivedBackgroundRecordingsOf(windows []models.TestWindow, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	classifier := models.NewWindowClassifier(windows)
	tracker := newPrevCallTracker()
	seenName := make(map[nameHash]struct{})
	collector := newDerivedBgCollector(
		derivedBgMaxEntries,
		derivedBgMaxBytes,
		derivedBgMaxPerShape,
		derivedBgHeadReserve,
		derivedBgHeadReserveBytes,
	)

	err := streamRecordedMerged(resident, disk, func(mk *models.Mock) bool {
		if mk == nil {
			return true
		}
		from := tracker.advance(mk.Spec.ReqTimestampMock)
		if tracker.seenInBatch(learn.MockID(mk)) {
			return true
		}
		tracker.recordCall(mk)
		if mk.Name == "" {
			return true
		}
		nh := hashName(mk.Name)
		if _, dup := seenName[nh]; dup {
			return true
		}
		seenName[nh] = struct{}{}
		if !classifier.IsWorker(mk) {
			return true
		}
		collector.add(mk, from)
		return true
	})
	if err != nil {
		return nil, err
	}
	return collector.recordings(), nil
}

// bgCallTime is what bounds a background recording's recorded span, of one
// data-plane recording: its request and answer time.
type bgCallTime struct {
	req, res time.Time
}

// backgroundRecordingsOf is, of the set's recordings (resident, and those
// disk keeps), the ones names names, each with its recorded span's start
// (models.BackgroundRecording.From): the request time of the latest
// data-plane recording sent before it, not itself, and not answered after
// it was sent, the rule learn.PreviousCall reads a call's previous call by.
// Under the proven switch each one startup names is a plain startup:
// recording (models.BackgroundRecording.Startup) when a user can declare a
// call identical to it (models.StartupDeclarable: an HTTP or HTTP/2
// recording that states its lifetime, and any of a kind a rule system
// registered) and no keploy.yaml sessionCalls entry
// names it exactly (models.Mock.DeclaredSessionCall): a declared session
// call stays the background's, and so does a recording no one can declare,
// as in v3.7.18. With the switch off none is, whatever the CLI
// sent. Each recording on disk is decoded once, and only
// the named ones are kept. nil for no names; err when disk cannot be read.
func backgroundRecordingsOf(names, startup []string, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
	if len(names) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	plain := make(map[string]bool, len(startup))
	if provenswitch.On() {
		for _, n := range startup {
			plain[n] = true
		}
	}
	var recs []models.BackgroundRecording
	got := make(map[string]bool, len(names))
	tracker := newPrevCallTracker()
	err := streamRecordedMerged(resident, disk, func(mk *models.Mock) bool {
		if mk == nil {
			return true
		}
		from := tracker.advance(mk.Spec.ReqTimestampMock)
		if tracker.seenInBatch(learn.MockID(mk)) {
			return true
		}
		tracker.recordCall(mk)
		if want[mk.Name] && !got[mk.Name] {
			got[mk.Name] = true
			recs = append(recs, models.BackgroundRecording{
				Mock:    mk,
				From:    from,
				Startup: plain[mk.Name] && models.StartupDeclarable(mk) && !mk.DeclaredSessionCall(),
			})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// streamRecordedMerged visits every non-nil mock across resident lists and
// disk in ascending ReqTimestampMock order, decoding at most one on-disk mock
// at a time. When timestamps tie, resident mocks precede disk mocks.
func streamRecordedMerged(resident [][]*models.Mock, disk *proxyPkg.DiskMocks, visit func(*models.Mock) bool) error {
	var res []*models.Mock
	for _, list := range resident {
		for _, mk := range list {
			if mk != nil {
				res = append(res, mk)
			}
		}
	}
	slices.SortStableFunc(res, func(a, b *models.Mock) int {
		return a.Spec.ReqTimestampMock.Compare(b.Spec.ReqTimestampMock)
	})

	ri := 0
	stopped := false
	if disk != nil {
		err := disk.StreamRecorded(func(dm *models.Mock) bool {
			if dm == nil {
				return true
			}
			for ri < len(res) && !res[ri].Spec.ReqTimestampMock.After(dm.Spec.ReqTimestampMock) {
				if !visit(res[ri]) {
					stopped = true
					return false
				}
				ri++
			}
			if !visit(dm) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	for ; ri < len(res); ri++ {
		if !visit(res[ri]) {
			return nil
		}
	}
	return nil
}

// pendingCallHeap is a min-heap of in-flight calls ordered by response time,
// retiring completed calls in O(log k) time where k is concurrent in-flight
// calls.
type pendingCallHeap []bgCallTime

func (h pendingCallHeap) Len() int           { return len(h) }
func (h pendingCallHeap) Less(i, j int) bool { return h[i].res.Before(h[j].res) }
func (h pendingCallHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pendingCallHeap) Push(x any)        { *h = append(*h, x.(bgCallTime)) }
func (h *pendingCallHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// prevCallTracker computes each recording's previous data-plane call request
// time (the exact semantics of previousCallAt / learn.PreviousCall.See) in a
// single streaming pass in non-decreasing ReqTimestampMock order, using
// O(in-flight) memory instead of retaining all call timestamps.
type prevCallTracker struct {
	pending         pendingCallHeap
	latestCompleted time.Time

	hasBatch   bool
	batchAt    time.Time
	batchCalls []bgCallTime
	batchIDs   map[string]struct{}
}

func newPrevCallTracker() *prevCallTracker {
	return &prevCallTracker{
		batchIDs: make(map[string]struct{}, 8),
	}
}

// advance moves the tracker to request instant at and returns the request time
// of the latest prior data-plane call sent strictly before at and not answered
// after at (zero for none or zero at).
func (t *prevCallTracker) advance(at time.Time) time.Time {
	if at.IsZero() {
		if !t.hasBatch || !t.batchAt.IsZero() {
			t.flushBatch()
			t.batchAt = time.Time{}
			t.hasBatch = true
			clear(t.batchIDs)
		}
		return time.Time{}
	}
	if !t.hasBatch || !at.Equal(t.batchAt) {
		t.flushBatch()
		for len(t.pending) > 0 && !t.pending[0].res.After(at) {
			c := heap.Pop(&t.pending).(bgCallTime)
			if t.latestCompleted.IsZero() || c.req.After(t.latestCompleted) {
				t.latestCompleted = c.req
			}
		}
		t.batchAt = at
		t.hasBatch = true
		clear(t.batchIDs)
	}
	return t.latestCompleted
}

// seenInBatch reports whether id was already seen at the current request
// instant and marks it seen. Because MockID includes ReqTimestampMock, two
// mocks with identical MockID always land in the same timestamp batch.
func (t *prevCallTracker) seenInBatch(id string) bool {
	if _, ok := t.batchIDs[id]; ok {
		return true
	}
	t.batchIDs[id] = struct{}{}
	return false
}

// recordCall records mk as a candidate previous call for future instants.
func (t *prevCallTracker) recordCall(mk *models.Mock) {
	if !mk.IsProtocolPhase() && !mk.Spec.ReqTimestampMock.IsZero() {
		t.batchCalls = append(t.batchCalls, bgCallTime{
			req: mk.Spec.ReqTimestampMock,
			res: mk.Spec.ResTimestampMock,
		})
	}
}

func (t *prevCallTracker) flushBatch() {
	for _, c := range t.batchCalls {
		if c.res.IsZero() || !c.res.After(c.req) {
			if t.latestCompleted.IsZero() || c.req.After(t.latestCompleted) {
				t.latestCompleted = c.req
			}
		} else {
			heap.Push(&t.pending, c)
		}
	}
	t.batchCalls = t.batchCalls[:0]
}

// nameHash is a pointer-free 128-bit digest used to deduplicate mock names
// across a streaming scan without retaining hundreds of thousands of name
// strings on the heap.
type nameHash struct{ a, b uint64 }

func hashName(s string) nameHash {
	a := xxhash.Sum64String(s)
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	b := uint64(offset64)
	for i := 0; i < len(s); i++ {
		b ^= uint64(s[i])
		b *= prime64
	}
	return nameHash{a: a, b: b}
}

// bgNode is one retained derived background recording inside
// derivedBgCollector's intrusive lists (global head/tail list + per-shape
// FIFO bucket).
type bgNode struct {
	rec   models.BackgroundRecording
	shape string
	bytes int64
	head  bool

	prev, next   *bgNode
	sPrev, sNext *bgNode
}

type bgList struct {
	head, tail *bgNode
	count      int
}

func (l *bgList) pushBack(n *bgNode) {
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

func (l *bgList) remove(n *bgNode) {
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

type bgShapeBucket struct {
	head, tail *bgNode
	count      int
}

func (b *bgShapeBucket) pushBack(n *bgNode) {
	n.sPrev = b.tail
	n.sNext = nil
	if b.tail != nil {
		b.tail.sNext = n
	} else {
		b.head = n
	}
	b.tail = n
	b.count++
}

func (b *bgShapeBucket) remove(n *bgNode) {
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
}

// derivedBgCollector maintains a strictly bounded set of derived background
// recordings while streaming mocks in recorded timestamp order:
//   - deduplicates recurring background traffic by canonical request shape,
//     retaining at most maxPerShape latest samples per shape;
//   - protects a small leading boot/startup prefix (up to headReserve entries
//     and headReserveBytes, clamped to at most 1/4 of the global caps so the
//     recency tail always keeps >= 75% of the budget);
//   - enforces global maxEntries and maxBytes ceilings by evicting the oldest
//     non-head recordings so background polls active around late test windows
//     always survive high-cardinality earlier traffic.
type derivedBgCollector struct {
	maxEntries       int
	maxBytes         int64
	maxPerShape      int
	headReserve      int
	headReserveBytes int64

	headList   bgList
	tailList   bgList
	headBytes  int64
	totalBytes int64
	headSealed bool
	byShape    map[string]*bgShapeBucket
}

func newDerivedBgCollector(maxEntries int, maxBytes int64, maxPerShape int, headReserve int, headReserveBytes int64) *derivedBgCollector {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxBytes < 1 {
		maxBytes = 1
	}
	if maxPerShape < 1 {
		maxPerShape = 1
	}
	if capHead := maxEntries / 4; headReserve > capHead {
		headReserve = capHead
	}
	if capHeadBytes := maxBytes / 4; headReserveBytes > capHeadBytes {
		headReserveBytes = capHeadBytes
	}
	return &derivedBgCollector{
		maxEntries:       maxEntries,
		maxBytes:         maxBytes,
		maxPerShape:      maxPerShape,
		headReserve:      headReserve,
		headReserveBytes: headReserveBytes,
		byShape:          make(map[string]*bgShapeBucket),
	}
}

func (c *derivedBgCollector) totalCount() int {
	return c.headList.count + c.tailList.count
}

func (c *derivedBgCollector) evict(n *bgNode) {
	if n == nil {
		return
	}
	if n.head {
		c.headList.remove(n)
		c.headBytes -= n.bytes
	} else {
		c.tailList.remove(n)
	}
	c.totalBytes -= n.bytes
	if b := c.byShape[n.shape]; b != nil {
		b.remove(n)
		if b.count == 0 {
			delete(c.byShape, n.shape)
		}
	}
}

func (c *derivedBgCollector) add(mk *models.Mock, from time.Time) {
	dm := stripDerivedResponse(mk)
	size := estimateDerivedMockBytes(dm)
	if size > c.maxBytes {
		return
	}
	shape := bgShapeKey(dm)

	// Determine whether a same-shape eviction would occur, and pre-check
	// admissibility BEFORE mutating byShape, headList, or tailList so an
	// oversized outlier can never evict a protected head entry or drain
	// tailList only to be dropped itself.
	var sameShapeEvict *bgNode
	if b := c.byShape[shape]; b != nil && b.count >= c.maxPerShape {
		sameShapeEvict = b.head
	}

	projHeadCount := c.headList.count
	projHeadBytes := c.headBytes
	projTotalCount := c.totalCount()
	projTotalBytes := c.totalBytes
	if sameShapeEvict != nil {
		projTotalCount--
		projTotalBytes -= sameShapeEvict.bytes
		if sameShapeEvict.head {
			projHeadCount--
			projHeadBytes -= sameShapeEvict.bytes
		}
	}

	admitHead := !c.headSealed &&
		projHeadCount < c.headReserve &&
		projHeadBytes+size <= c.headReserveBytes &&
		projTotalCount < c.maxEntries &&
		projTotalBytes+size <= c.maxBytes

	if !admitHead {
		if projHeadCount >= c.maxEntries || projHeadBytes+size > c.maxBytes {
			return
		}
	}

	if sameShapeEvict != nil {
		c.evict(sameShapeEvict)
	}

	node := &bgNode{
		rec:   models.BackgroundRecording{Mock: dm, From: from},
		shape: shape,
		bytes: size,
	}

	if admitHead {
		node.head = true
		c.headList.pushBack(node)
		c.headBytes += size
	} else {
		c.headSealed = true
		for (c.totalCount() >= c.maxEntries || c.totalBytes+size > c.maxBytes) && c.tailList.head != nil {
			c.evict(c.tailList.head)
		}
		c.tailList.pushBack(node)
	}

	c.totalBytes += size
	b := c.byShape[shape]
	if b == nil {
		b = &bgShapeBucket{}
		c.byShape[shape] = b
	}
	b.pushBack(node)
}

func (c *derivedBgCollector) recordings() []models.BackgroundRecording {
	n := c.totalCount()
	if n == 0 {
		return nil
	}
	out := make([]models.BackgroundRecording, 0, n)
	for cur := c.headList.head; cur != nil; cur = cur.next {
		out = append(out, cur.rec)
	}
	for cur := c.tailList.head; cur != nil; cur = cur.next {
		out = append(out, cur.rec)
	}
	slices.SortStableFunc(out, func(a, b models.BackgroundRecording) int {
		return a.Mock.Spec.ReqTimestampMock.Compare(b.Mock.Spec.ReqTimestampMock)
	})
	return out
}

// stripDerivedResponse returns a shallow copy of mk with response payloads and
// disk-hydrator closures cleared. Derived background recordings are read-only
// request matchers used only to classify background misses (never served), so
// eliding response payloads keeps the memory budget focused on request shapes,
// prevents pinning closed DiskMocks via response hydrators, and never mutates
// resident mocks.
func stripDerivedResponse(mk *models.Mock) *models.Mock {
	c := *mk
	c.Spec.HTTPResp = nil
	c.Spec.HTTP2Resp = nil
	c.Spec.GRPCResp = nil
	c.Spec.MongoResponses = nil
	c.Spec.GenericResponses = nil
	c.Spec.MySQLResponses = nil
	c.Spec.PostgresResponsesV2 = nil
	c.Spec.DNSResp = nil
	c.SetResponseHydrator(nil)
	return &c
}

// estimateDerivedMockBytes returns a fast, allocation-free estimate of the
// live heap retained by a response-stripped derived background mock.
func estimateDerivedMockBytes(m *models.Mock) int64 {
	if m == nil {
		return 0
	}
	// Base struct + MockSpec + map header overhead.
	n := int64(512) + int64(len(m.Name)+len(m.Kind)+len(m.Version))
	for k, v := range m.Spec.Metadata {
		n += int64(len(k)+len(v)) + 32
	}
	for _, s := range m.Noise {
		n += int64(len(s)) + 16
	}
	for k, vals := range m.Spec.ReqBodyNoise {
		n += int64(len(k)) + 32
		for _, v := range vals {
			n += int64(len(v)) + 16
		}
	}
	if r := m.Spec.HTTPReq; r != nil {
		n += 128 + int64(len(r.Method)+len(r.URL)+len(r.Body))
		for k, v := range r.Header {
			n += int64(len(k)+len(v)) + 32
		}
	}
	if r := m.Spec.HTTP2Req; r != nil {
		n += 128 + int64(len(r.Method)+len(r.URL)+len(r.Authority)+len(r.Scheme)+len(r.Body))
		for k, v := range r.Headers {
			n += int64(len(k)+len(v)) + 32
		}
	}
	if r := m.Spec.GRPCReq; r != nil {
		n += 128 + int64(len(r.Body.DecodedData))
		for k, v := range r.Headers.PseudoHeaders {
			n += int64(len(k)+len(v)) + 32
		}
		for k, v := range r.Headers.OrdinaryHeaders {
			n += int64(len(k)+len(v)) + 32
		}
		for _, msg := range r.AdditionalMessages {
			n += int64(len(msg.DecodedData)) + 32
		}
	}
	for _, mr := range m.Spec.MongoRequests {
		n += 64
		switch msg := mr.Message.(type) {
		case *models.MongoOpMessage:
			for _, sec := range msg.Sections {
				n += int64(len(sec)) + 16
			}
		case *models.MongoOpQuery:
			n += int64(len(msg.FullCollectionName) + len(msg.Query) + len(msg.ReturnFieldsSelector))
		case *models.MongoOpCompressed:
			n += int64(len(msg.CompressedData))
		case *models.MongoOpUnknown:
			n += int64(len(msg.Data))
		}
	}
	for _, p := range m.Spec.GenericRequests {
		n += int64(len(p.Message)) + 32
	}
	if m.Spec.PostgresV3 != nil && m.StoredSize() > n {
		n = m.StoredSize()
	}
	return n
}

// bgShapeKey computes a canonical request-shape key so repeated invocations of
// the same background operation (including ones whose query/body values vary
// with bare epoch timestamps) collapse into one per-shape bucket while distinct
// endpoints/operations stay separate.
func bgShapeKey(m *models.Mock) string {
	if m == nil {
		return ""
	}
	dest := m.Spec.Metadata["destAddr"]
	switch m.Kind {
	case models.HTTP:
		if r := m.Spec.HTTPReq; r != nil {
			uHost, pathShape := urlShape(r.URL)
			host := headerCaseInsensitive(r.Header, "Host")
			if host == "" {
				host = uHost
			}
			if host == "" {
				host = dest
			}
			amz := headerCaseInsensitive(r.Header, "X-Amz-Target")
			return string(m.Kind) + "\x00" + string(r.Method) + "\x00" + host + "\x00" + pathShape + "\x00" + amz
		}
	case models.HTTP2:
		if r := m.Spec.HTTP2Req; r != nil {
			uHost, pathShape := urlShape(r.URL)
			auth := r.Authority
			if auth == "" {
				auth = uHost
			}
			if auth == "" {
				auth = dest
			}
			return string(m.Kind) + "\x00" + string(r.Method) + "\x00" + auth + "\x00" + pathShape
		}
	case models.GRPC_EXPORT:
		if r := m.Spec.GRPCReq; r != nil {
			auth := r.Headers.PseudoHeaders[":authority"]
			if auth == "" {
				auth = dest
			}
			return string(m.Kind) + "\x00" + auth + "\x00" + r.Headers.PseudoHeaders[":path"]
		}
	case models.Mongo:
		return string(m.Kind) + "\x00" + dest + "\x00" + mongoRequestShape(m.Spec.MongoRequests)
	}
	if stmt := models.StatementKey(m); stmt != "" {
		return string(m.Kind) + "\x00" + dest + "\x00" + stmt
	}
	if keys := models.RequestKeys(m); len(keys) > 0 {
		return string(m.Kind) + "\x00" + dest + "\x00" + keys[0]
	}
	return string(m.Kind) + "\x00" + dest + "\x00" + m.Spec.Metadata["operation"]
}

// urlShape returns the URL host (if present) and a normalized path plus sorted
// unique query parameter names (dropping parameter values so varying epoch
// timestamps or cursors share one shape).
func urlShape(raw string) (host, shape string) {
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		path, _, _ := strings.Cut(raw, "?")
		return "", path
	}
	host = u.Host
	if u.RawQuery == "" {
		return host, u.Path
	}
	parts := strings.Split(u.RawQuery, "&")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	return host, u.Path + "?" + strings.Join(keys, "&")
}

func headerCaseInsensitive(h map[string]string, name string) string {
	if v, ok := h[name]; ok {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// mongoRequestShape extracts a compact command/collection signature from the
// first Mongo request so repeated polls on the same collection (e.g.
// findAndModify ptw_endpoints) share one shape regardless of varying query
// timestamps.
func mongoRequestShape(reqs []models.MongoRequest) string {
	if len(reqs) == 0 {
		return ""
	}
	switch msg := reqs[0].Message.(type) {
	case *models.MongoOpMessage:
		if len(msg.Sections) == 0 {
			return ""
		}
		return mongoSectionCommand(msg.Sections[0])
	case *models.MongoOpQuery:
		return "query:" + msg.FullCollectionName
	case *models.MongoOpUpdate:
		return "update:" + msg.FullCollectionName
	case *models.MongoOpInsert:
		return "insert:" + msg.FullCollectionName
	case *models.MongoOpDelete:
		return "delete:" + msg.FullCollectionName
	case *models.MongoOpGetMore:
		return "getmore:" + msg.FullCollectionName
	}
	return ""
}

// mongoSectionCommand extracts the leading `"cmd":"target"` or `"cmd":val`
// pair from a serialized OP_MSG section string.
func mongoSectionCommand(section string) string {
	s := section
	if idx := strings.Index(s, "msg:"); idx >= 0 {
		s = s[idx+4:]
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		if len(s) > 64 {
			return s[:64]
		}
		return s
	}
	s = strings.TrimSpace(s[start+1:])
	// Read the first key:value pair up to the first ',' or '}'.
	end := strings.IndexAny(s, ",}")
	if end >= 0 {
		s = s[:end]
	}
	s = strings.TrimSpace(s)
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}
