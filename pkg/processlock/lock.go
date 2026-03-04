package processlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const lockFileName = "keploy-cli.lock"

type Lock struct {
	file *flock.Flock
}

func Acquire() (*Lock, error) {
	tempDir := os.TempDir()
	lockPath := filepath.Join(tempDir, lockFileName)

	fileLock := flock.New(lockPath)

	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire process lock: %w", err)
	}

	if !locked {
		return nil, errors.New("another instance of keploy CLI is already running")
	}

	return &Lock{file: fileLock}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Unlock()
}
