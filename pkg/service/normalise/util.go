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

type TestCaseFile struct {
	Version string `yaml:"version"`
	Kind    string `yaml:"kind"`
	Name    string `yaml:"name"`
	Spec    struct {
		Metadata struct{} `yaml:"metadata"`
		Req      struct {
			Method     string            `yaml:"method"`
			ProtoMajor int               `yaml:"proto_major"`
			ProtoMinor int               `yaml:"proto_minor"`
			URL        string            `yaml:"url"`
			Header     map[string]string `yaml:"header"`
			Body       string            `yaml:"body"`
			Timestamp  string            `yaml:"timestamp"`
		} `yaml:"req"`
		Resp struct {
			StatusCode    int               `yaml:"status_code"`
			Header        map[string]string `yaml:"header"`
			Body          string            `yaml:"body"`
			StatusMessage string            `yaml:"status_message"`
			ProtoMajor    int               `yaml:"proto_major"`
			ProtoMinor    int               `yaml:"proto_minor"`
			Timestamp     string            `yaml:"timestamp"`
		} `yaml:"resp"`
		Objects    []interface{} `yaml:"objects"`
		Assertions struct {
			Noise map[string]interface{} `yaml:"noise"`
		} `yaml:"assertions"`
		Created int64 `yaml:"created"`
	} `yaml:"spec"`
	Curl string `yaml:"curl"`
}
