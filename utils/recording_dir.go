package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/utils/linkout"
)

// OpenRecordingDir opens dir, a folder whose files come with the repository
// keploy runs in (a recording's keploy folder, or a folder in it), as an
// os.Root anchored at the project:
//   - an absolute dir that is a keploy folder or in one, such as
//     <project>/keploy (keploy's --path) or <project>/keploy/tests, is opened
//     from the project, the folder its last keploy folder is in, and then
//     one folder at a time: keploy, then each folder inside it;
//   - any other absolute dir is opened from the folder it is in, taken for
//     the project: only its own last folder is held to that one. A caller
//     therefore never passes an absolute dir deeper in the project outside a
//     keploy folder (a custom layout), whose folders above its last one would
//     be opened as they are: it passes such a dir relative to the working
//     directory;
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
	return openRecordingDir(dir, false, 0)
}

// MkdirRecordingDir is os.MkdirAll for such a folder: each folder is made, if
// it is not there, only inside the one before it, from the project, as
// OpenRecordingDir opens them, so a link on the way that leads out is refused
// with why, and nothing is made where it leads, not even an empty folder.
// Only the project itself, the folder OpenRecordingDir opens dir from, is
// made as os.MkdirAll makes it.
func MkdirRecordingDir(dir string, perm fs.FileMode) error {
	r, err := MkdirOpenRecordingDir(dir, perm)
	if err != nil {
		return err
	}
	return r.Close()
}

// MkdirOpenRecordingDir makes dir as MkdirRecordingDir does and returns it
// opened as OpenRecordingDir opens it, in one pass: each folder is opened as
// it is made or found, so the folder returned is the one that was checked,
// and no link put on the way after the check is followed.
func MkdirOpenRecordingDir(dir string, perm fs.FileMode) (*os.Root, error) {
	return openRecordingDir(dir, true, perm)
}

func openRecordingDir(dir string, mkdir bool, perm fs.FileMode) (*os.Root, error) {
	base, names := recordingDirSteps(dir)
	perm = perm.Perm() // os.Root.Mkdir takes only these bits
	if mkdir {
		if err := os.MkdirAll(base, perm); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	at := base
	for _, name := range names {
		if mkdir {
			// A name that is there already, a link too, is left as it is
			// for OpenRoot to judge: Mkdir makes nothing through a link.
			if err := r.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
				_ = r.Close()
				return nil, fmt.Errorf("the folder %q is made only inside %q: %w", name, at, err)
			}
		}
		next, err := r.OpenRoot(name)
		if err != nil {
			err = openRefused(r, at, name, err)
			_ = r.Close()
			return nil, err
		}
		_ = r.Close()
		r, at = next, filepath.Join(at, name)
	}
	return r, nil
}

// openRefused says why r, the folder at, did not open name, a folder in it: a
// folder that is not there is only that, and is returned as it is, so
// os.IsNotExist still tells it; one that is a link r does not follow is
// named, with where it leads (linkout.Error).
func openRefused(r *os.Root, at, name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	refused := fmt.Sprintf("the folder %q is opened only inside %q", name, at)
	if out := linkout.Explain(r, at, name, refused, err); out != nil {
		return out
	}
	return fmt.Errorf("%s: %w", refused, err)
}

// recordingKeployFolder is the name of a recording's folder in its project.
const recordingKeployFolder = "keploy"

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
		// From dir up to its last keploy folder, nearest first: dir is
		// opened from the project that keploy folder is in, so a keploy
		// folder the repository carries as a link out of the project is
		// refused for every folder in it, as it is for the keploy folder.
		var up []string
		for d := clean; ; {
			above := filepath.Dir(d)
			if above == d {
				break
			}
			up = append(up, filepath.Base(d))
			if filepath.Base(d) == recordingKeployFolder {
				names := make([]string, len(up))
				for i, name := range up {
					names[len(up)-1-i] = name
				}
				return above, names
			}
			d = above
		}
		return parent, []string{filepath.Base(clean)}
	}
	// "." is a step of its own, which opens the working directory again.
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
