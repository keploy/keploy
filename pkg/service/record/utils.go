package record

import (
	"os"
	"path"

	"go.keploy.io/server/v2/pkg/models"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// ReadTestSets reads a YAML file and unmarshals it into a TestCase struct.
func ReadTestCase(filepath string, fileName os.DirEntry) (models.TestCase, error) {
    // Get the path or name from the DirEntry
    filePath := fileName.Name() // This gets just the name, assuming you're in the correct directory
	absPath := path.Join(filepath, filePath) // This gets the full path (directory + name

    // Read file content
    testCaseContent, err := os.ReadFile(absPath) // Use filePath here
    if err != nil {
        // Return an empty TestCase struct and the error
        return models.TestCase{}, err
    }

    // Initialize an instance of TestCase to unmarshal the data into
    var testCase models.TestCase

    // Parse file content into testCase
    err = yaml.Unmarshal(testCaseContent, &testCase)
    if err != nil {
        // Return an empty TestCase struct and the error if parsing fails
        return models.TestCase{}, err
    }

    // Return the populated TestCase struct and nil for the error if parsing succeeds
    return testCase, nil
}