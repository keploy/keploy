package contract

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

// GetVariablesType returns the type of each variable in the object.
func GetVariablesType(obj map[string]interface{}) map[string]map[string]interface{} {
	types := make(map[string]map[string]interface{}, 0)
	// Loop over body object and get the type of each value
	for key, value := range obj {
		var valueType string
		switch value.(type) {
		case float64:
			valueType = "number"
		case int, int32, int64:
			valueType = "integer"
		case string:
			valueType = "string"
		case bool:
			valueType = "boolean"
		case map[string]interface{}:
			valueType = "object"
		case []interface{}:
			valueType = "array"
		default:
			valueType = "string"
		}
		responseType := map[string]interface{}{
			"type": valueType,
		}
		// If the value is an object, recursively get its properties
		if valueType == "object" {
			responseType["properties"] = GetVariablesType(value.(map[string]interface{}))
		}
		// If the value is an array, get the type of the first element
		if valueType == "array" {
			arrayType := "string" // Default to string if array is empty or type can't be determined
			if len(value.([]interface{})) > 0 {
				firstElement := value.([]interface{})[0]
				switch firstElement.(type) {
				case float64:
					arrayType = "number"
				case int, int32, int64:
					arrayType = "integer"
				case string:
					arrayType = "string"
				case bool:
					arrayType = "boolean"
				case map[string]interface{}:
					arrayType = "object"
					responseType["items"] = map[string]interface{}{
						"type":       arrayType,
						"properties": GetVariablesType(firstElement.(map[string]interface{})),
					}
					continue
				default:
					arrayType = "string"
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

func UnmarshalAndConvertToJSON(bodyStr []byte, bodyObj map[string]interface{}) ([]byte, map[string]interface{}, error) {
	err := json.Unmarshal(bodyStr, &bodyObj)
	if err != nil {
		return nil, nil, err
	}
	// Convert the response body object back to a JSON string
	bodyJSON, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, nil, err
	}
	return bodyJSON, bodyObj, nil
}

func GenerateRepsonse(responseCode int, responseMessage string, responseTypes map[string]map[string]interface{}, responseBody map[string]interface{}) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", responseCode): {
			Description: responseMessage,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{
						Type:       "object",
						Properties: responseTypes,
					},
					Example: (responseBody),
				},
			},
		},
	}
	return byCode
}

func ExtractURLPath(fullURL string) (string, string) {
	parsedURL, err := url.Parse(fullURL)

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

func GenerateInPathParams(params map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range params {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "path",
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

// ExtractIdentifiersAndCount extracts numeric identifiers (integers or floats) from the path.
func ExtractIdentifiersAndCount(path string) ([]string, int) {
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

	return identifiers, len(identifiers)
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
func AppendInParameters(parameters []models.Parameter, inParameters map[string]string, count int, paramType string) []models.Parameter {
	if count == 0 {
		return parameters
	}
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
