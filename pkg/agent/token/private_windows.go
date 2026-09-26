//go:build windows

package token

import "os"

// isPrivate has nothing to check on windows. Mode bits do not carry its access
// control, and the temp directory os.MkdirTemp uses is the user's own
// (%LOCALAPPDATA%\Temp), which the profile's ACL already closes to other
// non-administrator users.
func isPrivate(os.FileInfo) bool { return true }
