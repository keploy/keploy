package schema

import (
    "testing"

    "github.com/stretchr/testify/assert"
    matcher "go.keploy.io/server/v3/pkg/matcher"
    "go.keploy.io/server/v3/pkg/models"
    "go.uber.org/zap"
)

// Test 1: Validates that non-determinism is fixed
func TestCompareAllResponseStatusCodes_Deterministic(t *testing.T) {
    logger, _ := zap.NewDevelopment()

    mockOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            "400": createTestResponse("string", "error"),
            "500": createTestResponse("string", "message"),
        },
    }

    testOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            "400": createTestResponse("string", "error"),
            "500": createTestResponse("string", "message"),
        },
    }

    // Run 10 times - should get same result every time
    results := make([]bool, 10)
    for i := 0; i < 10; i++ {
        result := compareAllResponseStatusCodes(
            mockOp, testOp,
            matcher.NewDiffsPrinter("test/mock"),  // ✅ FIXED: Create DiffsPrinter
            nil, logger,
            "test", "mock", "test-1", "mock-1",
            models.IdentifyMode,
        )
        results[i] = result.AllStatusCodesValid
    }

    // All results should be identical
    firstResult := results[0]
    for i := 1; i < 10; i++ {
        assert.Equal(t, firstResult, results[i], 
            "Results should be deterministic - run %d gave different result", i)
    }
}

// Test 2: Detects when ANY status code has wrong type
func TestCompareAllResponseStatusCodes_DetectsTypeMismatch(t *testing.T) {
    logger, _ := zap.NewDevelopment()

    mockOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            "400": createTestResponse("string", "error"),    // ← Correct type
            "500": createTestResponse("string", "message"),
        },
    }

    testOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            "400": createTestResponse("integer", "error"),   // ← WRONG TYPE!
            "500": createTestResponse("string", "message"),
        },
    }

    result := compareAllResponseStatusCodes(
        mockOp, testOp,
        matcher.NewDiffsPrinter("test/mock"),  // ✅ FIXED: Create DiffsPrinter
        nil, logger,
        "test", "mock", "test-1", "mock-1",
        models.IdentifyMode,
    )

    // Should detect mismatch
    assert.False(t, result.AllStatusCodesValid, "Should detect type mismatch in status 400")
    assert.Contains(t, result.InvalidStatusCodes, "400", "Status 400 should be marked invalid")
    assert.Equal(t, 2, result.MatchedCount, "Only 200 and 500 should match")
}

// Test 3: Detects missing status codes
func TestCompareAllResponseStatusCodes_DetectsMissingStatus(t *testing.T) {
    logger, _ := zap.NewDevelopment()

    mockOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            "404": createTestResponse("string", "error"),    // ← Mock has 404
        },
    }

    testOp := &models.Operation{
        Responses: map[string]models.ResponseItem{
            "200": createTestResponse("integer", "id"),
            // Missing "404"!
        },
    }

    result := compareAllResponseStatusCodes(
        mockOp, testOp,
        matcher.NewDiffsPrinter("test/mock"),  // ✅ FIXED: Create DiffsPrinter
        nil, logger,
        "test", "mock", "test-1", "mock-1",
        models.IdentifyMode,
    )

    // Should detect missing status code
    assert.False(t, result.AllStatusCodesValid, "Should detect missing status code")
    assert.Contains(t, result.InvalidStatusCodes, "404", "Status 404 should be marked as invalid")
}

// Helper function
func createTestResponse(fieldType, fieldName string) models.ResponseItem {
    return models.ResponseItem{
        Description: "Test response",
        Content: map[string]models.MediaType{
            "application/json": {
                Schema: models.Schema{
                    Type: "object",
                    Properties: map[string]map[string]interface{}{
                        fieldName: {"type": fieldType},
                    },
                },
            },
        },
    }
}