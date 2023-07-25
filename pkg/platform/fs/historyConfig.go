package fs

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

type HistCfg struct {
	TcPath   string              `json:"tc_path" yaml:"tc_path"`
	MockPath string              `json:"mock_path" yaml:"mock_path"`
	AppCmd  string              `json:"app_cmd" yaml:"app_cmd"`
	AppContainter string         `json:"app_container" yaml:"app_container"`
	NetworkName string           `json:"network_name" yaml:"network_name"`
	Delay   uint64              `json:"delay" yaml:"delay"`
	TestRuns map [string][]string `json:"test_runs" yaml:"test_runs"`
}

func NewHistCfgFS() *HistCfg {
	return &HistCfg{
		TcPath:   "",
		MockPath: "",
		AppCmd:  "",
		AppContainter: "",
		NetworkName: "",
		Delay:   0,
		TestRuns: map [string][]string{},
	}
}

func (hc *HistCfg) 	CaptureTestsEvent(tc_path, mock_path, app_cmd, appContainer, networkName string, delay uint64, testReportPath string, testReportNames []string) error {
	HistCfg := HistCfg{
		TcPath:   tc_path,
		MockPath: mock_path,
		AppCmd:  app_cmd,
		AppContainter: appContainer,
		NetworkName: networkName,
		Delay:   delay,
	}
	for _, v := range testReportNames {
		HistCfg.TestRuns = map[string][]string{testReportPath: {v},
	}
	}
	err := SetHistory(&HistCfg)
	if err != nil {
		return err
	}
	return nil
}

// Todo : optimize this function
func (hc *HistCfg) CapturedRecordEvents(tc_path, mock_path, app_cmd, appContainer, networkName string, delay uint64) error {
	HistCfg := HistCfg{
		TcPath:   tc_path,
		MockPath: mock_path,
		AppCmd:  app_cmd,
		AppContainter: appContainer,
		NetworkName: networkName,
		Delay:   delay,
	}
	err := SetHistory(&HistCfg)
	if err != nil {
		return err
	}
	return nil
}

func SetHistory(hc *HistCfg) error {
	currentHistory := make(map[string][]HistCfg)
	currentHistory["histCfg"] = append(currentHistory["histCfg"], *hc)

	path := UserHomeDir(true)
	fileName := "histCfg.yaml"
	filePath := filepath.Join(path, fileName)

	// Check if the file exists; if not, create it
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		_, err := CreateMockFile(path, "histCfg")
		if err != nil {
			return fmt.Errorf("failed to create file %s. error: %s", fileName, err.Error())
		}
	}

	// Read the existing content of the file
	exstingData, err := os.ReadFile(filePath)
	if len(exstingData) == 0 {
		Write(filePath, currentHistory)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read existing content from yaml file. error: %s", err.Error())
	}
	totalHist, err := ParseBytes(exstingData, currentHistory)
	if err != nil {
		return fmt.Errorf("failed to parse bytes. error: %s", err.Error())
	}
	Write(filePath, totalHist)

	return nil
}

func CreateMockFile(path string, fileName string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("Path does not exist: %s\n", path)
		} else {
			fmt.Printf("Error occurred while checking path: %s\n", err)
		}
	}
	if _, err := os.Stat(filepath.Join(path, fileName+".yaml")); err != nil {
		err := os.MkdirAll(filepath.Join(path), os.ModePerm)
		if err != nil {
			return false, fmt.Errorf("failed to create a mock dir. error: %v", err.Error())
		}
		_, err = os.Create(filepath.Join(path, fileName+".yaml"))
		if err != nil {
			return false, fmt.Errorf("failed to create a yaml file. error: %v", err.Error())
		}
		return true, nil
	}
	return false, nil
}

// UI can be rendered by fetching this method
func (hc *HistCfg) GetHistory() error {
	var (
		path    = UserHomeDir(true)
		history map[string][]HistCfg
	)

	file, err := os.OpenFile(filepath.Join(path, "histCfg.yaml"), os.O_RDONLY, os.ModePerm)
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&history)
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to decode the HistCfg yaml. error: %v", err.Error())
	}
	return nil
}

func Write(filePath string, data map[string][]HistCfg) error {
	d, err := yaml.Marshal(&data)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	err = os.WriteFile(filePath, d, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write histCfg in yaml file. Please check the Unix permissions error: %s", err.Error())
	}
	return nil
}

func ParseBytes(data []byte, hc map[string][]HistCfg) (map[string][]HistCfg, error) {

	var exstingData map[string][]HistCfg
	err := yaml.Unmarshal(data, &exstingData)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing content from yaml file. error: %s", err.Error())
	}

	if err != nil {
		return nil, fmt.Errorf("failed to Unmarshal document to yaml. error: %s", err.Error())
	}

	var prev = exstingData["histCfg"]
	var current = hc["histCfg"][0]
	var flag = false
	for i, v := range prev {
		if v.TcPath == current.TcPath && v.MockPath == current.MockPath {

			// iterate over all testrun path
			for j := range prev[i].TestRuns {
				if _, ok := current.TestRuns[j]; ok {
					n := len(prev[i].TestRuns[j])
					if prev[i].TestRuns[j][n] != current.TestRuns[j][0] {
						fmt.Println("first", prev[i].TestRuns[j][n] )
						fmt.Println("second", current.TestRuns[j][0])
						prev[i].TestRuns[j] = append(prev[i].TestRuns[j], current.TestRuns[j]...)
					}
				}
			}
			//for appending after record for the first time
			if len(prev[i].TestRuns) == 0 {
				prev[i].TestRuns = current.TestRuns
			}
			flag = true
			break
		}
	}
	if !flag {
		prev = append(prev, current)
	}

	exstingData["histCfg"] = prev
	return exstingData, nil
}