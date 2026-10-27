package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
}

// onDiskPredicates is every registry whose predicates read a mock's payload;
// a mock whose payload is on disk keeps each one's answer by its index.
var onDiskPredicates = [...]*mockPredicates{&carryOverHooks, &stagedHooks, &releaseHooks, &answerPlacedHooks, &idleHooks, &sessionReusableHooks}

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

// PayloadOnDisk reports whether m's payload was left on disk
// (LeavePayloadOnDisk): m is not the mock as recorded until WithPayload reads
// it back.
func (m *Mock) PayloadOnDisk() bool { return m != nil && m.onDisk != nil }

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

// MutatePayload has mutate change m's payload, and fails when it changed
// anything else that decides m's pool or its place in it
// (ErrPayloadMutatorChangedMock). A nil mutate changes nothing.
func MutatePayload(ctx context.Context, m *Mock, mutate PayloadMutator) error {
	if mutate == nil {
		return nil
	}
	before := poolKeyOf(m)
	if err := mutate(ctx, m); err != nil {
		return fmt.Errorf("the mock payload hook failed on mock %q: %w", before.name, err)
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
