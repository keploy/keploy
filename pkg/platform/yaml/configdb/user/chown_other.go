//go:build !unix

package user

import "os"

var (
	chownPathFn = func(string, int, int) error { return nil }
	chownFileFn = func(*os.File, int, int) error { return nil }
)

func sudoTargetOwner(string) (uid, gid int, needChown bool, err error) {
	return 0, 0, false, nil
}
