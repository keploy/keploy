//go:build windows

package tls

import (
	"errors"
	"os"
)

// syscallMkfifo: Windows has no FIFOs; the tests that hold a fake keytool
// (a /bin/sh script) do not run there.
func syscallMkfifo(string) error { return errors.New("no FIFOs on Windows") }

// sameOwner: the tests that ask do not run on Windows.
func sameOwner(os.FileInfo, os.FileInfo) bool { return true }
