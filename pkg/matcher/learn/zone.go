package learn

import (
	"bytes"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ReadZone is the zone a process writes its wall clock in, as the C library
// and Go read it, with its name where the process's files give one, so that
// a learning that depends on it names it (models.ClockOffset.AppZone,
// models.ZoneNameOf): TZ from its environment (tz, where set), else the
// zone file at /etc/localtime under root ("/" for keploy's own), read from
// root's files, never keploy's (zoneFile). A TZ of ""
// or "UTC" is UTC; an IANA name (":Asia/Kolkata" too) is that zone; a path
// is that zone file. /etc/localtime that links into a zoneinfo directory is
// the zone it names; a copied file is the zone /etc/timezone names where its
// bytes are that zone's file under root's /usr/share/zoneinfo, else its
// rules with no name (ZoneNameOf then names it by its fixed offset, where it
// has one). An error where keploy cannot read the zone: a TZ of a POSIX rule
// ("IST-5:30"), which keploy does not read (Go reads UTC there, and the C
// library the rule), or no zone file (a link to nothing).
func ReadZone(root, tz string, set bool) (*time.Location, error) {
	if !set {
		return zoneFile(root, "/etc/localtime")
	}
	tz = strings.TrimPrefix(tz, ":")
	switch {
	case tz == "" || tz == "UTC":
		return time.UTC, nil
	case strings.HasPrefix(tz, "/"):
		return zoneFile(root, tz)
	}
	if _, err := os.Lstat(under(root, "/usr/share/zoneinfo/"+tz)); err == nil && !strings.Contains(tz, "..") {
		return zoneFile(root, "/usr/share/zoneinfo/"+tz)
	}
	return time.LoadLocation(tz)
}

// zoneFile is ReadZone of the zone file at name, a path in the process's
// own view, under root: its bytes, read through its links under root
// (readZoneFile), named by the zoneinfo path its last link names, else by
// name's own, else by /etc/timezone where the bytes are that zone's file
// under root's /usr/share/zoneinfo; with no name otherwise. A file that is
// not there (a link to nothing) is an error, as Go and the C library read
// UTC there, never the zone its link names.
func zoneFile(root, name string) (*time.Location, error) {
	data, final, err := readZoneFile(root, name)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{final, name} {
		if zn := zoneinfoName(p); zn != "" {
			return time.LoadLocationFromTZData(zn, data)
		}
	}
	if b, err := os.ReadFile(under(root, "/etc/timezone")); err == nil {
		zn := strings.TrimSpace(string(b))
		if zn != "" && !strings.Contains(zn, "..") {
			if same, err := os.ReadFile(under(root, "/usr/share/zoneinfo/"+zn)); err == nil && bytes.Equal(same, data) {
				return time.LoadLocationFromTZData(zn, data)
			}
		}
	}
	return time.LoadLocationFromTZData("", data)
}

// under is p, a path in the process's own view, as keploy reaches it under
// root: never above root, whatever ".." p holds.
func under(root, p string) string {
	return filepath.Join(root, path.Clean("/"+p))
}

// readZoneFile reads the zone file at p, a path in the process's own view,
// under root, following its links there (a link's target, absolute or
// relative, is a path of the process's view, never keploy's): its bytes
// and the path, in that view, of the file they were read from.
func readZoneFile(root, p string) ([]byte, string, error) {
	p = path.Clean("/" + p)
	for range 8 {
		target, err := os.Readlink(under(root, p))
		if err != nil {
			data, err := os.ReadFile(under(root, p))
			return data, p, err
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(p), target)
		}
		p = path.Clean("/" + target)
	}
	return nil, "", errors.New("too many links to the zone file " + p)
}

// zoneinfoName is the IANA name a zone file's path names, after its
// zoneinfo directory ("/usr/share/zoneinfo/Asia/Kolkata": "Asia/Kolkata",
// and so of its posix/ tree, the same zones); "" where it names none.
func zoneinfoName(p string) string {
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
