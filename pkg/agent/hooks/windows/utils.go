//go:build windows && amd64

package windows

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// isAdmin checks if the current process has administrator privileges on Windows.
// This is required for loading the WinDivert driver.
func isAdmin() bool {
	// Get the current process token
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()

	// Check if the token is elevated
	var elevation uint32
	var returnLength uint32
	err = windows.GetTokenInformation(
		token,
		windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)),
		&returnLength,
	)
	if err != nil {
		return false
	}

	// elevation is non-zero if the token is elevated
	return elevation != 0
}
