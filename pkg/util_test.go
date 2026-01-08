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

// TestLooksLikeTimestamp_888 tests timestamp detection using dateparse library
func TestLooksLikeTimestamp_888(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		// Standard formats that should be detected
		{"RFC3339", "2023-12-18T10:30:00Z", true},
		{"RFC3339Nano", "2023-12-18T10:30:00.123456789Z", true},
		{"RFC1123", "Mon, 18 Dec 2023 10:30:00 GMT", true},
		{"RFC822", "18 Dec 23 10:30 GMT", true},
		{"ANSIC", "Mon Dec 18 10:30:00 2023", true},
		{"UnixDate", "Mon Dec 18 10:30:00 UTC 2023", true},
		{"ISO8601", "2023-12-18", true},
		{"ISO8601Time", "2023-12-18T10:30:00", true},
		{"USDate", "12/18/2023", true},
		{"USDateTime", "12/18/2023 10:30:00", true},
		{"EuropeanDate", "18/12/2023", false}, // day/month/year not universally supported by dateparse
		{"TimeOnly", "10:30:00", true},
		{"MonthDayYear", "December 18, 2023", true},
		// Strings that should NOT be timestamps
		{"EmptyString", "", false},
		{"RandomString", "hello world", false},
		{"UUID", "550e8400-e29b-41d4-a716-446655440000", false},
		{"Number", "12345", false},
		{"Email", "test@example.com", false},
		{"URL", "https://example.com", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := LooksLikeTimestamp(tc.input)
			assert.Equal(t, tc.expected, result, "LooksLikeTimestamp(%q) = %v, want %v", tc.input, result, tc.expected)
		})
	}
}

// TestLooksLikeRandomID_889 tests random ID detection (UUID, KSUID, ULID, ObjectID, Snowflake, NanoID)
func TestLooksLikeRandomID_889(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		// UUIDs (library-based detection)
		{"UUIDv4", "550e8400-e29b-41d4-a716-446655440000", true},
		{"UUIDv4Uppercase", "550E8400-E29B-41D4-A716-446655440000", true},
		{"UUIDv1", "f47ac10b-58cc-1111-bc00-d4d5c2cd9b3a", true},
		// KSUID (27 characters, library-based)
		{"KSUID", "0ujsswThIGTUYm2K8FjOOfXtY1K", true},
		{"KSUID2", "1srOrx2ZWZBpBUvZwXKQmoEYga2", true},
		// ULID (26 characters, library-based)
		{"ULID", "01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"ULID2", "01H9YP3SXNRWQY6DSMZR5XATJ6", true},
		// MongoDB ObjectID (24 hex characters)
		{"ObjectID", "507f1f77bcf86cd799439011", true},
		{"ObjectIDUppercase", "507F1F77BCF86CD799439011", true},
		// Snowflake ID (18-19 digits)
		{"SnowflakeID", "175928847299117063", true},
		{"SnowflakeID19", "1175928847299117063", true},
		// NanoID (21-22 characters alphanumeric with _ and -)
		{"NanoID21", "V1StGXR8_Z5jdHi6B-myT", true},
		{"NanoID22", "V1StGXR8_Z5jdHi6B-myT1", true},
		// Prefixed hex strings (like enc_xxx, id_xxx, token_xxx)
		{"PrefixedHex_enc", "enc_ad1aeab2973130fbe617c10705c6fdb91c0be2ae", true},
		{"PrefixedHex_id", "id_b758cec93c419abd3991ddd7655e5a891f59ecb1", true},
		{"PrefixedHex_token", "token_e347db44c0cf388911ca259ac190cb21c79012b3", true},
		{"PrefixedHex_short", "enc_abc123def456789a", true},
		// Pure long hex strings (SHA256, API keys, etc.)
		{"SHA256Hash", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"LongHex32", "507f1f77bcf86cd799439011abcdef12", true},
		// High entropy base64-like tokens
		{"Base64Token", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", true},
		{"SessionToken", "a352d1f893bda510a0b39a64e129b828", true},
		// Strings that should NOT be random IDs
		{"EmptyString", "", false},
		{"ShortString", "abc", false},
		{"RegularWord", "hello-world", false},
		{"Timestamp", "2023-12-18T10:30:00Z", false},
		{"Email", "test@example.com", false},
		{"PhoneNumber", "+1234567890", false},
		{"IPv4", "192.168.1.1", false},
		{"MalformedUUID", "550e8400-e29b-41d4-a716", true}, // Partial UUID/hex-like string is still considered noisy
		{"RegularSentence", "The quick brown fox jumps over the lazy dog", false},
		{"SimpleWord", "hello", false},
		{"ShortHex", "abc123", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := LooksLikeRandomID(tc.input)
			assert.Equal(t, tc.expected, result, "LooksLikeRandomID(%q) = %v, want %v", tc.input, result, tc.expected)
		})
	}
}

// TestIsNoiseValue_890 tests the combined noise value detection (timestamp OR random ID)
func TestIsNoiseValue_890(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		// Timestamps
		{"Timestamp", "2023-12-18T10:30:00Z", true},
		// Random IDs
		{"UUID", "550e8400-e29b-41d4-a716-446655440000", true},
		{"KSUID", "0ujsswThIGTUYm2K8FjOOfXtY1K", true},
		// Neither
		{"RegularString", "hello world", false},
		{"EmptyString", "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isNoiseValue(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// TestFindJSONPathsWithPatterns_891 tests finding JSON paths with noise patterns
func TestFindJSONPathsWithPatterns_891(t *testing.T) {
	t.Run("NestedObjectWithTimestamp", func(t *testing.T) {
		jsonData := map[string]interface{}{
			"id":   "550e8400-e29b-41d4-a716-446655440000",
			"name": "test",
			"meta": map[string]interface{}{
				"createdAt": "2023-12-18T10:30:00Z",
				"updatedAt": "2023-12-18T11:30:00Z",
			},
		}
		paths := findJSONPathsWithPatterns(jsonData, "")
		assert.Contains(t, paths, "id")
		assert.Contains(t, paths, "meta.createdAt")
		assert.Contains(t, paths, "meta.updatedAt")
		assert.NotContains(t, paths, "name")
	})

	t.Run("ArrayWithRandomIDs", func(t *testing.T) {
		jsonData := []interface{}{
			"550e8400-e29b-41d4-a716-446655440000",
			"regular-string",
			"0ujsswThIGTUYm2K8FjOOfXtY1K",
		}
		paths := findJSONPathsWithPatterns(jsonData, "items")
		assert.Contains(t, paths, "items.0")
		assert.Contains(t, paths, "items.2")
		assert.NotContains(t, paths, "items.1")
	})

	t.Run("EmptyData", func(t *testing.T) {
		paths := findJSONPathsWithPatterns(map[string]interface{}{}, "")
		assert.Empty(t, paths)
	})

	t.Run("NoNoiseValues", func(t *testing.T) {
		jsonData := map[string]interface{}{
			"name":    "John",
			"age":     30,
			"active":  true,
			"balance": 100.50,
		}
		paths := findJSONPathsWithPatterns(jsonData, "")
		assert.Empty(t, paths)
	})
}

// TestDetectNoiseFieldsInResp_892 tests the main noise detection function for HTTP responses
func TestDetectNoiseFieldsInResp_892(t *testing.T) {
	t.Run("NilResponse", func(t *testing.T) {
		result := DetectNoiseFieldsInResp(nil)
		assert.NotNil(t, result)
		assert.Empty(t, result)
	})

	t.Run("HeaderWithTimestamp", func(t *testing.T) {
		resp := &models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"Date":         "Mon, 18 Dec 2023 10:30:00 GMT",
				"Content-Type": "application/json",
			},
			Body: "{}",
		}
		result := DetectNoiseFieldsInResp(resp)
		assert.Contains(t, result, "header.date")
		assert.NotContains(t, result, "header.content-type")
	})

	t.Run("JSONBodyWithRandomIDs", func(t *testing.T) {
		resp := &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			Body:       `{"id": "550e8400-e29b-41d4-a716-446655440000", "name": "test", "requestId": "0ujsswThIGTUYm2K8FjOOfXtY1K"}`,
		}
		result := DetectNoiseFieldsInResp(resp)
		assert.Contains(t, result, "body.id")
		assert.Contains(t, result, "body.requestId")
		assert.NotContains(t, result, "body.name")
	})

	t.Run("JSONBodyWithNestedTimestamps", func(t *testing.T) {
		resp := &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			Body:       `{"data": {"createdAt": "2023-12-18T10:30:00Z", "title": "Hello"}}`,
		}
		result := DetectNoiseFieldsInResp(resp)
		assert.Contains(t, result, "body.data.createdAt")
		assert.NotContains(t, result, "body.data.title")
	})

	t.Run("NonJSONBodyWithNoiseValue", func(t *testing.T) {
		// isNoiseValue checks the entire body as one string - it detects if body IS a noise value
		// not if body CONTAINS a noise value. Use a pure UUID as body.
		resp := &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			Body:       "550e8400-e29b-41d4-a716-446655440000",
		}
		result := DetectNoiseFieldsInResp(resp)
		// Non-JSON body that is itself a noise pattern marks the whole body as noisy
		assert.Contains(t, result, "body")
	})

	t.Run("EmptyBody", func(t *testing.T) {
		resp := &models.HTTPResp{
			StatusCode: 204,
			Header:     map[string]string{},
			Body:       "",
		}
		result := DetectNoiseFieldsInResp(resp)
		assert.Empty(t, result)
	})

	t.Run("ArrayInJSONBody", func(t *testing.T) {
		resp := &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			Body:       `{"items": ["550e8400-e29b-41d4-a716-446655440000", "regular-string", "2023-12-18T10:30:00Z"]}`,
		}
		result := DetectNoiseFieldsInResp(resp)
		assert.Contains(t, result, "body.items.0")
		assert.Contains(t, result, "body.items.2")
		assert.NotContains(t, result, "body.items.1")
	})
}
