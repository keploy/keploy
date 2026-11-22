//go:build !windows

package utils

import (
	"os"
	"strconv"
	"syscall"
)

// asInvokingUser is how GitIgnores runs git: as the user sudo ran keploy
// for (SUDO_UID and SUDO_GID), when keploy runs as root under sudo. git
// refuses a work tree another user owns (its safe.directory check), and the
// recording is that user's; run as them, git reads their repository as they
// would, and with no more than their rights. nil (as keploy runs) otherwise.
func asInvokingUser() *syscall.SysProcAttr {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, err := strconv.ParseUint(os.Getenv("SUDO_UID"), 10, 32)
	if err != nil || uid == 0 {
		return nil
	}
	gid, err := strconv.ParseUint(os.Getenv("SUDO_GID"), 10, 32)
	if err != nil {
		return nil
	}
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
}
