package pkg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestSimulateHTTP_NewRequestError_303 ensures that SimulateHTTP returns an error
// when http.NewRequestWithContext fails. This is triggered by providing an invalid
// HTTP method string in the test case.
func TestSimulateHTTP_NewRequestError_303(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-case-new-request-error",
		HTTPReq: models.HTTPReq{
			Method: "INVALID METHOD", // Invalid method
			URL:    "http://example.com/test",
			Body:   `{"key":"value"}`,
		},
	}

	// Act
	resp, err := SimulateHTTP(ctx, tc, "test-set", logger, 10)

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid method")
	assert.Nil(t, resp)
}

// TestIsTime_VariousFormats_808 covers multiple scenarios for the IsTime function,
// including standard date formats (RFC3339, UnixDate), numeric timestamps as strings,
// and invalid inputs to ensure it correctly identifies time-like strings.
func TestIsTime_VariousFormats_808(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"RFC3339", "2023-01-17T16:34:58Z", true},
		{"UnixDate", "Tue Jan 17 16:34:58 UTC 2023", true},
		{"NumericTimestamp", fmt.Sprintf("%f", float64(time.Now().UnixNano())), true},
		{"AlmostNow", fmt.Sprintf("%f", float64(time.Now().Add(-time.Hour).UnixNano())), true},
		{"TooOldNumeric", fmt.Sprintf("%f", float64(time.Now().Add(-48*time.Hour).UnixNano())), false},
		{"InvalidString", "not a date", false},
		{"EmptyString", "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, IsTime(tc.input))
		})
	}
}

// TestToHTTPHeader_WithTimeValue_909 verifies that the ToHTTPHeader function correctly
// converts a map of strings to an http.Header object. It specifically checks that
// header values recognized as timestamps are not split by commas, while other
// comma-separated values are correctly split into slices.
func TestToHTTPHeader_WithTimeValue_909(t *testing.T) {
	// Arrange
	mockHeader := map[string]string{
		"Date":            "Tue, 17 Jan 2023 16:34:58 IST",
		"X-Custom-Header": "value1,value2",
		"Content-Type":    "application/json",
	}

	// Act
	httpHeader := ToHTTPHeader(mockHeader)

	// Assert
	require.NotNil(t, httpHeader)
	assert.Equal(t, []string{"Tue, 17 Jan 2023 16:34:58 IST"}, httpHeader["Date"])
	assert.Equal(t, []string{"value1", "value2"}, httpHeader["X-Custom-Header"])
	assert.Equal(t, []string{"application/json"}, httpHeader["Content-Type"])
}

// TestParseHTTPRequest_And_Response_111 contains sub-tests for ParseHTTPRequest and
// ParseHTTPResponse, validating both success and failure cases for parsing raw
// HTTP data into their respective struct representations.
func TestParseHTTPRequest_And_Response_111(t *testing.T) {
	t.Run("ParseHTTPRequest_Valid", func(t *testing.T) {
		rawReq := "GET /test HTTP/1.1\r\nHost: example.com\r\n\r\n"
		req, err := ParseHTTPRequest([]byte(rawReq))
		require.NoError(t, err)
		assert.Equal(t, "GET", req.Method)
		assert.Equal(t, "/test", req.URL.Path)
		assert.Equal(t, "example.com", req.Host)
	})

	t.Run("ParseHTTPRequest_Invalid", func(t *testing.T) {
		rawReq := "this is not a valid request"
		_, err := ParseHTTPRequest([]byte(rawReq))
		require.Error(t, err)
	})

	t.Run("ParseHTTPResponse_Valid", func(t *testing.T) {
		rawResp := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello, world!"
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		resp, err := ParseHTTPResponse([]byte(rawResp), req)
		require.NoError(t, err)
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("failed to close response body: %v", err)
			}
		}()
		assert.Equal(t, 200, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "Hello, world!", string(body))
	})

	t.Run("ParseHTTPResponse_Invalid", func(t *testing.T) {
		rawResp := "this is not a valid response"
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		_, err := ParseHTTPResponse([]byte(rawResp), req)
		require.Error(t, err)
	})
}

// TestCompressDecompress_AllEncodings_555 provides comprehensive testing for the
// Compress and Decompress functions. It checks:
// - Successful round-trip (compress then decompress) for 'gzip' and 'br' (brotli).
// - No-op behavior for unknown encodings.
// - Error handling for invalid compressed data.
func TestCompressDecompress_AllEncodings_555(t *testing.T) {
	logger := zap.NewNop()

	t.Run("Gzip", func(t *testing.T) {
		originalData := []byte("hello world, this is a test")
		compressedData, err := Compress(logger, "gzip", originalData)
		require.NoError(t, err)
		assert.NotEqual(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "gzip", compressedData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("Brotli", func(t *testing.T) {
		originalData := []byte("hello world, this is a brotli test")
		compressedData, err := Compress(logger, "br", originalData)
		require.NoError(t, err)
		assert.NotEqual(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "br", compressedData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("UnknownEncoding", func(t *testing.T) {
		originalData := []byte("hello world")
		compressedData, err := Compress(logger, "unknown", originalData)
		require.NoError(t, err)
		assert.Equal(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "unknown", originalData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("DecompressError", func(t *testing.T) {
		invalidGzipData := []byte("not gzip")
		_, err := Decompress(logger, "gzip", invalidGzipData)
		require.Error(t, err)

		invalidBrotliData := []byte{0xce, 0xb2, 0xcf, 0x81}
		_, err = Decompress(logger, "br", invalidBrotliData)
		require.Error(t, err)
	})
}

// TestFilterMocks_678 validates the filtering and sorting of mocks in Test and Config modes.
func TestFilterMocks_678(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	now := time.Now()
	mock1 := &models.Mock{Name: "mock1", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now.Add(-2 * time.Hour), ResTimestampMock: now.Add(-2 * time.Hour)}}
	mock2 := &models.Mock{Name: "mock2", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now}}
	mock3 := &models.Mock{Name: "mock3", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now.Add(2 * time.Hour), ResTimestampMock: now.Add(2 * time.Hour)}}
	mockNoTime := &models.Mock{Name: "mockNoTime", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{}}
	mockNonKeploy := &models.Mock{Name: "mockNonKeploy", Version: "v1", Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now}}

	allMocks := []*models.Mock{mock3, mock1, mock2, mockNoTime, mockNonKeploy}

	t.Run("filterByTimeStamp_NoFilters", func(t *testing.T) {
		filtered, unfiltered := filterByTimeStamp(ctx, logger, allMocks, time.Time{}, time.Time{})
		assert.Len(t, filtered, 5)
		assert.Len(t, unfiltered, 0)
	})

	t.Run("filterByTimeStamp_ValidRange", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		filtered, unfiltered := filterByTimeStamp(ctx, logger, allMocks, after, before)

		// Check filtered mocks
		require.Len(t, filtered, 3)
		filteredNames := []string{}
		for _, m := range filtered {
			filteredNames = append(filteredNames, m.Name)
			assert.True(t, m.TestModeInfo.IsFiltered)
		}
		assert.ElementsMatch(t, []string{"mock2", "mockNoTime", "mockNonKeploy"}, filteredNames)

		// Check unfiltered mocks
		require.Len(t, unfiltered, 2)
		unfilteredNames := []string{}
		for _, m := range unfiltered {
			unfilteredNames = append(unfilteredNames, m.Name)
			assert.False(t, m.TestModeInfo.IsFiltered)
		}
		assert.ElementsMatch(t, []string{"mock1", "mock3"}, unfilteredNames)
	})

	t.Run("FilterTcsMocks", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		result := FilterTcsMocks(ctx, logger, allMocks, after, before)
		require.Len(t, result, 3)
		assert.Equal(t, "mockNoTime", result[0].Name) // Zero time comes first
		assert.Equal(t, "mock2", result[1].Name)
		assert.Equal(t, "mockNonKeploy", result[2].Name)
	})

	t.Run("FilterConfigMocks", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		result := FilterConfigMocks(ctx, logger, allMocks, after, before)
		require.Len(t, result, 5)
		// Sorted filtered part
		assert.Equal(t, "mockNoTime", result[0].Name)
		assert.Equal(t, "mock2", result[1].Name)
		assert.Equal(t, "mockNonKeploy", result[2].Name)
		// Sorted unfiltered part
		assert.Equal(t, "mock1", result[3].Name)
		assert.Equal(t, "mock3", result[4].Name)
	})
}
