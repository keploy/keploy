package record

import (
	"bytes"
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// TestCaseContext is passed to test-case hooks.
// Use a struct so new fields can be added without breaking implementations.
type TestCaseContext struct {
	TestCase  *models.TestCase
	TestSetID string
}

// MockContext is passed to mock hooks.
type MockContext struct {
	Mock      *models.Mock
	TestSetID string
	// Encoded, on AfterMockInsert, is the document the MockDB wrote to the
	// mocks file for Mock, byte for byte, in EncodedFormat: one YAML document
	// (without the "---" line that separates it from the one before, or the
	// version comment that heads the file), or one JSON line. A hook that keeps
	// a copy of the mocks file (k8s-proxy's full-mock archive) takes it from
	// here instead of encoding the mock a second time.
	//
	// mockdb.MockYaml hands its document back (models.WithMockDocReceiver).
	// Encoded is nil when the MockDB handed none: it wrote gob (in the
	// background), it is a MockDB that does not hand documents back, or it
	// handed two for the mock (it wrote it to two stores). A hook that needs
	// the document encodes the mock itself then.
	//
	// It is a copy made for this mock: a hook may keep it. It must not modify
	// it, since every hook in a chain is handed the same bytes.
	Encoded []byte
	// EncodedFormat is Encoded's format, "yaml" or "json" (the storageFormat
	// spelling), and empty when Encoded is nil. It is the format the MockDB
	// wrote in, which is not the configured one when the test set already had
	// a mocks file in the other.
	EncodedFormat string
	// Skip, when set by a BeforeMockInsert hook, tells the recorder to DROP this
	// mock (do not persist/map/correlate it) — e.g. the AsyncRecorder collapsing
	// an unchanged poll cycle.
	Skip bool
}

// RecordingCompleteContext is passed to the end-of-recording hook, once every
// test case and mock in the set has been drained to disk.
type RecordingCompleteContext struct {
	TestSetID string
	Path      string // keploy base path; the test set lives at <Path>/<TestSetID>
}

// RecordHooks allows enterprise (or any consumer) to inject behaviour into the
// OSS recording pipeline — the same pattern as TestHooks for the replay service.
type RecordHooks interface {
	BeforeTestCaseInsert(ctx context.Context, info *TestCaseContext) error
	AfterTestCaseInsert(ctx context.Context, info *TestCaseContext) error
	BeforeMockInsert(ctx context.Context, info *MockContext) error
	AfterMockInsert(ctx context.Context, info *MockContext) error
	// AfterRecordingComplete runs once, after the whole test set is persisted.
	// Used for cross-artifact passes that need every test case and mock on disk
	// (e.g. the Basic-Auth re-key correlation).
	AfterRecordingComplete(ctx context.Context, info *RecordingCompleteContext) error
}

// BaseRecordHooks is an embeddable no-op implementation.
// Consumers embed this and override only the hooks they need.
type BaseRecordHooks struct{}

func (BaseRecordHooks) BeforeTestCaseInsert(context.Context, *TestCaseContext) error { return nil }
func (BaseRecordHooks) AfterTestCaseInsert(context.Context, *TestCaseContext) error  { return nil }
func (BaseRecordHooks) BeforeMockInsert(context.Context, *MockContext) error         { return nil }
func (BaseRecordHooks) AfterMockInsert(context.Context, *MockContext) error          { return nil }
func (BaseRecordHooks) AfterRecordingComplete(context.Context, *RecordingCompleteContext) error {
	return nil
}

// InsertedMockDoc inserts a recorder's mocks and returns the document the
// MockDB wrote for each, for the AfterMockInsert hooks (MockContext.Encoded).
// A recorder makes one for the goroutine that inserts its mocks.
type InsertedMockDoc struct {
	ctx    context.Context
	mock   *models.Mock
	doc    []byte
	format string
	handed int
}

// NewInsertedMockDoc returns the InsertedMockDoc of a recorder that inserts its
// mocks on ctx and hands them to hooks. Its context carries one receiver for
// every mock's document (models.WithMockDocReceiver), unless hooks are the
// no-op ones, which would not read it.
func NewInsertedMockDoc(ctx context.Context, hooks RecordHooks) *InsertedMockDoc {
	d := &InsertedMockDoc{ctx: ctx}
	if _, noop := hooks.(BaseRecordHooks); !noop {
		d.ctx = models.WithMockDocReceiver(ctx, d.receive)
	}
	return d
}

// Insert inserts mock into db, and returns the document db handed back for it
// and its format; nil and "" when it handed none. The document is a copy (the
// MockDB's buffer is reused once InsertMock returns), which is what handing it
// on costs per mock: one allocation of its size. The hooks may keep it.
func (d *InsertedMockDoc) Insert(db interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
}, mock *models.Mock, testSetID string) (doc []byte, format string, err error) {
	d.mock = mock
	err = db.InsertMock(d.ctx, mock, testSetID)
	doc, format, handed := d.doc, d.format, d.handed
	d.mock, d.doc, d.format, d.handed = nil, nil, "", 0
	if err != nil || handed != 1 {
		return nil, "", err
	}
	return doc, format, nil
}

// receive keeps a copy of a document handed back for the mock being inserted.
// One handed back for another mock is not this one's, and an empty one is no
// document. Of two for this mock (a MockDB that wrote it to two stores),
// neither is known to be the mocks file's: Insert returns none.
func (d *InsertedMockDoc) receive(mock *models.Mock, doc []byte, format string) {
	if mock != d.mock || len(doc) == 0 || format == "" {
		return
	}
	d.handed++
	if d.handed == 1 {
		d.doc, d.format = bytes.Clone(doc), format
	}
}
