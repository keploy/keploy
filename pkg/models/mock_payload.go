package models

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"unsafe"
)

// A replay reads a test set's mocks before its first test: it sorts them into
// the agent's pools, works out what each test owns and how the reports name
// each mock, and sends them to the agent, which parks the per-test ones on its
// own disk (the agent's disk store) and reads back only what a test's window
// needs. Holding the whole set in memory on the way there is what ran the CLI
// out of memory on a large recording: the decoded set is about the size of
// its mock file, and the parse that builds it about doubles that at its peak.
//
// So a read of a yaml or json mock file leaves every mock's payload in the
// file. The mock it keeps (LeavePayloadOnDisk) is a small entry: its name,
// kind, lifetime and test-mode info, its connection, its metadata less the
// operation it records (the one value that grows with the call), its request
// and response times, its async block, and where in the file its document is.
// What a replay asks of a mock before it sends it, and of the payload only
// (the reports' summary and target, the registered predicates, a MySQL
// command's place in a statement's lifecycle), is answered while the payload
// is still in memory and kept with the entry. The payload is read back, one
// mock at a time, only to be sent (WithPayload), and dropped once it is.

// PayloadSource is a mock file a read left its mocks' payloads in.
type PayloadSource interface {
	// LoadPayload reads back the mock at at, as the read that left its
	// payload decoded it.
	LoadPayload(ctx context.Context, at PayloadAt) (*Mock, error)
	// SetPayloadMutator has mutate change every payload read back from
	// then on (Keploy Enterprise decrypts it), before WithPayload returns
	// it.
	SetPayloadMutator(mutate PayloadMutator)
	// PayloadMutator is the mutator set, nil when none is.
	PayloadMutator() PayloadMutator
}

// PayloadMutator changes one mock's payload before the mock is sent to the
// agent: a replay's hooks decrypt it. It is handed the mock whole, and may
// change its payload only (MutatePayload).
type PayloadMutator func(ctx context.Context, m *Mock) error

// BatchPayloadSource is an optional PayloadSource capability that reads a
// batch of on-disk mock payloads with a single file open, stat, and SameFile
// check instead of opening and closing the file once per mock.
type BatchPayloadSource interface {
	LoadPayloadBatch(ctx context.Context, ats []PayloadAt) ([]*Mock, error)
}

// BatchPayloadMutator changes a batch of mocks' payloads before they are sent
// to the agent (for example, batching KMS decryption across a 500-mock batch).
// Like PayloadMutator, it may change each mock's payload only.
type BatchPayloadMutator func(ctx context.Context, mocks []*Mock) error

// BatchPayloadMutatorSource is an optional PayloadSource capability for
// registering a BatchPayloadMutator.
type BatchPayloadMutatorSource interface {
	SetBatchPayloadMutator(mutate BatchPayloadMutator)
	BatchPayloadMutator() BatchPayloadMutator
}

// PayloadAt is where a mock's document is in its file: the Length bytes from
// Offset, which decode to the mock named Name.
type PayloadAt struct {
	Offset int64
	Length int
	Name   string
}

// ErrPayloadFileReplaced is the error of reading back a payload whose mock
// file was replaced or removed since its mocks were read: the replay rewrites
// a test set's mock file once the set has run (it prunes the mocks no test
// consumed, folds learned noise in), so the mock is of a set that is over.
var ErrPayloadFileReplaced = errors.New("the mock file was rewritten after its mocks were read")

// ErrPayloadMutatorChangedMock is the error of a PayloadMutator that changed
// more than a mock's payload: what decides the mock's pool and its place in it
// was read before the payload was, and the agent is told the pools' sizes
// before it is sent any mock.
var ErrPayloadMutatorChangedMock = errors.New("a mock payload hook changed what decides the mock's pool")

// MockPayloadRefusal is the error of a PayloadMutator that failed on a mock:
// the replay's hooks refused to hand the mock to the agent (Keploy Enterprise
// refuses a sealed value its key service did not open). Err is the hook's
// reason. The replay fails the mock's test set, unrun, with it, and goes on
// to the next set.
type MockPayloadRefusal struct {
	Mock string
	Err  error
}

func (e *MockPayloadRefusal) Error() string {
	return fmt.Sprintf("the mock payload hook failed on mock %q: %v", e.Mock, e.Err)
}

func (e *MockPayloadRefusal) Unwrap() error { return e.Err }

// TestCaseRefusal is the error of a TestCaseMutator's BeforeTestCaseRun that
// refuses to run a test case (for example, a declarative transform whose
// precondition failed at runtime). The replay fails the test case unsent with
// Err instead of sending an untransformed request, and goes on to the next
// test case.
//
// Refusal errors are logged and written verbatim to TemplateFailure.Why, so
// callers must never format recorded or live secret values into Err.
type TestCaseRefusal struct {
	TestCase string
	Err      error
}

func (e *TestCaseRefusal) Error() string {
	return fmt.Sprintf("the BeforeTestCaseRun hook refused test case %q: %v", e.TestCase, e.Err)
}

func (e *TestCaseRefusal) Unwrap() error { return e.Err }

// payloadOnDisk is where a mock's payload was left, and what the replay asks
// of the payload, answered before it was.
type payloadOnDisk struct {
	src PayloadSource
	at  PayloadAt
	// summary and target are MockSummaryFromSpec's and MockTargetFromSpec's,
	// when no longer than maxKeptDisplay. A longer one (a Mongo mock's carries
	// its whole recorded command) is not kept (longSummary, longTarget): it
	// is worked out again from the payload when it is asked for, which only
	// a report naming the mock does.
	summary, target         string
	longSummary, longTarget bool
	// fingerprint is RequestFingerprint's.
	fingerprint string
	// matched has the bit of each registry in onDiskPredicates that matched
	// the mock.
	matched uint8
	// mysqlStatementLifecycle and mysqlSessionReusable are
	// IsMySQLStatementLifecycle's and mysqlIsSessionReusableCommand's.
	mysqlStatementLifecycle bool
	mysqlSessionReusable    bool
	// authored is IsAuthored's: whether the mock's Spec.Authored was set.
	authored bool
	// messageDelivery is isBuiltInMessageDelivery's: whether the mock carries
	// a built-in broker message delivery (SQS ReceiveMessage with Messages,
	// Kafka Fetch with records), answered before the payload was dropped.
	messageDelivery bool
	// replaces is Replaces': the recording a derived mock replaces in its
	// own test.
	replaces string
	// fault is a copy of the mock's Spec.Fault (FaultOf), nil for none: what
	// a replay's summary names when the mock was served (FaultServed).
	fault *Fault
	// httpPassThrough keeps the minimal HTTP request metadata (host, port,
	// method, path, and fingerprint headers only) needed to run
	// AnsweredByPassThrough after Spec.HTTPReq is dropped.
	httpPassThrough *httpPassThroughMeta
}

// onDiskPredicates is every registry whose predicates read a mock's payload;
// a mock whose payload is on disk keeps each one's answer by its index.
var onDiskPredicates = [...]*mockPredicates{&carryOverHooks, &stagedHooks, &releaseHooks, &answerPlacedHooks, &idleHooks, &sessionReusableHooks, &messageDeliveryHooks}

// predicateBit is r's bit in payloadOnDisk.matched.
func predicateBit(r *mockPredicates) uint8 {
	for i, p := range onDiskPredicates {
		if p == r {
			return 1 << i
		}
	}
	panic("models: a mock predicate registry that is not in onDiskPredicates")
}

// LeavePayloadOnDisk drops m's payload from memory: src reads it back, from
// the document at at, when the mock is sent (WithPayload). m keeps what a
// replay reads of it before then (see above). It is for a mock just decoded
// whose lifetime is derived (DeriveLifetime), and nothing else holds its
// payload.
func (m *Mock) LeavePayloadOnDisk(src PayloadSource, at PayloadAt) {
	d := &payloadOnDisk{
		src:                     src,
		at:                      at,
		mysqlStatementLifecycle: m.IsMySQLStatementLifecycle(),
		mysqlSessionReusable:    mysqlIsSessionReusableCommand(m),
	}
	d.fingerprint = requestFingerprint(m)
	d.summary, d.longSummary = keptDisplay(MockSummaryFromSpec(m))
	d.target, d.longTarget = keptDisplay(MockTargetFromSpec(m))
	d.authored = m.Spec.Authored != nil
	d.messageDelivery, _ = isBuiltInMessageDelivery(m)
	d.replaces = m.Spec.Authored.replaces()
	d.httpPassThrough = extractHTTPPassThrough(m)
	if f := m.Spec.Fault; f != nil {
		kept := *f
		kept.Observed = strings.Clone(f.Observed)
		d.fault = &kept
	}
	for i, r := range onDiskPredicates {
		if r.match(m) {
			d.matched |= 1 << i
		}
	}
	metadata := m.Spec.Metadata
	delete(metadata, "operation")
	m.Spec = MockSpec{
		Metadata:         metadata,
		Created:          m.Spec.Created,
		ReqTimestampMock: m.Spec.ReqTimestampMock,
		ResTimestampMock: m.Spec.ResTimestampMock,
		Async:            m.Spec.Async,
	}
	m.Noise = nil
	m.onDisk = d
}

// maxKeptDisplay is the longest summary or target (MockSummaryFromSpec,
// MockTargetFromSpec) a mock whose payload is on disk keeps.
const maxKeptDisplay = 256

// keptDisplay is s as a mock whose payload is on disk keeps it: a copy of its
// own, which holds nothing of the payload, or none when s is longer than
// maxKeptDisplay (long).
func keptDisplay(s string) (kept string, long bool) {
	if len(s) > maxKeptDisplay {
		return "", true
	}
	return strings.Clone(s), false
}

// display works out of (MockSummaryFromSpec or MockTargetFromSpec) for m, a
// mock whose payload is on disk, from its payload read back as recorded:
// the source's mutator does not change it. A payload that cannot be read
// back (its file was rewritten once its set ran) has its kind's own, as a mock
// that carries nothing identifying does.
func (d *payloadOnDisk) display(m *Mock, of func(*Mock) string, unknown string) string {
	read, err := d.src.LoadPayload(context.Background(), d.at)
	if err != nil {
		return unknown
	}
	c := *m
	c.onDisk = nil
	c.Spec = read.Spec
	return of(&c)
}

// OnDiskView is what a report reads of a mock whose payload a replay's read
// left on disk (LeavePayloadOnDisk), kept without the mock: its name and kind,
// when its request was recorded, its lifetime, and what the read kept to name
// it (its summary, target and request fingerprint), the payload read back
// for what it did not keep. A replay's lookup of a set's mocks holds a view
// of each, not the mock: the mock (the Mock, its metadata) is the set's read,
// about a kilobyte a mock, and goes once the mocks are on the agent, rather
// than for the rest of the set's run.
type OnDiskView struct {
	name       string
	kind       Kind
	recordedAt time.Time
	lifetime   Lifetime
	d          *payloadOnDisk
}

// OnDiskView is m's view (above); nil when m's payload is in memory.
func (m *Mock) OnDiskView() *OnDiskView {
	if !m.PayloadOnDisk() {
		return nil
	}
	return &OnDiskView{name: m.Name, kind: m.Kind, recordedAt: m.Spec.ReqTimestampMock, lifetime: m.TestModeInfo.Lifetime, d: m.onDisk}
}

// skeleton is the mock the view is of, as display reads it: its name and
// kind, its payload on disk.
func (v *OnDiskView) skeleton() *Mock {
	return &Mock{Name: v.name, Kind: v.kind}
}

// Summary is MockSummaryFromSpec of the mock.
func (v *OnDiskView) Summary() string {
	if v.d.longSummary {
		return v.d.display(v.skeleton(), MockSummaryFromSpec, string(v.kind))
	}
	return v.d.summary
}

// Target is MockTargetFromSpec of the mock.
func (v *OnDiskView) Target() string {
	if v.d.longTarget {
		return v.d.display(v.skeleton(), MockTargetFromSpec, "")
	}
	return v.d.target
}

// Operation is MockOperation of the mock as recorded, its payload read back:
// a read keeps none of it.
func (v *OnDiskView) Operation() string {
	return v.d.display(v.skeleton(), MockOperation, "")
}

// RequestFingerprint is the mock's Mock.RequestFingerprint.
func (v *OnDiskView) RequestFingerprint() string { return v.d.fingerprint }

// RecordedAt is when the mock's request was recorded (its
// Spec.ReqTimestampMock).
func (v *OnDiskView) RecordedAt() time.Time { return v.recordedAt }

// Lifetime is the mock's TestModeInfo.Lifetime.
func (v *OnDiskView) Lifetime() Lifetime { return v.lifetime }

// IndexEntryFrom is a copy of m, a mock whose payload is on disk
// (LeavePayloadOnDisk), whose payload src reads back from the same place in
// its file: the entry a read of a mock file left, reused for a later read of a
// file of the same content (mockdb's index cache). The copy is m as that read
// left it, unpooled, and shares nothing a replay changes in place with m: its
// metadata, async block and origin are its own. It is nil when m's payload is
// not on disk.
func (m *Mock) IndexEntryFrom(src PayloadSource) *Mock {
	if !m.PayloadOnDisk() {
		return nil
	}
	c := *m
	c.pooled = false
	c.responseHydrator = nil
	d := *m.onDisk
	d.src = src
	c.onDisk = &d
	if m.Spec.Metadata != nil {
		c.Spec.Metadata = maps.Clone(m.Spec.Metadata)
	}
	if m.Spec.Async != nil {
		a := *m.Spec.Async
		c.Spec.Async = &a
	}
	c.Origin = m.Origin.clone()
	return &c
}

// IndexEntryBytes estimates the heap m, a mock whose payload is on disk, holds
// of its own: the Mock and what LeavePayloadOnDisk kept of it (its strings,
// metadata, async block and origin, and the payload's place and the answers
// read from it). It is what a kept entry costs a cache of entries, 0 when m's
// payload is not on disk.
func (m *Mock) IndexEntryBytes() int64 {
	if !m.PayloadOnDisk() {
		return 0
	}
	// A string's bytes are allocated in a size class, a little more than
	// its length; a map takes its header and groups of eight slots, each
	// slot a key's and a value's string header, filled to seven.
	const slot = 2 * int(unsafe.Sizeof(""))
	str := func(s string) int {
		if s == "" {
			return 0
		}
		return len(s) + 8
	}
	n := int(unsafe.Sizeof(*m)) + int(unsafe.Sizeof(*m.onDisk))
	n += str(string(m.Version)) + str(m.Name) + str(string(m.Kind)) + str(m.ConnectionID) + str(m.Start) + str(m.StartRef)
	if md := m.Spec.Metadata; md != nil {
		n += 48 + max(1, (len(md)+6)/7)*(8+8*slot)
		for k, v := range md {
			n += str(k) + str(v)
		}
	}
	if a := m.Spec.Async; a != nil {
		n += int(unsafe.Sizeof(*a)) + str(a.Lane) + str(a.AnchorAfter)
	}
	if o := m.Origin; o != nil {
		n += int(unsafe.Sizeof(*o)) + str(o.Clock) + str(o.Comm) + str(o.Exe)
	}
	d := m.onDisk
	// d.at.Name is m.Name's string, not a copy.
	n += str(d.summary) + str(d.target) + str(d.fingerprint)
	if f := d.fault; f != nil {
		n += int(unsafe.Sizeof(*f)) + str(f.Observed)
	}
	if pt := d.httpPassThrough; pt != nil {
		n += int(unsafe.Sizeof(*pt)) + str(pt.host) + str(pt.method) + str(pt.path)
		if pt.header != nil {
			n += 48 + max(1, (len(pt.header)+6)/7)*(8+8*slot)
			for k, vs := range pt.header {
				n += str(k)
				for _, v := range vs {
					n += str(v)
				}
			}
		}
	}
	return int64(n)
}

// PayloadOnDisk reports whether m's payload was left on disk
// (LeavePayloadOnDisk): m is not the mock as recorded until WithPayload reads
// it back.
func (m *Mock) PayloadOnDisk() bool { return m != nil && m.onDisk != nil }

// PayloadDiskBytes returns the on-disk document length in bytes when m's
// payload was left on disk, or 0 if m is already in memory.
func (m *Mock) PayloadDiskBytes() int {
	if !m.PayloadOnDisk() {
		return 0
	}
	return m.onDisk.at.Length
}

// WithPayload is the mock to send: m itself when its payload is in memory,
// else a copy of m with its payload read back from its file and changed by the
// source's mutator. The copy is to be sent and dropped: m keeps its payload on
// disk, so holding m holds no payload.
func (m *Mock) WithPayload(ctx context.Context) (*Mock, error) {
	if !m.PayloadOnDisk() {
		return m, nil
	}
	d := m.onDisk
	read, err := d.src.LoadPayload(ctx, d.at)
	if err != nil {
		return nil, err
	}
	c := m.ShallowCopy()
	c.onDisk = nil
	c.Spec = read.Spec
	c.Noise = read.Noise
	if err := MutatePayload(ctx, c, d.src.PayloadMutator()); err != nil {
		return nil, err
	}
	return c, nil
}

// WithPayloads resolves a batch of mocks for sending, preserving order without
// mutating caller slices. Mocks whose payloads were left on disk are grouped by
// their PayloadSource: sources implementing BatchPayloadSource read all mocks
// for that source in a single LoadPayloadBatch call, and sources with a
// BatchPayloadMutator mutate them in a single call (falling back to single-mock
// LoadPayload / PayloadMutator otherwise). Returns the resolved mocks and how
// many were read back from disk.
func WithPayloads(ctx context.Context, mocks []*Mock) ([]*Mock, int64, error) {
	out := make([]*Mock, len(mocks))
	if len(mocks) == 0 {
		return out, 0, nil
	}

	type srcGroup struct {
		src     PayloadSource
		indices []int
		ats     []PayloadAt
	}
	var groups []srcGroup
	var readBack int64

	for i, m := range mocks {
		if !m.PayloadOnDisk() {
			out[i] = m
			continue
		}
		readBack++
		d := m.onDisk
		gi := -1
		for k := range groups {
			if groups[k].src == d.src {
				gi = k
				break
			}
		}
		if gi < 0 {
			groups = append(groups, srcGroup{src: d.src})
			gi = len(groups) - 1
		}
		groups[gi].indices = append(groups[gi].indices, i)
		groups[gi].ats = append(groups[gi].ats, d.at)
	}

	for _, g := range groups {
		var loaded []*Mock
		if batcher, ok := g.src.(BatchPayloadSource); ok {
			var err error
			loaded, err = batcher.LoadPayloadBatch(ctx, g.ats)
			if err != nil {
				return nil, readBack, err
			}
			if len(loaded) != len(g.ats) {
				return nil, readBack, fmt.Errorf("batch payload load returned %d mocks, want %d", len(loaded), len(g.ats))
			}
		} else {
			loaded = make([]*Mock, len(g.ats))
			for j, at := range g.ats {
				read, err := g.src.LoadPayload(ctx, at)
				if err != nil {
					return nil, readBack, err
				}
				loaded[j] = read
			}
		}

		groupCopies := make([]*Mock, len(g.indices))
		for j, idx := range g.indices {
			c := mocks[idx].ShallowCopy()
			c.onDisk = nil
			c.Spec = loaded[j].Spec
			c.Noise = loaded[j].Noise
			groupCopies[j] = c
			out[idx] = c
		}

		if bmSrc, ok := g.src.(BatchPayloadMutatorSource); ok {
			if bm := bmSrc.BatchPayloadMutator(); bm != nil {
				before := make([]poolKey, len(groupCopies))
				for j, c := range groupCopies {
					before[j] = poolKeyOf(c)
				}
				if err := bm(ctx, groupCopies); err != nil {
					var refusal *MockPayloadRefusal
					if errors.As(err, &refusal) {
						return nil, readBack, err
					}
					name := ""
					if len(before) > 0 {
						name = before[0].name
					}
					return nil, readBack, &MockPayloadRefusal{Mock: name, Err: err}
				}
				for j, c := range groupCopies {
					if after := poolKeyOf(c); after != before[j] {
						return nil, readBack, fmt.Errorf("%w: mock %q, before %+v, after %+v", ErrPayloadMutatorChangedMock, before[j].name, before[j], after)
					}
				}
				continue
			}
		}

		if mut := g.src.PayloadMutator(); mut != nil {
			for _, c := range groupCopies {
				if err := MutatePayload(ctx, c, mut); err != nil {
					return nil, readBack, err
				}
			}
		}
	}

	return out, readBack, nil
}

// MutatePayload has mutate change m's payload, and fails when mutate fails
// (MockPayloadRefusal) or changed anything else that decides m's pool or its
// place in it (ErrPayloadMutatorChangedMock). A nil mutate changes nothing.
func MutatePayload(ctx context.Context, m *Mock, mutate PayloadMutator) error {
	if mutate == nil {
		return nil
	}
	before := poolKeyOf(m)
	if err := mutate(ctx, m); err != nil {
		return &MockPayloadRefusal{Mock: before.name, Err: err}
	}
	if after := poolKeyOf(m); after != before {
		return fmt.Errorf("%w: mock %q, before %+v, after %+v", ErrPayloadMutatorChangedMock, before.name, before, after)
	}
	return nil
}

// poolKey is what of a mock decides its pool and its place in it.
type poolKey struct {
	name, kind, typ, lifetime string
	info                      TestModeInfo
	req, res                  int64
}

func poolKeyOf(m *Mock) poolKey {
	return poolKey{
		name:     m.Name,
		kind:     string(m.Kind),
		typ:      m.Spec.Metadata["type"],
		lifetime: m.Spec.Metadata[MetadataLifetimeKey],
		info:     m.TestModeInfo,
		req:      m.Spec.ReqTimestampMock.UnixNano(),
		res:      m.Spec.ResTimestampMock.UnixNano(),
	}
}
