package testsuite

import (
	"fmt"
	"os"

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
