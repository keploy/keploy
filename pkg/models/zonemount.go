package models

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// mountLineCap bounds one line of a mount table MountAt reads: an
// overlay's options (its lower layers) run to a few KiB.
const mountLineCap = 1 << 20

// MountAt is what the mount table in file (a /proc/<pid>/mountinfo, or
// /proc/self/mountinfo) mounts at point, a path in the view its mount
// points are written in (the process's own root): the root of that mount
// (the path, in the filesystem it mounts, of the file or directory mounted
// there: field 4, as the kernel writes it, "//deleted" after the path of
// a file since unlinked or replaced) and true, where the mount that point is read through is
// one AT point (a file bind-mounted over it, as docker's
// `-v /etc/localtime:/etc/localtime` does on the file the image's link
// names), false where it is one at a directory above point, or none. Of the
// mounts at point or above it, the last the table lists is the one point is
// read through: a later mount hides what earlier ones mounted under it. An
// error where keploy cannot read the table.
func MountAt(file, point string) (string, bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), mountLineCap)
	root, at := "", false
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMount(fields[4])
		if mp != point && mp != "/" && !strings.HasPrefix(point, mp+"/") {
			continue
		}
		root, at = unescapeMount(fields[3]), mp == point
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("reading %s: %w", file, err)
	}
	if !at {
		return "", false, nil
	}
	return root, true, nil
}

// unescapeMount is a mount table's path as the kernel wrote it, its
// space, tab, newline and backslash escaped as \040, \011, \012 and \134,
// read back.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
