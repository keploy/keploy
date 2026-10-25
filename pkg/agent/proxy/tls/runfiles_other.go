//go:build !unix && !windows

package tls

import (
	"errors"
	"os"
)

// runFileLocking: keploy has no file lock here, so a run cannot tell a live
// run's file from a dead one's and sweeps none (sweepDeadRunFiles). A run
// writes its files under the older, unswept names and says so
// (createRunFile), and still removes them when its command returns.
const runFileLocking = false

// privateDirName: no user id to name the private dir by here.
func privateDirName() (string, error) { return "keploy-run", nil }

// checkPrivateDirOwner: no owner to check here.
func checkPrivateDirOwner(string, os.FileInfo) error { return nil }

func setPrivateDirMode(string) error { return nil }

func openRunFileLock(string) (*os.File, error) { return nil, errors.ErrUnsupported }

func tryLockRunFile(*os.File) (bool, error) { return false, errors.ErrUnsupported }

func isSymlinkRefused(error) bool { return false }

func isInUse(error) bool { return false }
