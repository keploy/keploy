package models

import (
	"os"
	"path/filepath"
	"testing"
)

// MountAt reads what a mount table mounts at a path: the root of the last
// mount listed at the path or above it, where that mount is at the path
// itself. A later mount hides what earlier ones mounted under it; the
// kernel's escapes of a path's space, tab, newline and backslash are read
// back; a table keploy cannot read is an error.
func TestMountAtReadsTheFileMountedAtAPath(t *testing.T) {
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	const utc = "/usr/share/zoneinfo/Etc/UTC"
	for _, row := range []struct {
		what, table, point, want string
		at                       bool
	}{
		{"no mount but the root's", root, utc, "", false},
		{"a file mounted at the path", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata /usr/share/zoneinfo/Etc/UTC ro - xfs /dev/sda1 rw\n", utc, "/usr/share/zoneinfo/Asia/Kolkata", true},
		{"one at another path", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata /usr/share/zoneinfo/Etc/UTCX ro - xfs /dev/sda1 rw\n", utc, "", false},
		{"one at a directory above the path", root + "2 1 8:1 /zi /usr/share/zoneinfo ro - xfs /dev/sda1 rw\n", utc, "", false},
		{"the later of two at the path", root + "2 1 8:1 /a /usr/share/zoneinfo/Etc/UTC ro - xfs /dev/sda1 rw\n3 2 8:1 /b /usr/share/zoneinfo/Etc/UTC ro - xfs /dev/sda1 rw\n", utc, "/b", true},
		{"one at the path, hidden by a later one above it", root + "2 1 8:1 /a /usr/share/zoneinfo/Etc/UTC ro - xfs /dev/sda1 rw\n3 1 8:1 /zi /usr/share ro - xfs /dev/sda1 rw\n", utc, "", false},
		{"one above, then one at the path", root + "2 1 8:1 /zi /usr/share ro - xfs /dev/sda1 rw\n3 2 8:1 /a /usr/share/zoneinfo/Etc/UTC ro - xfs /dev/sda1 rw\n", utc, "/a", true},
		{"escaped paths", root + `2 1 8:1 /host\040zones/Asia/Kolkata\134x /my\040zone\011dir/UTC ro - xfs /dev/sda1 rw` + "\n", "/my zone\tdir/UTC", `/host zones/Asia/Kolkata\x`, true},
		{"a short escape left as written", root + `2 1 8:1 /a\04 /b\0 ro - xfs /dev/sda1 rw` + "\n", `/b\0`, `/a\04`, true},
		{"a line of too few fields", root + "2 1 8:1 /a\n", utc, "", false},
	} {
		p := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(p, []byte(row.table), 0o644); err != nil {
			t.Fatal(err)
		}
		got, at, err := MountAt(p, row.point)
		if err != nil || got != row.want || at != row.at {
			t.Errorf("%s: %q %v %v; want %q %v", row.what, got, at, err, row.want, row.at)
		}
	}
	if _, _, err := MountAt(filepath.Join(t.TempDir(), "none"), utc); err == nil {
		t.Errorf("a table keploy cannot read: no error")
	}
}
