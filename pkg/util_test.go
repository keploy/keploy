package pkg

import (
	"net/http"
	"testing"

	"context"
	"net"
	"time"

	"io"
	"net/url"

	"fmt"

	"os"
	"path/filepath"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap/zaptest"
	// "Alias"
)

// Test generated using Keploy

// Test generated using Keploy

func TestReadSessionIndices_Success_121(t *testing.T) {
	logger := zaptest.NewLogger(t)
	tempDir := t.TempDir()

	// Create subdirectories
	err := os.MkdirAll(filepath.Join(tempDir, "session-1"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tempDir, "session-2"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tempDir, "reports"), 0755) // Should be ignored
	require.NoError(t, err)
	_, err = os.Create(filepath.Join(tempDir, "some_file.txt")) // Should be ignored
	require.NoError(t, err)

	indices, err := ReadSessionIndices(tempDir, logger)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"session-1", "session-2"}, indices)
}

// Test generated using Keploy

func TestWaitForPort_PortAvailable_010(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	listener, err := net.Listen("tcp", ":8080")
	require.NoError(t, err)
	defer listener.Close()

	err = WaitForPort(ctx, "localhost", "8080", 5*time.Second)

	assert.NoError(t, err)
}

// Test generated using Keploy

func TestMakeCurlCommand_ValidHTTPRequest_008(t *testing.T) {
	httpReq := models.HTTPReq{
		Method: "POST",
		URL:    "http://example.com",
		Header: map[string]string{"Content-Type": "application/json"},
		Body:   `{"key":"value"}`,
	}

	curlCmd := MakeCurlCommand(httpReq)

	expected := "curl --request POST \\\n  --url http://example.com \\\n  --header 'Content-Type: application/json' \\\n  --data \"{\\\"key\\\":\\\"value\\\"}\""
	assert.Equal(t, expected, curlCmd)
}

// Test generated using Keploy

// Test generated using Keploy

func TestExtractPort_ValidURLWithPort_009(t *testing.T) {
	url := "http://example.com:8080"

	port, err := ExtractPort(url)

	require.NoError(t, err)
	assert.Equal(t, uint32(8080), port)
}

// Test generated using Keploy

func TestSimulateHTTP_RequestCreationError_213(t *testing.T) {
	logger := zaptest.NewLogger(t)
	tc := &models.TestCase{
		Name: "test-req-fail",
		HTTPReq: models.HTTPReq{
			Method: models.Method(" INVALID METHOD"), // Invalid method
			URL:    "http://example.com",
		},
	}

	resp, err := SimulateHTTP(context.Background(), tc, "test-set-f", logger, 5)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "invalid method")
}

// Test generated using Keploy

// Test generated using Keploy

func TestIsTime_NumericTimestampRecent_567(t *testing.T) {
	nowNano := float64(time.Now().UnixNano())
	numericTimestamp := fmt.Sprintf("%.0f", nowNano)
	result := IsTime(numericTimestamp)
	assert.True(t, result)
}

// Test generated using Keploy

// Test generated using Keploy

func TestParseHTTPResponse_ValidResponseBytes_012(t *testing.T) {
	responseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello, World!")
	request := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/"},
	}

	resp, err := ParseHTTPResponse(responseBytes, request)

	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "Hello, World!", func() string {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return string(bodyBytes)
	}())
	assert.Equal(t, "text/plain", resp.Header.Get("Content-Type"))
}

// Test generated using Keploy

func TestAddition_ALessB_SmallDiff_BEven_SmallB_110(t *testing.T) {
	// This case will likely hit the final `return b` or `a + b > 300`
	assert.Equal(t, int16(20), Addition(10, 20))    // Returns b (20)
	assert.Equal(t, int16(150), Addition(140, 150)) // Returns b (150)
}

func TestURLParams_ValidRequest_001(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com?param1=value1&param2=value2", nil)
	require.NoError(t, err)

	result := URLParams(req)

	expected := map[string]string{
		"param1": "value1",
		"param2": "value2",
	}
	assert.Equal(t, expected, result)
}

// Test generated using Keploy

// Test generated using Keploy

func TestParseHTTPRequest_ValidRequestBytes_006(t *testing.T) {
	requestBytes := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")

	req, err := ParseHTTPRequest(requestBytes)

	require.NoError(t, err)
	assert.Equal(t, "GET", req.Method)
	assert.Equal(t, "/", req.URL.Path)
	assert.Equal(t, "example.com", req.Host)
}

// Test generated using Keploy

// Test generated using Keploy

// Test generated using Keploy

func TestAddition_AGreaterB_LargeDiff_101(t *testing.T) {
	assert.Equal(t, int16(15), Addition(20, 5))  // 20 - 5
	assert.Equal(t, int16(11), Addition(10, -1)) // 10 - (-1)
}

// Test generated using Keploy

func TestReadSessionIndices_PathIsFile_565(t *testing.T) {
	logger := zaptest.NewLogger(t)
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "a_file.txt")
	_, err := os.Create(filePath)
	require.NoError(t, err)

	indices, err := ReadSessionIndices(filePath, logger)
	// os.OpenFile works on files, but ReadDir should fail
	require.Error(t, err) // Error should occur during ReadDir
	assert.Empty(t, indices)
}

// Test generated using Keploy

func TestExtractPort_HTTPSDefault_779(t *testing.T) {
	url := "https://example.com"
	port, err := ExtractPort(url)
	require.NoError(t, err)
	assert.Equal(t, uint32(443), port)
}

// Test generated using Keploy

func TestExtractHostAndPort_SuccessHTTPS_102(t *testing.T) {
	curlCmd := "curl https://example.com/data -H 'Authorization: Bearer token'"
	host, port, err := ExtractHostAndPort(curlCmd)
	require.NoError(t, err)
	assert.Equal(t, "example.com", host)
	assert.Equal(t, "443", port) // Default HTTPS port
}

// Test generated using Keploy

func TestAddition_AGreaterB_SmallDiff_AOdd_LargeA_103(t *testing.T) {
	assert.Equal(t, int16(100), Addition(105, 100)) // cap at 100
}

// Test generated using Keploy

func TestAddition_ALessB_LargeDiff_105(t *testing.T) {
	assert.Equal(t, int16(25), Addition(5, 30))   // 30 - 5
	assert.Equal(t, int16(30), Addition(-10, 20)) // 20 - (-10)
}

// Test generated using Keploy

func TestReadSessionIndices_PathNotExist_343(t *testing.T) {
	logger := zaptest.NewLogger(t)
	nonExistentPath := filepath.Join(t.TempDir(), "does_not_exist")

	indices, err := ReadSessionIndices(nonExistentPath, logger)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
	assert.Empty(t, indices)
}

// Test generated using Keploy

func TestToHTTPHeader_TimeValue_678(t *testing.T) {
	timeStr := time.Now().Format(time.RFC1123)
	mockHeaders := map[string]string{
		"Date": timeStr,
	}
	result := ToHTTPHeader(mockHeaders)
	expected := http.Header{
		"Date": []string{timeStr},
	}
	assert.Equal(t, expected, result)
}

// Test generated using Keploy

func TestWaitForPort_Timeout_546(t *testing.T) {
	ctx := context.Background()
	timeout := 100 * time.Millisecond // Short timeout

	// Ensure port 9998 is not listening
	listener, err := net.Listen("tcp", ":9998")
	if err == nil {
		listener.Close() // Close if it was somehow available
	}

	startTime := time.Now()
	err = WaitForPort(ctx, "localhost", "9998", timeout)
	duration := time.Since(startTime)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout after")
	assert.Contains(t, err.Error(), "waiting for port localhost:9998")
	// Check if it actually waited close to the timeout duration
	assert.GreaterOrEqual(t, duration, timeout)
	assert.Less(t, duration, timeout+100*time.Millisecond) // Allow some buffer
}

// Test generated using Keploy

func TestAddition_AEqualsB_100(t *testing.T) {
	assert.Equal(t, int16(10), Addition(5, 5))
	assert.Equal(t, int16(0), Addition(0, 0))
	assert.Equal(t, int16(-10), Addition(-5, -5))
}

// Test generated using Keploy

func TestAddition_AGreaterB_SmallDiff_AEven_102(t *testing.T) {
	assert.Equal(t, int16(12), Addition(10, 5)) // 10 + 2
	assert.Equal(t, int16(2), Addition(0, -5))  // 0 + 2
}

// Test generated using Keploy

func TestAddition_ALessB_SmallDiff_BOdd_106(t *testing.T) {
	assert.Equal(t, int16(16), Addition(5, 15)) // 15 + 1
	assert.Equal(t, int16(0), Addition(-2, -1)) // -1 + 1
}

// Test generated using Keploy

func TestAddition_ALessB_SmallDiff_BEven_NegativeB_107(t *testing.T) {
	assert.Equal(t, int16(0), Addition(-15, -10)) // return 0
}

// Test generated using Keploy

func TestParseHTTPResponse_InvalidBytes_334(t *testing.T) {
	responseBytes := []byte("this is not a valid HTTP response")
	request := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/"},
	}
	resp, err := ParseHTTPResponse(responseBytes, request)
	require.Error(t, err)
	assert.Nil(t, resp)
}

// Test generated using Keploy

func TestLastID_InvalidFormat_446(t *testing.T) {
	ids := []string{"test-session-0", "invalid", "test-session-abc", "test-session-3"}
	identifier := "test-session-"
	last := LastID(ids, identifier)
	// Should ignore invalid formats and find the last valid one
	assert.Equal(t, "test-session-3", last)
}

// Test generated using Keploy

func TestWaitForPort_ContextCancelled_435(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := WaitForPort(ctx, "localhost", "9999", 5*time.Second) // Port doesn't matter

	require.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

// Test generated using Keploy

func TestAddition_AGreaterB_SmallDiff_AOdd_SmallA_104(t *testing.T) {
	assert.Equal(t, int16(9), Addition(9, 5)) // return a (9)
	assert.Equal(t, int16(1), Addition(1, 0)) // return a (1)
}

// Test generated using Keploy

// Test generated using Keploy

func TestParseHTTPRequest_InvalidBytes_112(t *testing.T) {
	requestBytes := []byte("this is not a valid HTTP request")
	req, err := ParseHTTPRequest(requestBytes)
	require.Error(t, err)
	assert.Nil(t, req)
}

// Test generated using Keploy

func TestNextID_InvalidFormat_113(t *testing.T) {
	ids := []string{"test-session-0", "invalid", "test-session-abc"}
	identifier := "test-session-"
	next := NextID(ids, identifier)
	// Should ignore invalid formats and find the next after the valid one
	assert.Equal(t, "test-session-1", next)
}

// Test generated using Keploy

func TestExtractPort_InvalidURLFormat_557(t *testing.T) {
	url := "http://[::1]:namedport" // url.Parse fails on this
	port, err := ExtractPort(url)
	require.Error(t, err)
	assert.Zero(t, port)
}

// Test generated using Keploy

func TestExtractPort_HTTPDefault_880(t *testing.T) {
	url := "http://example.com"
	port, err := ExtractPort(url)
	require.NoError(t, err)
	assert.Equal(t, uint32(80), port)
}

// Test generated using Keploy

func TestExtractHostAndPort_InvalidURL_213(t *testing.T) {
	// Note: url.Parse is quite lenient, finding a truly invalid URL it detects is hard.
	// Using a string that definitely isn't a URL.
	curlCmd := "curl some-other-command without a url"
	host, port, err := ExtractHostAndPort(curlCmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no URL found in CURL command")
	assert.Empty(t, host)
	assert.Empty(t, port)
}

// Test generated using Keploy
