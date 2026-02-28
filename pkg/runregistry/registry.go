package runregistry

import (
	"errors"
)

func RegisterRun(run TestRun) error {
	mu.Lock()
	defer mu.Unlock()

	runs, err := loadRuns()
	if err != nil {
		return err
	}

	runs = append(runs, run)
	return saveRuns(runs)
}

func ListRuns() ([]TestRun, error) {
	return loadRuns()
}

func GetRun(id string) (*TestRun, error) {
	runs, err := loadRuns()
	if err != nil {
		return nil, err
	}

	for _, r := range runs {
		if r.ID == id {
			return &r, nil
		}
	}

	return nil, errors.New("run not found")
}
