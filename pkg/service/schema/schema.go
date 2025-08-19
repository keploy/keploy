package schema

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// schemaService implements the Service interface
type schemaService struct {
	logger        *zap.Logger
	openAPIDB     OpenAPIDB
	testSetConfig TestSetConfig
	telemetry     Telemetry
	config        *config.Config
}

// New creates a new schema service instance
func New(logger *zap.Logger, openAPIDB OpenAPIDB, testSetConfig TestSetConfig, telemetry Telemetry, config *config.Config) Service {
	return &schemaService{
		logger:        logger,
		openAPIDB:     openAPIDB,
		testSetConfig: testSetConfig,
		telemetry:     telemetry,
		config:        config,
	}
}

// APIEndpoint represents a parsed API endpoint
type APIEndpoint struct {
	Method       string
	Endpoint     string
	ResponseCode int
	ResponseBody string
	RequestBody  string
}

// SchemaAssertionResult represents the result of schema assertion
func (s *schemaService) GenerateSchema(ctx context.Context, filePath string) error {
	s.logger.Info("Starting schema generation", zap.String("filePath", filePath))

	// Parse the API file
	endpoints, err := s.parseAPIFile(filePath)
	if err != nil {
		utils.LogError(s.logger, err, "failed to parse API file")
		return err
	}

	s.logger.Info("Parsed endpoints", zap.Int("count", len(endpoints)))

	// Group endpoints by path and method
	endpointGroups := s.groupEndpoints(endpoints)

	// Generate OpenAPI schemas for each endpoint group
	for key, endpointGroup := range endpointGroups {
		openAPISchema, err := s.generateOpenAPISchema(endpointGroup)
		if err != nil {
			utils.LogError(s.logger, err, "failed to generate OpenAPI schema", zap.String("endpoint", key))
			continue
		}

		// Save schema to file
		schemaDir := filepath.Join(s.config.Path, "api-schema")
		schemaName := s.sanitizeFilename(key)

		err = s.openAPIDB.WriteSchema(ctx, s.logger, schemaDir, schemaName, *openAPISchema, false)
		if err != nil {
			utils.LogError(s.logger, err, "failed to write schema to file", zap.String("endpoint", key))
			continue
		}

		s.logger.Info("Generated schema for endpoint", zap.String("endpoint", key), zap.String("file", schemaName+".yaml"))
	}

	telemetryData := &sync.Map{}
	telemetryData.Store("endpoint_count", len(endpoints))
	telemetryData.Store("file_path", filePath)
	s.telemetry.SendTelemetry("schema_generated", telemetryData)

	s.logger.Info("Schema generation completed successfully")
	return nil
}

// parseAPIFile parses the input file and extracts API endpoints
func (s *schemaService) parseAPIFile(filePath string) ([]APIEndpoint, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var endpoints []APIEndpoint
	var currentEndpoint APIEndpoint
	var inResponseBody bool
	var responseBodyLines []string

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip separator lines
		if strings.HasPrefix(line, "----") {
			if inResponseBody && len(responseBodyLines) > 0 {
				currentEndpoint.ResponseBody = strings.Join(responseBodyLines, "\n")
				endpoints = append(endpoints, currentEndpoint)
				responseBodyLines = []string{}
				inResponseBody = false
			}
			continue
		}

		// Parse method
		if strings.HasPrefix(line, "Method:") {
			currentEndpoint = APIEndpoint{}
			currentEndpoint.Method = strings.TrimSpace(strings.TrimPrefix(line, "Method:"))
		}

		// Parse endpoint
		if strings.HasPrefix(line, "Endpoint:") {
			currentEndpoint.Endpoint = strings.TrimSpace(strings.TrimPrefix(line, "Endpoint:"))
		}

		// Parse response code
		if strings.HasPrefix(line, "Response Code:") {
			codeStr := strings.TrimSpace(strings.TrimPrefix(line, "Response Code:"))
			if code, err := strconv.Atoi(codeStr); err == nil {
				currentEndpoint.ResponseCode = code
			}
		}

		// Parse response body
		if strings.HasPrefix(line, "Response Body:") {
			inResponseBody = true
			continue
		}

		// Collect response body lines
		if inResponseBody && line != "" {
			responseBodyLines = append(responseBodyLines, line)
		}
	}

	// Handle last endpoint if file doesn't end with separator
	if inResponseBody && len(responseBodyLines) > 0 {
		currentEndpoint.ResponseBody = strings.Join(responseBodyLines, "\n")
		endpoints = append(endpoints, currentEndpoint)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	return endpoints, nil
}

// groupEndpoints groups endpoints by method and path
func (s *schemaService) groupEndpoints(endpoints []APIEndpoint) map[string][]APIEndpoint {
	groups := make(map[string][]APIEndpoint)

	for _, endpoint := range endpoints {
		key := fmt.Sprintf("%s_%s", endpoint.Method, endpoint.Endpoint)
		groups[key] = append(groups[key], endpoint)
	}

	return groups
}

// generateOpenAPISchema generates OpenAPI schema from endpoints
func (s *schemaService) generateOpenAPISchema(endpoints []APIEndpoint) (*models.OpenAPI, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints provided")
	}

	// Use the first endpoint as the base
	baseEndpoint := endpoints[0]

	openAPI := &models.OpenAPI{
		OpenAPI: "3.0.0",
		Info: models.Info{
			Title:       "Generated API Schema",
			Version:     "1.0.0",
			Description: "Auto-generated schema from API responses",
		},
		Servers: []map[string]string{
			{"url": "http://localhost:8080"},
		},
		Paths:      make(map[string]models.PathItem),
		Components: make(map[string]interface{}),
	}

	// Create path item
	pathItem := models.PathItem{}

	// Parse response body to generate schema
	responseSchema, err := s.generateSchemaFromJSON(baseEndpoint.ResponseBody)
	if err != nil {
		s.logger.Warn("failed to parse response body as JSON, using raw schema", zap.String("endpoint", baseEndpoint.Endpoint))
		responseSchema = models.Schema{
			Type: "object",
			Properties: map[string]map[string]interface{}{
				"data": {
					"type":        "string",
					"description": "Raw response data",
				},
			},
		}
	}

	// Create operation
	operation := &models.Operation{
		Summary:     fmt.Sprintf("%s %s", baseEndpoint.Method, baseEndpoint.Endpoint),
		Description: fmt.Sprintf("Auto-generated operation for %s %s", baseEndpoint.Method, baseEndpoint.Endpoint),
		Parameters:  []models.Parameter{},
		Responses: map[string]models.ResponseItem{
			strconv.Itoa(baseEndpoint.ResponseCode): {
				Description: "Successful response",
				Content: map[string]models.MediaType{
					"application/json": {
						Schema:  responseSchema,
						Example: s.parseJSONExample(baseEndpoint.ResponseBody),
					},
				},
			},
		},
	}

	// Set operation based on method
	switch strings.ToUpper(baseEndpoint.Method) {
	case "GET":
		pathItem.Get = operation
	case "POST":
		pathItem.Post = operation
	case "PUT":
		pathItem.Put = operation
	case "DELETE":
		pathItem.Delete = operation
	case "PATCH":
		pathItem.Patch = operation
	}

	openAPI.Paths[baseEndpoint.Endpoint] = pathItem

	return openAPI, nil
}

// generateSchemaFromJSON creates a schema from JSON response
func (s *schemaService) generateSchemaFromJSON(jsonStr string) (models.Schema, error) {
	var data interface{}
	err := json.Unmarshal([]byte(jsonStr), &data)
	if err != nil {
		return models.Schema{}, err
	}

	return s.createSchemaFromValue(data), nil
}

// createSchemaFromValue recursively creates schema from Go values
func (s *schemaService) createSchemaFromValue(value interface{}) models.Schema {
	schema := models.Schema{
		Properties: make(map[string]map[string]interface{}),
	}

	switch v := value.(type) {
	case map[string]interface{}:
		schema.Type = "object"
		for key, val := range v {
			propertySchema := s.createSchemaFromValue(val)
			schema.Properties[key] = map[string]interface{}{
				"type": propertySchema.Type,
			}
			if len(propertySchema.Properties) > 0 {
				schema.Properties[key]["properties"] = propertySchema.Properties
			}
		}
	case []interface{}:
		schema.Type = "array"
		if len(v) > 0 {
			itemSchema := s.createSchemaFromValue(v[0])
			schema.Properties["items"] = map[string]interface{}{
				"type": itemSchema.Type,
			}
			if len(itemSchema.Properties) > 0 {
				schema.Properties["items"]["properties"] = itemSchema.Properties
			}
		}
	case string:
		schema.Type = "string"
	case float64:
		schema.Type = "number"
	case bool:
		schema.Type = "boolean"
	case nil:
		schema.Type = "null"
	default:
		schema.Type = "string" // fallback
	}

	return schema
}

// parseJSONExample safely parses JSON example
func (s *schemaService) parseJSONExample(jsonStr string) map[string]interface{} {
	var example map[string]interface{}
	err := json.Unmarshal([]byte(jsonStr), &example)
	if err != nil {
		// Return a simple example if parsing fails
		return map[string]interface{}{
			"data": jsonStr,
		}
	}
	return example
}

// sanitizeFilename creates a safe filename from endpoint info
func (s *schemaService) sanitizeFilename(input string) string {
	// Replace special characters with underscores
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	return reg.ReplaceAllString(input, "_")
}

// AssertSchema validates requests/responses against stored schemas
func (s *schemaService) AssertSchema(ctx context.Context, filePath string) (*models.SchemaAssertionResult, error) {
	s.logger.Info("Starting schema assertion", zap.String("filePath", filePath))

	// Parse the input file to get endpoints to validate
	endpoints, err := s.parseAPIFile(filePath)
	if err != nil {
		utils.LogError(s.logger, err, "failed to parse API file for assertion")
		return nil, err
	}

	result := &models.SchemaAssertionResult{
		TotalEndpoints: len(endpoints),
		PassedCount:    0,
		FailedCount:    0,
		Errors:         []models.SchemaError{},
	}

	// Load existing schemas
	configPath := s.config.Path
	if configPath == "" {
		// If config path is empty, use current directory with keploy subdirectory
		configPath = "./keploy"
	}
	schemaDir := filepath.Join(configPath, "api-schema")

	for _, endpoint := range endpoints {
		err := s.assertEndpoint(ctx, endpoint, schemaDir, result)
		if err != nil {
			s.logger.Warn("assertion failed for endpoint",
				zap.String("method", endpoint.Method),
				zap.String("endpoint", endpoint.Endpoint),
				zap.Error(err))
		}
	}

	telemetryData := &sync.Map{}
	telemetryData.Store("total_endpoints", result.TotalEndpoints)
	telemetryData.Store("passed_count", result.PassedCount)
	telemetryData.Store("failed_count", result.FailedCount)
	s.telemetry.SendTelemetry("schema_asserted", telemetryData)

	s.logger.Info("Schema assertion completed",
		zap.Int("total", result.TotalEndpoints),
		zap.Int("passed", result.PassedCount),
		zap.Int("failed", result.FailedCount))

	return result, nil
}

// assertEndpoint validates a single endpoint against stored schema
func (s *schemaService) assertEndpoint(ctx context.Context, endpoint APIEndpoint, schemaDir string, result *models.SchemaAssertionResult) error {
	// Find corresponding schema file
	key := fmt.Sprintf("%s_%s", endpoint.Method, endpoint.Endpoint)
	schemaName := s.sanitizeFilename(key)

	// Load schema from file directly
	schema, err := s.loadSchemaFromFile(ctx, schemaDir, schemaName)
	if err != nil {
		result.FailedCount++
		result.Errors = append(result.Errors, models.SchemaError{
			Endpoint: endpoint.Endpoint,
			Method:   endpoint.Method,
			Error:    fmt.Sprintf("Schema not found for %s %s", endpoint.Method, endpoint.Endpoint),
		})
		return fmt.Errorf("schema not found")
	}

	// Parse actual response
	var actualResponse interface{}
	err = json.Unmarshal([]byte(endpoint.ResponseBody), &actualResponse)
	if err != nil {
		result.FailedCount++
		result.Errors = append(result.Errors, models.SchemaError{
			Endpoint: endpoint.Endpoint,
			Method:   endpoint.Method,
			Error:    fmt.Sprintf("Invalid JSON in response: %v", err),
		})
		return err
	}

	// Validate against schema (simplified validation)
	validationErrors := s.validateAgainstSchema(actualResponse, schema, endpoint)

	if len(validationErrors) > 0 {
		result.FailedCount++
		result.Errors = append(result.Errors, validationErrors...)
	} else {
		result.PassedCount++
	}

	return nil
}

// loadSchemaFromFile loads a schema file from disk
func (s *schemaService) loadSchemaFromFile(ctx context.Context, schemaDir, schemaName string) (*models.OpenAPI, error) {
	schemaPath := filepath.Join(schemaDir, schemaName+".yaml")

	// Check if file exists
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("schema file not found: %s", schemaPath)
	}

	// Read file content
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema file: %w", err)
	}

	// Parse YAML
	var schema models.OpenAPI
	err = yaml.Unmarshal(data, &schema)
	if err != nil {
		return nil, fmt.Errorf("failed to parse schema YAML: %w", err)
	}

	return &schema, nil
}

// validateAgainstSchema performs basic schema validation
func (s *schemaService) validateAgainstSchema(data interface{}, schema *models.OpenAPI, endpoint APIEndpoint) []models.SchemaError {
	var errors []models.SchemaError

	// Get the path and operation from schema
	pathItem, exists := schema.Paths[endpoint.Endpoint]
	if !exists {
		errors = append(errors, models.SchemaError{
			Endpoint: endpoint.Endpoint,
			Method:   endpoint.Method,
			Error:    "Endpoint not found in schema",
		})
		return errors
	}

	var operation *models.Operation
	switch strings.ToUpper(endpoint.Method) {
	case "GET":
		operation = pathItem.Get
	case "POST":
		operation = pathItem.Post
	case "PUT":
		operation = pathItem.Put
	case "DELETE":
		operation = pathItem.Delete
	case "PATCH":
		operation = pathItem.Patch
	}

	if operation == nil {
		errors = append(errors, models.SchemaError{
			Endpoint: endpoint.Endpoint,
			Method:   endpoint.Method,
			Error:    fmt.Sprintf("Method %s not found in schema", endpoint.Method),
		})
		return errors
	}

	// Validate response structure (simplified)
	responseSchema, exists := operation.Responses[strconv.Itoa(endpoint.ResponseCode)]
	if !exists {
		errors = append(errors, models.SchemaError{
			Endpoint: endpoint.Endpoint,
			Method:   endpoint.Method,
			Error:    fmt.Sprintf("Response code %d not found in schema", endpoint.ResponseCode),
		})
		return errors
	}

	// Basic type validation
	for contentType, mediaType := range responseSchema.Content {
		if contentType == "application/json" {
			if dataMap, ok := data.(map[string]interface{}); ok && mediaType.Schema.Type == "object" {
				// Validate object response
				schemaErrors := s.validateObjectAgainstSchema(dataMap, mediaType.Schema, endpoint)
				errors = append(errors, schemaErrors...)
			} else if dataArray, ok := data.([]interface{}); ok && mediaType.Schema.Type == "array" {
				// Validate array response
				schemaErrors := s.validateArrayResponse(dataArray, mediaType.Schema, endpoint)
				errors = append(errors, schemaErrors...)
			}
		}
	}

	return errors
}

// validateArrayResponse validates array responses
func (s *schemaService) validateArrayResponse(data []interface{}, schema models.Schema, endpoint APIEndpoint) []models.SchemaError {
	var errors []models.SchemaError

	// Get the items schema for array elements
	if itemsProperty, hasItems := schema.Properties["items"]; hasItems {
		if itemsSchema, ok := itemsProperty["properties"].(map[string]interface{}); ok {
			// Validate each array item
			for i, item := range data {
				if itemObj, ok := item.(map[string]interface{}); ok {
					itemErrors := s.validateArrayItem(itemObj, itemsSchema, endpoint, "array", i)
					errors = append(errors, itemErrors...)
				}
			}
		}
	}

	return errors
}

// validateObjectAgainstSchema validates object properties
func (s *schemaService) validateObjectAgainstSchema(data map[string]interface{}, schema models.Schema, endpoint APIEndpoint) []models.SchemaError {
	var errors []models.SchemaError

	// Check if all schema properties exist in data and types match
	for property, propSchema := range schema.Properties {
		value, exists := data[property]
		if !exists {
			errors = append(errors, models.SchemaError{
				Endpoint: endpoint.Endpoint,
				Method:   endpoint.Method,
				Error:    fmt.Sprintf("Missing required property '%s'", property),
			})
			continue
		}

		expectedType, ok := propSchema["type"].(string)
		if !ok {
			continue
		}

		actualType := s.getJSONType(value)
		if actualType != expectedType {
			errors = append(errors, models.SchemaError{
				Endpoint: endpoint.Endpoint,
				Method:   endpoint.Method,
				Error:    fmt.Sprintf("Property '%s' type mismatch: expected %s, got %s", property, expectedType, actualType),
			})
		}

		// If it's an array, validate the items
		if actualType == "array" && expectedType == "array" {
			if arrayData, ok := value.([]interface{}); ok && len(arrayData) > 0 {
				// Get the items schema from the property schema
				if itemsSchema, hasItems := propSchema["properties"]; hasItems {
					if itemsMap, ok := itemsSchema.(map[string]interface{}); ok {
						// Validate each array item
						for i, item := range arrayData {
							if itemObj, ok := item.(map[string]interface{}); ok {
								itemErrors := s.validateArrayItem(itemObj, itemsMap, endpoint, property, i)
								errors = append(errors, itemErrors...)
							}
						}
					}
				}
			}
		}
	}

	// Check for extra properties in data that don't exist in schema
	for property := range data {
		if _, exists := schema.Properties[property]; !exists {
			errors = append(errors, models.SchemaError{
				Endpoint: endpoint.Endpoint,
				Method:   endpoint.Method,
				Error:    fmt.Sprintf("Unexpected property '%s' not defined in schema", property),
			})
		}
	}

	return errors
}

// validateArrayItem validates individual array items
func (s *schemaService) validateArrayItem(item map[string]interface{}, itemSchema map[string]interface{}, endpoint APIEndpoint, arrayProperty string, index int) []models.SchemaError {
	var errors []models.SchemaError

	// Check each property in the item schema
	for property, propDef := range itemSchema {
		if propDefMap, ok := propDef.(map[string]interface{}); ok {
			if expectedType, ok := propDefMap["type"].(string); ok {
				value, exists := item[property]
				if !exists {
					errors = append(errors, models.SchemaError{
						Endpoint: endpoint.Endpoint,
						Method:   endpoint.Method,
						Error:    fmt.Sprintf("Array item %d in '%s' missing property '%s'", index, arrayProperty, property),
					})
					continue
				}

				actualType := s.getJSONType(value)
				if actualType != expectedType {
					errors = append(errors, models.SchemaError{
						Endpoint: endpoint.Endpoint,
						Method:   endpoint.Method,
						Error:    fmt.Sprintf("Array item %d in '%s' property '%s' type mismatch: expected %s, got %s", index, arrayProperty, property, expectedType, actualType),
					})
				}
			}
		}
	}

	// Check for extra properties in array item
	for property := range item {
		if _, exists := itemSchema[property]; !exists {
			errors = append(errors, models.SchemaError{
				Endpoint: endpoint.Endpoint,
				Method:   endpoint.Method,
				Error:    fmt.Sprintf("Array item %d in '%s' has unexpected property '%s'", index, arrayProperty, property),
			})
		}
	}

	return errors
}

// getJSONType returns the JSON type of a Go value
func (s *schemaService) getJSONType(value interface{}) string {
	switch value.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case nil:
		return "null"
	default:
		return reflect.TypeOf(value).String()
	}
}
