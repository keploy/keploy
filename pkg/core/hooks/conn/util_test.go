package conn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// TestConvertUnixNanoToTime_Simple_123 tests the basic conversion of a unix timestamp without nanoseconds.
func TestConvertUnixNanoToTime_Simple_123(t *testing.T) {
	unixNano := uint64(1672531200000000000) // 2023-01-01 00:00:00 UTC
	expected := time.Unix(1672531200, 0)
	result := convertUnixNanoToTime(unixNano)
	assert.Equal(t, expected, result)
}

// TestExtractFormData_Valid_201 checks if the function correctly parses a valid multipart form data body.
func TestExtractFormData_Valid_201(t *testing.T) {
	logger := zap.NewNop()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("key1", "value1")
	_ = writer.WriteField("key2", "value2")
	writer.Close()

	contentType := writer.FormDataContentType()
	formData := extractFormData(logger, body.Bytes(), contentType)

	require.Len(t, formData, 2)
	assert.Equal(t, "key1", formData[0].Key)
	assert.Equal(t, []string{"value1"}, formData[0].Values)
	assert.Equal(t, "key2", formData[1].Key)
	assert.Equal(t, []string{"value2"}, formData[1].Values)
}

// TestExtractFormData_InvalidContentType_212 ensures the function returns nil for a content type missing the boundary.
func TestExtractFormData_InvalidContentType_212(t *testing.T) {
	logger := zap.NewNop()
	body := []byte("some data")
	contentType := "multipart/form-data" // Missing boundary
	formData := extractFormData(logger, body, contentType)
	assert.Nil(t, formData)
}

// TestExtractFormData_PartWithNoName_234 ensures that parts without a form name are skipped.
func TestExtractFormData_PartWithNoName_234(t *testing.T) {
	logger := zap.NewNop()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	// Create a part without a name
	part, _ := writer.CreatePart(textproto.MIMEHeader{})
	_, _ = part.Write([]byte("some data"))
	_ = writer.WriteField("key1", "value1")
	writer.Close()

	contentType := writer.FormDataContentType()
	formData := extractFormData(logger, body.Bytes(), contentType)

	// Should skip the part with no name and only return the valid one
	require.Len(t, formData, 1)
	assert.Equal(t, "key1", formData[0].Key)
}

// TestExtractFormData_ErrorReadingPartValue_245 checks how the function handles an error while reading a part's value.
func TestExtractFormData_ErrorReadingPartValue_245(t *testing.T) {
	logger := zap.NewNop()

	originalIOReadAll := ioReadAll381
	defer func() { ioReadAll381 = originalIOReadAll }()
	ioReadAll381 = func(r io.Reader) ([]byte, error) {
		if _, ok := r.(*multipart.Part); ok {
			return nil, errors.New("mock read error")
		}
		return originalIOReadAll(r)
	}

	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("key1", "value1")
	writer.Close()

	contentType := writer.FormDataContentType()
	formData := extractFormData(logger, body.Bytes(), contentType)

	assert.Empty(t, formData)
}

// TestCapture_RequestBodyReadError_301 verifies that a test case is still captured (with an empty body) when reading the request body fails.
type mockReadCloser struct {
	io.Reader
	closeErr error
}

func (m *mockReadCloser) Close() error {
	return m.closeErr
}

func TestCapture_RequestBodyReadError_301(t *testing.T) {
	logger := zap.NewNop()
	originalIOReadAll := ioReadAll381
	defer func() { ioReadAll381 = originalIOReadAll }()

	var reqReadCalled bool
	ioReadAll381 = func(r io.Reader) ([]byte, error) {
		if !reqReadCalled {
			reqReadCalled = true
			return nil, errors.New("read error")
		}
		return originalIOReadAll(r)
	}

	req, _ := http.NewRequest("POST", "http://example.com", strings.NewReader("body"))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("response"))),
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		assert.Equal(t, "", tc.HTTPReq.Body)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for test case")
	}
}

// TestCapture_ResponseBodyReadError_312 ensures no test case is captured if reading the response body fails.
func TestCapture_ResponseBodyReadError_312(t *testing.T) {
	logger := zap.NewNop()
	originalIOReadAll := ioReadAll381
	defer func() { ioReadAll381 = originalIOReadAll }()
	ioReadAll381 = func(r io.Reader) ([]byte, error) {
		if _, ok := r.(*bytes.Reader); !ok {
			return nil, errors.New("read error")
		}
		return originalIOReadAll(r)
	}

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &mockReadCloser{Reader: &bytes.Buffer{}, closeErr: nil},
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		t.Fatalf("Expected no test case, but got one: %v", tc)
	default:
		// Success
	}
}

// TestCapture_UrlEncodedRequest_334 verifies that URL-encoded request bodies are correctly decoded.
func TestCapture_UrlEncodedRequest_334(t *testing.T) {
	logger := zap.NewNop()
	form := url.Values{}
	form.Add("param1", "value 1")
	form.Add("param2", "value&2")
	encodedBody := form.Encode()

	req, _ := http.NewRequest("POST", "http://example.com", strings.NewReader(encodedBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("response"))),
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		decodedBody, _ := url.QueryUnescape(encodedBody)
		assert.Equal(t, decodedBody, tc.HTTPReq.Body)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for test case")
	}
}

// TestCapture_UrlEncodedRequestDecodeError_345 ensures no test case is captured if URL decoding fails.
func TestCapture_UrlEncodedRequestDecodeError_345(t *testing.T) {
	logger := zap.NewNop()
	malformedBody := "a=%"

	req, _ := http.NewRequest("POST", "http://example.com", strings.NewReader(malformedBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("response"))),
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		t.Fatalf("Expected no test case, but got one: %v", tc)
	default:
		// Success
	}
}

// TestCapture_MultipartFormData_367 verifies that multipart form data is correctly extracted and the request body is cleared.
func TestCapture_MultipartFormData_367(t *testing.T) {
	logger := zap.NewNop()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("key", "value")
	writer.Close()

	req, _ := http.NewRequest("POST", "http://example.com", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("response"))),
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		require.NotNil(t, tc)
		assert.Equal(t, "", tc.HTTPReq.Body) // Body should be cleared
		require.Len(t, tc.HTTPReq.Form, 1)
		assert.Equal(t, "key", tc.HTTPReq.Form[0].Key)
		assert.Equal(t, []string{"value"}, tc.HTTPReq.Form[0].Values)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for test case")
	}
}

// TestCapture_ResponseBodyCloseError_378 ensures a test case is still captured even if closing the response body fails.
func TestCapture_ResponseBodyCloseError_378(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &mockReadCloser{Reader: bytes.NewReader([]byte("response")), closeErr: errors.New("close error")},
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{})

	select {
	case tc := <-tChan:
		require.NotNil(t, tc)
		assert.Equal(t, "response", tc.HTTPResp.Body)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for test case")
	}
}

// TestCaptureGRPC_Success_401 checks that a valid gRPC stream is successfully captured as a test case.
func TestCaptureGRPC_Success_401(t *testing.T) {
	logger := zap.NewNop()
	tChan := make(chan *models.TestCase, 1)
	http2Stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":path": "/service/method"},
				OrdinaryHeaders: map[string]string{"Keploy-Test-Name": "my-grpc-test"},
			},
			Body: models.GrpcLengthPrefixedMessage{DecodedData: "request-body"},
		},
		GRPCResp: &models.GrpcResp{
			Body: models.GrpcLengthPrefixedMessage{DecodedData: "response-body"},
		},
	}

	CaptureGRPC(context.Background(), logger, tChan, http2Stream)

	select {
	case tc := <-tChan:
		require.NotNil(t, tc)
		assert.Equal(t, models.GRPC_EXPORT, tc.Kind)
		assert.Equal(t, "my-grpc-test", tc.Name)
		assert.Equal(t, "request-body", tc.GrpcReq.Body.DecodedData)
		assert.Equal(t, "response-body", tc.GrpcResp.Body.DecodedData)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for test case")
	}
}

// TestCaptureGRPC_NilStream_412 ensures that no test case is captured when the stream is nil.
func TestCaptureGRPC_NilStream_412(t *testing.T) {
	logger := zap.NewNop()
	tChan := make(chan *models.TestCase, 1)

	CaptureGRPC(context.Background(), logger, tChan, nil)

	select {
	case tc := <-tChan:
		t.Fatalf("Expected no test case for nil stream, but got one: %v", tc)
	default:
		// Success
	}
}

// TestCaptureGRPC_NilRequest_423 ensures that no test case is captured when the gRPC request is nil.
func TestCaptureGRPC_NilRequest_423(t *testing.T) {
	logger := zap.NewNop()
	tChan := make(chan *models.TestCase, 1)
	http2Stream := &pkg.HTTP2Stream{
		GRPCReq:  nil,
		GRPCResp: &models.GrpcResp{},
	}

	CaptureGRPC(context.Background(), logger, tChan, http2Stream)

	select {
	case tc := <-tChan:
		t.Fatalf("Expected no test case for nil request, but got one: %v", tc)
	default:
		// Success
	}
}

// TestCaptureGRPC_ContextCanceled_445 verifies that the function does not block when the context is canceled.
func TestCaptureGRPC_ContextCanceled_445(t *testing.T) {
	logger := zap.NewNop()
	tChan := make(chan *models.TestCase) // Unbuffered channel
	http2Stream := &pkg.HTTP2Stream{
		GRPCReq:  &models.GrpcReq{},
		GRPCResp: &models.GrpcResp{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	CaptureGRPC(ctx, logger, tChan, http2Stream)
	// Test passes if it doesn't hang
}

// TestIsFiltered_UrlMethodMatch_789 verifies that a request is filtered if its method matches one in the filter list.
func TestIsFiltered_UrlMethodMatch_789(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("POST", "http://example.com/path", nil)
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				URLMethods: []string{"POST", "PUT"},
			},
		},
	}
	assert.True(t, isFiltered(logger, req, opts))
}

// TestIsFiltered_HeaderMatch_101 verifies that a request is filtered if its headers match the filter's header regex.
func TestIsFiltered_HeaderMatch_101(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/path", nil)
	req.Header.Set("Content-Type", "application/json")
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		},
	}
	assert.True(t, isFiltered(logger, req, opts))
}

// TestIsFiltered_HeaderBadRegex_112 ensures the function handles an invalid regex in a header filter gracefully and does not filter the request.
func TestIsFiltered_HeaderBadRegex_112(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/path", nil)
	req.Header.Set("X-Test", "value")
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				Headers: map[string]string{
					"X-Test": "[", // Invalid regex
				},
			},
		},
	}
	// Should not panic and should not filter
	assert.False(t, isFiltered(logger, req, opts))
}

// TestIsFiltered_AND_Match_131 verifies that a request is filtered when all conditions (bypass, method, header) match with AND logic.
func TestIsFiltered_AND_Match_131(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("POST", "http://example.com/api/v1", nil)
	req.Header.Set("X-API-KEY", "test-key")
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				BypassRule: config.BypassRule{
					Path: "/api/v1",
				},
				URLMethods: []string{"POST"},
				Headers: map[string]string{
					"X-API-KEY": "test-key",
				},
				MatchType: config.AND,
			},
		},
	}
	assert.True(t, isFiltered(logger, req, opts))
}

// TestIsFiltered_AND_NoMatch_141 verifies that a request is not filtered with AND logic if one of the conditions does not match.
func TestIsFiltered_AND_NoMatch_141(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/api/v1", nil) // Method is GET, not POST
	req.Header.Set("X-API-KEY", "test-key")
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				BypassRule: config.BypassRule{
					Path: "/api/v1",
				},
				URLMethods: []string{"POST"},
				Headers: map[string]string{
					"X-API-KEY": "test-key",
				},
				MatchType: config.AND,
			},
		},
	}
	assert.False(t, isFiltered(logger, req, opts))
}

// TestIsFiltered_OR_Match_151 verifies that a request is filtered with OR logic if at least one of the conditions matches.
func TestIsFiltered_OR_Match_151(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/api/v1", nil) // Method doesn't match
	req.Header.Set("X-API-KEY", "test-key")                            // Header matches
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{
				URLMethods: []string{"POST"},
				Headers: map[string]string{
					"X-API-KEY": "test-key",
				},
				MatchType: config.OR, // Default is OR
			},
		},
	}
	assert.True(t, isFiltered(logger, req, opts))
}

// TestCapture_FilteredRequest_323 verifies that a request that matches a filter is not captured.
func TestCapture_FilteredRequest_323(t *testing.T) {
	logger := zap.NewNop()
	req, _ := http.NewRequest("GET", "http://example.com/filtered", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("response"))),
	}
	opts := models.IncomingOptions{
		Filters: []config.Filter{
			{BypassRule: config.BypassRule{Path: "/filtered"}},
		},
	}
	tChan := make(chan *models.TestCase, 1)

	Capture(context.Background(), logger, tChan, req, resp, time.Now(), time.Now(), opts)

	select {
	case tc := <-tChan:
		t.Fatalf("Expected no test case for filtered request, but got one: %v", tc)
	default:
		// Success
	}
}
