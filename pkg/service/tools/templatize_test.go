package tools

import (
	"errors"
	"testing"

	"context"

	"github.com/7sDream/geko"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// TestRenderIfTemplatized_NonTemplatizedString_001 tests RenderIfTemplatized with a non-templatized string input.
func TestRenderIfTemplatized_NonTemplatizedString_001(t *testing.T) {
	val := "non-templatized string"
	isTemplatized, renderedVal, err := RenderIfTemplatized(val)
	require.NoError(t, err)
	assert.False(t, isTemplatized)
	assert.Equal(t, val, renderedVal)
}

// TestRenderIfTemplatized_TemplatizedString_002 tests RenderIfTemplatized with a valid templatized string input.
func TestRenderIfTemplatized_TemplatizedString_002(t *testing.T) {
	val := "{{string .key}}"
	utils.TemplatizedValues = map[string]interface{}{"key": "value"}
	isTemplatized, renderedVal, err := RenderIfTemplatized(val)
	require.NoError(t, err)
	assert.True(t, isTemplatized)
	assert.Equal(t, "value", renderedVal)
}

// TestAddQuotesInTemplates_ValidTemplates_003 tests addQuotesInTemplates with valid JSON templates.
func TestAddQuotesInTemplates_ValidTemplates_003(t *testing.T) {
	input := `{"id":{{int .id}}}`
	expected := `{"id":"{{int .id}}"}`
	result := addQuotesInTemplates(input)
	assert.Equal(t, expected, result)
}

// TestRemoveQuotesInTemplates_ValidTemplates_004 tests removeQuotesInTemplates with valid JSON templates.
func TestRemoveQuotesInTemplates_ValidTemplates_004(t *testing.T) {
	input := `{"id":"{{int .id}}"}`
	expected := `{"id":{{int .id}}}`
	result := removeQuotesInTemplates(input)
	assert.Equal(t, expected, result)
}

// TestRender_ValidTemplate_005 tests render function with a valid template.
func TestRender_ValidTemplate_005(t *testing.T) {
	utils.TemplatizedValues = map[string]interface{}{"key": 123}
	val := "{{int .key}}"
	result, err := render(val)
	require.NoError(t, err)
	assert.Equal(t, 123, result)
}

// TestInsertUnique_ValidKey_006 tests insertUnique function with a valid key.
func TestInsertUnique_ValidKey_006(t *testing.T) {
	myMap := map[string]interface{}{"key": "value"}
	result := insertUnique("key", "value", myMap)
	assert.Equal(t, "key", result)
	assert.Equal(t, "value", myMap["key"])
}

// TestMarshalJSON_ValidData_007 tests marshalJSON function with valid data.
func TestMarshalJSON_ValidData_007(t *testing.T) {
	logger := zap.NewNop()
	data := map[string]interface{}{"key": "value"}
	result := marshalJSON(data, logger)
	assert.Equal(t, `{"key":"value"}`, result)
}

// TestParseIntoJSON_ValidData_008 tests parseIntoJSON function with valid JSON data.
func TestParseIntoJSON_ValidData_008(t *testing.T) {
	input := `{"key":"value"}`
	result, err := parseIntoJSON(input)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

// TestTemplatize_NoTestSets_789 tests the Templatize function when no test sets are found.
// It ensures that the function returns nil without error and logs a warning.
func TestTemplatize_NoTestSets_789(t *testing.T) {
	// Arrange
	mockTestDB := new(MockTestDB)
	mockTestSetConf := new(MockTestSetConfig)
	logger := zap.NewNop()
	cfg := &config.Config{
		Templatize: config.Templatize{
			TestSets: []string{},
		},
	}
	toolsSvc := &Tools{
		logger:      logger,
		testDB:      mockTestDB,
		testSetConf: mockTestSetConf,
		config:      cfg,
	}
	ctx := context.Background()

	mockTestDB.On("GetAllTestSetIDs", mock.Anything).Return([]string{}, nil)

	// Act
	err := toolsSvc.Templatize(ctx)

	// Assert
	require.NoError(t, err)
	mockTestDB.AssertExpectations(t)
}

// TestTemplatize_GetAllTestSetsError_456 tests the error handling of Templatize
// when fetching all test set IDs from the database fails.
func TestTemplatize_GetAllTestSetsError_456(t *testing.T) {
	// Arrange
	mockTestDB := new(MockTestDB)
	logger := zap.NewNop()
	cfg := &config.Config{
		Templatize: config.Templatize{
			TestSets: []string{},
		},
	}
	toolsSvc := &Tools{
		logger: logger,
		testDB: mockTestDB,
		config: cfg,
	}
	ctx := context.Background()
	dbErr := errors.New("db error")

	mockTestDB.On("GetAllTestSetIDs", mock.Anything).Return(nil, dbErr)

	// Act
	err := toolsSvc.Templatize(ctx)

	// Assert
	require.Error(t, err)
	assert.Equal(t, dbErr, err)
	mockTestDB.AssertExpectations(t)
}

// TestTemplatize_GetTestCasesError_123 tests the error handling of Templatize
// when fetching test cases for a specific test set fails.
func TestTemplatize_GetTestCasesError_123(t *testing.T) {
	// Arrange
	mockTestDB := new(MockTestDB)
	mockTestSetConf := new(MockTestSetConfig)
	logger := zap.NewNop()
	cfg := &config.Config{
		Templatize: config.Templatize{
			TestSets: []string{"test-set-1"},
		},
	}
	toolsSvc := &Tools{
		logger:      logger,
		testDB:      mockTestDB,
		testSetConf: mockTestSetConf,
		config:      cfg,
	}
	ctx := context.Background()
	testSetID := "test-set-1"
	dbErr := errors.New("db error")

	mockTestSetConf.On("Read", mock.Anything, testSetID).Return(nil, nil)
	mockTestDB.On("GetTestCases", mock.Anything, testSetID).Return(nil, dbErr)

	// Act
	err := toolsSvc.Templatize(ctx)

	// Assert
	require.Error(t, err)
	assert.Equal(t, dbErr, err)
	mockTestDB.AssertExpectations(t)
	mockTestSetConf.AssertExpectations(t)
}

// TestParseIntoJSON_EmptyAndError_112 tests edge cases for parseIntoJSON.
func TestParseIntoJSON_EmptyAndError_112(t *testing.T) {
	// Test with empty string
	res, err := parseIntoJSON("")
	require.NoError(t, err)
	assert.Nil(t, res)

	// Test with invalid JSON
	res, err = parseIntoJSON("{invalid json")
	require.Error(t, err)
	assert.Nil(t, res)
}

// TestGetType_AllCases_343 ensures getType returns the correct type string for different inputs.
func TestGetType_AllCases_343(t *testing.T) {
	assert.Equal(t, "string", getType("a string"))
	assert.Equal(t, "int", getType(123))
	assert.Equal(t, "int", getType(int64(123)))
	assert.Equal(t, "float", getType(123.45))
	assert.Equal(t, "float", getType(float32(123.45)))
	assert.Equal(t, "", getType(true)) // Unsupported type
}

// TestInsertUnique_AllCases_565 tests the logic for inserting unique keys into a map.
func TestInsertUnique_AllCases_565(t *testing.T) {
	myMap := make(map[string]interface{})

	// New key
	key1 := insertUnique("id", "123", myMap)
	assert.Equal(t, "id", key1)
	assert.Equal(t, "123", myMap["id"])

	// Existing key, same value
	key2 := insertUnique("id", "123", myMap)
	assert.Equal(t, "id", key2)

	// Existing key, different value
	key3 := insertUnique("id", "456", myMap)
	assert.Equal(t, "id1", key3)
	assert.Equal(t, "456", myMap["id1"])

	// Key with hyphen
	key4 := insertUnique("user-id", "789", myMap)
	assert.Equal(t, "userid", key4)
	assert.Equal(t, "789", myMap["userid"])
}

// TestCompareReqHeaders_ErrorAndNoMatch_787 tests edge cases for compareReqHeaders.
func TestCompareReqHeaders_ErrorAndNoMatch_787(t *testing.T) {
	logger := zap.NewNop()
	// Reset global state
	utils.TemplatizedValues = make(map[string]interface{})

	// Test no match
	req1 := map[string]string{"key1": "val1"}
	req2 := map[string]string{"key2": "val2"}
	originalReq1 := map[string]string{"key1": "val1"}
	compareReqHeaders(logger, req1, req2)
	assert.Equal(t, originalReq1, req1)

	// Test render error
	req1Error := map[string]string{"key": "{{.missing}}"}
	req2Error := map[string]string{"key": "val"}
	compareReqHeaders(logger, req1Error, req2Error)
	// Should log an error and return, no change expected
	assert.Equal(t, "{{.missing}}", req1Error["key"])
}

// TestRemoveQuotesInTemplates_Corrected_011 ensures that quotes are removed from non-string
// templates but preserved for string templates.
func TestRemoveQuotesInTemplates_Corrected_011(t *testing.T) {
	input := `{"id":"{{int .id}}", "name":"{{string .name}}"}`
	expected := `{"id":{{int .id}}, "name":"{{string .name}}"}`
	result := removeQuotesInTemplates(input)
	assert.Equal(t, expected, result)
}

// TestAddQuotesInTemplates_EdgeCases_223 tests edge cases like escaped quotes
// and unterminated templates to ensure robust parsing.
func TestAddQuotesInTemplates_EdgeCases_223(t *testing.T) {
	// Test with escaped quotes inside a string
	inputWithEscapedQuote := `{"key":"value with \"{{...}}\" quotes"}`
	result := addQuotesInTemplates(inputWithEscapedQuote)
	assert.Equal(t, inputWithEscapedQuote, result)

	// Test with an unterminated template
	inputUnterminated := `{"key":{{.id}`
	result = addQuotesInTemplates(inputUnterminated)
	assert.Equal(t, inputUnterminated, result)
}

// TestAddTemplates_UnsupportedType_444 tests the handling of unsupported types in addTemplates.
func TestAddTemplates_UnsupportedType_444(t *testing.T) {
	logger := zap.NewNop()
	unsupportedType := true // boolean is not a supported type in the switch
	jsonBody, _ := parseIntoJSON(`{"key": "value"}`)

	// This should not panic and should return false
	result := addTemplates(logger, unsupportedType, jsonBody)
	assert.False(t, result)
}

// TestAddTemplates1_UnsupportedType_890 tests the handling of unsupported types in addTemplates1.
func TestAddTemplates1_UnsupportedType_890(t *testing.T) {
	logger := zap.NewNop()
	val1 := "some_value"
	unsupportedBody := true // boolean is not a supported type in the switch

	// This should not panic and should return false
	result := addTemplates1(logger, &val1, unsupportedBody)
	assert.False(t, result)
}

// TestAddTemplates_RenderError_578 ensures that addTemplates handles errors from RenderIfTemplatized gracefully.
func TestAddTemplates_RenderError_578(t *testing.T) {
	logger := zap.NewNop()
	utils.TemplatizedValues = make(map[string]interface{})

	// Test for map[string]string case
	headers := map[string]string{"Authorization": "{{ .missing }}"}
	result := addTemplates(logger, headers, `{"some":"body"}`)
	assert.False(t, result, "addTemplates should return false on render error")

	// Test for *string case
	url := "{{ .missing }}"
	result = addTemplates(logger, &url, `{"some":"body"}`)
	assert.False(t, result, "addTemplates should return false on render error")

	// Test for geko.ObjectItems case
	obj, _ := geko.JSONUnmarshal([]byte(`{"key":"{{ .missing }}"}`))
	result = addTemplates(logger, obj, `{"some":"body"}`)
	assert.False(t, result, "addTemplates should return false on render error")

	// Test for geko.Array case
	arr, _ := geko.JSONUnmarshal([]byte(`["{{ .missing }}"]`))
	result = addTemplates(logger, arr, `{"some":"body"}`)
	assert.False(t, result, "addTemplates should return false on render error")
}

// TestAddTemplates1_RenderError_714 ensures that addTemplates1 handles errors from RenderIfTemplatized gracefully.
func TestAddTemplates1_RenderError_714(t *testing.T) {
	logger := zap.NewNop()
	utils.TemplatizedValues = make(map[string]interface{})
	val1 := "some_value"

	// Test for geko.ObjectItems case
	obj, _ := geko.JSONUnmarshal([]byte(`{"key":"{{ .missing }}"}`))
	result := addTemplates1(logger, &val1, obj)
	assert.False(t, result, "addTemplates1 should return false on render error")

	// Test for map[string]string case
	headers := map[string]string{"key": "{{ .missing }}"}
	result = addTemplates1(logger, &val1, headers)
	assert.False(t, result, "addTemplates1 should return false on render error")

	// Test for *string case
	str := "{{ .missing }}"
	result = addTemplates1(logger, &val1, &str)
	assert.False(t, result, "addTemplates1 should return false on render error")
}

// TestAddTemplates_URLParseError_628 checks that addTemplates handles invalid URLs gracefully.
func TestAddTemplates_URLParseError_628(t *testing.T) {
	logger := zap.NewNop()
	// An invalid URL with a control character
	invalidURL := "http://example.com/\x7f"
	body := `{"key":"val"}`

	// This should not panic and return false
	result := addTemplates(logger, &invalidURL, body)
	assert.False(t, result)
}

// TestMarshalJSON_Error_890 tests marshalJSON's error handling.
func TestMarshalJSON_Error_890(t *testing.T) {
	// Arrange
	originalMarshal := jsonMarshal463
	jsonMarshal463 = func(v interface{}) ([]byte, error) {
		return nil, errors.New("marshal error")
	}
	defer func() { jsonMarshal463 = originalMarshal }()

	logger := zap.NewNop()
	data := make(chan int) // Unmarshallable type

	// Act
	result := marshalJSON(data, logger)

	// Assert
	assert.Equal(t, "", result)
}

// TestProcessRespBodyToReqHeader_ParseError_201 ensures the function handles JSON parsing errors gracefully.
func TestProcessRespBodyToReqHeader_ParseError_201(t *testing.T) {
	logger := zap.NewNop()
	toolsSvc := &Tools{logger: logger}
	tcs := []*models.TestCase{
		{Name: "tc1", HTTPResp: models.HTTPResp{Body: `invalid-json`}},
		{Name: "tc2", HTTPReq: models.HTTPReq{Header: map[string]string{"h": "v"}}},
	}
	ctx := context.Background()

	// The function should log an error and continue, not panic or return an error.
	assert.NotPanics(t, func() {
		toolsSvc.processRespBodyToReqHeader(ctx, tcs)
	})
	// Ensure original data is not corrupted
	assert.Equal(t, map[string]string{"h": "v"}, tcs[1].HTTPReq.Header)
}

// TestProcessRespBodyToReqBody_ParseError_266 ensures the function handles JSON parsing errors gracefully.
func TestProcessRespBodyToReqBody_ParseError_266(t *testing.T) {
	logger := zap.NewNop()
	toolsSvc := &Tools{logger: logger}
	ctx := context.Background()

	t.Run("InnerLoopRequestParseError", func(t *testing.T) {
		tcs := []*models.TestCase{
			{Name: "tc1", HTTPResp: models.HTTPResp{Body: `{"key":"val"}`}},
			{Name: "tc2", HTTPReq: models.HTTPReq{Body: `invalid-json`}},
		}
		assert.NotPanics(t, func() {
			toolsSvc.processRespBodyToReqBody(ctx, tcs)
		})
		// The original response body should be marshaled back after processing
		assert.Contains(t, tcs[0].HTTPResp.Body, `"key"`)
		assert.Contains(t, tcs[0].HTTPResp.Body, `"val"`)
	})

	t.Run("OuterLoopResponseParseError", func(t *testing.T) {
		tcs := []*models.TestCase{
			{Name: "tc1", HTTPResp: models.HTTPResp{Body: `invalid-json`}},
			{Name: "tc2", HTTPReq: models.HTTPReq{Body: `{"key":"val"}`}},
		}
		assert.NotPanics(t, func() {
			toolsSvc.processRespBodyToReqBody(ctx, tcs)
		})
		assert.Equal(t, `invalid-json`, tcs[0].HTTPResp.Body, "Invalid response body should be unchanged")
	})
}

// TestAddTemplates_GekoArray_547 tests the geko.Array case in addTemplates.
func TestAddTemplates_GekoArray_547(t *testing.T) {
	// Setup
	logger := zap.NewNop()
	originalTemplatizedValues := utils.TemplatizedValues
	utils.TemplatizedValues = make(map[string]interface{})
	defer func() { utils.TemplatizedValues = originalTemplatizedValues }()

	// geko array with a value that matches a key in the body
	gkoArray, err := geko.JSONUnmarshal([]byte(`["value1", 123, "value3"]`))
	require.NoError(t, err)
	// body with a key that matches a value in the array
	body, err := geko.JSONUnmarshal([]byte(`{"keyFor123": 123}`))
	require.NoError(t, err)

	// Act
	addTemplates(logger, gkoArray, body)

	// Assert
	resultJSON := marshalJSON(gkoArray, logger)
	// The number 123 should be templatized
	expectedJSON := `["value1",{{float .keyFor123}},"value3"]`
	assert.Equal(t, expectedJSON, removeQuotesInTemplates(resultJSON))
}

// TestAddTemplates1_NumericTypes_857 tests the numeric pointer cases in addTemplates1.
func TestAddTemplates1_NumericTypes_857(t *testing.T) {
	logger := zap.NewNop()
	val1 := "123.45"

	t.Run("Float64Match", func(t *testing.T) {
		var body float64 = 123.45
		ok := addTemplates1(logger, &val1, &body)
		assert.True(t, ok)
	})

	t.Run("Float32Match", func(t *testing.T) {
		val1_32 := "54.321"
		var body float32 = 54.321
		ok := addTemplates1(logger, &val1_32, &body)
		assert.True(t, ok)
	})

	t.Run("IntMatch", func(t *testing.T) {
		val1_int := "987"
		var body int = 987
		ok := addTemplates1(logger, &val1_int, &body)
		assert.True(t, ok)
	})

	t.Run("Int64Match", func(t *testing.T) {
		val1_i64 := "654"
		var body int64 = 654
		ok := addTemplates1(logger, &val1_i64, &body)
		assert.True(t, ok)
	})

	t.Run("NoMatch", func(t *testing.T) {
		val1_nomatch := "111"
		var body int = 222
		ok := addTemplates1(logger, &val1_nomatch, &body)
		assert.False(t, ok)
	})
}

// TestProcessTestCases_ErrorScenarios_567 covers error handling in ProcessTestCases,
// such as when updating a test case or writing the test set fails.
func TestProcessTestCases_ErrorScenarios_567(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	// Setup for resetting global state
	originalTemplatizedValues := utils.TemplatizedValues
	originalSecretValues := utils.SecretValues
	defer func() {
		utils.TemplatizedValues = originalTemplatizedValues
		utils.SecretValues = originalSecretValues
	}()

	t.Run("UpdateTestCase Error", func(t *testing.T) {
		mockTestDB := new(MockTestDB)
		mockTestSetConf := new(MockTestSetConfig)
		toolsSvc := &Tools{logger: logger, testDB: mockTestDB, testSetConf: mockTestSetConf}
		tcs := []*models.TestCase{{Name: "tc1", HTTPReq: models.HTTPReq{}, HTTPResp: models.HTTPResp{}}}
		updateErr := errors.New("update error")

		mockTestDB.On("UpdateTestCase", mock.Anything, tcs[0], "test-set-1", false).Return(updateErr)

		err := toolsSvc.ProcessTestCases(ctx, tcs, "test-set-1")
		require.Error(t, err)
		assert.Equal(t, updateErr, err)
		mockTestDB.AssertExpectations(t)
	})

	t.Run("WriteTestSet Error", func(t *testing.T) {
		mockTestDB := new(MockTestDB)
		mockTestSetConf := new(MockTestSetConfig)
		cfg := &config.Config{Path: "/tmp"}
		toolsSvc := &Tools{logger: logger, testDB: mockTestDB, testSetConf: mockTestSetConf, config: cfg}
		tcs := []*models.TestCase{{Name: "tc1", HTTPReq: models.HTTPReq{}, HTTPResp: models.HTTPResp{}}}
		writeErr := errors.New("write error")

		mockTestDB.On("UpdateTestCase", mock.Anything, tcs[0], "test-set-1", false).Return(nil)
		mockTestSetConf.On("Read", mock.Anything, "test-set-1").Return(nil, nil)
		mockTestSetConf.On("Write", mock.Anything, "test-set-1", mock.AnythingOfType("*models.TestSet")).Return(writeErr)

		err := toolsSvc.ProcessTestCases(ctx, tcs, "test-set-1")
		require.Error(t, err)
		assert.Equal(t, writeErr, err)
		mockTestDB.AssertExpectations(t)
		mockTestSetConf.AssertExpectations(t)
	})
}

// TestProcessTestCases_FullFlow_999 simulates a realistic scenario where a token from a login
// response is used in a subsequent request's header, and a resource ID from a URL is reflected
// in the response body. It verifies that both are correctly templatized.
func TestProcessTestCases_FullFlow_999(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	mockTestDB := new(MockTestDB)
	mockTestSetConf := new(MockTestSetConfig)
	cfg := &config.Config{Path: "/tmp"}
	toolsSvc := &Tools{logger: logger, testDB: mockTestDB, testSetConf: mockTestSetConf, config: cfg}
	ctx := context.Background()
	testSetID := "flow-test"

	// Setup: Reset global state
	originalTemplatizedValues := utils.TemplatizedValues
	originalSecretValues := utils.SecretValues
	utils.TemplatizedValues = make(map[string]interface{})
	utils.SecretValues = make(map[string]interface{})
	defer func() {
		utils.TemplatizedValues = originalTemplatizedValues
		utils.SecretValues = originalSecretValues
	}()

	tcs := []*models.TestCase{
		{ // TC1: Login, returns a token
			Name: "login",
			HTTPReq: models.HTTPReq{
				Body: `{"user":"admin","pass":"123"}`,
			},
			HTTPResp: models.HTTPResp{
				Body: `{"access_token":"xyz-token-123"}`,
			},
		},
		{ // TC2: Use the token to get a resource
			Name: "get-resource",
			HTTPReq: models.HTTPReq{
				URL:    "http://example.com/resource/abc",
				Header: map[string]string{"Authorization": "Bearer xyz-token-123"},
			},
			HTTPResp: models.HTTPResp{
				Body: `{"id":"abc","value":"resource-data"}`,
			},
		},
	}

	mockTestDB.On("UpdateTestCase", mock.Anything, mock.AnythingOfType("*models.TestCase"), testSetID, false).Return(nil).Twice()
	mockTestSetConf.On("Read", mock.Anything, testSetID).Return(nil, nil)
	mockTestSetConf.On("Write", mock.Anything, testSetID, mock.Anything).Return(nil)

	// Act
	err := toolsSvc.ProcessTestCases(ctx, tcs, testSetID)
	require.NoError(t, err)

	// Assert
	// 1. The token from TC1 response is templatized in TC2 request header.
	// Note the space before the closing brackets, which matches the current implementation.
	expectedTC2ReqHeader := map[string]string{"Authorization": "Bearer {{string .access_token }}"}
	assert.Equal(t, expectedTC2ReqHeader, tcs[1].HTTPReq.Header)

	// 2. The resource ID from TC2 URL ("abc") is used to templatize the response body.
	// The key is derived from the response body ("id").
	// The final body is after removeQuotesInTemplates, which keeps quotes for string templates.
	expectedTC2RespBody := `{"id":"{{string .id }}","value":"resource-data"}`
	assert.Equal(t, expectedTC2RespBody, tcs[1].HTTPResp.Body)

	// 3. The global templatized values map is updated correctly.
	assert.Equal(t, "xyz-token-123", utils.TemplatizedValues["access_token"])
	assert.Equal(t, "abc", utils.TemplatizedValues["id"])

	mockTestDB.AssertExpectations(t)
	mockTestSetConf.AssertExpectations(t)
}

// TestTemplatize_ErrorFlows_401 covers scenarios where dependencies of Templatize
// return errors, such as when reading a test set configuration or when
// ProcessTestCases itself fails.
func TestTemplatize_ErrorFlows_401(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	t.Run("ReadTestSetError", func(t *testing.T) {
		mockTestSetConf := new(MockTestSetConfig)
		cfg := &config.Config{Templatize: config.Templatize{TestSets: []string{"ts-1"}}}
		toolsSvc := &Tools{logger: logger, testSetConf: mockTestSetConf, config: cfg}
		readErr := errors.New("read error")

		// We return an error, but also a non-nil test set to cover more logic.
		mockTestSetConf.On("Read", mock.Anything, "ts-1").Return(&models.TestSet{}, readErr)

		// The error from Read is logged but not returned, so the flow continues.
		// To make the test meaningful, we'd need to check logs or subsequent behavior.
		// For now, we just ensure it doesn't panic.
		// A subsequent call to GetTestCases would be made. Let's mock that.
		mockTestDB := new(MockTestDB)
		toolsSvc.testDB = mockTestDB
		mockTestDB.On("GetTestCases", mock.Anything, "ts-1").Return([]*models.TestCase{}, nil)

		err := toolsSvc.Templatize(ctx)
		require.NoError(t, err, "Error from Read should be logged, not returned from Templatize")
		mockTestSetConf.AssertExpectations(t)
	})

	t.Run("ProcessTestCasesError", func(t *testing.T) {
		mockTestDB := new(MockTestDB)
		mockTestSetConf := new(MockTestSetConfig)
		cfg := &config.Config{Templatize: config.Templatize{TestSets: []string{"ts-1"}}}
		toolsSvc := &Tools{logger: logger, testDB: mockTestDB, testSetConf: mockTestSetConf, config: cfg}
		processErr := errors.New("process error")

		mockTestSetConf.On("Read", mock.Anything, "ts-1").Return(nil, nil)
		tcs := []*models.TestCase{{Name: "tc1"}}
		mockTestDB.On("GetTestCases", mock.Anything, "ts-1").Return(tcs, nil)

		// Mock the first call within ProcessTestCases to fail
		mockTestDB.On("UpdateTestCase", mock.Anything, tcs[0], "ts-1", false).Return(processErr)

		err := toolsSvc.Templatize(ctx)
		require.Error(t, err)
		assert.Equal(t, processErr, err)
		mockTestDB.AssertExpectations(t)
		mockTestSetConf.AssertExpectations(t)
	})
}
