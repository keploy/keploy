package record

import (
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
