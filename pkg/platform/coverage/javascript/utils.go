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
	walkfn := func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Walk calls the callback with a nil info when it cannot stat an entry; returning the error is what keeps info non-nil below.
			return walkErr
		}
		if info.IsDir() || strings.HasSuffix(path, "index.json") {
			return nil
		}
		fileData, err := os.ReadFile(path)
		if err != nil {
			return nil // skip the unreadable entries
		}
		var processInfo ProcessInfo
		if err := json.Unmarshal(fileData, &processInfo); err != nil {
			return nil // not a processinfo document
		}
		if len(processInfo.Files) > 0 && processInfo.CoverageFilename != "" {
			filePaths = append(filePaths, processInfo.CoverageFilename)
		}
		return nil
	}
	err := filepath.Walk(path, walkfn)
	if err != nil {
		return nil, err
	}
	return filePaths, nil
}
