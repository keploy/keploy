//go:build !linux && !windows

package tls

// copyExtendedAttributes copies none on macOS: its extended attributes
// (com.apple.quarantine, com.apple.provenance and the like) say where a file
// came from, not who may read it, and some cannot be set by any process.
// The owner and mode are kept (keepKeystoreAttributes).
func copyExtendedAttributes(string, string) error { return nil }
