//go:build !windows

package tls

import "strings"

// envSafePath reports whether path is safe to place in a whitespace-split
// environment variable (JAVA_TOOL_OPTIONS). Off Windows there is no short-path
// equivalent, so the only requirement is that it contain no spaces — which a
// temp path under /tmp or /var/folders does not.
func envSafePath(path string) (string, bool) {
	return path, !strings.ContainsRune(path, ' ')
}
