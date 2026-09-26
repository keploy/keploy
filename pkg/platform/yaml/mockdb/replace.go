package mockdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// ErrGobFormatUnsupported is returned by ReplaceMocks for test sets stored in
// the gob (low-latency record) format. Splicing gob sets is not implemented:
// the format has no whole-file rewrite primitive, and the local record/replay
// flow that needs splicing always writes yaml (or json). Returning an explicit
// error is deliberate — silently falling back to a yaml write would leave the
// set with two mock files of different formats, and the reader prefers gob, so
// the write would appear to succeed while changing nothing.
var ErrGobFormatUnsupported = errors.New("mockdb: ReplaceMocks does not support the gob mock format")

// mockFileName resolves the on-disk base name for this instance's mocks file.
func (ys *MockYaml) mockFileName() string {
	if ys.MockName != "" {
		return ys.MockName
	}
	return "mocks"
}

// GetAllMocks returns every mock in the test set, in the order they appear in
// the file, with no filtering of any kind.
//
// It exists because neither GetFilteredMocks nor GetUnFilteredMocks reads the
// whole file: together they form a replay-time PARTITION (per-test pool vs
// cross-test pool), and each returns only its half. Code that edits the file —
// as opposed to feeding the replayer — must see every mock, or writing back
// what it read silently deletes the other half.
//
// Order is file order, deliberately. The two partition readers sort by
// Spec.ReqTimestampMock, so a read-modify-write built on them reshuffles the
// whole file even when nothing changed. Callers that want a minimal diff need
// the original order preserved.
//
// A missing mocks file is not an error: it returns (nil, nil), matching how the
// partition readers treat an absent file.
func (ys *MockYaml) GetAllMocks(ctx context.Context, testSetID string) ([]*models.Mock, error) {
	path := filepath.Join(ys.MockPath, testSetID)
	fileName := ys.mockFileName()

	lock := getMockFileLock(mockFileLockKey(path, fileName, ys.Format))
	lock.RLock()
	defer lock.RUnlock()

	return ys.readAllMocksLocked(ctx, path, fileName)
}

// readAllMocksLocked is the unlocked body of GetAllMocks.
//
// The caller MUST already hold the stripe lock for (path, fileName, format) —
// read lock for a pure read, write lock when it will write back. The stripe
// locks are plain *sync.RWMutex and are therefore NOT reentrant, so ReplaceMocks
// cannot call GetAllMocks while holding the write lock; it calls this instead.
func (ys *MockYaml) readAllMocksLocked(ctx context.Context, path, fileName string) ([]*models.Mock, error) {
	// Prefer gob when present. Mirrors GetFilteredMocks: gob is mutually
	// exclusive with the text formats, so this short-circuits before the
	// auto-detecting text reader runs.
	gobPath := filepath.Join(path, fileName+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		return readGobMocks(gobPath)
	}

	reader, err := yaml.NewMockReaderAny(ctx, ys.Logger, path, fileName, ys.Format)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		utils.LogError(ys.Logger, err, "failed to read the mocks from file",
			zap.String("session", filepath.Base(path)))
		return nil, err
	}
	defer func() {
		if cErr := reader.Close(); cErr != nil {
			ys.Logger.Debug("failed to close the mock reader", zap.Error(cErr))
		}
	}()

	isJSON := reader.Format() == yaml.FormatJSON

	var all []*models.Mock
	for {
		var decoded []*models.Mock

		if isJSON {
			doc, err := reader.ReadNextDocJSON()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the mock file documents: %w", err)
			}
			decoded, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the mocks from json doc",
					zap.String("session", filepath.Base(path)))
				return nil, err
			}
		} else {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the mock file documents: %w", err)
			}
			decoded, err = DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the mocks from doc",
					zap.String("session", filepath.Base(path)))
				return nil, err
			}
		}

		all = append(all, decoded...)
	}

	return all, nil
}

// ReplaceMocks rewrites a test set's mocks file, removing the mocks named in
// drop and inserting those in add, leaving every other mock untouched and in
// its original position.
//
// This is the storage primitive behind replacing a single recorded test case:
// only that case's mocks change, so the resulting git diff shows the real
// change rather than a reshuffled file.
//
// Behaviour:
//
//   - Order. Kept mocks stay in file order. The added mocks are inserted at the
//     position of the first dropped mock, so a replacement lands where the
//     original was; with nothing dropped they are appended.
//   - Validation. Every name in drop must exist, and no name in add may collide
//     with a mock that survives. Both are caller bugs — a stale view of the file
//     — and are reported rather than silently absorbed.
//   - No-op. Empty drop and empty add returns without touching the file, so a
//     caller that finds nothing to do cannot churn it.
//   - Emptying. Dropping every mock with nothing to add removes the file, which
//     is writeMocksAtomically's existing contract for an empty set.
//
// It takes the write side of the same stripe lock the readers use, so a
// concurrent read never observes a half-written file; the write itself is
// atomic (temp file + rename).
func (ys *MockYaml) ReplaceMocks(ctx context.Context, testSetID string, drop []string, add []*models.Mock) error {
	if len(drop) == 0 && len(add) == 0 {
		return nil
	}

	path := filepath.Join(ys.MockPath, testSetID)
	fileName := ys.mockFileName()

	lock := getMockFileLock(mockFileLockKey(path, fileName, ys.Format))
	lock.Lock()
	defer lock.Unlock()

	// Reject gob up front, before any work: the reader prefers gob over the
	// text formats, so writing yaml beside an existing gob file would look
	// like a successful no-op to every subsequent read.
	if _, err := os.Stat(filepath.Join(path, fileName+".gob")); err == nil {
		return ErrGobFormatUnsupported
	} else if !os.IsNotExist(err) {
		return err
	}
	if ys.useGobFormat() {
		return ErrGobFormatUnsupported
	}

	existing, err := ys.readAllMocksLocked(ctx, path, fileName)
	if err != nil {
		return err
	}

	dropSet := make(map[string]bool, len(drop))
	for _, name := range drop {
		dropSet[name] = true
	}

	kept := make([]*models.Mock, 0, len(existing))
	insertAt := -1
	seenDropped := make(map[string]bool, len(drop))

	for _, mock := range existing {
		if dropSet[mock.Name] {
			seenDropped[mock.Name] = true
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		kept = append(kept, mock)
	}

	if missing := missingNames(drop, seenDropped); len(missing) > 0 {
		return fmt.Errorf("mockdb: cannot drop mocks not present in test set %q: %v", testSetID, missing)
	}

	keptNames := make(map[string]bool, len(kept))
	for _, mock := range kept {
		keptNames[mock.Name] = true
	}
	var collisions []string
	for _, mock := range add {
		if mock == nil {
			return fmt.Errorf("mockdb: cannot add a nil mock to test set %q", testSetID)
		}
		if keptNames[mock.Name] {
			collisions = append(collisions, mock.Name)
		}
	}
	if len(collisions) > 0 {
		return fmt.Errorf("mockdb: cannot add mocks whose names already exist in test set %q: %v", testSetID, collisions)
	}

	if insertAt < 0 {
		insertAt = len(kept)
	}

	result := make([]*models.Mock, 0, len(kept)+len(add))
	result = append(result, kept[:insertAt]...)
	result = append(result, add...)
	result = append(result, kept[insertAt:]...)

	if err := ys.writeMocksAtomically(path, fileName, result, ys.Format); err != nil {
		return err
	}

	ys.Logger.Debug("replaced mocks in test set",
		zap.String("testSetID", testSetID),
		zap.Int("dropped", len(drop)),
		zap.Int("added", len(add)),
		zap.Int("total", len(result)))

	return nil
}

// missingNames returns the entries of want that were never seen, preserving
// order and ignoring duplicates.
func missingNames(want []string, seen map[string]bool) []string {
	var missing []string
	reported := make(map[string]bool, len(want))
	for _, name := range want {
		if seen[name] || reported[name] {
			continue
		}
		reported[name] = true
		missing = append(missing, name)
	}
	return missing
}
