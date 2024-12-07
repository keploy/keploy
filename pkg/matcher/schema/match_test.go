package schema

import (
    "testing"
    "go.keploy.io/server/v2/pkg/models"
)


// Test generated using Keploy
func TestCompareOperationTypes_SameTypes(t *testing.T) {
    mockOperationType := "GET"
    testOperationType := "GET"
    pass, err := compareOperationTypes(mockOperationType, testOperationType)
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    if !pass {
        t.Errorf("Expected pass to be true, got false")
    }
}

// Test generated using Keploy
func TestCompareOperationTypes_DifferentTypes(t *testing.T) {
    mockOperationType := "GET"
    testOperationType := "POST"
    pass, err := compareOperationTypes(mockOperationType, testOperationType)
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    if pass {
        t.Errorf("Expected pass to be false, got true")
    }
}


// Test generated using Keploy
func TestCompareParameters_EqualParameters(t *testing.T) {
    mockParameters := []models.Parameter{
        {
            Name:     "id",
            In:       "query",
            Required: true,
            Schema: models.ParamSchema{
                Type: "string",
            },
        },
    }
    testParameters := []models.Parameter{
        {
            Name:     "id",
            In:       "query",
            Required: true,
            Schema: models.ParamSchema{
                Type: "string",
            },
        },
    }
    pass, err := compareParameters(mockParameters, testParameters)
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    if !pass {
        t.Errorf("Expected pass to be true, got false")
    }
}

