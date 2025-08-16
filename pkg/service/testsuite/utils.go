package testsuite

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TSParser parses a YAML file into a TestSuite struct
func TSParser(path string) (TestSuite, error) {
	var ts TestSuite

	fileInfo, err := os.Stat(path)
	if err != nil {
		return ts, fmt.Errorf("error accessing file: %w", err)
	}

	if fileInfo.IsDir() {
		return ts, fmt.Errorf("path is a directory, expected a file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ts, fmt.Errorf("error reading file: %w", err)
	}

	err = yaml.Unmarshal(data, &ts)
	if err != nil {
		return ts, fmt.Errorf("error parsing YAML: %w", err)
	}

	if ts.Kind != "TestSuite" {
		return ts, fmt.Errorf("invalid Kind: expected 'TestSuite', got '%s'", ts.Kind)
	}

	if len(ts.Spec.Steps) == 0 {
		return ts, fmt.Errorf("no test steps found in the test suite")
	}

	return ts, nil
}

// Helper function to extract JSON values using dot notation
func extractJsonValue(data interface{}, path string) (interface{}, error) {
	parts := strings.Split(path, ".")
	current := data

	for _, part := range parts {
		// Check if it's an array index
		if strings.HasSuffix(part, "]") && strings.Contains(part, "[") {
			// Extract the base name and index
			openBracket := strings.Index(part, "[")
			closeBracket := strings.Index(part, "]")

			if openBracket > 0 && closeBracket > openBracket {
				baseName := part[:openBracket]
				indexStr := part[openBracket+1 : closeBracket]
				index := 0

				if _, err := fmt.Sscanf(indexStr, "%d", &index); err != nil {
					return nil, fmt.Errorf("invalid array index %s", indexStr)
				}

				switch v := current.(type) {
				case map[string]interface{}:
					array, ok := v[baseName]
					if !ok {
						return nil, fmt.Errorf("key %s not found in JSON", baseName)
					}

					arraySlice, ok := array.([]interface{})
					if !ok {
						return nil, fmt.Errorf("%s is not an array", baseName)
					}

					if index < 0 || index >= len(arraySlice) {
						return nil, fmt.Errorf("index %d is out of bounds for array %s", index, baseName)
					}

					current = arraySlice[index]
				default:
					return nil, fmt.Errorf("can't access %s in non-object value", part)
				}
			} else {
				return nil, fmt.Errorf("invalid array syntax in path part %s", part)
			}
		} else {
			switch v := current.(type) {
			case map[string]interface{}:
				var ok bool
				current, ok = v[part]
				if !ok {
					return nil, fmt.Errorf("key %s not found in JSON", part)
				}
			default:
				return nil, fmt.Errorf("can't access %s in non-object value", part)
			}
		}
	}

	return current, nil
}
