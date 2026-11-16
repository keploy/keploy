// Package localfile writes the files keploy keeps local in a set's folder of
// a recording: the receipts of a run and the misses file, which names the
// calls a run missed whole. Each is written through a temporary file renamed
// into place, inside the recording only, and git-ignored by keploy/.gitignore.
// keploy mock replay (pkg/service/mock) and keploy test (pkg/service/replay)
// each write their own misses file here (MissesFile, TestMissesFile), so one
// writer, one gitignore check and one name for each one's temporary file
// (TempPrefixOf, HoldsCallsWhole) hold for both.
package localfile

import (
	"io"
	"path/filepath"
	"strings"

	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// TempPrefix begins the name of the temporary file a local file's write goes
// through (Write). Deliberately NOT *.yaml: an editor watching
// keploy/*/*.yaml would pick the transient file up as though it were a
// receipt. keploy/.gitignore ignores it ("/*/" + TempPrefix + "*"): a kill
// between creating it and renaming it leaves one behind, and `git add -A`
// would commit it.
const TempPrefix = ".last-replay-"

// TempPrefixOf begins the name of the temporary file a write of name, a file
// keploy keeps local in a set's folder, goes through (Write names it
// TempPrefixOf(name) + "*.tmp"), and the one place that says which: the
// writer, keploy/.gitignore's check (missesIgnored) and HoldsCallsWhole all
// read it here. Each is under TempPrefix, which keploy/.gitignore ignores;
// MissesFile's is missesTempPrefix and TestMissesFile's testMissesTempPrefix,
// for HoldsCallsWhole to tell them from the receipts'.
func TempPrefixOf(name string) string {
	switch name {
	case MissesFile:
		return missesTempPrefix
	case TestMissesFile:
		return testMissesTempPrefix
	}
	return TempPrefix
}

// Write writes data to dir/name through a temporary file renamed into place
// (its name begins TempPrefixOf(name)), so a kill halfway leaves the previous
// file or none, never half of one, and at most the temporary file beside it.
// Under sudo the file belongs to the developer who ran keploy, not root. dir
// is a folder of the recording at keployDir, and every step stays inside the
// recording (setfile's ...InRecording): a set or keploy folder that links
// out, or a link out at dir/name, is refused with why, and nothing is
// written there.
func Write(logger *zap.Logger, keployDir, dir, name string, data []byte) error {
	tmp, err := setfile.CreateTempInRecording(keployDir, dir, TempPrefixOf(name)+"*.tmp", 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = setfile.RemoveInRecording(keployDir, tmp.Name())
		}
	}()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(0o644)
	}
	// Ownership on the open descriptor, before it can be renamed into place.
	utils.RestoreFileOwnershipOf(logger, tmp, tmp.Name())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		tempWritten(tmp.Name())
		werr = setfile.RenameInRecording(keployDir, tmp.Name(), filepath.Join(dir, name))
	}
	if werr != nil {
		return werr
	}
	committed = true
	return nil
}

// tempWritten is called with the path of the temporary file a write goes
// through once the data is in it and it is closed, just before it is renamed
// into place: the file a kill there leaves behind. It does nothing; a test
// sets it (SetTempWrittenForTest) to see that file.
var tempWritten = func(string) {}

// SetTempWrittenForTest makes f see each temporary file a write goes through,
// as tempWritten does, until restore is called. For tests only.
func SetTempWrittenForTest(f func(path string)) (restore func()) {
	was := tempWritten
	tempWritten = f
	return func() { tempWritten = was }
}

// readGitIgnore is keploy/.gitignore, read inside the recording (a link out
// is read as no file: AddToGitIgnore then refuses it, and says why).
func readGitIgnore(keployDir string) []byte {
	f, err := setfile.OpenInRecording(keployDir, filepath.Join(keployDir, ".gitignore"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	return b
}

// Ignore adds entries to keploy/.gitignore, and reads the file first so the
// common case -- they are already there -- writes nothing. It returns why
// each entry it could not add is not there (nil when every one is).
//
// The helper is an unlocked read-modify-write, so two runs starting at once
// in one repository can both decide to append. Reading first makes that a
// first-run-only race instead of one on every run.
func Ignore(logger *zap.Logger, keployDir string, entries []string) (failed map[string]error) {
	existing := readGitIgnore(keployDir)
	lines := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, entry := range entries {
		if lines[entry] {
			continue
		}
		if err := addGitIgnore(logger, keployDir, entry); err != nil {
			logger.Debug("failed to git-ignore a file keploy keeps local", zap.String("entry", entry), zap.Error(err))
			if failed == nil {
				failed = map[string]error{}
			}
			failed[entry] = err
		}
	}
	return failed
}

// addGitIgnore is utils.AddToGitIgnore; a variable so a test can make one
// entry fail (SetAddGitIgnoreForTest).
var addGitIgnore = utils.AddToGitIgnore

// SetAddGitIgnoreForTest makes f add each entry Ignore adds to
// keploy/.gitignore, in place of utils.AddToGitIgnore, until restore is
// called. For tests only.
func SetAddGitIgnoreForTest(f func(logger *zap.Logger, path, entry string) error) (restore func()) {
	was := addGitIgnore
	addGitIgnore = f
	return func() { addGitIgnore = was }
}
