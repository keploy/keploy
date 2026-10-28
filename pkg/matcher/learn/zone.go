package learn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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
	return readZone(root, tz, set, true)
}

// readZone is ReadZone; a TZ naming a zone root holds no file of is read
// from keploy's own zone files (or the ones built into it) only where own
// is set (keploy's own zone), and is an error otherwise: an application
// reads UTC there, or a copy of the zone built into it, and keploy cannot
// tell which.
func readZone(root, tz string, set, own bool) (*time.Location, error) {
	if !set {
		loc, err := zoneFile(root, "/etc/localtime")
		if err != nil && localtimeAbsent(root) {
			// No /etc/localtime at all (an Alpine or distroless image):
			// the C library, Go, Python and Node read UTC.
			return time.UTC, nil
		}
		return loc, err
	}
	tz = strings.TrimPrefix(tz, ":")
	switch {
	case tz == "" || tz == "UTC":
		return time.UTC, nil
	case strings.HasPrefix(tz, "/"):
		return zoneFile(root, tz)
	}
	if _, err := resolveUnder(root, "/usr/share/zoneinfo/"+tz); err == nil && !strings.Contains(tz, "..") {
		return zoneFile(root, "/usr/share/zoneinfo/"+tz)
	}
	if !own {
		return nil, fmt.Errorf("TZ=%s names no zone file under the process's root", tz)
	}
	return time.LoadLocation(tz)
}

// ProcZone is the zone one process writes a wall clock of no zone in, as
// the agent read it from the process (ReadProcZone, ProcZones): Loc nil
// where keploy could not read it; Name its name (models.ZoneNameOf), ""
// where it has none keploy can write.
type ProcZone struct {
	Loc  *time.Location
	Name string
}

// ReadProcZone is the zone the process pid writes its wall clock in, read
// under procRoot ("/proc"): TZ from the environment it started with
// (<pid>/environ), else its /etc/localtime, both read in its own root
// (<pid>/root, so a container's files, never keploy's), as ReadZone reads
// them, except that a TZ naming a zone its root holds no file of is an
// error. An error where keploy cannot read the process's files (another
// user's process, one gone, a pid of none), its zone, or its environment
// (an exiting process's reads empty: procExiting). An empty environment of
// a live process (env -i) names no TZ. It is the zone the process started
// with, as its environment and files name it: a zone the application sets
// as it runs (Django's TIME_ZONE, process.env.TZ in Node, Java's
// TimeZone.setDefault) is not read, nor a JVM's -Duser.timezone (a flag of
// its command line, which Java reads over TZ), nor a TZ an environment
// overwritten in place (setproctitle: gunicorn, celery) no longer holds.
// That is a stated limit: a value such an application writes in its own
// zone is read in the one it started with, and fails where the two differ.
func ReadProcZone(procRoot string, pid uint32) (ProcZone, error) {
	if pid == 0 {
		return ProcZone{}, errors.New("no process is known for the call")
	}
	dir := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10))
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return ProcZone{}, err
	}
	if len(bytes.Trim(env, "\x00")) == 0 && procExiting(procRoot, pid) {
		// A process that is exiting has let go of its memory, so its
		// environment reads empty whatever TZ it started with. Read after
		// the environment: the kernel marks the process exiting before it
		// lets go.
		return ProcZone{}, errors.New("the process is exiting, so its environment reads empty and keploy cannot tell whether it set TZ")
	}
	tz, set := "", false
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("TZ=")); ok {
			tz, set = string(v), true
			break
		}
	}
	loc, err := readZone(filepath.Join(dir, "root"), tz, set, false)
	if err != nil {
		return ProcZone{}, err
	}
	return ProcZone{Loc: loc, Name: models.ZoneNameOf(loc, time.Now())}, nil
}

// procZoneCap bounds ProcZones: a cache holding it starts again.
const procZoneCap = 4096

// ProcZones reads each process's zone once (ReadProcZone), at its first
// call: a pid another process has since taken, which its start time tells
// apart, is read again. A process keploy cannot read is read again at each
// call. It holds at most procZoneCap processes. It reads none where keploy
// runs in a PID namespace of its own (utils.KernelPidsLocalIn: a container
// without --pid=host, keploy's own docker and compose modes), where a
// kernel pid names another process or none: every zone is then unknown,
// never another process's.
type ProcZones struct {
	root  string
	local bool
	mu    sync.Mutex
	m     map[uint32]procZone
}

type procZone struct {
	start string
	z     ProcZone
}

// NewProcZones reads processes under procRoot ("/proc").
func NewProcZones(procRoot string) *ProcZones {
	return &ProcZones{root: procRoot, local: utils.KernelPidsLocalIn(procRoot), m: map[uint32]procZone{}}
}

// errPIDNamespace is ProcZones.Of's error where keploy runs in a PID
// namespace of its own.
var errPIDNamespace = errors.New("keploy runs in a PID namespace of its own, where the kernel's process numbers name other processes or none, so it reads no process's zone by one; run keploy in the host PID namespace (--pid=host, or hostPID in Kubernetes) to have it read")

// Of is the zone pid writes its wall clock in (ReadProcZone); unknown, with
// an error, on a nil c.
func (c *ProcZones) Of(pid uint32) (ProcZone, error) {
	if c == nil {
		return ProcZone{}, errors.New("no process zones are read")
	}
	if !c.local {
		return ProcZone{}, errPIDNamespace
	}
	start := procStart(c.root, pid)
	if start != "" {
		c.mu.Lock()
		e, ok := c.m[pid]
		c.mu.Unlock()
		if ok && e.start == start {
			return e.z, nil
		}
	}
	z, err := ReadProcZone(c.root, pid)
	if err != nil || start == "" || procStart(c.root, pid) != start {
		return z, err
	}
	c.mu.Lock()
	if len(c.m) >= procZoneCap {
		c.m = map[uint32]procZone{}
	}
	c.m[pid] = procZone{start: start, z: z}
	c.mu.Unlock()
	return z, nil
}

// pfExiting is the kernel's PF_EXITING, a process's flag (field 9 of
// <pid>/stat) from the moment it starts to exit.
const pfExiting = 0x4

// procExiting reports whether pid is exiting or gone as <pid>/stat shows
// it: a zombie (state Z) or a dead one (X), one flagged PF_EXITING, or one
// whose stat cannot be read.
func procExiting(root string, pid uint32) bool {
	b, err := os.ReadFile(filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return true
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 7 {
		return true
	}
	if st := f[0]; st == "Z" || st == "X" || st == "x" {
		return true
	}
	flags, err := strconv.ParseUint(f[6], 10, 64)
	return err != nil || flags&pfExiting != 0
}

// procStart is pid's start time (field 22 of <pid>/stat, after its name,
// which may hold spaces and parentheses), "" where it cannot be read.
func procStart(root string, pid uint32) string {
	if pid == 0 {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return ""
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

type procZoneKey struct{}

// WithProcZone is ctx carrying z, the zone of the process that made the
// connection ctx serves (ProcZones.Of; a zero z where keploy could not read
// it): its calls are learned in it (Tracker.DriftsOf) and a replay checks
// their clock reads in it (ClockSpanFrom).
func WithProcZone(ctx context.Context, z ProcZone) context.Context {
	return context.WithValue(ctx, procZoneKey{}, z)
}

// procZoneFrom is the zone ctx carries (WithProcZone), ok where it carries
// one, known or not.
func procZoneFrom(ctx context.Context) (ProcZone, bool) {
	if ctx == nil {
		return ProcZone{}, false
	}
	z, ok := ctx.Value(procZoneKey{}).(ProcZone)
	return z, ok
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
			return loadZone(zn, data)
		}
	}
	if b, err := readUnder(root, "/etc/timezone"); err == nil {
		zn := strings.TrimSpace(string(b))
		if zn != "" && !strings.Contains(zn, "..") {
			if same, err := readUnder(root, "/usr/share/zoneinfo/"+zn); err == nil && bytes.Equal(same, data) {
				return loadZone(zn, data)
			}
		}
	}
	return loadZone("", data)
}

// zoneLocCap bounds zoneLocs: a cache holding it starts again.
const zoneLocCap = 256

// zoneLocs are the zones zoneFile loaded, by their name and their file's
// bytes, so the processes that share a zone file (every process of one
// image, or of one host) share one *time.Location (ProcZones holds up to
// procZoneCap processes), never one parsed each. Bytes and name alike give
// one zone, so a zone file another image ships differently (other rules)
// is its own.
var zoneLocs = struct {
	mu sync.Mutex
	m  map[zoneLocKey]*time.Location
}{m: map[zoneLocKey]*time.Location{}}

type zoneLocKey struct {
	name string
	sum  [sha256.Size]byte
}

// loadZone is time.LoadLocationFromTZData(name, data), the one zoneLocs
// holds where it holds one of that name and bytes.
func loadZone(name string, data []byte) (*time.Location, error) {
	k := zoneLocKey{name: name, sum: sha256.Sum256(data)}
	zoneLocs.mu.Lock()
	loc, ok := zoneLocs.m[k]
	zoneLocs.mu.Unlock()
	if ok {
		return loc, nil
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, err
	}
	zoneLocs.mu.Lock()
	if len(zoneLocs.m) >= zoneLocCap {
		zoneLocs.m = map[zoneLocKey]*time.Location{}
	}
	zoneLocs.m[k] = loc
	zoneLocs.mu.Unlock()
	return loc, nil
}

// under is p, a path in the process's own view, as keploy reaches it under
// root: never above root, whatever ".." p holds.
func under(root, p string) string {
	return filepath.Join(root, path.Clean("/"+p))
}

// readZoneFile reads the zone file at p, a path in the process's own view,
// under root, each link on it resolved in that view (resolveUnder): its
// bytes and the path, in that view, of the file they were read from.
func readZoneFile(root, p string) ([]byte, string, error) {
	real, err := resolveUnder(root, p)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(under(root, real))
	return data, real, err
}

// readUnder reads the file at p, a path in the process's own view, under
// root (readZoneFile).
func readUnder(root, p string) ([]byte, error) {
	data, _, err := readZoneFile(root, p)
	return data, err
}

// localtimeAbsent reports whether root holds no /etc/localtime at all (not
// a link to nothing, which the C library reads as UTC too, but keploy
// refuses as a zone it cannot tell).
func localtimeAbsent(root string) bool {
	etc, err := resolveUnder(root, "/etc")
	if err != nil {
		return false
	}
	_, err = os.Lstat(under(root, path.Join(etc, "localtime")))
	return errors.Is(err, fs.ErrNotExist)
}

// resolveUnder is p, a path in the process's own view, with each link on
// it, at any of its components, resolved in that view, never keploy's: an
// absolute link's target is read from root, and ".." stops at root. The
// path it gives names no link, so the kernel follows none reading it under
// root (under), where it would read an absolute link of a directory on the
// way from keploy's own /. An error where a link names nothing, or there
// are too many.
func resolveUnder(root, p string) (string, error) {
	rest := strings.Split(path.Clean("/"+p), "/")
	cur, links := "/", 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, c)
		fi, err := os.Lstat(under(root, next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", errors.New("too many links on the path " + p)
		}
		target, err := os.Readlink(under(root, next))
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			cur = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return cur, nil
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
