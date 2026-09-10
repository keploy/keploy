//go:build windows

package tls

import (
	"strings"

	"golang.org/x/sys/windows"
)

// envSafePath returns a form of path with no spaces, safe to place in a
// whitespace-split environment variable like JAVA_TOOL_OPTIONS, and whether it
// succeeded. On Windows a temp path routinely contains a space (a username with
// a space lands under C:\Users\Some User\...); the JVM splits JAVA_TOOL_OPTIONS
// on whitespace with no quoting, so a spaced path there makes the JVM refuse to
// start. We convert to the 8.3 short path, which has no spaces. If 8.3 name
// generation is disabled and the short path still contains a space, we report
// failure so the caller declines rather than break the JVM.
func envSafePath(path string) (string, bool) {
	if !strings.ContainsRune(path, ' ') {
		return path, true
	}
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path, false
	}
	buf := make([]uint16, len(path)+16)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		return path, false
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if _, err := windows.GetShortPathName(long, &buf[0], n); err != nil {
			return path, false
		}
	}
	short := windows.UTF16ToString(buf)
	if strings.ContainsRune(short, ' ') {
		return short, false
	}
	return short, true
}
