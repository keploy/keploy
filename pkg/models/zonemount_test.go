package models

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// MountOf walks a mount table as a lookup of a path crosses its mounts, by
// mount id and parent id, never by the order the table lists them in
// (kernel 6.8 lists them as they were made; a mount moved in later lists
// before the one it sits on): a mount on a directory hides what its parent
// mounted below it, a mount stacked at one point hides the one under it.
// The kernel's escapes of a path's space, tab, newline and backslash are
// read back, "//deleted" after a root is cut and told; mounts whose
// parents loop are never reached; two mounts of one parent at one point on
// the path and a table keploy cannot read are errors.
func TestMountOfWalksTheMountsALookupCrosses(t *testing.T) {
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	const utc = "/usr/share/zoneinfo/Etc/UTC"
	line := func(id, parent, root, point string) string {
		return id + " " + parent + " 8:1 " + root + " " + point + " ro - xfs /dev/sda1 rw\n"
	}
	for _, row := range []struct {
		what, table, point string
		want               Mount
		ok                 bool
		err                string
	}{
		{"no mount", "", utc, Mount{}, false, ""},
		{"the root's alone", root, utc, Mount{Point: "/", Root: "/"}, true, ""},
		{"a file mounted at the path", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata", utc), utc, Mount{Point: utc, Root: "/usr/share/zoneinfo/Asia/Kolkata"}, true, ""},
		{"one at another path", root + line("2", "1", "/k", utc+"X"), utc, Mount{Point: "/", Root: "/"}, true, ""},
		{"one at a directory above the path", root + line("2", "1", "/zi", "/usr/share/zoneinfo"), utc, Mount{Point: "/usr/share/zoneinfo", Root: "/zi"}, true, ""},
		{"two stacked at the path: the upper", root + line("2", "1", "/a", utc) + line("3", "2", "/b", utc), utc, Mount{Point: utc, Root: "/b"}, true, ""},
		{"two stacked, the upper listed first", root + line("3", "2", "/b", utc) + line("2", "1", "/a", utc), utc, Mount{Point: utc, Root: "/b"}, true, ""},
		{"one at the path, hidden by a later one above it", root + line("2", "1", "/a", utc) + line("3", "1", "/zi", "/usr/share"), utc, Mount{Point: "/usr/share", Root: "/zi"}, true, ""},
		{"one at the path, hidden by one above it listed after it, made first", root + line("3", "1", "/zi", "/usr/share") + line("2", "1", "/a", utc), utc, Mount{Point: "/usr/share", Root: "/zi"}, true, ""},
		{"one above, then one at the path on it", root + line("2", "1", "/zi", "/usr/share") + line("3", "2", "/a", utc), utc, Mount{Point: utc, Root: "/a"}, true, ""},
		{"one at the path on one above, listed before it (moved in later)", root + line("3", "2", "/a", utc) + line("2", "1", "/zi", "/usr/share"), utc, Mount{Point: utc, Root: "/a"}, true, ""},
		{"a root mount listed last", line("2", "1", "/a", utc) + root, utc, Mount{Point: utc, Root: "/a"}, true, ""},
		{"a file since replaced", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata//deleted", utc), utc, Mount{Point: utc, Root: "/usr/share/zoneinfo/Asia/Kolkata", Replaced: true}, true, ""},
		{"escaped paths", root + line("2", "1", `/host\040zones/Asia/Kolkata\134x`, `/my\040zone\011dir/UTC`), "/my zone\tdir/UTC", Mount{Point: "/my zone\tdir/UTC", Root: `/host zones/Asia/Kolkata\x`}, true, ""},
		{"a short escape left as written", root + line("2", "1", `/a\04`, `/b\0`), `/b\0`, Mount{Point: `/b\0`, Root: `/a\04`}, true, ""},
		{"a line of too few fields", root + "2 1 8:1 /a\n", utc, Mount{Point: "/", Root: "/"}, true, ""},
		{"two of one parent at one point", root + line("2", "1", "/a", "/usr/share") + line("3", "1", "/b", "/usr/share"), utc, Mount{}, false, "two mounts (ids 2 and 3) at /usr/share at one level"},
		{"two of one parent at one point, hidden by a shorter one listed after them", root + line("2", "1", "/a", "/usr/share") + line("3", "1", "/b", "/usr/share") + line("4", "1", "/c", "/usr"), utc, Mount{Point: "/usr", Root: "/c"}, true, ""},
		{"two of one parent at one point off the path", root + line("2", "1", "/a", "/opt") + line("3", "1", "/b", "/opt"), utc, Mount{Point: "/", Root: "/"}, true, ""},
		{"mounts whose parents loop, never reached", root + line("2", "3", "/a", "/usr") + line("3", "2", "/b", "/usr") + line("4", "1", "/c", "/usr") + line("5", "4", "/d", "/usr"), utc, Mount{Point: "/usr", Root: "/d"}, true, ""},
	} {
		p := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(p, []byte(row.table), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok, err := MountOf(p, row.point)
		if (err != nil) != (row.err != "") || err != nil && !strings.Contains(err.Error(), row.err) || got != row.want || ok != row.ok {
			t.Errorf("%s: %+v %v %v; want %+v %v %q", row.what, got, ok, err, row.want, row.ok, row.err)
		}
	}
	if _, _, err := MountOf(filepath.Join(t.TempDir(), "none"), utc); err == nil {
		t.Errorf("a table keploy cannot read: no error")
	}
}

// ZoneFileMount tells a zone file read from another zone's file through a
// mount: one of a file at it, or of a directory inside the part of its
// path that names its zone, whose file (its root and the rest of the path)
// is not the zone's. A directory mounted at or above the zoneinfo directory
// keeps every name in it; the same zone's file, the file the path's own
// name is (an alias linked to it), or another of IANA's names of UTC are
// the zone.
func TestZoneFileMountTellsAnotherZonesFile(t *testing.T) {
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	const utc = "/usr/share/zoneinfo/Etc/UTC"
	line := func(id, parent, root, point string) string {
		return id + " " + parent + " 8:1 " + root + " " + point + " ro - xfs /dev/sda1 rw\n"
	}
	for _, row := range []struct {
		what, table, p, zn string
		want               *ZoneMount
	}{
		{"nothing mounted", root, utc, "Etc/UTC", nil},
		{"Kolkata's file at it", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata", utc), utc, "Etc/UTC", &ZoneMount{Source: "/usr/share/zoneinfo/Asia/Kolkata", Zone: "Asia/Kolkata"}},
		{"a copy at it", root + line("2", "1", "/etc/localtime", utc), utc, "Etc/UTC", &ZoneMount{Source: "/etc/localtime"}},
		{"Kolkata's since replaced", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata//deleted", utc), utc, "Etc/UTC", &ZoneMount{Source: "/usr/share/zoneinfo/Asia/Kolkata", Zone: "Asia/Kolkata", Replaced: true}},
		{"the same zone's", root + line("2", "1", "/sub/usr/share/zoneinfo/Etc/UTC", utc), utc, "Etc/UTC", nil},
		{"another of UTC's names", root + line("2", "1", "/usr/share/zoneinfo/UTC", utc), utc, "Etc/UTC", nil},
		{"the file an alias links to, of its own name", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata", "/usr/share/zoneinfo/Asia/Kolkata"), "/usr/share/zoneinfo/Asia/Kolkata", "Asia/Calcutta", nil},
		{"a directory at the zoneinfo directory", root + line("2", "1", "/", "/usr/share/zoneinfo"), utc, "Etc/UTC", nil},
		{"a directory above it", root + line("2", "1", "/x", "/usr"), utc, "Etc/UTC", nil},
		{"a directory inside the name, of the same names", root + line("2", "1", "/usr/share/zoneinfo/Etc", "/usr/share/zoneinfo/Etc"), utc, "Etc/UTC", nil},
		{"a directory inside the name, of another region", root + line("2", "1", "/usr/share/zoneinfo/Asia", "/usr/share/zoneinfo/Etc"), utc, "Etc/UTC", &ZoneMount{Source: "/usr/share/zoneinfo/Asia/UTC", Zone: "Asia/UTC"}},
		{"a volume's root inside the name", root + line("2", "1", "/", "/usr/share/zoneinfo/Etc"), utc, "Etc/UTC", &ZoneMount{Source: "/UTC"}},
		{"a file whose path names no zone, mounted from Kolkata's", root + line("2", "1", "/usr/share/zoneinfo/Asia/Kolkata", "/zones/utc"), "/zones/utc", "Etc/UTC", &ZoneMount{Source: "/usr/share/zoneinfo/Asia/Kolkata", Zone: "Asia/Kolkata"}},
		{"a directory above a file whose path names no zone", root + line("2", "1", "/x", "/zones"), "/zones/utc", "Etc/UTC", nil},
	} {
		p := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(p, []byte(row.table), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ZoneFileMount(p, row.p, row.zn)
		if err != nil || (got == nil) != (row.want == nil) || got != nil && *got != *row.want {
			t.Errorf("%s: %+v %v; want %+v", row.what, got, err, row.want)
		}
	}
}

// ZoneLocation refuses an IANA name whose file in keploy's own zone files
// is mounted from another zone's: native mode in an application's
// container run with the host's /etc/localtime bind-mounted (which lands on
// the file the image's link names), where a JVM's zone id would otherwise
// be read with the host's rules. IANA's names of UTC are UTC with no file
// read (every runtime's zone data holds them so). The same zone's file,
// another of UTC's names, a name linked to the mounted file's own name, and
// a name keploy reads from no host file (time/tzdata) are read; a table
// keploy cannot read is an error, read again; a name Go reads no zone of
// checks no file. A refusal is kept, so the table is read once per name.
func TestZoneLocationRefusesItsOwnFileMountedFromAnotherZones(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no mount table off Linux")
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "usr/share/zoneinfo")
	// The mount lines below are written by hand with tmp in them as is: a
	// TMPDIR holding whitespace, which mountinfo writes escaped (\040),
	// mis-writes them. A stated limit of these rows, not of keploy.
	// Etc/UTC and Asia/Kolkata as files, Asia/Calcutta a link to Kolkata's:
	// the mount check reads these (goZoneDirs), Go the zones from its own
	// (its zone directories, else time/tzdata), so a row checks the mount
	// alone.
	for _, f := range []string{"Etc/UTC", "Europe/Berlin", "Asia/Kolkata"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("TZif"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("Kolkata", filepath.Join(dir, "Asia/Calcutta")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZONEINFO", "")
	table := filepath.Join(t.TempDir(), "mountinfo")
	t.Cleanup(SetOwnZoneFilesForTest(table, []string{dir + "/"}))
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	for _, row := range []struct {
		what, mounts, name string
		want               *time.Location // nil: an error
		own                bool           // the error is IsOwnZoneMount's
		why                string
	}{
		{"Berlin's file mounted from Kolkata's", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Europe/Berlin ro - xfs /dev/sda1 rw\n", "Europe/Berlin", nil, true,
			`keploy reads "Europe/Berlin" from its own zone file ` + dir + `/Europe/Berlin, which is a file mounted from /usr/share/zoneinfo/Asia/Kolkata, the zone file of "Asia/Kolkata", so keploy cannot tell whether it holds "Europe/Berlin"'s rules`},
		{"Berlin's file mounted from a copy, since replaced", root + "2 1 8:1 /etc/localtime//deleted " + dir + "/Europe/Berlin ro - xfs /dev/sda1 rw\n", "Europe/Berlin", nil, true,
			"a file whose path names no zone (a copy), since replaced on the host"},
		{"Etc/UTC's file mounted from Kolkata's: UTC to every runtime, no file read", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Etc/UTC ro - xfs /dev/sda1 rw\n", "Etc/UTC", time.UTC, false, ""},
		{"Etc/UTC's file mounted from the same zone's", root + "2 1 8:1 /usr/share/zoneinfo/Etc/UTC " + dir + "/Etc/UTC ro - xfs /dev/sda1 rw\n", "Etc/UTC", time.UTC, false, ""},
		{"Etc/UTC's file mounted from UTC's", root + "2 1 8:1 /usr/share/zoneinfo/UTC " + dir + "/Etc/UTC ro - xfs /dev/sda1 rw\n", "Etc/UTC", time.UTC, false, ""},
		{"Asia/Calcutta, linked to Kolkata's file, mounted from the host's Kolkata", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Asia/Kolkata ro - xfs /dev/sda1 rw\n", "Asia/Calcutta", kolkata, false, ""},
		{"Asia/Calcutta, linked to Kolkata's file, mounted from Etc/UTC's", root + "2 1 8:1 /usr/share/zoneinfo/Etc/UTC " + dir + "/Asia/Kolkata ro - xfs /dev/sda1 rw\n", "Asia/Calcutta", nil, true, `mounted from /usr/share/zoneinfo/Etc/UTC`},
		{"nothing mounted", root, "Etc/UTC", time.UTC, false, ""},
		{"a name of no host file", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Europe/Berlin ro - xfs /dev/sda1 rw\n", "America/New_York", nil, false, "-"},
		{"a table keploy cannot read", "-", "Europe/Berlin", nil, false, "keploy cannot read its own mounts"},
		{"a junk name Go reads no zone of: no file checked", root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Europe/Berlin ro - xfs /dev/sda1 rw\n", "../zoneinfo/Europe/Berlin", nil, false, "invalid"},
	} {
		zoneLocations.Clear()
		_ = os.Remove(table)
		if row.mounts != "-" {
			if err := os.WriteFile(table, []byte(row.mounts), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		loc, err := ZoneLocation(row.name)
		switch {
		case row.why == "-":
			if err != nil || loc.String() != row.name {
				t.Errorf("%s: %v %v; want %s", row.what, loc, err, row.name)
			}
		case row.want == nil:
			if err == nil || IsOwnZoneMount(err) != row.own || !strings.Contains(err.Error(), row.why) {
				t.Errorf("%s: %v %v; want an error (own mount %v) holding %q", row.what, loc, err, row.own, row.why)
			}
		case err != nil || !sameZoneAt(loc, row.want, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)):
			t.Errorf("%s: %v %v; want %v", row.what, loc, err, row.want)
		}
	}
	// Go reads a zone from $ZONEINFO first where that is a directory; a zip
	// file is passed over for the zone directories.
	zi := t.TempDir()
	if err := os.MkdirAll(filepath.Join(zi, "Etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zi, "Etc/UTC"), []byte("TZif"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZONEINFO", zi)
	if got := goZoneFile("Etc/UTC"); got != zi+"/Etc/UTC" {
		t.Errorf("ZONEINFO a directory: %q; want %q", got, zi+"/Etc/UTC")
	}
	if got := goZoneFile("Asia/Kolkata"); got != dir+"/Asia/Kolkata" {
		t.Errorf("ZONEINFO a directory without the zone: %q; want %q", got, dir+"/Asia/Kolkata")
	}
	t.Setenv("ZONEINFO", filepath.Join(zi, "Etc/UTC"))
	if got := goZoneFile("Etc/UTC"); got != dir+"/Etc/UTC" {
		t.Errorf("ZONEINFO a file: %q; want %q", got, dir+"/Etc/UTC")
	}
	if got := goZoneFile("Etc"); got != "" {
		t.Errorf("a directory of the name: %q; want none", got)
	}
	t.Setenv("ZONEINFO", "")

	// A refusal is kept: the table is read once per name. An error where
	// keploy cannot tell is not: the name is read again.
	zoneLocations.Clear()
	if err := os.WriteFile(table, []byte(root+"2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata "+dir+"/Europe/Berlin ro - xfs /dev/sda1 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ZoneLocation("Europe/Berlin"); !IsOwnZoneMount(err) {
		t.Fatalf("Berlin mounted from Kolkata's: %v", err)
	}
	if err := os.Remove(table); err != nil {
		t.Fatal(err)
	}
	if _, err := ZoneLocation("Europe/Berlin"); !IsOwnZoneMount(err) {
		t.Errorf("Berlin read again: %v; want the kept refusal", err)
	}
	if _, err := ZoneLocation("Asia/Kolkata"); err == nil || IsOwnZoneMount(err) {
		t.Fatalf("Kolkata, the table unreadable: %v; want an error keploy cannot tell", err)
	}
	if err := os.WriteFile(table, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if loc, err := ZoneLocation("Asia/Kolkata"); err != nil || loc.String() != "Asia/Kolkata" {
		t.Errorf("Kolkata, the table readable again: %v %v; want it read", loc, err)
	}
}
