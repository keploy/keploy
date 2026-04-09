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
}

// RecordHooks allows enterprise (or any consumer) to inject behaviour into the
// OSS recording pipeline — the same pattern as TestHooks for the replay service.
type RecordHooks interface {
	BeforeTestCaseInsert(ctx context.Context, info *TestCaseContext) error
	AfterTestCaseInsert(ctx context.Context, info *TestCaseContext) error
	BeforeMockInsert(ctx context.Context, info *MockContext) error
	AfterMockInsert(ctx context.Context, info *MockContext) error
}

// BaseRecordHooks is an embeddable no-op implementation.
// Consumers embed this and override only the hooks they need.
type BaseRecordHooks struct{}

func (BaseRecordHooks) BeforeTestCaseInsert(context.Context, *TestCaseContext) error { return nil }
func (BaseRecordHooks) AfterTestCaseInsert(context.Context, *TestCaseContext) error  { return nil }
func (BaseRecordHooks) BeforeMockInsert(context.Context, *MockContext) error         { return nil }
func (BaseRecordHooks) AfterMockInsert(context.Context, *MockContext) error          { return nil }
