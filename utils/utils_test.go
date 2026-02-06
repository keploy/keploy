// Package utils provides utility functions for the Keploy application.
// This file contains unit tests for the utility functions.
package utils

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// IsShutdownError Tests
// =============================================================================

// TestIsShutdownError validates the detection of shutdown-related errors.
func TestIsShutdownError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "io.EOF error",
			err:      io.EOF,
			expected: true,
		},
		{
			name:     "io.ErrUnexpectedEOF error",
			err:      io.ErrUnexpectedEOF,
			expected: true,
		},
		{
			name:     "connection refused error",
			err:      errors.New("dial tcp: connection refused"),
			expected: true,
		},
		{
			name:     "connection reset error",
			err:      errors.New("read: connection reset by peer"),
			expected: true,
		},
		{
			name:     "broken pipe error",
			err:      errors.New("write: broken pipe"),
			expected: true,
		},
		{
			name:     "closed network connection error",
			err:      errors.New("use of closed network connection"),
			expected: true,
		},
		{
			name:     "EOF in error message",
			err:      errors.New("unexpected EOF while reading"),
			expected: true,
		},
		{
			name:     "regular error",
			err:      errors.New("some random error"),
			expected: false,
		},
		{
			name:     "timeout error (not shutdown)",
			err:      errors.New("context deadline exceeded"),
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := IsShutdownError(tc.err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// ReplaceHost Tests
// =============================================================================

// TestReplaceHost validates URL host replacement functionality.
func TestReplaceHost(t *testing.T) {
	testCases := []struct {
		name        string
		currentURL  string
		ipAddress   string
		expected    string
		expectError bool
	}{
		{
			name:        "valid URL with host replacement",
			currentURL:  "http://localhost:8080/api/v1",
			ipAddress:   "192.168.1.100",
			expected:    "http://192.168.1.100:8080/api/v1",
			expectError: false,
		},
		{
			name:        "URL without port",
			currentURL:  "http://example.com/path",
			ipAddress:   "10.0.0.1",
			expected:    "http://10.0.0.1/path",
			expectError: false,
		},
		{
			name:        "HTTPS URL",
			currentURL:  "https://secure.example.com:443/secure",
			ipAddress:   "172.16.0.1",
			expected:    "https://172.16.0.1:443/secure",
			expectError: false,
		},
		{
			name:        "empty IP address",
			currentURL:  "http://localhost:8080/api",
			ipAddress:   "",
			expected:    "http://localhost:8080/api",
			expectError: true,
		},
		{
			name:        "URL with query parameters",
			currentURL:  "http://api.example.com:3000/users?id=123",
			ipAddress:   "192.168.0.50",
			expected:    "http://192.168.0.50:3000/users?id=123",
			expectError: false,
		},
		{
			name:        "URL with fragment",
			currentURL:  "http://docs.example.com/page#section",
			ipAddress:   "10.10.10.10",
			expected:    "http://10.10.10.10/page#section",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReplaceHost(tc.currentURL, tc.ipAddress)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// =============================================================================
// ReplaceGrpcHost Tests
// =============================================================================

// TestReplaceGrpcHost validates gRPC authority host replacement.
func TestReplaceGrpcHost(t *testing.T) {
	testCases := []struct {
		name        string
		authority   string
		ipAddress   string
		expected    string
		expectError bool
	}{
		{
			name:        "valid authority replacement",
			authority:   "localhost:50051",
			ipAddress:   "192.168.1.100",
			expected:    "192.168.1.100:50051",
			expectError: false,
		},
		{
			name:        "empty IP address",
			authority:   "localhost:50051",
			ipAddress:   "",
			expected:    "localhost:50051",
			expectError: true,
		},
		{
			name:        "invalid authority format (no port)",
			authority:   "localhost",
			ipAddress:   "192.168.1.100",
			expected:    "localhost",
			expectError: true,
		},
		{
			name:        "IPv6 authority",
			authority:   "[::1]:50051",
			ipAddress:   "192.168.1.100",
			expected:    "192.168.1.100:50051",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReplaceGrpcHost(tc.authority, tc.ipAddress)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// =============================================================================
// ReplaceGrpcPort Tests
// =============================================================================

// TestReplaceGrpcPort validates gRPC authority port replacement.
func TestReplaceGrpcPort(t *testing.T) {
	testCases := []struct {
		name        string
		authority   string
		port        string
		expected    string
		expectError bool
	}{
		{
			name:        "valid port replacement",
			authority:   "localhost:50051",
			port:        "8080",
			expected:    "localhost:8080",
			expectError: false,
		},
		{
			name:        "empty port",
			authority:   "localhost:50051",
			port:        "",
			expected:    "localhost:50051",
			expectError: true,
		},
		{
			name:        "authority without port - adds port",
			authority:   "localhost",
			port:        "9090",
			expected:    "localhost:9090",
			expectError: false,
		},
		{
			name:        "IPv6 authority with port replacement",
			authority:   "[::1]:50051",
			port:        "8080",
			expected:    "[::1]:8080",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReplaceGrpcPort(tc.authority, tc.port)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// =============================================================================
// ReplaceBaseURL Tests
// =============================================================================

// TestReplaceBaseURL validates base URL replacement in URLs.
func TestReplaceBaseURL(t *testing.T) {
	testCases := []struct {
		name        string
		currentURL  string
		baseURL     string
		expected    string
		expectError bool
	}{
		{
			name:        "valid base URL replacement",
			currentURL:  "http://localhost:8080/api/v1/users",
			baseURL:     "https://production.example.com",
			expected:    "https://production.example.com/api/v1/users",
			expectError: false,
		},
		{
			name:        "empty base URL",
			currentURL:  "http://localhost:8080/api",
			baseURL:     "",
			expected:    "http://localhost:8080/api",
			expectError: true,
		},
		{
			name:        "base URL with port",
			currentURL:  "http://localhost/path",
			baseURL:     "https://api.example.com:443",
			expected:    "https://api.example.com:443/path",
			expectError: false,
		},
		{
			name:        "preserve query parameters",
			currentURL:  "http://localhost:3000/search?q=test",
			baseURL:     "https://search.example.com",
			expected:    "https://search.example.com/search?q=test",
			expectError: false,
		},
		{
			name:        "preserve fragment",
			currentURL:  "http://localhost/page#section",
			baseURL:     "https://docs.example.com",
			expected:    "https://docs.example.com/page#section",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReplaceBaseURL(tc.currentURL, tc.baseURL)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// =============================================================================
// ReplacePort Tests
// =============================================================================

// TestReplacePort validates port replacement in URLs.
func TestReplacePort(t *testing.T) {
	testCases := []struct {
		name        string
		currentURL  string
		port        string
		expected    string
		expectError bool
	}{
		{
			name:        "replace existing port",
			currentURL:  "http://localhost:8080/api",
			port:        "3000",
			expected:    "http://localhost:3000/api",
			expectError: false,
		},
		{
			name:        "add port to URL without port",
			currentURL:  "http://localhost/api",
			port:        "8080",
			expected:    "http://localhost:8080/api",
			expectError: false,
		},
		{
			name:        "empty port",
			currentURL:  "http://localhost:8080/api",
			port:        "",
			expected:    "http://localhost:8080/api",
			expectError: true,
		},
		{
			name:        "HTTPS URL port replacement",
			currentURL:  "https://secure.example.com:443/secure",
			port:        "8443",
			expected:    "https://secure.example.com:8443/secure",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReplacePort(tc.currentURL, tc.port)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// =============================================================================
// GetReqMeta Tests
// =============================================================================

// TestGetReqMeta validates HTTP request metadata extraction.
func TestGetReqMeta(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		result := GetReqMeta(nil)
		assert.NotNil(t, result)
		assert.Empty(t, result)
	})

	t.Run("valid request", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com/api/users", nil)
		require.NoError(t, err)
		req.Host = "example.com"

		result := GetReqMeta(req)
		assert.Equal(t, "GET", result["method"])
		assert.Equal(t, "http://example.com/api/users", result["url"])
		assert.Equal(t, "example.com", result["host"])
	})

	t.Run("POST request with different host", func(t *testing.T) {
		req, err := http.NewRequest("POST", "http://api.example.com/data", nil)
		require.NoError(t, err)
		req.Host = "api.example.com"

		result := GetReqMeta(req)
		assert.Equal(t, "POST", result["method"])
		assert.Equal(t, "api.example.com", result["host"])
	})
}

// =============================================================================
// kebabToCamel Tests
// =============================================================================

// TestKebabToCamel validates kebab-case to camelCase conversion.
func TestKebabToCamel(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple kebab case",
			input:    "hello-world",
			expected: "helloWorld",
		},
		{
			name:     "multiple hyphens",
			input:    "this-is-a-test",
			expected: "thisIsATest",
		},
		{
			name:     "single word no hyphen",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "two words",
			input:    "first-second",
			expected: "firstSecond",
		},
		{
			name:     "starts with hyphen",
			input:    "-leading",
			expected: "Leading",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := kebabToCamel(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// FindDockerCmd Tests
// =============================================================================

// TestFindDockerCmd validates Docker command type detection.
func TestFindDockerCmd(t *testing.T) {
	testCases := []struct {
		name     string
		cmd      string
		expected CmdType
	}{
		{
			name:     "empty command",
			cmd:      "",
			expected: Empty,
		},
		{
			name:     "docker run command",
			cmd:      "docker run -d nginx",
			expected: DockerRun,
		},
		{
			name:     "sudo docker run command",
			cmd:      "sudo docker run -it ubuntu bash",
			expected: DockerRun,
		},
		{
			name:     "docker container run command",
			cmd:      "docker container run nginx",
			expected: DockerRun,
		},
		{
			name:     "sudo docker container run command",
			cmd:      "sudo docker container run nginx",
			expected: DockerRun,
		},
		{
			name:     "docker start command",
			cmd:      "docker start my-container",
			expected: DockerStart,
		},
		{
			name:     "sudo docker start command",
			cmd:      "sudo docker start my-container",
			expected: DockerStart,
		},
		{
			name:     "docker container start command",
			cmd:      "docker container start my-container",
			expected: DockerStart,
		},
		{
			name:     "docker-compose command",
			cmd:      "docker-compose up -d",
			expected: DockerCompose,
		},
		{
			name:     "docker compose command (v2)",
			cmd:      "docker compose up -d",
			expected: DockerCompose,
		},
		{
			name:     "sudo docker-compose command",
			cmd:      "sudo docker-compose build",
			expected: DockerCompose,
		},
		{
			name:     "sudo docker compose command",
			cmd:      "sudo docker compose down",
			expected: DockerCompose,
		},
		{
			name:     "native command (go run)",
			cmd:      "go run main.go",
			expected: Native,
		},
		{
			name:     "native command (python)",
			cmd:      "python app.py",
			expected: Native,
		},
		{
			name:     "native command (npm)",
			cmd:      "npm start",
			expected: Native,
		},
		{
			name:     "uppercase docker run",
			cmd:      "DOCKER RUN nginx",
			expected: DockerRun,
		},
		{
			name:     "mixed case docker compose",
			cmd:      "Docker Compose up",
			expected: DockerCompose,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := FindDockerCmd(tc.cmd)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// IsDockerCmd Tests
// =============================================================================

// TestIsDockerCmd validates Docker command type checking.
func TestIsDockerCmd(t *testing.T) {
	testCases := []struct {
		name     string
		kind     CmdType
		expected bool
	}{
		{
			name:     "DockerRun is docker cmd",
			kind:     DockerRun,
			expected: true,
		},
		{
			name:     "DockerStart is docker cmd",
			kind:     DockerStart,
			expected: true,
		},
		{
			name:     "DockerCompose is docker cmd",
			kind:     DockerCompose,
			expected: true,
		},
		{
			name:     "Native is not docker cmd",
			kind:     Native,
			expected: false,
		},
		{
			name:     "Empty is not docker cmd",
			kind:     Empty,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := IsDockerCmd(tc.kind)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// ToInt Tests
// =============================================================================

// TestToInt validates type conversion to int.
func TestToInt(t *testing.T) {
	testCases := []struct {
		name     string
		value    interface{}
		expected int
	}{
		{
			name:     "int value",
			value:    42,
			expected: 42,
		},
		{
			name:     "int64 value",
			value:    int64(100),
			expected: 100,
		},
		{
			name:     "int32 value",
			value:    int32(50),
			expected: 50,
		},
		{
			name:     "float32 value",
			value:    float32(3.7),
			expected: 3,
		},
		{
			name:     "float64 value",
			value:    float64(9.99),
			expected: 9,
		},
		{
			name:     "string value",
			value:    "123",
			expected: 123,
		},
		{
			name:     "invalid string value",
			value:    "not-a-number",
			expected: 0,
		},
		{
			name:     "json.Number int",
			value:    json.Number("42"),
			expected: 42,
		},
		{
			name:     "json.Number float",
			value:    json.Number("42.5"),
			expected: 42,
		},
		{
			name:     "nil value",
			value:    nil,
			expected: 0,
		},
		{
			name:     "unsupported type (bool)",
			value:    true,
			expected: 0,
		},
		{
			name:     "negative int",
			value:    -10,
			expected: -10,
		},
		{
			name:     "zero",
			value:    0,
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ToInt(tc.value)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// ToString Tests
// =============================================================================

// TestToString validates type conversion to string.
func TestToString(t *testing.T) {
	testCases := []struct {
		name     string
		value    interface{}
		expected string
	}{
		{
			name:     "int value",
			value:    42,
			expected: "42",
		},
		{
			name:     "float64 value",
			value:    3.14,
			expected: "3.14",
		},
		{
			name:     "float32 value",
			value:    float32(2.5),
			expected: "2.5",
		},
		{
			name:     "int64 value",
			value:    int64(1000000),
			expected: "1000000",
		},
		{
			name:     "int32 value",
			value:    int32(500),
			expected: "500",
		},
		{
			name:     "string value",
			value:    "hello",
			expected: "hello",
		},
		{
			name:     "empty string",
			value:    "",
			expected: "",
		},
		{
			name:     "unsupported type (bool)",
			value:    true,
			expected: "",
		},
		{
			name:     "nil value",
			value:    nil,
			expected: "",
		},
		{
			name:     "negative int",
			value:    -100,
			expected: "-100",
		},
		{
			name:     "negative float",
			value:    -3.14,
			expected: "-3.14",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ToString(tc.value)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// ToFloat Tests
// =============================================================================

// TestToFloat validates type conversion to float64.
func TestToFloat(t *testing.T) {
	testCases := []struct {
		name     string
		value    interface{}
		expected float64
	}{
		{
			name:     "float64 value",
			value:    3.14,
			expected: 3.14,
		},
		{
			name:     "string value",
			value:    "2.718",
			expected: 2.718,
		},
		{
			name:     "int value",
			value:    42,
			expected: 42.0,
		},
		{
			name:     "invalid string value",
			value:    "not-a-float",
			expected: 0,
		},
		{
			name:     "nil value",
			value:    nil,
			expected: 0,
		},
		{
			name:     "unsupported type (bool)",
			value:    true,
			expected: 0,
		},
		{
			name:     "negative float",
			value:    -3.14,
			expected: -3.14,
		},
		{
			name:     "zero",
			value:    0.0,
			expected: 0.0,
		},
		{
			name:     "string integer",
			value:    "100",
			expected: 100.0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ToFloat(tc.value)
			assert.InDelta(t, tc.expected, result, 0.001)
		})
	}
}

// =============================================================================
// Keys Tests
// =============================================================================

// TestKeys validates map key extraction.
func TestKeys(t *testing.T) {
	t.Run("empty map", func(t *testing.T) {
		m := map[string][]string{}
		result := Keys(m)
		assert.Empty(t, result)
	})

	t.Run("single key", func(t *testing.T) {
		m := map[string][]string{
			"key1": {"value1"},
		}
		result := Keys(m)
		assert.Len(t, result, 1)
		assert.Contains(t, result, "key1")
	})

	t.Run("multiple keys", func(t *testing.T) {
		m := map[string][]string{
			"key1": {"value1"},
			"key2": {"value2", "value3"},
			"key3": {},
		}
		result := Keys(m)
		assert.Len(t, result, 3)
		assert.ElementsMatch(t, []string{"key1", "key2", "key3"}, result)
	})

	t.Run("nil map", func(t *testing.T) {
		var m map[string][]string
		result := Keys(m)
		assert.Empty(t, result)
	})
}

// =============================================================================
// CheckFileExists Tests
// =============================================================================

// TestCheckFileExists validates file existence checking.
func TestCheckFileExists(t *testing.T) {
	// Create a temporary file for testing
	tempFile, err := os.CreateTemp("", "test-file-*.txt")
	require.NoError(t, err)
	tempFilePath := tempFile.Name()
	tempFile.Close()
	defer os.Remove(tempFilePath)

	t.Run("existing file", func(t *testing.T) {
		result := CheckFileExists(tempFilePath)
		assert.True(t, result)
	})

	t.Run("non-existing file", func(t *testing.T) {
		result := CheckFileExists("/path/to/non-existing-file.txt")
		assert.False(t, result)
	})

	t.Run("empty path", func(t *testing.T) {
		result := CheckFileExists("")
		assert.False(t, result)
	})
}

// =============================================================================
// FileExists Tests
// =============================================================================

// TestFileExists validates file existence checking with directory distinction.
func TestFileExists(t *testing.T) {
	// Create a temporary file for testing
	tempFile, err := os.CreateTemp("", "test-file-*.txt")
	require.NoError(t, err)
	tempFilePath := tempFile.Name()
	tempFile.Close()
	defer os.Remove(tempFilePath)

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "test-dir-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	t.Run("existing file", func(t *testing.T) {
		exists, err := FileExists(tempFilePath)
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("directory returns false", func(t *testing.T) {
		exists, err := FileExists(tempDir)
		assert.NoError(t, err)
		assert.False(t, exists) // FileExists returns false for directories
	})

	t.Run("non-existing file", func(t *testing.T) {
		exists, err := FileExists("/path/to/non-existing-file.txt")
		assert.NoError(t, err)
		assert.False(t, exists)
	})
}

// =============================================================================
// GetAbsPath Tests
// =============================================================================

// TestGetAbsPath validates absolute path resolution.
func TestGetAbsPath(t *testing.T) {
	t.Run("relative path", func(t *testing.T) {
		result, err := GetAbsPath(".")
		assert.NoError(t, err)
		assert.True(t, filepath.IsAbs(result))
	})

	t.Run("current directory", func(t *testing.T) {
		cwd, err := os.Getwd()
		require.NoError(t, err)

		result, err := GetAbsPath(".")
		assert.NoError(t, err)
		assert.Equal(t, cwd, result)
	})

	t.Run("nested relative path", func(t *testing.T) {
		result, err := GetAbsPath("./subdir/file.txt")
		assert.NoError(t, err)
		assert.True(t, filepath.IsAbs(result))
		assert.Contains(t, result, "subdir")
	})

	t.Run("absolute path unchanged", func(t *testing.T) {
		if absPath, err := filepath.Abs("/tmp"); err == nil {
			result, err := GetAbsPath(absPath)
			assert.NoError(t, err)
			assert.True(t, filepath.IsAbs(result))
		}
	})
}

// =============================================================================
// Hash Tests
// =============================================================================

// TestHash validates SHA256 hashing.
func TestHash(t *testing.T) {
	t.Run("hash of empty data", func(t *testing.T) {
		result := Hash([]byte{})
		// SHA256 of empty string is known
		assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", result)
	})

	t.Run("hash of hello world", func(t *testing.T) {
		result := Hash([]byte("hello world"))
		// SHA256 of "hello world" is known
		assert.Equal(t, "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", result)
	})

	t.Run("same input produces same hash", func(t *testing.T) {
		data := []byte("test data")
		hash1 := Hash(data)
		hash2 := Hash(data)
		assert.Equal(t, hash1, hash2)
	})

	t.Run("different input produces different hash", func(t *testing.T) {
		hash1 := Hash([]byte("data1"))
		hash2 := Hash([]byte("data2"))
		assert.NotEqual(t, hash1, hash2)
	})

	t.Run("hash length is 64 characters", func(t *testing.T) {
		result := Hash([]byte("any data"))
		assert.Len(t, result, 64) // SHA256 produces 32 bytes = 64 hex chars
	})
}

// =============================================================================
// GetVersionAsComment Tests
// =============================================================================

// TestGetVersionAsComment validates version comment generation.
func TestGetVersionAsComment(t *testing.T) {
	// Save original version and restore after test
	originalVersion := Version
	defer func() { Version = originalVersion }()

	t.Run("with version set", func(t *testing.T) {
		Version = "1.0.0"
		result := GetVersionAsComment()
		assert.Equal(t, "# Generated by Keploy (1.0.0)\n", result)
	})

	t.Run("with empty version", func(t *testing.T) {
		Version = ""
		result := GetVersionAsComment()
		assert.Equal(t, "# Generated by Keploy ()\n", result)
	})

	t.Run("with dev version", func(t *testing.T) {
		Version = "3-dev"
		result := GetVersionAsComment()
		assert.Equal(t, "# Generated by Keploy (3-dev)\n", result)
	})
}

// =============================================================================
// RemoveDoubleQuotes Tests
// =============================================================================

// TestRemoveDoubleQuotes validates double quote removal from map values.
func TestRemoveDoubleQuotes(t *testing.T) {
	t.Run("map with quoted strings", func(t *testing.T) {
		tempMap := map[string]interface{}{
			"key1": `"value1"`,
			"key2": `"value2"`,
		}
		RemoveDoubleQuotes(tempMap)
		assert.Equal(t, "value1", tempMap["key1"])
		assert.Equal(t, "value2", tempMap["key2"])
	})

	t.Run("map with mixed values", func(t *testing.T) {
		tempMap := map[string]interface{}{
			"string": `"quoted"`,
			"number": 42,
			"bool":   true,
		}
		RemoveDoubleQuotes(tempMap)
		assert.Equal(t, "quoted", tempMap["string"])
		assert.Equal(t, 42, tempMap["number"])
		assert.Equal(t, true, tempMap["bool"])
	})

	t.Run("empty map", func(t *testing.T) {
		tempMap := map[string]interface{}{}
		RemoveDoubleQuotes(tempMap) // Should not panic
		assert.Empty(t, tempMap)
	})

	t.Run("complex quoted string", func(t *testing.T) {
		tempMap := map[string]interface{}{
			"header": `"Not/A)Brand";v="8", "Chromium";v="126"`,
		}
		RemoveDoubleQuotes(tempMap)
		assert.Equal(t, "Not/A)Brand;v=8, Chromium;v=126", tempMap["header"])
	})
}

// =============================================================================
// ExpandPath Tests
// =============================================================================

// TestExpandPath validates path expansion with tilde.
func TestExpandPath(t *testing.T) {
	t.Run("path without tilde", func(t *testing.T) {
		path := "/usr/local/bin"
		result, err := ExpandPath(path)
		assert.NoError(t, err)
		assert.Equal(t, path, result)
	})

	t.Run("relative path without tilde", func(t *testing.T) {
		path := "./local/path"
		result, err := ExpandPath(path)
		assert.NoError(t, err)
		assert.Equal(t, path, result)
	})

	t.Run("path with tilde", func(t *testing.T) {
		path := "~/mydir/file.txt"
		result, err := ExpandPath(path)
		assert.NoError(t, err)
		assert.NotContains(t, result, "~")
		assert.Contains(t, result, "mydir/file.txt")
	})

	t.Run("just tilde slash", func(t *testing.T) {
		path := "~/"
		result, err := ExpandPath(path)
		assert.NoError(t, err)
		assert.NotEqual(t, path, result)
	})
}

// =============================================================================
// IsFileEmpty Tests
// =============================================================================

// TestIsFileEmpty validates empty file detection.
func TestIsFileEmpty(t *testing.T) {
	// Create an empty temporary file
	emptyFile, err := os.CreateTemp("", "empty-*.txt")
	require.NoError(t, err)
	emptyFilePath := emptyFile.Name()
	emptyFile.Close()
	defer os.Remove(emptyFilePath)

	// Create a non-empty temporary file
	nonEmptyFile, err := os.CreateTemp("", "nonempty-*.txt")
	require.NoError(t, err)
	nonEmptyFilePath := nonEmptyFile.Name()
	_, err = nonEmptyFile.WriteString("some content")
	require.NoError(t, err)
	nonEmptyFile.Close()
	defer os.Remove(nonEmptyFilePath)

	t.Run("empty file", func(t *testing.T) {
		isEmpty, err := IsFileEmpty(emptyFilePath)
		assert.NoError(t, err)
		assert.True(t, isEmpty)
	})

	t.Run("non-empty file", func(t *testing.T) {
		isEmpty, err := IsFileEmpty(nonEmptyFilePath)
		assert.NoError(t, err)
		assert.False(t, isEmpty)
	})

	t.Run("non-existing file", func(t *testing.T) {
		_, err := IsFileEmpty("/path/to/non-existing-file.txt")
		assert.Error(t, err)
	})
}
