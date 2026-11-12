//go:build !linux

package learn

import (
	"os"
	"syscall"
)

// openRegular opens the regular file at p, a path with its links resolved
// (resolveUnder), under root, for reading, and no other (a FIFO, which
// would hold the read, a directory). Only Linux reads an application's
// zone from its process (/proc), so the files read here are keploy's own:
// the file is opened, without waiting on a FIFO, and then its type is
// read.
func openRegular(root, p string) (*os.File, error) {
	f, err := os.OpenFile(under(root, p), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, lasting(errNotRegular{fi.Mode()})
	}
	return f, nil
}
