package tools

import "os"

// Windows has no O_NOFOLLOW. A reparse point at the final component is still
// followed here, so the check-then-write window described in
// ResolveConfigTarget stays open on this platform; creating a symlink needs a
// privilege or developer mode, which narrows who can use it.
const oNoFollow = 0

// openNoFollowIn is os.OpenFile of name, one path element, in the folder dir.
// Windows has no O_NOFOLLOW: a reparse point there whose target is in dir is
// followed, and one that leads out of dir is refused (os.Root).
func openNoFollowIn(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	return dir.OpenFile(name, flag, perm)
}
