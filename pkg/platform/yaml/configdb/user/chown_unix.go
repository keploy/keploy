//go:build unix

package user

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"syscall"
)

// Injectable hooks for deterministic unit tests without requiring root.
var (
	euidFn        = os.Geteuid
	chownPathFn   = os.Lchown
	chownFileFn   = (*os.File).Chown
	dirOwnerUIDFn = func(path string) (uint32, bool) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return st.Uid, true
	}
)

// sudoTargetOwner reports whether ~/.keploy and ~/.keploy/installation-id
// created while running as root under sudo should be chowned back to the
// invoking user (SUDO_UID / SUDO_GID), matching utils/log/logfile_unix.go.
//
// When keploy re-executes through sudo while preserving HOME, leaving a
// root-owned ~/.keploy (0700) or installation-id (0600) would lock out later
// non-sudo runs and cause the same machine to emit two distinct IDs.
func sudoTargetOwner(home string) (uid, gid int, needChown bool, err error) {
	if euidFn() != 0 {
		return 0, 0, false, nil
	}
	sudoUID := os.Getenv("SUDO_UID")
	if sudoUID == "" {
		return 0, 0, false, nil
	}
	parsedUID, err := strconv.ParseUint(sudoUID, 10, 32)
	if err != nil {
		return 0, 0, false, fmt.Errorf("SUDO_UID %q is not a valid uid: %w", sudoUID, err)
	}
	// Chown takes a signed int (32-bit on 386/arm) and interprets -1
	// (4294967295) as "leave owner unchanged", so refuse uids above MaxInt32.
	if parsedUID > math.MaxInt32 {
		return 0, 0, false, fmt.Errorf("SUDO_UID %d exceeds maximum supported uid %d", parsedUID, math.MaxInt32)
	}
	if parsedUID == 0 {
		return 0, 0, false, nil
	}
	// If HOME itself belongs to root (e.g. `sudo -H` -> /root), keep root ownership.
	if ownerUID, ok := dirOwnerUIDFn(home); ok && ownerUID == 0 {
		return 0, 0, false, nil
	}
	gid = -1
	if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
		if parsedGID, gErr := strconv.ParseUint(sudoGID, 10, 32); gErr == nil && parsedGID <= math.MaxInt32 {
			gid = int(parsedGID)
		}
	}
	return int(parsedUID), gid, true, nil
}
