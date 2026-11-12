package util

import "errors"

// ErrNoSocketTable is TCPListeners' error on a platform where it does not
// read the kernel's socket table (all but Linux).
var ErrNoSocketTable = errors.New("the kernel's socket table is not read on this platform")
