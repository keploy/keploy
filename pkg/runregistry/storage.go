package runregistry

import (
	"encoding/json"
	"os"
	"sync"
)

const registryDir = ".keploy"
const registryFile = ".keploy/runs.json"

var mu sync.Mutex

func loadRuns() ([]TestRun, error) {
	file, err := os.ReadFile(registryFile)

	if os.IsNotExist(err) {
		return []TestRun{}, nil
	}
	if err != nil {
		return nil, err
	}

	var runs []TestRun
	if err := json.Unmarshal(file, &runs); err != nil {
		// Backup corrupted file
		backupPath := registryFile + ".backup"
		_ = os.Rename(registryFile, backupPath)

		// Return empty registry to recover gracefully
		return []TestRun{}, nil
	}

	return runs, nil
}

func saveRuns(runs []TestRun) error {
	data, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(registryDir, os.ModePerm); err != nil {
		return err
	}

	return os.WriteFile(registryFile, data, 0644)
}
