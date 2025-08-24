package testdb

import (
	"testing"

	"strconv"

	"sort"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	yaml2 "go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestEncodeTestcase_HTTP_ValidCurl_123 tests the EncodeTestcase function for an HTTP test case with a valid Curl value.
func TestEncodeTestcase_HTTP_ValidCurl_123(t *testing.T) {
	logger := zap.NewNop()
	httpReq := models.HTTPReq{
		Method:     "GET",
		URL:        "http://example.com",
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       "",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	httpResp := models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       "{}",
	}
	tc := models.TestCase{
		Kind:     models.HTTP,
		Name:     "Test HTTP",
		Curl:     "curl -X GET http://example.com",
		HTTPReq:  httpReq,
		HTTPResp: httpResp,
	}

	doc, err := EncodeTestcase(tc, logger)

	require.NoError(t, err)
	require.NotNil(t, doc)
	assert.Equal(t, tc.Curl, doc.Curl)
}

// TestEncodeTestcase_HTTP_ValidCurl_456 tests the EncodeTestcase function for an HTTP test case with a valid Curl value.
func TestEncodeTestcase_HTTP_ValidCurl_456(t *testing.T) {
	logger := zap.NewNop()
	httpReq := models.HTTPReq{
		Method:     "POST",
		URL:        "http://example.com/api",
		Header:     map[string]string{"Authorization": "Bearer token"},
		Body:       `{"key": "value"}`,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	httpResp := models.HTTPResp{
		StatusCode: 201,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"success": true}`,
	}
	tc := models.TestCase{
		Kind:     models.HTTP,
		Name:     "Test HTTP POST",
		Curl:     "curl -X POST http://example.com/api -H 'Authorization: Bearer token' -d '{\"key\": \"value\"}'",
		HTTPReq:  httpReq,
		HTTPResp: httpResp,
	}

	doc, err := EncodeTestcase(tc, logger)

	require.NoError(t, err)
	require.NotNil(t, doc)
	assert.Equal(t, tc.Curl, doc.Curl)
	assert.Equal(t, tc.Name, doc.Name)
	assert.Equal(t, tc.Kind, doc.Kind)
}

// TestDecode_AllCases_555 covers multiple scenarios for the Decode function.
// It tests error handling for invalid kinds, spec decoding failures, and malformed
// noise assertion data structures to ensure the function is robust.
func TestDecode_AllCases_555(t *testing.T) {
	logger := zap.NewNop()

	t.Run("InvalidKindError", func(t *testing.T) {
		doc := &yaml2.NetworkTrafficDoc{
			Kind: "InvalidKind",
		}
		tc, err := Decode(doc, logger)
		require.Error(t, err)
		assert.Nil(t, tc)
		assert.Equal(t, "invalid testcase kind", err.Error())
	})

	t.Run("SpecDecodeError", func(t *testing.T) {
		// Create a spec node with an invalid type (e.g., a scalar instead of a map)
		var specNode yaml.Node
		err := specNode.Encode("this is not a valid http spec")
		require.NoError(t, err)

		doc := &yaml2.NetworkTrafficDoc{
			Kind: models.HTTP,
			Spec: specNode,
		}
		_, err = Decode(doc, logger)
		require.Error(t, err)
	})

	t.Run("MalformedNoise", func(t *testing.T) {
		// Case 1: Noise assertion is not a map
		httpSpec1 := models.HTTPSchema{
			Assertions: map[models.AssertionType]interface{}{
				models.NoiseAssertion: "not-a-map",
			},
		}
		var specNode1 yaml.Node
		_ = specNode1.Encode(httpSpec1)
		doc1 := &yaml2.NetworkTrafficDoc{Kind: models.HTTP, Spec: specNode1}
		tc1, err1 := Decode(doc1, logger)
		require.NoError(t, err1) // The function just logs a warning and continues
		assert.Empty(t, tc1.Noise)

		// Case 2: Inner value of noise map is not a slice
		noiseData := map[string]interface{}{
			"header.Date": "not-a-slice",
		}
		httpSpec2 := models.HTTPSchema{
			Assertions: map[models.AssertionType]interface{}{
				models.NoiseAssertion: noiseData,
			},
		}
		var specNode2 yaml.Node
		_ = specNode2.Encode(httpSpec2)
		doc2 := &yaml2.NetworkTrafficDoc{Kind: models.HTTP, Spec: specNode2}
		tc2, err2 := Decode(doc2, logger)
		require.NoError(t, err2) // The function just logs a warning and continues
		assert.Empty(t, tc2.Noise["header.Date"])
	})
}

// TestFlatten_AllCases_921 provides comprehensive testing for the Flatten function.
// It covers nil inputs, simple and nested maps, slices of objects, primitive types,
// and invalid input types to ensure robust and correct behavior.
func TestFlatten_AllCases_921(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		flat := Flatten(nil)
		assert.Equal(t, map[string][]string{"": {""}}, flat)
	})

	t.Run("simple map", func(t *testing.T) {
		input := map[string]interface{}{
			"name":    "keploy",
			"active":  true,
			"version": 1.23,
		}
		expected := map[string][]string{
			"name":    {"keploy"},
			"active":  {"true"},
			"version": {strconv.FormatFloat(1.23, 'E', -1, 64)},
		}
		flat := Flatten(input)
		assert.Equal(t, expected, flat)
	})

	t.Run("nested map", func(t *testing.T) {
		input := map[string]interface{}{
			"user": map[string]interface{}{
				"name": "john",
				"id":   float64(123),
			},
		}
		expected := map[string][]string{
			"user.name": {"john"},
			"user.id":   {strconv.FormatFloat(123, 'E', -1, 64)},
		}
		flat := Flatten(input)
		assert.Equal(t, expected, flat)
	})

	t.Run("slice of objects", func(t *testing.T) {
		input := map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"id": "a"},
				map[string]interface{}{"id": "b"},
			},
		}
		expected := map[string][]string{
			"items.id": {"a", "b"},
		}
		flat := Flatten(input)
		assert.Equal(t, expected, flat)
	})

	t.Run("unsupported type", func(t *testing.T) {
		// This will print a log message but not fail
		flat := Flatten(123) // int is not handled
		assert.Empty(t, flat)
	})

	t.Run("invalid map type", func(t *testing.T) {
		input := map[int]interface{}{1: "test"}
		flat := Flatten(input)
		assert.Empty(t, flat)
	})

	t.Run("invalid slice type", func(t *testing.T) {
		input := []string{"a", "b"}
		flat := Flatten(input)
		assert.Empty(t, flat)
	})
}

// TestFindNoisyFields_AllCases_789 tests the FindNoisyFields function.
// It uses a comparator based on pkg.IsTime to identify and return fields
// that contain date-time strings.
func TestFindNoisyFields_AllCases_789(t *testing.T) {
	m := map[string][]string{
		"body.timestamp": {"2023-10-27T10:00:00Z"},
		"header.Date":    {"Fri, 27 Oct 2023 10:00:00 GMT"},
		"body.id":        {"12345"},
		"body.name":      {"test"},
	}

	comparator := func(_ string, vals []string) bool {
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}
		return false
	}

	noisyFields := FindNoisyFields(m, comparator)
	sort.Strings(noisyFields)

	expected := []string{"body.timestamp", "header.Date"}
	sort.Strings(expected)

	assert.Equal(t, expected, noisyFields)
}

// TestContainsMatchingURL_AllCases_662 provides comprehensive tests for the ContainsMatchingURL function.
// It covers URL and method matches, mismatches, cases with no method filter, and error handling
// for invalid URLs and regex patterns.
func TestContainsMatchingURL_AllCases_662(t *testing.T) {
	t.Run("url and method match", func(t *testing.T) {
		match, err := ContainsMatchingURL([]string{"GET"}, "/api/v1/users/\\d+", "http://localhost:8080/api/v1/users/123", "GET")
		require.NoError(t, err)
		assert.True(t, match)
	})

	t.Run("url match, method mismatch", func(t *testing.T) {
		match, err := ContainsMatchingURL([]string{"POST"}, "/api/v1/users/\\d+", "http://localhost:8080/api/v1/users/123", "GET")
		require.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("url match, no method filter", func(t *testing.T) {
		match, err := ContainsMatchingURL([]string{}, "/api/v1/users/\\d+", "http://localhost:8080/api/v1/users/123", "GET")
		require.NoError(t, err)
		assert.True(t, match)
	})

	t.Run("url mismatch", func(t *testing.T) {
		match, err := ContainsMatchingURL([]string{"GET"}, "/api/v1/products/\\d+", "http://localhost:8080/api/v1/users/123", "GET")
		require.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("empty url string", func(t *testing.T) {
		match, err := ContainsMatchingURL([]string{"GET"}, "", "http://localhost:8080/api/v1/users/123", "GET")
		require.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("invalid request url", func(t *testing.T) {
		_, err := ContainsMatchingURL([]string{"GET"}, "/api", "::invalid-url", "GET")
		require.Error(t, err)
	})

	t.Run("invalid regex url string", func(t *testing.T) {
		_, err := ContainsMatchingURL([]string{"GET"}, "[", "http://localhost/api", "GET")
		require.Error(t, err)
	})
}

// TestAddHTTPBodyToMap_AllCases_298 tests the AddHTTPBodyToMap function with valid JSON,
// non-JSON text, and invalid JSON to ensure the body is correctly flattened or added as raw text.
func TestAddHTTPBodyToMap_AllCases_298(t *testing.T) {
	t.Run("valid json body", func(t *testing.T) {
		m := make(map[string][]string)
		body := `{"user":{"name":"test"}, "ids":[1,2]}`
		err := AddHTTPBodyToMap(body, m)
		require.NoError(t, err)
		expected := map[string][]string{
			"body.user.name": {"test"},
			"body.ids":       {strconv.FormatFloat(1, 'E', -1, 64), strconv.FormatFloat(2, 'E', -1, 64)},
		}
		assert.Equal(t, expected, m)
	})

	t.Run("non-json body", func(t *testing.T) {
		m := make(map[string][]string)
		body := "this is plain text"
		err := AddHTTPBodyToMap(body, m)
		require.NoError(t, err)
		expected := map[string][]string{
			"body": {"this is plain text"},
		}
		assert.Equal(t, expected, m)
	})

	t.Run("invalid json body", func(t *testing.T) {
		// json.Valid returns false, so it's treated as raw text
		m := make(map[string][]string)
		body := `{"key": "value"`
		err := AddHTTPBodyToMap(body, m)
		require.NoError(t, err)
		expected := map[string][]string{
			"body": {`{"key": "value"`},
		}
		assert.Equal(t, expected, m)
	})
}
