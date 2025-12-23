//go:build windows && amd64

package windows

import (
	"golang.org/x/sys/windows"
)

// isAdmin checks if the current process has administrator privileges on Windows.
// This is required for loading the WinDivert driver.
func isAdmin() bool {
	var sid *windows.SID

	// Get the SID for the built-in Administrators group
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	// Check if the current process token is a member of the Administrators group
	token := windows.Token(0)
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}

	return member
}
