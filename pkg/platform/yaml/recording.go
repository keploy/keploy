package yaml

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/linkout"
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
//
// root itself is opened from the project (utils.OpenRecordingDir), so a
// keploy folder that is a link out of the project is refused as well. Every
// store is rooted at the recording, not at a folder in it (a report under
// keploy/reports has keploy as its root), so a folder in the recording that
// links out is a link out of the root, and refused.
//
// What changes the recording keeps to the same rule, for the same reason: a
// linked mocks.yaml would otherwise have `keploy record` add to any file the
// user can write. The ...InRecording calls below are the os calls of that
// name, made inside root (os.Root). A path, or a link, that leads out of root
// is refused with why, and nothing is written, created, moved or removed
// there. A rename replaces a link without following it, so a link out at
// either end of one is looked for first, and refused too: it would otherwise
// go unsaid, and the link be lost. Nothing is ever written through it.

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

// ReadRecordingFileAnyBounded is ReadFileAnyBounded for a file of the
// recording at root, read inside it (see above): the preferred format's file,
// else the other's, as a regular file of at most limit bytes (safeyaml). Only
// a file that is not there passes on to the other format; a link out of the
// recording is refused, not passed over.
func ReadRecordingFileAnyBounded(ctx context.Context, root, path, name string, preferred Format, limit int64) ([]byte, Format, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	for _, f := range [2]Format{preferred, otherFormat(preferred)} {
		filePath := filepath.Join(path, name+"."+f.FileExtension())
		data, err := readBounded(root, filePath, limit)
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

// readBounded reads path, a file of the recording at root, inside it, as
// safeyaml.ReadFileIn reads it.
func readBounded(root, path string, limit int64) ([]byte, error) {
	r, rel, err := recordingRoot(root, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := safeyaml.ReadFileIn(r, rel, limit)
	if err != nil {
		refused := fmt.Sprintf("the recording's file %q is read only inside its recording %q", rel, root)
		if out := linkout.Explain(r, root, rel, refused, err); out != nil {
			return nil, out
		}
		// Wrapped, a file that is not there is still fs.ErrNotExist to
		// errors.Is.
		return nil, fmt.Errorf("%s: %w", refused, err)
	}
	return data, nil
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

// recordingRoot opens root and says where path is inside it.
func recordingRoot(root, path string) (*os.Root, string, error) {
	return recordingRootBy(root, path, utils.OpenRecordingDir)
}

// recordingRootBy is recordingRoot with root opened by open.
func recordingRootBy(root, path string, open func(dir string) (*os.Root, error)) (*os.Root, string, error) {
	dir := root
	if dir == "" {
		dir = "."
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return nil, "", changeRefused(root, path, err)
	}
	r, err := open(dir)
	if err != nil {
		return nil, "", changeRefused(root, rel, err)
	}
	return r, rel, nil
}

// changeRefused says why path, relative to root, could not be changed inside
// the recording. A file that is not there is only that, and is returned as it
// is, so os.IsNotExist still tells it.
func changeRefused(root, path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fmt.Errorf("%s: %w", changeRefusal(root, path), err)
}

// changeRefusal is what keploy says for any refusal to change path, relative
// to root, inside the recording.
func changeRefusal(root, path string) string {
	return fmt.Sprintf("the recording's file %q is changed only inside its recording %q", path, root)
}

// changeRefusedIn is changeRefused for path, inside r, the recording at root,
// that r refused: a link on its way that r does not follow is named, with
// where it leads, after what changeRefused says (linkout.Error).
func changeRefusedIn(r *os.Root, root, path string, err error) error {
	if out := linkout.Explain(r, root, path, changeRefusal(root, path), err); out != nil {
		return out
	}
	return changeRefused(root, path, err)
}

// inRecording calls op with path's place inside root.
func inRecording[T any](root, path string, op func(r *os.Root, rel string) (T, error)) (T, error) {
	r, rel, err := recordingRoot(root, path)
	if err != nil {
		var none T
		return none, err
	}
	defer func() { _ = r.Close() }()
	v, err := op(r, rel)
	if err != nil {
		return v, changeRefusedIn(r, root, rel, err)
	}
	return v, nil
}

// changeInRecording is inRecording for an op that returns only an error.
func changeInRecording(root, path string, op func(r *os.Root, rel string) error) error {
	_, err := inRecording(root, path, func(r *os.Root, rel string) (struct{}, error) {
		return struct{}{}, op(r, rel)
	})
	return err
}

// OpenFileInRecording is os.OpenFile inside root (see above).
func OpenFileInRecording(root, path string, flag int, perm fs.FileMode) (*os.File, error) {
	return inRecording(root, path, func(r *os.Root, rel string) (*os.File, error) {
		return r.OpenFile(rel, flag, perm)
	})
}

// StatInRecording is os.Stat inside root (see above): a link inside root is
// followed, and one that leads out of it is an error, not the file it leads
// to.
func StatInRecording(root, path string) (fs.FileInfo, error) {
	return inRecording(root, path, (*os.Root).Stat)
}

// WriteFileInRecording is os.WriteFile inside root (see above).
func WriteFileInRecording(root, path string, data []byte, perm fs.FileMode) error {
	return changeInRecording(root, path, func(r *os.Root, rel string) error {
		return r.WriteFile(rel, data, perm)
	})
}

// MkdirAllInRecording is os.MkdirAll inside root (see above). root itself is
// made when it is not there, the first recording has none yet, from the
// project as utils.MkdirOpenRecordingDir makes it: a link on its way that
// leads out of the project is refused, and no folder is made where it leads.
// root is made and opened in that one pass, so path is made inside the root
// that was checked. An empty root is the working directory, there already.
func MkdirAllInRecording(root, path string, perm fs.FileMode) error {
	r, rel, err := recordingRootBy(root, path, func(dir string) (*os.Root, error) {
		return utils.MkdirOpenRecordingDir(dir, perm)
	})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := r.MkdirAll(rel, perm); err != nil {
		return changeRefusedIn(r, root, rel, err)
	}
	return nil
}

// ChmodInRecording is os.Chmod inside root (see above).
func ChmodInRecording(root, path string, mode fs.FileMode) error {
	return changeInRecording(root, path, func(r *os.Root, rel string) error {
		return r.Chmod(rel, mode)
	})
}

// RemoveInRecording is os.Remove inside root (see above). A link at path is
// removed, not what it leads to.
func RemoveInRecording(root, path string) error {
	return changeInRecording(root, path, (*os.Root).Remove)
}

// RemoveAllInRecording is os.RemoveAll inside root (see above).
func RemoveAllInRecording(root, path string) error {
	return changeInRecording(root, path, (*os.Root).RemoveAll)
}

// RenameInRecording is os.Rename inside root (see above). A link that leads
// out of root, at either end, is refused.
func RenameInRecording(root, oldPath, newPath string) error {
	r, oldRel, err := recordingRoot(root, oldPath)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	newRel, err := filepath.Rel(r.Name(), newPath)
	if err != nil {
		return changeRefused(root, newPath, err)
	}
	for _, rel := range []string{oldRel, newRel} {
		if _, err := r.Stat(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changeRefusedIn(r, root, rel, err)
		}
	}
	if err := r.Rename(oldRel, newRel); err != nil {
		return changeRefusedIn(r, root, oldRel, err)
	}
	return nil
}

// ReplaceFileInRecording is ReplaceFile inside root (see above). Refused, it
// leaves dst as it was; src is the caller's to remove.
func ReplaceFileInRecording(logger *zap.Logger, root, src, dst string) error {
	if _, err := StatInRecording(root, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return ReplaceFileWith(logger, func(from, to string) error {
		return RenameInRecording(root, from, to)
	}, src, dst)
}

// tempNumber is the random part of a temp file's name: a variable, so a test
// can have two names meet.
var tempNumber = rand.Uint32

// CreateTempInRecording is os.CreateTemp in dir, inside root (see above), with
// perm as os.OpenFile takes it: the last "*" of pattern is the random part,
// which goes at the end when there is none. Like os.CreateTemp it refuses a
// pattern holding a path separator; unlike it, an empty dir is the working
// directory, not os.TempDir().
func CreateTempInRecording(root, dir, pattern string, perm fs.FileMode) (*os.File, error) {
	if strings.ContainsAny(pattern, "/"+string(os.PathSeparator)) {
		return nil, &fs.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains path separator")}
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(tempNumber()), 10)+suffix)
		f, err := OpenFileInRecording(root, name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: fs.ErrExist}
}

// DeleteRecordingFileF is DeleteFileF for a file of the recording at root,
// inside it (see above). A file that is not there is returned as
// fs.ErrNotExist with no ERROR said: the caller judges it.
func DeleteRecordingFileF(logger *zap.Logger, root, path, name string, format Format) error {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	if err := RemoveInRecording(root, filePath); err != nil {
		// A file that is not there is the caller's to judge (a delete may
		// count it as done): it says so through fs.ErrNotExist, not an ERROR.
		if !errors.Is(err, fs.ErrNotExist) {
			utils.LogError(logger, err, "failed to delete the file", zap.String("file", filePath))
		}
		return fmt.Errorf("failed to delete the file: %w", err)
	}
	return nil
}

// DeleteRecordingDir is DeleteDir for a directory of the recording at root,
// inside it (see above).
func DeleteRecordingDir(logger *zap.Logger, root, path string) error {
	if err := RemoveAllInRecording(root, path); err != nil {
		utils.LogError(logger, err, "failed to delete the directory", zap.String("path", path))
		return fmt.Errorf("failed to delete the directory: %w", err)
	}
	return nil
}
