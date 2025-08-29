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

// TestReplaceGrpcHost_ValidAndInvalidInputs_003 tests ReplaceGrpcHost function with valid and invalid inputs.
func TestReplaceGrpcHost_ValidAndInvalidInputs_003(t *testing.T) {
	validAuthority := "example.com:8080"
	invalidAuthority := "example.com"
	ipAddress := "192.168.1.1"

	// Test valid authority
	result, err := ReplaceGrpcHost(validAuthority, ipAddress)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.1:8080", result)

	// Test invalid authority
	result, err = ReplaceGrpcHost(invalidAuthority, ipAddress)
	require.Error(t, err)
	assert.Equal(t, invalidAuthority, result)

	// Test empty IP address
	result, err = ReplaceGrpcHost(validAuthority, "")
	require.Error(t, err)
	assert.Equal(t, validAuthority, result)
}

// TestReplacePort_ValidAndInvalidInputs_004 tests ReplacePort function with valid and invalid inputs.
func TestReplacePort_ValidAndInvalidInputs_004(t *testing.T) {
	validURL := "http://example.com:8080"
	invalidURL := "://invalid-url"
	port := "9090"

	// Test valid URL with existing port
	result, err := ReplacePort(validURL, port)
	require.NoError(t, err)
	assert.Equal(t, "http://example.com:9090", result)

	// Test valid URL without existing port
	result, err = ReplacePort("http://example.com", port)
	require.NoError(t, err)
	assert.Equal(t, "http://example.com:9090", result)

	// Test invalid URL
	result, err = ReplacePort(invalidURL, port)
	require.Error(t, err)
	assert.Equal(t, invalidURL, result)

	// Test empty port
	result, err = ReplacePort(validURL, "")
	require.Error(t, err)
	assert.Equal(t, validURL, result)
}

// TestRemoveDoubleQuotes_AllCases_005 tests RemoveDoubleQuotes function with various input cases.
func TestRemoveDoubleQuotes_AllCases_005(t *testing.T) {
	input := map[string]interface{}{
		"key1": `"value1"`,
		"key2": `"value2"`,
		"key3": "value3",
	}
	expected := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	RemoveDoubleQuotes(input)
	assert.Equal(t, expected, input)
}

// TestRecover_PanicHandling_006 tests Recover function to ensure it handles panic recovery and logs appropriately.
func TestRecover_PanicHandling_006(t *testing.T) {
	logger := zap.NewNop()

	defer func() {
		if r := recover(); r != nil {
			// Ensure Recover function is called and handles panic
			Recover(logger)
		}
	}()

	// Trigger a panic
	panic("test panic")
}

// TestEnsureRmBeforeName_AllCases_007 tests EnsureRmBeforeName function to ensure "--rm" is added before "--name".
func TestEnsureRmBeforeName_AllCases_007(t *testing.T) {
	cmdWithoutRm := "docker run --name my-container"
	cmdWithRm := "docker run --rm --name my-container"

	result := EnsureRmBeforeName(cmdWithoutRm)
	assert.Equal(t, cmdWithRm, result)

	result = EnsureRmBeforeName(cmdWithRm)
	assert.Equal(t, cmdWithRm, result)
}

// TestIsXMLResponse_AllCases_512 tests the IsXMLResponse function for various scenarios.
func TestIsXMLResponse_AllCases_512(t *testing.T) {
	tests := []struct {
		name string
		resp *models.HTTPResp
		want bool
	}{
		{
			name: "nil response",
			resp: nil,
			want: false,
		},
		{
			name: "nil header",
			resp: &models.HTTPResp{},
			want: false,
		},
		{
			name: "content-type header missing",
			resp: &models.HTTPResp{Header: map[string]string{}},
			want: false,
		},
		{
			name: "content-type header empty",
			resp: &models.HTTPResp{Header: map[string]string{"Content-Type": ""}},
			want: false,
		},
		{
			name: "content-type is application/xml",
			resp: &models.HTTPResp{Header: map[string]string{"Content-Type": "application/xml"}},
			want: true,
		},
		{
			name: "content-type is text/xml",
			resp: &models.HTTPResp{Header: map[string]string{"Content-Type": "text/xml"}},
			want: true,
		},
		{
			name: "content-type is application/xml with charset",
			resp: &models.HTTPResp{Header: map[string]string{"Content-Type": "application/xml; charset=utf-8"}},
			want: true,
		},
		{
			name: "content-type is application/json",
			resp: &models.HTTPResp{Header: map[string]string{"Content-Type": "application/json"}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsXMLResponse(tt.resp)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseMetadata_AllCases_843 tests the ParseMetadata function.
func TestParseMetadata_AllCases_843(t *testing.T) {
	tests := []struct {
		name          string
		metadataStr   string
		want          map[string]interface{}
		wantErr       bool
		expectedError string
	}{
		{
			name:        "empty string",
			metadataStr: "",
			want:        nil,
			wantErr:     false,
		},
		{
			name:        "valid metadata",
			metadataStr: "key1=value1,key2=value2",
			want:        map[string]interface{}{"key1": "value1", "key2": "value2"},
			wantErr:     false,
		},
		{
			name:          "invalid metadata",
			metadataStr:   "key1=value1,key2",
			want:          nil,
			wantErr:       true,
			expectedError: "cannot parse metadata: key-value pair must be separated by an equals sign '='",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMetadata(tt.metadataStr)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "cannot parse metadata")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestGetVersionAsComment_AllCases_991 tests the GetVersionAsComment function.
func TestGetVersionAsComment_AllCases_991(t *testing.T) {
	Version = "v1.2.3"
	expected := "# Generated by Keploy (v1.2.3)\n"
	got := GetVersionAsComment()
	assert.Equal(t, expected, got)
}

// TestIsDockerCmd_AllCases_753 tests the IsDockerCmd function.
func TestIsDockerCmd_AllCases_753(t *testing.T) {
	assert.True(t, IsDockerCmd(DockerRun))
	assert.True(t, IsDockerCmd(DockerStart))
	assert.True(t, IsDockerCmd(DockerCompose))
	assert.False(t, IsDockerCmd(Native))
	assert.False(t, IsDockerCmd(Empty))
}

// TestKeys_AllCases_421 tests the Keys function.
func TestKeys_AllCases_421(t *testing.T) {
	m := map[string][]string{
		"key1": {"val1"},
		"key2": {"val2"},
		"key3": {"val3"},
	}
	keys := Keys(m)
	assert.ElementsMatch(t, []string{"key1", "key2", "key3"}, keys)
}

// TestHash_AllCases_888 tests the Hash function.
func TestHash_AllCases_888(t *testing.T) {
	data := []byte("hello world")
	expectedHash := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	got := Hash(data)
	assert.Equal(t, expectedHash, got)
}

// TestFindDockerCmd_AllCases_555 tests the FindDockerCmd function.
func TestFindDockerCmd_AllCases_555(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want CmdType
	}{
		{"docker run", "docker run -p 8080:8080 my-app", DockerRun},
		{"sudo docker run", "sudo docker run my-app", DockerRun},
		{"docker start", "docker start my-container", DockerStart},
		{"sudo docker-compose", "sudo docker-compose up", DockerCompose},
		{"docker compose", "docker compose up -d", DockerCompose},
		{"native command", "./my-app", Native},
		{"empty command", "", Empty},
		{"command with spaces", "  docker run my-app  ", DockerRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindDockerCmd(tt.cmd)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestExtractIDFromStatusLine_AllCases_902 tests the extractIDFromStatusLine function.
func TestExtractIDFromStatusLine_AllCases_902(t *testing.T) {
	tests := []struct {
		name string
		line string
		want int
	}{
		{"valid line", "NSpgid:\t12345", 12345},
		{"not enough fields", "NSpgid:", -1},
		{"value not an int", "NSpgid:\tabc", -1},
		{"empty string", "", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIDFromStatusLine(tt.line)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestSentryInit_AllCases_789 tests the SentryInit function.
func TestSentryInit_AllCases_789(t *testing.T) {
	logger := zaptest.NewLogger(t)
	// Test with an invalid DSN, should not panic and log a debug message
	SentryInit(logger, "invalid-dsn")

	// Test with a valid but test-only DSN
	// This will still fail to send events but initializes without error
	SentryInit(logger, "http://public@example.com/1")
}

// TestRecover_NilLogger_556 tests the Recover function's behavior with a nil logger.
func TestRecover_NilLogger_556(t *testing.T) {
	// This test just ensures that calling Recover with a nil logger doesn't cause a panic.
	// The function should print to stdout and return.
	assert.NotPanics(t, func() {
		Recover(nil)
	})

	// Test the panic recovery path with a nil logger
	defer func() {
		// This outer recover is to prevent the test from crashing
		if r := recover(); r != nil {
			// Call the function under test
			Recover(nil)
		}
	}()
	panic("test panic with nil logger")
}
