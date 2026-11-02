package util

import "errors"

// ErrNoSocketTable is TCPListeners' and IsLocalAddr's error on a platform
// where they do not read the kernel's tables (all but Linux).
var ErrNoSocketTable = errors.New("the kernel's socket table is not read on this platform")
