package models

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// mountLineCap bounds one line of a mount table MountOf reads: an
// overlay's options (its lower layers) run to a few KiB.
const mountLineCap = 1 << 20

// Mount is one mount of a mount table (a /proc/<pid>/mountinfo): where it
// is mounted (field 5, a path in the view of the process the table is
// of) and its root (field 4: the path, in the filesystem it mounts, of the
// file or directory mounted there), each as the kernel writes it, escapes
// read back. Replaced is set where the kernel wrote the root with
// "//deleted" after it, cut from Root here: the file mounted there was
// unlinked since (a host's tzdata upgrade renames new zone files over the
// old), and the mount still holds the file it held.
type Mount struct {
	Point, Root string
	Replaced    bool
}

// Source is p's path in the filesystem m mounts, p a path at m's point or
// below it: m's root, and then the rest of p below m's point.
func (m Mount) Source(p string) string {
	rest := strings.TrimPrefix(p, m.Point)
	if m.Point == "/" {
		rest = p
	}
	if rest == "" {
		return m.Root
	}
	return path.Join(m.Root, rest)
}

// MountOf is the mount of the mount table in file (a /proc/<pid>/mountinfo,
// or /proc/self/mountinfo) that p, a clean absolute path in the view the
// table writes its mount points in (that process's own root), is read
// through, and true; false where the table lists no mount at p or above
// it. It walks the mounts as a lookup of p crosses them, by each mount's
// id and its parent's (fields 1 and 2), never by the order the table lists
// them in (kernel 6.8 lists them in the order they were made, and a mount
// moved in later can list before the one it sits on): from the mounts
// whose parent the table does not list (the process's root), to the child
// whose point is the first p's path crosses (the one at the shortest
// point at or above p: a mount on a directory hides what its parent
// mounted below it), and on, until no child of the mount reached is at or
// above p. An error where keploy cannot read the table, or where two
// mounts sit at the first point on p's path at one level (two of one
// parent, or two whose parents the table does not list), so keploy cannot
// tell which p is read through. Mounts whose parents loop (no table the kernel
// writes) are never reached.
func MountOf(file, p string) (Mount, bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return Mount{}, false, err
	}
	defer func() { _ = f.Close() }()
	type entry struct {
		id, parent string
		m          Mount
	}
	var ms []entry
	ids := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), mountLineCap)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		root, replaced := strings.CutSuffix(unescapeMount(fields[3]), "//deleted")
		ms = append(ms, entry{id: fields[0], parent: fields[1], m: Mount{Point: unescapeMount(fields[4]), Root: root, Replaced: replaced}})
		ids[fields[0]] = true
	}
	if err := sc.Err(); err != nil {
		return Mount{}, false, fmt.Errorf("reading %s: %w", file, err)
	}
	children := map[string][]int{}
	var level []int
	for i, e := range ms {
		if e.parent == e.id || !ids[e.parent] {
			level = append(level, i)
			continue
		}
		children[e.parent] = append(children[e.parent], i)
	}
	at := -1
	// Each step goes to a child of the mount reached, from one whose parent
	// the table does not list: no mount whose parents loop is reached (its
	// parent is in the loop), so the walk ends.
	for {
		next, tie := -1, -1
		for _, i := range level {
			mp := ms[i].m.Point
			if mp != p && mp != "/" && !strings.HasPrefix(p, mp+"/") {
				continue
			}
			switch {
			case next < 0 || len(mp) < len(ms[next].m.Point):
				next, tie = i, -1
			case len(mp) == len(ms[next].m.Point):
				tie = i
			}
		}
		if tie >= 0 {
			return Mount{}, false, fmt.Errorf("%s lists two mounts (ids %s and %s) at %s at one level of the mount tree, so keploy cannot tell which %s is read through", file, ms[next].id, ms[tie].id, ms[next].m.Point, p)
		}
		if next < 0 {
			if at < 0 {
				return Mount{}, false, nil
			}
			return ms[at].m, true, nil
		}
		at, level = next, children[ms[next].id]
	}
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

// ZoneinfoName is the IANA name a zone file's path names, after its
// zoneinfo directory ("/usr/share/zoneinfo/Asia/Kolkata": "Asia/Kolkata",
// and so of its posix/ tree, the same zones); "" where it names none.
func ZoneinfoName(p string) string {
	i := strings.LastIndex(p, "zoneinfo/")
	if i < 0 {
		return ""
	}
	n := p[i+len("zoneinfo/"):]
	n = strings.TrimPrefix(n, "posix/")
	if n == "" || strings.Contains(n, "..") {
		return ""
	}
	return n
}

// utcNames are IANA's names of UTC: each reads UTC+00:00 with no change
// of offset in every release of its data (the backward file links them
// to Etc/UTC or Etc/GMT), so a file of one mounted over another's holds
// that zone's rules (Fedora's /usr/share/zoneinfo/UTC over an image's
// Etc/UTC).
var utcNames = map[string]bool{
	"UTC": true, "Etc/UTC": true, "UCT": true, "Etc/UCT": true, "Universal": true, "Etc/Universal": true,
	"Zulu": true, "Etc/Zulu": true, "GMT": true, "Etc/GMT": true, "GMT0": true, "Etc/GMT0": true,
	"GMT+0": true, "Etc/GMT+0": true, "GMT-0": true, "Etc/GMT-0": true, "Greenwich": true, "Etc/Greenwich": true,
}

// ZoneMount is a zone file read from another zone's file through a mount
// (ZoneFileMount).
type ZoneMount struct {
	// Source is the path the file is read from in the filesystem the
	// mount comes from (Mount.Source), Zone the zone that path names by
	// its zoneinfo path ("" where it names none: a copy).
	Source, Zone string
	// Replaced: the mounted file was since replaced on the host.
	Replaced bool
}

// ZoneFileMount is what the mount table in table (MountOf) says of the zone
// file at p, a clean path in the table's view with its links resolved,
// which the process reads as the zone zn: nil where p is read as zn's
// file, as far as the mounts tell: through no mount of p itself, nor of a
// directory inside the part of p's path that names its zone (below its
// zoneinfo directory: a directory mounted at or above it keeps every
// file's name in it), or through one whose file (Mount.Source) is zn's, or
// p's own zone's (zn an alias the image links to it: Asia/Calcutta to
// Asia/Kolkata), or of another of IANA's names of UTC where zn is one
// (utcNames). Where it is another zone's file, or one whose path names no
// zone (a host's /etc/localtime that is a copy), the mount (ZoneMount): the
// C library, Go and Python read that file's rules, which are not zn's. An
// error where keploy cannot read the table.
func ZoneFileMount(table, p, zn string) (*ZoneMount, error) {
	m, ok, err := MountOf(table, p)
	if err != nil || !ok {
		return nil, err
	}
	own := ZoneinfoName(p)
	if m.Point != p {
		i := strings.LastIndex(p, "zoneinfo/")
		if i < 0 || len(m.Point) <= i+len("zoneinfo") {
			return nil, nil
		}
	}
	src := m.Source(p)
	sn := ZoneinfoName(src)
	if sn != "" && (sn == zn || sn == own || utcNames[sn] && utcNames[zn]) {
		return nil, nil
	}
	return &ZoneMount{Source: src, Zone: sn, Replaced: m.Replaced}, nil
}

// ownZoneMounts is the mount table keploy's own zone files are read
// through (ZoneLocation); a variable, so a row reads a table of its own.
var ownZoneMounts = "/proc/self/mountinfo"

// goZoneDirs are the directories time.LoadLocation reads a zone's file
// from on Linux, in its order, after $ZONEINFO's.
var goZoneDirs = []string{"/usr/share/zoneinfo/", "/usr/share/lib/zoneinfo/", "/usr/lib/locale/TZ/", "/etc/zoneinfo/"}

// errOwnZoneMount marks ZoneLocation's refusal of a name whose file in
// keploy's own zone files is mounted from another zone's (IsOwnZoneMount).
var errOwnZoneMount = errors.New("keploy's own zone file is mounted from another zone's")

// IsOwnZoneMount reports whether err is ZoneLocation's refusal of a zone
// whose file in keploy's own zone files is another zone's, mounted over it
// where keploy runs: a record made again with the same keploy reads it the
// same way.
func IsOwnZoneMount(err error) bool { return errors.Is(err, errOwnZoneMount) }

// ownZoneFileMounted is nil where the file time.LoadLocation reads name
// from (goZoneFile), if any, is name's own as far as keploy's own mounts
// tell (ZoneFileMount); an error naming both where it is mounted from
// another zone's file (a host's /etc/localtime bind-mounted over the link
// to the zone where keploy runs natively, in the application's
// container), or where keploy cannot tell. Off Linux no mount table is
// read: macOS's zone files are no container's.
func ownZoneFileMounted(name string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	f := goZoneFile(name)
	if f == "" {
		return nil
	}
	p, err := filepath.EvalSymlinks(f)
	if err != nil {
		return fmt.Errorf("keploy cannot resolve its own zone file %s of %q, so it cannot tell whether it is a file mounted from another zone's: %w", f, name, err)
	}
	m, err := ZoneFileMount(ownZoneMounts, p, name)
	if err != nil {
		return fmt.Errorf("keploy cannot read its own mounts, so it cannot tell whether its zone file %s of %q is a file mounted from another zone's: %w", p, name, err)
	}
	if m == nil {
		return nil
	}
	what := fmt.Sprintf("the zone file of %q", m.Zone)
	if m.Zone == "" {
		what = "a file whose path names no zone (a copy)"
	}
	if m.Replaced {
		what += ", since replaced on the host"
	}
	return fmt.Errorf("%w: keploy reads %q from its own zone file %s, which is a file mounted from %s, %s, so keploy cannot tell whether it holds %q's rules, which a JVM and Node read %q by in their own zone data. A host's /etc/localtime bind-mounted over the link to that zone where keploy runs (-v /etc/localtime:/etc/localtime:ro lands on the file the link names) gives this: drop the mount, or set TZ to the zone the application runs in",
		errOwnZoneMount, name, p, m.Source, what, name, name)
}

// goZoneFile is the file time.LoadLocation reads name from: under
// $ZONEINFO where that is a directory holding it, else under the first of
// Go's zone directories that holds it (goZoneDirs); "" where it reads none
// of them (keploy's built-in zone data: time/tzdata). A $ZONEINFO that is a
// zip file is passed over: Go reads name from it where it holds it, so the
// file checked is then one it does not read, never one it reads unchecked.
func goZoneFile(name string) string {
	dirs := goZoneDirs
	if z := os.Getenv("ZONEINFO"); z != "" {
		if fi, err := os.Stat(z); err == nil && fi.IsDir() {
			dirs = append([]string{z + "/"}, dirs...)
		}
	}
	for _, d := range dirs {
		if fi, err := os.Stat(d + name); err == nil && fi.Mode().IsRegular() {
			return d + name
		}
	}
	return ""
}
