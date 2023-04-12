package fs

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

type HistoryConfig struct {
	TestCasesPath string              `json:"test_cases_path" yaml:"test_cases_path"`
	MocksPath     string              `json:"mocks_path" yaml:"mocks_path"`
	TestRuns      map[string][]string `json:"test_runs" yaml:"test_runs"`
}

func NewHistoryConfigFS() *HistoryConfig {
	return &HistoryConfig{
		TestCasesPath: "",
		MocksPath:     "",
		TestRuns:      map[string][]string{},
	}
}

func (hc *HistoryConfig) CaptureTestsEvent(test_cases_path, mocks_path, test_run_path, test_run_id string) error {
	historyConfig := HistoryConfig{
		TestCasesPath: test_cases_path,
		MocksPath:     mocks_path,
		TestRuns: map[string][]string{
			test_run_path: {test_run_id},
		},
	}
	err := SetHistory(&historyConfig)
	if err != nil {
		return err
	}
	return nil
}

func (hc *HistoryConfig) CapturedRecordEvents(test_cases_path, mocks_path string) error {
	historyConfig := HistoryConfig{
		TestCasesPath: test_cases_path,
		MocksPath:     mocks_path,
	}
	SetHistory(&historyConfig)
	return nil
}

func SetHistory(hc *HistoryConfig) error {
	currentHistory := make(map[string][]HistoryConfig)
	currentHistory["historyCfg"] = append(currentHistory["historyCfg"], *hc)
	path := UserHomeDir(false)
	fileName := "historyCfg.yaml"
	filePath := filepath.Join(path, fileName)

	// Check if the file exists; if not, create it
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		_, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("failed to create file %s. error: %s", fileName, err.Error())
		}
	}

	// Read the existing content of the file
	existingData, err := os.ReadFile(filePath)
	if len(existingData) == 0 {
		print("File is empty")
		Write(filePath, currentHistory)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read existing content from yaml file. error: %s", err.Error())
	}

	totalHist, err := ParseBytes(existingData, currentHistory)
	if err != nil {
		return fmt.Errorf("failed to parse bytes. error: %s", err.Error())
	}

	Write(filePath, totalHist)

	return nil
}

// UI can be rendered by fetching this method
func (hc *HistoryConfig) GetHistory() error {
	var (
		path    = UserHomeDir(false)
		history map[string][]HistoryConfig
	)

	file, err := os.OpenFile(filepath.Join(path, "historyCfg.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&history)
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to decode the historyCfg yaml. error: %v", err.Error())
	}
	if err != nil {
		return fmt.Errorf("failed to decode the historyCfg yaml. error: %v", err.Error())
	}
	return nil
}

func Write(filePath string, data map[string][]HistoryConfig) error {
	d, err := yaml.Marshal(&data)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	err = os.WriteFile(filePath, d, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
	}
	return nil
}

func ParseBytes(data []byte, hc map[string][]HistoryConfig) (map[string][]HistoryConfig, error) {
	var existingData map[string][]HistoryConfig
	err := yaml.Unmarshal(data, &existingData)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing content from yaml file. error: %s", err.Error())
	}

	if err != nil {
		return nil, fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	if err != nil {
		fmt.Printf("failed to decode the yaml file documents. error: %v", err.Error())
	}
	var prev = existingData["historyCfg"]
	var current = hc["historyCfg"][0]
	var flag = false
	for i, v := range prev {
		if v.TestCasesPath == current.TestCasesPath && v.MocksPath == current.MocksPath {
			for j := range prev[i].TestRuns {
				// print("found same testrun path .... ")
				prev[i].TestRuns[j] = append(current.TestRuns[j], v.TestRuns[j]...)
			}
			//for appending after record
			if len(prev[i].TestRuns) == 0 {
				print("prev[i].TestRuns is nil")
				prev[i].TestRuns = current.TestRuns
			}
			flag = true
			break
		}
	}
	if !flag {
		prev = append(prev, current)
	}

	existingData["historyCfg"] = prev
	return existingData, nil
}
