package utils

import (
	"testing"

	"os"
	"path/filepath"
	"strings"

	"net/http"
	"net/url"

	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// TestReplaceHost_ValidAndInvalidInputs_001 tests ReplaceHost function with valid and invalid inputs.
func TestReplaceHost_ValidAndInvalidInputs_001(t *testing.T) {
	validURL := "http://example.com"
	invalidURL := "://invalid-url"
	ipAddress := "192.168.1.1"

	// Test valid URL
	result, err := ReplaceHost(validURL, ipAddress)
	require.NoError(t, err)
	assert.Equal(t, "http://192.168.1.1", result)

	// Test invalid URL
	result, err = ReplaceHost(invalidURL, ipAddress)
	require.Error(t, err)
	assert.Equal(t, invalidURL, result)

	// Test empty IP address
	result, err = ReplaceHost(validURL, "")
	require.Error(t, err)
	assert.Equal(t, validURL, result)
}

// TestReplaceBaseURL_ValidAndInvalidInputs_002 tests ReplaceBaseURL function with valid and invalid inputs.
func TestReplaceBaseURL_ValidAndInvalidInputs_002(t *testing.T) {
	validURL := "http://example.com/path"
	baseURL := "https://newbase.com"
	invalidBaseURL := "://invalid-base-url"

	// Test valid baseURL
	result, err := ReplaceBaseURL(validURL, baseURL)
	require.NoError(t, err)
	assert.Equal(t, "https://newbase.com/path", result)

	// Test invalid baseURL
	result, err = ReplaceBaseURL(validURL, invalidBaseURL)
	require.Error(t, err)
	assert.Equal(t, validURL, result)

	// Test empty baseURL
	result, err = ReplaceBaseURL(validURL, "")
	require.Error(t, err)
	assert.Equal(t, validURL, result)
}

// TestPathAndFileFunctions_AllCases_114 tests functions related to path manipulation and file system interactions.
func TestPathAndFileFunctions_AllCases_114(t *testing.T) {
	logger := zap.NewNop()

	t.Run("GetAbsPath and ToAbsPath", func(t *testing.T) {
		absPath, err := GetAbsPath(".")
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(absPath))

		toAbs := ToAbsPath(logger, "some/relative/path")
		assert.True(t, filepath.IsAbs(toAbs))
		assert.True(t, strings.HasSuffix(toAbs, "/some/relative/path/keploy"))

		toAbsEmpty := ToAbsPath(logger, "")
		assert.True(t, filepath.IsAbs(toAbsEmpty))
		assert.True(t, strings.HasSuffix(toAbsEmpty, "/keploy"))
	})

	t.Run("makeDirectory and DeleteFileIfNotExists", func(t *testing.T) {
		tempDir := t.TempDir()
		newDir := filepath.Join(tempDir, "newdir")
		err := makeDirectory(newDir)
		require.NoError(t, err)
		_, err = os.Stat(newDir)
		assert.NoError(t, err)

		newFile := filepath.Join(tempDir, "newfile.txt")
		err = os.WriteFile(newFile, []byte("content"), 0644)
		require.NoError(t, err)
		err = DeleteFileIfNotExists(logger, newFile)
		require.NoError(t, err)
		_, err = os.Stat(newFile)
		assert.True(t, os.IsNotExist(err))

		err = DeleteFileIfNotExists(logger, "/non/existent/file")
		require.NoError(t, err)
	})

	t.Run("CheckFileExists and FileExists", func(t *testing.T) {
		tempDir := t.TempDir()
		existingFile := filepath.Join(tempDir, "exists.txt")
		err := os.WriteFile(existingFile, []byte("content"), 0644)
		require.NoError(t, err)

		assert.True(t, CheckFileExists(existingFile))
		assert.False(t, CheckFileExists(filepath.Join(tempDir, "notexists.txt")))

		exists, err := FileExists(existingFile)
		require.NoError(t, err)
		assert.True(t, exists)

		exists, err = FileExists(tempDir) // is a directory
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("IsFileEmpty", func(t *testing.T) {
		tempDir := t.TempDir()
		emptyFile := filepath.Join(tempDir, "empty.txt")
		err := os.WriteFile(emptyFile, []byte{}, 0644)
		require.NoError(t, err)
		nonEmptyFile := filepath.Join(tempDir, "nonempty.txt")
		err = os.WriteFile(nonEmptyFile, []byte("data"), 0644)
		require.NoError(t, err)

		isEmpty, err := IsFileEmpty(emptyFile)
		require.NoError(t, err)
		assert.True(t, isEmpty)

		isEmpty, err = IsFileEmpty(nonEmptyFile)
		require.NoError(t, err)
		assert.False(t, isEmpty)

		_, err = IsFileEmpty("nonexistent.txt")
		require.Error(t, err)
	})

	t.Run("GetLastDirectory", func(t *testing.T) {
		// This is hard to test reliably, but we can check it doesn't error
		_, err := GetLastDirectory()
		assert.NoError(t, err)
	})
}

// TestTypeConversionFunctions_AllCases_116 tests the type conversion utility functions ToInt, ToString, and ToFloat.
func TestTypeConversionFunctions_AllCases_116(t *testing.T) {
	t.Run("ToInt", func(t *testing.T) {
		assert.Equal(t, 123, ToInt(123))
		assert.Equal(t, 45, ToInt("45"))
		assert.Equal(t, 78, ToInt(78.9))
		assert.Equal(t, 0, ToInt("abc"))
		assert.Equal(t, 0, ToInt(nil))
	})
	t.Run("ToString", func(t *testing.T) {
		assert.Equal(t, "123", ToString(123))
		assert.Equal(t, "45.6", ToString(45.6))
		assert.Equal(t, "hello", ToString("hello"))
		assert.Equal(t, "789", ToString(int64(789)))
		assert.Equal(t, "1234", ToString(int32(1234)))
		assert.Equal(t, "3.14", ToString(float32(3.14)))
		assert.Equal(t, "", ToString(nil))
	})
	t.Run("ToFloat", func(t *testing.T) {
		assert.Equal(t, 123.0, ToFloat(123))
		assert.Equal(t, 45.6, ToFloat("45.6"))
		assert.Equal(t, 78.9, ToFloat(78.9))
		assert.Equal(t, 0.0, ToFloat("abc"))
		assert.Equal(t, 0.0, ToFloat(nil))
	})
}

// TestConfigAndViper_AllCases_119 tests configuration related functions, including flag binding with Viper.
func TestConfigAndViper_AllCases_119(t *testing.T) {
	logger := zap.NewNop()

	t.Run("BindFlagsToViper", func(t *testing.T) {
		viper.Reset()
		cmd := &cobra.Command{Use: "testcmd"}
		cmd.Flags().String("my-flag", "default", "a test flag")
		cmd.Flags().Int("another-flag", 123, "another flag")

		err := BindFlagsToViper(logger, cmd, "keploy")
		require.NoError(t, err)

		assert.Equal(t, "default", viper.GetString("keploy.myFlag"))
		assert.Equal(t, 123, viper.GetInt("keploy.anotherFlag"))

		// Test env var binding
		os.Setenv("KEPLOY_MYFLAG", "from_env")
		defer os.Unsetenv("KEPLOY_MYFLAG")
		// Re-bind to pick up env var
		err = BindFlagsToViper(logger, cmd, "keploy")
		require.NoError(t, err)
		assert.Equal(t, "from_env", viper.GetString("keploy.myFlag"))
	})

	t.Run("SetCoveragePath", func(t *testing.T) {
		tempDir := t.TempDir()
		// Case 1: Empty path
		covPath, err := SetCoveragePath(logger, "")
		require.NoError(t, err)
		assert.Contains(t, covPath, "coverage-reports")
		os.RemoveAll(covPath) // clean up

		// Case 2: Valid directory
		covPath, err = SetCoveragePath(logger, tempDir)
		require.NoError(t, err)
		assert.Equal(t, tempDir, covPath)

		// Case 3: Path is a file
		file, err := os.Create(filepath.Join(tempDir, "file.txt"))
		require.NoError(t, err)
		file.Close()
		_, err = SetCoveragePath(logger, file.Name())
		require.Error(t, err)

		// Case 4: Path does not exist
		_, err = SetCoveragePath(logger, filepath.Join(tempDir, "nonexistent"))
		require.Error(t, err)
	})
}

// TestIsPassThrough_AllCases_121 tests the IsPassThrough function with various rules and request combinations.
func TestIsPassThrough_AllCases_121(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/path/123", nil)
	req.Host = "example.com:8080"

	tests := []struct {
		name     string
		opts     models.OutgoingOptions
		destPort uint
		want     bool
	}{
		{
			name:     "match host and port",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: "example.com", Port: 8080}}},
			destPort: 8080,
			want:     true,
		},
		{
			name:     "match host regex",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: `^ex.*\.com$`, Port: 8080}}},
			destPort: 8080,
			want:     true,
		},
		{
			name:     "match path regex",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Path: `/path/\d+$`}}},
			destPort: 80,
			want:     true,
		},
		{
			name:     "match host but not port",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: "example.com", Port: 9090}}},
			destPort: 8080,
			want:     false,
		},
		{
			name:     "no match",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: "google.com"}}},
			destPort: 8080,
			want:     false,
		},
		{
			name:     "match with port 0",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: "example.com", Port: 0}}},
			destPort: 8080,
			want:     true,
		},
		{
			name:     "invalid host regex",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Host: `[`}}},
			destPort: 8080,
			want:     false,
		},
		{
			name:     "invalid path regex",
			opts:     models.OutgoingOptions{Rules: []config.BypassRule{{Path: `[`}}},
			destPort: 8080,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Since req.URL.String() includes the host, we need to parse it correctly for path matching
			parsedURL, _ := url.Parse(req.URL.String())
			req.URL = parsedURL
			req.Host = "example.com" // Host should not contain port for regex matching against host rule

			got := IsPassThrough(logger, req, tt.destPort, tt.opts)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAskForConfirmation_AllCases_122 tests the user confirmation prompt function under various conditions.
func TestAskForConfirmation_AllCases_122(t *testing.T) {
	originalStdin := os.Stdin
	defer func() { os.Stdin = originalStdin }()

	runTest := func(input string, expected bool, expectErr bool) {
		r, w, _ := os.Pipe()
		os.Stdin = r
		go func() {
			defer w.Close()
			w.Write([]byte(input + "\n"))
		}()

		// Capture stdout to prevent printing to console during test
		oldStdout := os.Stdout
		devNull, _ := os.Open(os.DevNull)
		os.Stdout = devNull
		defer func() {
			os.Stdout = oldStdout
			devNull.Close()
		}()

		got, err := AskForConfirmation(context.Background(), "Confirm?")
		if expectErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			assert.Equal(t, expected, got)
		}
	}

	t.Run("confirm with y", func(t *testing.T) { runTest("y", true, false) })
	t.Run("confirm with yes", func(t *testing.T) { runTest("yes", true, false) })
	t.Run("decline with n", func(t *testing.T) { runTest("n", false, false) })
	t.Run("decline with other", func(t *testing.T) { runTest("anything else", false, false) })

	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := AskForConfirmation(ctx, "Confirm?")
		require.NoError(t, err)
		assert.False(t, got)
	})
}