package agent

import (
	"container/heap"
	"iter"
	"slices"
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
func (a *Agent) seedBackgroundRecordings(params models.MockFilterParams, resident [][]*models.Mock, disks ...*proxyPkg.DiskMocks) {
	seeder, ok := a.Proxy.(coreAgent.BackgroundRecordingsSeeder)
	if !ok {
		return
	}
	var recs []models.BackgroundRecording
	var err error
	if len(params.BackgroundMocks) > 0 {
		recs, err = backgroundRecordingsOf(params.BackgroundMocks, params.StartupMocks, resident, disks...)
	} else if params.DeriveBackgroundFromWindows && len(params.RecordedWindows) > 0 && !params.ExactStaging {
		recs, err = derivedBackgroundRecordingsWith(params.RecordedWindows, params.FirstRecordedTestStart, a.logger, resident, disks...)
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
// Each on-disk mock across all disk stores (filtered and unfilteredDisk) is
// decoded at most once and dropped immediately unless admitted into a bounded
// collector capped by canonical request shape (derivedBgMaxPerShape), total
// entry count (derivedBgMaxEntries), and estimated decoded bytes
// (derivedBgMaxBytes), while preserving both the leading boot/startup prefix
// and the most recent outside-window recordings closest to the test windows.
// Derived recordings are returned with Startup=false because
// DeriveWindowMappings places all outside-window recordings in Worker.
func derivedBackgroundRecordingsOf(windows []models.TestWindow, resident [][]*models.Mock, disks ...*proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
	return derivedBackgroundRecordingsWith(windows, time.Time{}, nil, resident, disks...)
}

func derivedBackgroundRecordingsWith(windows []models.TestWindow, recordedFirstStart time.Time, logger *zap.Logger, resident [][]*models.Mock, disks ...*proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
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

	err := streamRecordedMerged(resident, disks, func(mk *models.Mock) bool {
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
		kind, _ := classifier.ClassifyForBackground(mk, recordedFirstStart)
		if kind == models.WindowBgNone {
			return true
		}
		collector.add(mk, from)
		return true
	})
	if err != nil {
		return nil, err
	}
	if st := collector.col.Stats(); st.DroppedByCap > 0 && logger != nil {
		logger.Info("agent derived background collector reached capacity cap; dropped oldest shapes by recency while preserving active pollers",
			zap.Int("kept", st.Kept),
			zap.Int("distinctShapes", st.DistinctShapes),
			zap.Int("droppedByCap", st.DroppedByCap),
			zap.Int("droppedShapes", st.DroppedShapes))
	}
	return collector.recordings(), nil
}

// bgCallTime is what bounds a background recording's recorded span, of one
// data-plane recording: its request and answer time.
type bgCallTime struct {
	req, res time.Time
}

// backgroundRecordingsOf is, of the set's recordings (resident, and those
// disks keep), the ones names names, each with its recorded span's start
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
// the named ones are kept. nil for no names; err when any disk cannot be read.
func backgroundRecordingsOf(names, startup []string, resident [][]*models.Mock, disks ...*proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
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
	err := streamRecordedMerged(resident, disks, func(mk *models.Mock) bool {
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

// streamRecordedMerged visits every non-nil mock across resident lists and all
// non-nil DiskMocks stores in ascending ReqTimestampMock order, decoding at
// most one on-disk mock per store at a time via StreamRecorded. When timestamps
// tie, resident mocks precede disk mocks, and earlier disk stores precede later
// ones.
func streamRecordedMerged(resident [][]*models.Mock, disks []*proxyPkg.DiskMocks, visit func(*models.Mock) bool) error {
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

	var active []*proxyPkg.DiskMocks
	for _, d := range disks {
		if d != nil {
			active = append(active, d)
		}
	}

	if len(active) == 0 {
		for _, mk := range res {
			if !visit(mk) {
				return nil
			}
		}
		return nil
	}

	if len(active) == 1 {
		ri := 0
		stopped := false
		err := active[0].StreamRecorded(func(dm *models.Mock) bool {
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
		for ; ri < len(res); ri++ {
			if !visit(res[ri]) {
				return nil
			}
		}
		return nil
	}

	type diskPullStream struct {
		next func() (*models.Mock, bool)
		stop func()
		err  *error
		cur  *models.Mock
		ok   bool
	}
	streams := make([]diskPullStream, 0, len(active))
	defer func() {
		for i := range streams {
			streams[i].stop()
		}
	}()
	for _, d := range active {
		var streamErr error
		diskRef := d
		next, stop := iter.Pull(func(yield func(*models.Mock) bool) {
			streamErr = diskRef.StreamRecorded(func(dm *models.Mock) bool {
				if dm == nil {
					return true
				}
				return yield(dm)
			})
		})
		cur, ok := next()
		if !ok && streamErr != nil {
			stop()
			return streamErr
		}
		streams = append(streams, diskPullStream{
			next: next,
			stop: stop,
			err:  &streamErr,
			cur:  cur,
			ok:   ok,
		})
	}

	ri := 0
	for {
		bestIdx := -1
		for i := range streams {
			if !streams[i].ok {
				continue
			}
			if bestIdx < 0 || streams[i].cur.Spec.ReqTimestampMock.Before(streams[bestIdx].cur.Spec.ReqTimestampMock) {
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		nextDisk := streams[bestIdx].cur
		for ri < len(res) && !res[ri].Spec.ReqTimestampMock.After(nextDisk.Spec.ReqTimestampMock) {
			if !visit(res[ri]) {
				return nil
			}
			ri++
		}
		if !visit(nextDisk) {
			return nil
		}
		streams[bestIdx].cur = nil
		cur, ok := streams[bestIdx].next()
		if !ok && *streams[bestIdx].err != nil {
			return *streams[bestIdx].err
		}
		streams[bestIdx].cur = cur
		streams[bestIdx].ok = ok
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

// derivedBgCollector wraps the shared models.DerivedBgCollector for the
// agent's Pass 1 in-RAM derived background recording pass, stripping response
// payloads and estimating decoded byte sizes before admission.
type derivedBgCollector struct {
	col *models.DerivedBgCollector
}

func newDerivedBgCollector(maxEntries int, maxBytes int64, maxPerShape int, headReserve int, headReserveBytes int64) *derivedBgCollector {
	minPerShape := 0
	if maxPerShape >= learn.MinEpochReads+1 {
		minPerShape = learn.MinEpochReads + 1
	}
	return &derivedBgCollector{
		col: models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
			MaxEntries:       maxEntries,
			MaxBytes:         maxBytes,
			MaxPerShape:      maxPerShape,
			MinPerShape:      minPerShape,
			HeadReserve:      headReserve,
			HeadReserveBytes: headReserveBytes,
		}),
	}
}

func (c *derivedBgCollector) totalCount() int {
	return c.col.TotalCount()
}

func (c *derivedBgCollector) totalBytes() int64 {
	return c.col.TotalBytes()
}

func (c *derivedBgCollector) headCount() int {
	return c.col.HeadCount()
}

func (c *derivedBgCollector) tailCount() int {
	return c.col.TailCount()
}

func (c *derivedBgCollector) shapeCount() int {
	return c.col.ShapeCount()
}

func (c *derivedBgCollector) add(mk *models.Mock, from time.Time) {
	dm := stripDerivedResponse(mk)
	size := estimateDerivedMockBytes(dm)
	c.col.Add(models.DerivedBgItem{
		Recording: models.BackgroundRecording{Mock: dm, From: from},
		Shape:     models.BackgroundShapeKey(dm),
		At:        dm.Spec.ReqTimestampMock,
		Bytes:     size,
	})
}

func (c *derivedBgCollector) recordings() []models.BackgroundRecording {
	return c.col.Recordings()
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
