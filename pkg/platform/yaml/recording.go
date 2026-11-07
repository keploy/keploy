package yaml

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
)

// A recording's own files (its test cases under <set>/tests, its mocks.yaml
// or mocks.gob, its mappings.yaml) are read only inside the recording's
// directory, root: the directory its test sets are kept under (keploy's
// --path; testdb's TcsPath, mockdb's MockPath, mapdb's path), by the rule a
// file a test case names is read by (pkg.OpenRecordingFile). A symbolic link
// whose target stays inside root is followed. A path, or a link, that leads
// out of root, by ".." or by an absolute target (os.Root refuses those even
// when they name a file inside), is refused with why. git keeps symbolic
// links, so without this a cloned repository whose recording links mocks.yaml
// or a test case to a file elsewhere would have keploy read that file, replay
// it, and upload it with the recording.
//
// An empty root is the working directory: the files are then at paths
// relative to it, and are read inside it.

// OpenInRecording opens path, a file of the recording at root (a path that
// starts with root, as filepath.Join(root, ...) makes it), within root (see
// above). The caller closes it.
func OpenInRecording(root, path string) (*os.File, error) {
	dir := root
	if dir == "" {
		dir = "."
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return nil, fmt.Errorf("the recording's file %q is read only inside its recording %q: %w", path, root, err)
	}
	return pkg.OpenRecordingFile(dir, rel)
}

// ReadRecordingFileF is ReadFileF for a file of the recording at root: path
// is a directory under root (filepath.Join(root, ...)), and the file is read
// inside root (see above). A file that is not there is an error that
// errors.Is fs.ErrNotExist.
func ReadRecordingFileF(ctx context.Context, logger *zap.Logger, root, path, name string, format Format) ([]byte, error) {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	file, err := OpenInRecording(root, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %w", err)
	}
	return readOpenFile(ctx, logger, file, filePath)
}

// ReadRecordingFileAny is ReadFileAny for a file of the recording at root,
// read inside it (see ReadRecordingFileF): the preferred format's file, else
// the other's. Only a file that is not there passes on to the other format; a
// link out of the recording is refused, not passed over.
func ReadRecordingFileAny(ctx context.Context, logger *zap.Logger, root, path, name string, preferred Format) ([]byte, Format, error) {
	for _, f := range [2]Format{preferred, otherFormat(preferred)} {
		data, err := ReadRecordingFileF(ctx, logger, root, path, name, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return data, f, nil
	}
	return nil, "", fs.ErrNotExist
}

// NewRecordingMockReaderF is NewMockReaderF for a mock file of the recording
// at root: path is a directory under root, and the file is opened inside root
// (see above).
func NewRecordingMockReaderF(ctx context.Context, logger *zap.Logger, root, path, name string, format Format) (*MockReader, error) {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	file, err := OpenInRecording(root, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open mock file: %w", err)
	}
	return newMockReader(ctx, logger, file, filePath, format), nil
}

// NewRecordingMockReaderAny is NewMockReaderAny for a mock file of the
// recording at root, opened inside it: the preferred format's file, else the
// other's. Only a file that is not there passes on to the other format.
func NewRecordingMockReaderAny(ctx context.Context, logger *zap.Logger, root, path, name string, preferred Format) (*MockReader, error) {
	for _, f := range [2]Format{preferred, otherFormat(preferred)} {
		r, err := NewRecordingMockReaderF(ctx, logger, root, path, name, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	return nil, fmt.Errorf("failed to open mock file: %w", os.ErrNotExist)
}
