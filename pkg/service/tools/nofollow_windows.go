package tools

// Windows has no O_NOFOLLOW. A reparse point at the final component is still
// followed here, so the check-then-write window described in
// ResolveConfigTarget stays open on this platform; creating a symlink needs a
// privilege or developer mode, which narrows who can use it.
const oNoFollow = 0
