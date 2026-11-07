package pkg

import (
	"fmt"
	"os"
)

// A test case keeps a part of its request that is too large to hold in a file
// beside it: a body (models.BodyRef), a form value or a form file
// (models.FormData.Paths). It names that file by a path relative to the
// recording's root, the directory its testdb writes under (keploy's --path in
// a replay, testdb.TestYaml.Root for the runner).
//
// A recording's own files (its test cases, mocks and mappings, which
// pkg/platform/yaml reads through OpenRecordingFile) are read the same way.
//
// A recording's file is read only inside its recording: root is opened, and
// the path is read within it (os.Root). A path that leads out of root, by
// "..", as an absolute path, or through a symbolic link, is refused with why,
// and so is every path when no root is known: nothing is read then, and the
// working directory is never the root. A stored test case can therefore not
// make the machine that replays it send a file from elsewhere.

// openRecordingRoot opens root, the recording's directory, for path.
func openRecordingRoot(root, path string) (*os.Root, error) {
	if root == "" {
		return nil, fmt.Errorf("the recording's file %q is read only inside its recording, and no recording directory was given", path)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open the recording directory %q to read its file %q: %w", root, path, err)
	}
	return r, nil
}

// recordingFileError says why path, a file of the recording, could not be read
// inside the recording at root.
func recordingFileError(root, path string, err error) error {
	return fmt.Errorf("the recording's file %q is read only inside its recording %q: %w", path, root, err)
}

// ReadRecordingFile reads path, a file a test case names or one of the
// recording's own, inside root, its
// recording's directory (see above).
func ReadRecordingFile(root, path string) ([]byte, error) {
	r, err := openRecordingRoot(root, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	b, err := r.ReadFile(path)
	if err != nil {
		return nil, recordingFileError(root, path, err)
	}
	return b, nil
}

// OpenRecordingFile opens path, a file a test case names or one of the
// recording's own, inside root, its
// recording's directory (see above). The caller closes it.
func OpenRecordingFile(root, path string) (*os.File, error) {
	r, err := openRecordingRoot(root, path)
	if err != nil {
		return nil, err
	}
	// Only the directory: a file opened in it stays open.
	defer func() { _ = r.Close() }()
	f, err := r.Open(path)
	if err != nil {
		return nil, recordingFileError(root, path, err)
	}
	return f, nil
}
