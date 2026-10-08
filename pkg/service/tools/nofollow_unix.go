//go:build !windows

package tools

import "syscall"

// oNoFollow makes a create-or-truncate refuse a symlink at the final
// component. The config path is fully resolved before it is opened, so it can
// never legitimately be a link; without this, one planted between the check
// and the write is followed, and the generated config lands wherever it
// points.
const oNoFollow = syscall.O_NOFOLLOW
