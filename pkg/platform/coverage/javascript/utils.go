package javascript

import (
	"encoding/json"
	"fmt"
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

func CalculateCoverageMetrics(execSegmentCoveredPerFile map[string]map[string]bool) (int, int, map[string]int) {
	totalExecSegments := 0
	totalCoveredExecSegments := 0
	coveredExecSegmentsPerFile := make(map[string]int)
	for filename, execSegment := range execSegmentCoveredPerFile {
		for _, isCovered := range execSegment {
			totalExecSegments++
			if isCovered {
				totalCoveredExecSegments++
				coveredExecSegmentsPerFile[filename]++
			}
		}
	}
	return totalExecSegments, totalCoveredExecSegments, coveredExecSegmentsPerFile
}

func AddCovInfoPerFile(execSegmentCovPerFile map[string]map[string]bool, coverageMap map[string]interface{}, filename string) {
	if _, ok := execSegmentCovPerFile[filename]; !ok {
		execSegmentCovPerFile[filename] = make(map[string]bool)
	}
	for i, isExecSegmentCovered := range coverageMap {
		if _, ok := execSegmentCovPerFile[filename][i]; !ok {
			execSegmentCovPerFile[filename][i] = false
		}
		switch isExecSegmentCov := isExecSegmentCovered.(type) {
		case float64:
			if isExecSegmentCov > 0 {
				execSegmentCovPerFile[filename][i] = true
			}
		case []interface{}:
			for j, covOrNot := range isExecSegmentCov {
				if covOrNot.(float64) > 0 {
					execSegmentCovPerFile[filename][i+"_"+fmt.Sprintf("%v", j)] = true
				}
			}
		default:
			execSegmentCovPerFile[filename][i] = false
		}
	}
}
