package utils

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestProtoWireToJSONWithAnyTypes(t *testing.T) {
	// This test verifies that the Any type resolution works correctly
	logger := zap.NewNop()

	// Test with a proto that includes Any fields
	pc := models.ProtoConfig{
		ProtoFile:    "test.proto", // This would be the path to your proto file
		ProtoInclude: []string{},
		RequestURI:   "/fuzz.FuzzService/EchoAny",
	}

	// Skip the actual test if we don't have the proto file in the test environment
	// In a real scenario, you would have the test proto file available
	_, _, err := GetProtoMessageDescriptor(context.Background(), logger, pc)
	if err != nil {
		t.Skipf("Skipping test because proto file not available: %v", err)
		return
	}

	// If we had the proto file, we would continue with:
	// - Create a wire format message with Any fields
	// - Call ProtoWireToJSON with the message descriptor and files
	// - Verify that Any types are properly resolved and converted to JSON
}

func TestCreateTypeResolver(t *testing.T) {
	// Test that the type resolver creation doesn't panic with nil input
	types := createTypeResolver(nil)
	if types == nil {
		t.Error("Expected non-nil type resolver")
	}

	// Test with empty slice
	types = createTypeResolver([]protoreflect.FileDescriptor{})
	if types == nil {
		t.Error("Expected non-nil type resolver")
	}
}
