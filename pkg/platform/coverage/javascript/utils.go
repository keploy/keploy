package javascript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type ProcessInfo struct {
	Parent           string   `json:"parent"`
	Pid              int      `json:"pid"`
	Argv             []string `json:"argv"`
	ExecArgv         []string `json:"execArgv"`
	Cwd              string   `json:"cwd"`
	Time             int      `json:"time"`
	Ppid             int      `json:"ppid"`
	CoverageFilename string   `json:"coverageFilename"`
	ExternalID       string   `json:"externalId"`
	UUID             string   `json:"uuid"`
	Files            []string `json:"files"`
}

func getCoverageFilePathsJavascript(path string) ([]string, error) {
	filePaths := []string{}
	walkfn := func(path string, info os.FileInfo, _ error) error {
		if !info.IsDir() && !strings.HasSuffix(path, "index.json") {
			fileData, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var processInfo ProcessInfo
			err = json.Unmarshal(fileData, &processInfo)
			if err != nil {
				return err
			}
			if len(processInfo.Files) > 0 {
				filePaths = append(filePaths, processInfo.CoverageFilename)
			}
		}
		return nil
	}
	err := filepath.Walk(path, walkfn)
	if err != nil {
		return nil, err
	}
	return filePaths, nil
}

func CalculateCoverageMetrics(execSegmentCoveredPerFile map[string][]int) (int, int) {
	totalExecSegments := 0
	totalCoveredExecSegments := 0
	for _, coveredSlice := range execSegmentCoveredPerFile {
		totalExecSegments += coveredSlice[0] + coveredSlice[1]
		totalCoveredExecSegments += coveredSlice[0]
	}
	return totalExecSegments, totalCoveredExecSegments
}

func AddCovInfoPerFile(execSegmentCovPerFile map[string][]int, coverageMap map[string]interface{}, filename string) {
	if _, ok := execSegmentCovPerFile[filename]; !ok {
		execSegmentCovPerFile[filename] = make([]int, 2)
	}
	for _, isExecSegmentCovered := range coverageMap {
		switch isExecSegmentCovered.(type) {
		case float64:
			if isExecSegmentCovered.(float64) > 0 {
				execSegmentCovPerFile[filename][0]++
			} else {
				execSegmentCovPerFile[filename][1]++
			}
		case []interface{}:
			for _, covOrNot := range isExecSegmentCovered.([]interface{}) {
				if covOrNot.(float64) > 0 {
					execSegmentCovPerFile[filename][0]++
				} else {
					execSegmentCovPerFile[filename][1]++
				}
			}
		}
	}
}
