//go:build windows

package tls

// chownToOwnerOf does nothing on Windows, where keploy never writes an NSS
// database (installNSSCA runs on the Linux install path only).
func chownToOwnerOf(_, _ string) error { return nil }
