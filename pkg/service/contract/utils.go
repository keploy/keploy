package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type Response struct {
	Code    int
	Message string
	Types   map[string]map[string]interface{}
	Body    map[string]interface{}
}

// ExtractVariableTypes returns the type of each variable in the object.
func ExtractVariableTypes(obj map[string]interface{}) map[string]map[string]interface{} {
	types := make(map[string]map[string]interface{}, len(obj))

	getType := func(value interface{}) string {
		switch value.(type) {
		case float64:
			return "number"
		case int, int32, int64:
			return "integer"
		case string:
			return "string"
		case bool:
			return "boolean"
		case map[string]interface{}:
			return "object"
		case []interface{}:
			return "array"
		default:
			return "string"
		}
	}

	for key, value := range obj {
		valueType := getType(value)
		responseType := map[string]interface{}{
			"type": valueType,
		}

		switch valueType {
		case "object":
			responseType["properties"] = ExtractVariableTypes(value.(map[string]interface{}))
		case "array":
			arrayItems := value.([]interface{})
			arrayType := "string" // Default to string if array is empty

			if len(arrayItems) > 0 {
				firstElement := arrayItems[0]
				arrayType = getType(firstElement)
				if arrayType == "object" {
					responseType["items"] = map[string]interface{}{
						"type":       arrayType,
						"properties": ExtractVariableTypes(firstElement.(map[string]interface{})),
					}
					types[key] = responseType
					continue
				}
			}
			responseType["items"] = map[string]interface{}{
				"type": arrayType,
			}
		}

		types[key] = responseType
	}

	return types
}

func GenerateResponse(response Response) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", response.Code): {
			Description: response.Message,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{
						Type:       "object",
						Properties: response.Types,
					},
					Example: (response.Body),
				},
			},
		},
	}
	return byCode
}

func ExtractURLPath(URL string) (string, string) {
	parsedURL, err := url.Parse(URL)

	if err != nil {
		return "", ""
	}
	return parsedURL.Path, parsedURL.Host
}

func GenerateHeader(header map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range header {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "header",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

// isNumeric checks if a string is a valid numeric value (integer or float).
func isNumeric(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// ExtractIdentifiers extracts numeric identifiers (integers or floats) from the path.
func ExtractIdentifiers(path string) []string {
	segments := strings.Split(path, "/")
	segments2 := strings.Split(segments[len(segments)-1], "?")
	segments = append(segments[:len(segments)-1], segments2[0])
	var identifiers []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				identifiers = append(identifiers, segment)
			}
		}
	}

	return identifiers
}

// ExtractQueryParams extracts the query parameters and their names from the URL.
func ExtractQueryParams(rawURL string) (map[string]string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	queryParams := parsedURL.Query()
	params := make(map[string]string)
	for key, values := range queryParams {
		if len(values) > 0 {
			// Take the first value if multiple are present
			params[key] = values[0]
		}
	}
	return params, nil
}

// GenerateDummyNamesForIdentifiers generates dummy names for the path identifiers.
func GenerateDummyNamesForIdentifiers(identifiers []string) map[string]string {
	dummyNames := make(map[string]string)
	for i, id := range identifiers {
		dummyName := fmt.Sprintf("param%d", i+1)
		dummyNames[dummyName] = id
	}
	return dummyNames
}
func AppendInParameters(parameters []models.Parameter, inParameters map[string]string, paramType string) []models.Parameter {

	for key, value := range inParameters {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       paramType,
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}

	return parameters
}

// ReplacePathIdentifiers replaces numeric identifiers in the path with their corresponding dummy names.
func ReplacePathIdentifiers(path string, dummyNames map[string]string) string {
	segments := strings.Split(path, "/")
	var replacedPath []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				dummyName := ""
				for key, value := range dummyNames {
					if value == segment {
						// i want to put '{' and '}' around the key
						dummyName = "{" + key + "}"
						break
					}
				}
				if dummyName != "" {
					replacedPath = append(replacedPath, dummyName)
				} else {
					replacedPath = append(replacedPath, segment)
				}
			} else {
				replacedPath = append(replacedPath, segment)
			}
		}
	}
	finalPath := strings.Join(replacedPath, "/")
	// Add slash at the beginning of the path
	finalPath = "/" + finalPath
	return finalPath
}

func generateUniqueID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// handle error
		return ""
	}
	return hex.EncodeToString(b) + "-" + time.Now().Format("20060102150405")
}

func checkConfigFile(servicesMapping map[string][]string) error {
	// Check if the size of servicesMapping is less than 1
	if len(servicesMapping) < 1 {
		return errors.New("services mapping must contain at least 1 services")
	}
	return nil
}

func saveServiceMappings(servicesMapping config.Mappings, filePath string) error {
	// Marshal the services mapping to YAML
	servicesMappingYAML, err := yamlLib.Marshal(servicesMapping)
	if err != nil {
		return fmt.Errorf("failed to marshal services mapping: %w", err)
	}

	// Write the services mapping to the specified file path
	err = yaml.WriteFile(context.Background(), zap.NewNop(), filePath, "serviceMappings", servicesMappingYAML, false)
	if err != nil {
		return fmt.Errorf("failed to write services mapping to file: %w", err)
	}

	return nil
}
