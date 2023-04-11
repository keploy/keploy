package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	// "sync"
	"bytes"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
	"io"
)

type HistoryConfig struct {
	// app_path        string       `json:"app_path" yaml:"app_path"`
	TestCasesPath string       `json:"test_cases_path" yaml:"test_cases_path"`
	MocksPath     string       `json:"mocks_path" yaml:"mocks_path"`
	TestRuns      []AppTestRun `json:"test_runs" yaml:"test_runs"`
	// m               sync.Mutex
}
type AppTestRun struct {
	TestRunPath string `json:"test_run_path" yaml:"test_run_path"`
	TestRunId   string `json:"test_run_id" yaml:"test_run_id"`
	Error       string `json:"error" yaml:"error"`
}

func NewHistoryConfigFS() *HistoryConfig {
	return &HistoryConfig{
		TestCasesPath: "",
		MocksPath:     "",
		TestRuns:      []AppTestRun{},
		// m:               sync.Mutex{},
	}
}

func (hc *HistoryConfig) CaptureTestsEvent(test_cases_path, mocks_path, test_run_path, test_run_id string) error {
	historyConfig := HistoryConfig{
		TestCasesPath: test_cases_path,
		MocksPath:     mocks_path,
		// I think this should be map of test_run paths and list of test_run_ids
		TestRuns: []AppTestRun{
			{
				TestRunPath: test_run_path,
				TestRunId:   test_run_id,
				Error:       "",
			},
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

func (hc *HistoryConfig) Read(ctx context.Context, path, name string) (models.TestReport, error) {
	if !pkg.IsValidPath(path) {
		return models.TestReport{}, fmt.Errorf("file path should be absolute. got test report path: %s and its name: %s", pkg.SanitiseInput(path), pkg.SanitiseInput(name))
	}
	if strings.Contains(name, "/") || !pkg.IsValidPath(name) {
		return models.TestReport{}, errors.New("invalid name for test-report. It should not include any slashes")
	}
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return models.TestReport{}, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return models.TestReport{}, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
	}
	return doc, nil
}

func SetHistory(hc *HistoryConfig) error {
	path := UserHomeDir(false)
	fileName := "History-Config.yaml"
	filePath := filepath.Join(path, fileName)

	// Check if the file exists; if not, create it
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		_, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("failed to create file %s. error: %s", fileName, err.Error())
		}
	}

	existingData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read existing content from yaml file. error: %s", err.Error())
	}

	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}

	if len(existingData) > 0 {
		existingData = append(existingData, []byte("---\n")...)
	}

	history, flag := ParseBytes(existingData, hc)
	d := []byte{}
	d, err = yaml.Marshal(&history)
	if flag {
		err = os.WriteFile(filePath, d, os.ModePerm)
		if err != nil {
			return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
		}
		return nil
	}

	// Append the new data to the existing content
	data := append(existingData, d...)

	err = os.WriteFile(filePath, data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
	}
	return nil
}

func ParseBytes(data []byte, hc *HistoryConfig) (HistoryConfig, bool) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc HistoryConfig
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Printf("failed to decode the yaml file documents. error: %v", err.Error())
		}

		
		if doc.TestCasesPath == hc.TestCasesPath{
			fmt.Println("found the same test case path")
			doc.TestRuns = append(hc.TestRuns, doc.TestRuns...)
			return doc, true
		}
	}

	return *hc, false
}

func GetHistory() (*HistoryConfig, error) {
	var (
		path    = UserHomeDir(false)
		history HistoryConfig
	)

	file, err := os.OpenFile(filepath.Join(path, "History-Config.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&history)
	if errors.Is(err, io.EOF) {
		return &history, fmt.Errorf("failed to decode the History-Config yaml. error: %v", err.Error())
	}
	if err != nil {
		return &history, fmt.Errorf("failed to decode the History-Config yaml. error: %v", err.Error())
	}

	return &history, nil
}
