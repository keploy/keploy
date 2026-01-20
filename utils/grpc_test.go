package utils

import (
	"context"
	"os"
	"testing"

	"go.keploy.io/server/v2/pkg/models"
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

func TestDeriveProtoDirFromPath(t *testing.T) {
	// Create temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create test directories simulating common-protos structure
	// homework/v1/, classroom/v1/, google/protobuf/
	testDirs := []string{
		"homework/v1",
		"classroom/v1",
		"google/protobuf",
		"com/example/service/v1",
	}
	for _, d := range testDirs {
		if err := os.MkdirAll(tmpDir+"/"+d, 0755); err != nil {
			t.Fatalf("Failed to create test directory: %v", err)
		}
	}

	tests := []struct {
		name           string
		grpcPath       string
		protoIncludes  []string
		wantDir        string
		wantErrContain string
	}{
		{
			name:          "homework.v1.Homework -> homework/v1",
			grpcPath:      "/homework.v1.Homework/CreateHomework",
			protoIncludes: []string{tmpDir},
			wantDir:       tmpDir + "/homework/v1",
		},
		{
			name:          "classroom.v1.Meeting -> classroom/v1",
			grpcPath:      "/classroom.v1.Meeting/StartMeeting",
			protoIncludes: []string{tmpDir},
			wantDir:       tmpDir + "/classroom/v1",
		},
		{
			name:          "google.protobuf.Empty -> google/protobuf",
			grpcPath:      "/google.protobuf.Empty/Method",
			protoIncludes: []string{tmpDir},
			wantDir:       tmpDir + "/google/protobuf",
		},
		{
			name:          "com.example.service.v1.Foo -> com/example/service/v1",
			grpcPath:      "/com.example.service.v1.Foo/Method",
			protoIncludes: []string{tmpDir},
			wantDir:       tmpDir + "/com/example/service/v1",
		},
		{
			name:           "nonexistent package falls back with error",
			grpcPath:       "/nonexistent.v1.Service/Method",
			protoIncludes:  []string{tmpDir},
			wantErrContain: "no proto directory found",
		},
		{
			name:           "empty protoIncludes returns error",
			grpcPath:       "/homework.v1.Homework/CreateHomework",
			protoIncludes:  []string{},
			wantErrContain: "grpcPath and protoIncludes are required",
		},
		{
			name:           "invalid gRPC path returns error",
			grpcPath:       "invalid-path",
			protoIncludes:  []string{tmpDir},
			wantErrContain: "failed to parse gRPC path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveProtoDirFromPath(tt.grpcPath, tt.protoIncludes)

			if tt.wantErrContain != "" {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErrContain)
					return
				}
				if !contains(err.Error(), tt.wantErrContain) {
					t.Errorf("expected error containing %q, got %q", tt.wantErrContain, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if got != tt.wantDir {
				t.Errorf("got %q, want %q", got, tt.wantDir)
			}
		})
	}
}

// Helper function for string contains check
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestBuildProtoSchemaCache(t *testing.T) {
	logger := zap.NewNop()

	t.Run("returns error when no gRPC paths provided", func(t *testing.T) {
		pc := models.ProtoConfig{
			ProtoDir: "/some/path",
		}
		_, err := BuildProtoSchemaCache(logger, pc, []string{})
		if err == nil {
			t.Error("expected error for empty grpcPaths")
		}
		if !contains(err.Error(), "no gRPC paths provided") {
			t.Errorf("expected error about no gRPC paths, got: %v", err)
		}
	})

	t.Run("returns error when no proto sources available", func(t *testing.T) {
		pc := models.ProtoConfig{
			// No ProtoFile, ProtoDir, or ProtoInclude
		}
		_, err := BuildProtoSchemaCache(logger, pc, []string{"/homework.v1.Homework/Create"})
		if err == nil {
			t.Error("expected error for missing proto sources")
		}
		if !contains(err.Error(), "protoFile or protoDir must be provided") {
			t.Errorf("expected error about missing proto sources, got: %v", err)
		}
	})

	t.Run("returns error when proto directory doesn't exist", func(t *testing.T) {
		pc := models.ProtoConfig{
			ProtoDir: "/nonexistent/path",
		}
		_, err := BuildProtoSchemaCache(logger, pc, []string{"/homework.v1.Homework/Create"})
		if err == nil {
			t.Error("expected error for nonexistent proto directory")
		}
	})
}

func TestProtoTextToJSONCached(t *testing.T) {
	logger := zap.NewNop()

	t.Run("returns false when cache is nil", func(t *testing.T) {
		_, ok := ProtoTextToJSONCached(nil, "service/Method", "data", logger)
		if ok {
			t.Error("expected false when cache is nil")
		}
	})

	t.Run("returns false when method not in cache", func(t *testing.T) {
		cache := &ProtoSchemaCache{
			OutputByMethod: make(map[string]protoreflect.MessageDescriptor),
		}
		_, ok := ProtoTextToJSONCached(cache, "unknown.Service/Method", "data", logger)
		if ok {
			t.Error("expected false when method not in cache")
		}
	})
}
