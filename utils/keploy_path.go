package utils

import (
	"fmt"
	"os"
)

// EnsureKeployPathIsFolder fails when keployPath -- <path>/keploy, where keploy
// keeps its tests and mocks -- exists and is not a folder. The CLI checks it
// wherever cli/provider resolves --path to that folder (keployFolder), docker
// runs too. Before, of those commands only native record, test and mock were
// stopped, by the permission check; the rest went on and failed later with
// "readdirent <path>/keploy: not a directory", or did nothing. `export
// postman` and `import postman` build ./keploy themselves and are not
// checked: they still fail later, on "not a directory".
//
// It follows symlinks: a link to a folder is a folder. It refuses only a
// path that exists and is not a folder. A path that cannot be stat'd at all
// passes -- absent is the usual case, and the folder is created later -- and
// so does any other stat error (permission denied; ENOTDIR, when --path is
// itself a file). Only native record, test and mock meet a check after this
// one, CheckKeployFolderPermissions; docker runs and the other commands go on
// and fail later, or do nothing.
func EnsureKeployPathIsFolder(keployPath string) error {
	info, err := os.Stat(keployPath)
	if err != nil || info.IsDir() {
		return nil
	}
	return notAFolderError(keployPath)
}

// notAFolderError explains a FILE where keploy keeps its tests. Usually it is
// the keploy binary itself: downloaded into a project and run from there
// (`./keploy record`), it sits at exactly <path>/keploy under the default
// --path of ".". "exists but is not a directory" named the path and left the
// user to work out why.
//
// The remedy depends on which it is. Moving the running binary out of the
// folder is right; telling someone to move an unrelated file -- or an old copy
// of keploy, while the one running is installed -- into /usr/local/bin would
// overwrite their real install.
func notAFolderError(keployPath string) error {
	if isRunningBinary(keployPath) {
		return fmt.Errorf("keploy keeps its tests and mocks in %s, but that is this keploy binary, not a folder; "+
			"move the keploy binary out of this folder (for example into /usr/local/bin), "+
			"or pass --path to keep the tests somewhere else", keployPath)
	}
	return fmt.Errorf("keploy keeps its tests and mocks in %s, but that is a file, not a folder; "+
		"remove or rename that file, or pass --path to keep the tests somewhere else", keployPath)
}

func isRunningBinary(path string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exeInfo, exeErr := os.Stat(exe)
	pathInfo, pathErr := os.Stat(path)
	return exeErr == nil && pathErr == nil && os.SameFile(exeInfo, pathInfo)
}
