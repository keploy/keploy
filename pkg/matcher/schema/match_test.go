package schema

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMatch_StatusCodeSelection(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	// Helper to create a dummy OpenAPI object with specific paths/methods/responses
	createOpenAPI := func(path, method, statusCode string) models.OpenAPI {
		op := &models.Operation{
			Responses: map[string]models.ResponseItem{
				statusCode: {
					Content: map[string]models.MediaType{
						"application/json": {
							Schema: models.Schema{
								Properties: map[string]map[string]interface{}{},
							},
						},
					},
				},
			},
		}

		pathItem := models.PathItem{}
		switch method {
		case "GET":
			pathItem.Get = op
		case "POST":
			pathItem.Post = op
		}

		return models.OpenAPI{
			Info: models.Info{Title: "TestService"},
			Paths: map[string]models.PathItem{
				path: pathItem,
			},
		}
	}

	// Helper to add another response to an existing OpenAPI object
	addResponse := func(doc *models.OpenAPI, path, method, statusCode string) {
		pathItem := doc.Paths[path]
		var op *models.Operation
		switch method {
		case "GET":
			op = pathItem.Get
		case "POST":
			op = pathItem.Post
		}

		if op.Responses == nil {
			op.Responses = make(map[string]models.ResponseItem)
		}
		op.Responses[statusCode] = models.ResponseItem{
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{
						Properties: map[string]map[string]interface{}{},
					},
				},
			},
		}
	}

	t.Run("Status Code Match Found - Matching 200", func(t *testing.T) {
		path := "/user"
		method := "GET"

		// Mock has 200 and 400
		mockDoc := createOpenAPI(path, method, "200")
		addResponse(&mockDoc, path, method, "400")

		// Test has 200
		testDoc := createOpenAPI(path, method, "200")

		// Match check
		// IdentifiedMode requires iterating through finding the match
		_, pass, err := Match(mockDoc, testDoc, "test-set-1", "mock-set-1", logger, models.IdentifyMode)
		if err != nil {
			t.Fatalf("Match failed with error: %v", err)
		}
		if !pass {
			t.Errorf("Expected Match to pass for matching status code 200, but it failed")
		}
	})

	t.Run("Status Code Match Found - Matching 400", func(t *testing.T) {
		path := "/user"
		method := "GET"

		// Mock has 200 and 400
		mockDoc := createOpenAPI(path, method, "200")
		addResponse(&mockDoc, path, method, "400")

		// Test has 400
		testDoc := createOpenAPI(path, method, "400")

		// Match check
		_, pass, err := Match(mockDoc, testDoc, "test-set-1", "mock-set-1", logger, models.IdentifyMode)
		if err != nil {
			t.Fatalf("Match failed with error: %v", err)
		}
		if !pass {
			t.Errorf("Expected Match to pass for matching status code 400, but it failed")
		}
	})

	// I will enforce pass/fail after applying the fix.
	// For now, I just want to run it to see it compiles and runs.
}
