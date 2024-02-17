package Normalise

import (
	"os"

	"gopkg.in/yaml.v2"
)

// getDirectories returns a list of directories in the given path.
func getDirectories(path string) ([]string, error) {
	var dirs []string

	// Open the directory
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	// Read the directory entries
	fileInfos, err := dir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	// Filter directories
	for _, fileInfo := range fileInfos {
		if fileInfo.IsDir() {
			dirs = append(dirs, fileInfo.Name())
		}
	}

	return dirs, nil
}

// mergeYAML merges the existing YAML content with the updated content.
func mergeYAML(existingYAML, updatedYAML []byte) []byte {
	var existingData map[string]interface{}
	if err := yaml.Unmarshal(existingYAML, &existingData); err != nil {
		panic(err)
	}

	var updatedData map[string]interface{}
	if err := yaml.Unmarshal(updatedYAML, &updatedData); err != nil {
		panic(err)
	}

	// Merge the updated data with the existing data
	for key, value := range updatedData {
		existingData[key] = value
	}

	// Marshal the merged data back to YAML
	mergedYAML, err := yaml.Marshal(existingData)
	if err != nil {
		panic(err)
	}

	return mergedYAML
}
