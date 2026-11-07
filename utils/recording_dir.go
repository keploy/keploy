package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// OpenRecordingDir opens dir, a folder whose files come with the repository
// keploy runs in (a recording's keploy folder, or a folder in it), as an
// os.Root anchored at the project:
//   - an absolute dir, as keploy's --path is made, is opened from the folder
//     it is in, the project: only its own last folder is held to that one;
//   - a relative dir is opened from the working directory, the project, one
//     folder at a time, each only inside the one before it. Leading ".."
//     elements say where the project is, and are opened as they are.
//
// git keeps symbolic links, so the keploy folder, or a folder in it, may be a
// link a cloned repository carries. One whose target stays inside the folder
// it is in is followed. One with an absolute target, or one that climbs out
// by "..", is refused with why (os.Root), so nothing under it is read or
// written: without this, a keploy folder linking to ../.. would have keploy
// read, upload and write files of the user's from outside the project.
func OpenRecordingDir(dir string) (*os.Root, error) {
	base, names := recordingDirSteps(dir)
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	at := base
	for _, name := range names {
		next, err := r.OpenRoot(name)
		_ = r.Close()
		if errors.Is(err, fs.ErrNotExist) {
			// Only not there: returned as it is, so os.IsNotExist tells it.
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("the folder %q is opened only inside %q: %w", name, at, err)
		}
		r, at = next, filepath.Join(at, name)
	}
	return r, nil
}

// recordingDirSteps says where OpenRecordingDir opens dir from, base, and the
// folders it then opens, one inside the other.
func recordingDirSteps(dir string) (string, []string) {
	clean := filepath.Clean(dir)
	if filepath.IsAbs(clean) {
		parent := filepath.Dir(clean)
		if parent == clean {
			// The file system's root is in no folder.
			return clean, nil
		}
		return parent, []string{filepath.Base(clean)}
	}
	if clean == "." {
		return clean, nil
	}
	parts := strings.Split(clean, string(filepath.Separator))
	up := 0
	for up < len(parts) && parts[up] == ".." {
		up++
	}
	if up == 0 {
		return ".", parts
	}
	return filepath.Join(parts[:up]...), parts[up:]
}
