package normalise

import (
	"fmt"
	"os"
)

func getDirectories(path string) ([]string, error) {
	var dirs []string

	// Open the directory
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := dir.Close(); cerr != nil {
			// If there is an error during close, log it or handle it appropriately
			// In this case, you could log the error
			fmt.Printf("Error closing directory: %v", cerr)
		}
	}()

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

func contains(list []string, item string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
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
