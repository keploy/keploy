package learn

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// ReadZone is the zone a process writes its wall clock in, as the C library
// and Go read it, with its name where the process's files give one, so that
// a learning that depends on it names it (models.ClockOffset.AppZone,
// models.ZoneNameOf): TZ from its environment (tz, where set), else the
// zone file at /etc/localtime under root ("/" for keploy's own), read from
// root's files, never keploy's (zoneFileIn). root is a directory in keploy's
// own view, its files read through keploy's own mounts (a process's root,
// read through its own, is ReadProcZone's). A TZ of ""
// or "UTC" is UTC where the file the C library may read for it is absent
// or UTC's (utcZone); an IANA name (":Asia/Kolkata" too) is that zone; a path
// is that zone file. /etc/localtime that links into a zoneinfo directory is
// the zone it names; a copied file is the zone /etc/timezone names where its
// bytes are that zone's file under root's /usr/share/zoneinfo, else its
// rules with no name (ZoneNameOf then names it by its fixed offset, where it
// has one). An error where keploy cannot read the zone: a TZ of a POSIX rule
// ("IST-5:30"), which keploy does not read (Go reads UTC there, and the C
// library the rule), no zone file (a link to nothing), or a file whose rules
// are not those of the zone its name gives (loadZone), or that is mounted
// from another zone's file (mountedZone).
func ReadZone(root, tz string, set bool) (*time.Location, error) {
	return readZone(root, ownMounts, tz, set, true)
}

// readZone is ReadZone, of a root whose files are read through mounts
// (mountedZone); a TZ naming a zone root holds no file of is read
// from keploy's own zone files (or the ones built into it) only where own
// is set (keploy's own zone), and is an error otherwise: an application
// reads UTC there, or a copy of the zone built into it, and keploy cannot
// tell which.
func readZone(root string, mounts zoneMounts, tz string, set, own bool) (*time.Location, error) {
	return readZoneIn(dirFiles{root: root, mounts: mounts}, tz, set, own)
}

// readZoneIn is readZone of the root whose files f gives.
func readZoneIn(f ZoneFiles, tz string, set, own bool) (*time.Location, error) {
	if !set {
		loc, err := zoneFileIn(f, "/etc/localtime")
		if err != nil && localtimeAbsentIn(f) {
			// No /etc/localtime at all (an Alpine or distroless image):
			// the C library, Go, Python and Node read UTC.
			return time.UTC, nil
		}
		return loc, err
	}
	tz = strings.TrimPrefix(tz, ":")
	switch {
	case tz == "" || tz == "UTC":
		return utcZone(f, tz)
	case strings.HasPrefix(tz, "/"):
		return zoneFileIn(f, tz)
	}
	_, rerr := resolveIn(f, "/usr/share/zoneinfo/"+tz)
	if rerr == nil && !strings.Contains(tz, "..") {
		return zoneFileIn(f, "/usr/share/zoneinfo/"+tz)
	}
	if !own {
		if rerr != nil && !absent(rerr) && !isLasting(rerr) {
			return nil, fmt.Errorf("keploy cannot read the zone file TZ=%s names under the process's root: %w", tz, rerr)
		}
		return nil, lasting(fmt.Errorf("TZ=%s names no zone file under the process's root", tz))
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

// lastingError is an error of a process's zone that the process's own
// environment, command line or files give (a user.timezone Java does not
// know, an /etc/timezone and an /etc/localtime that disagree, a TZ naming
// no zone file, a link to nothing), so a read gives it again for as long
// as the process runs its program: ProcZones keeps it for the process, as
// it keeps a zone, never reading the process again at each call. An error
// of keploy's own reads (a file it cannot read, a process exiting or gone)
// is not lasting: it is read again. Lasting only where the process still
// runs that program after the read (ProcZones.Of checks it): a gone
// process's files read as absent.
type lastingError struct{ error }

func (e lastingError) Unwrap() error { return e.error }

// lasting is err, kept for the process's life (lastingError); nil for nil.
func lasting(err error) error {
	if err == nil {
		return nil
	}
	return lastingError{err}
}

// absent reports whether err says a path names nothing, as the process's
// own files give it: no such file (ENOENT), or a component on its way that
// is no directory (ENOTDIR).
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// isLasting reports whether err, or an error it wraps, is lasting
// (lastingError).
func isLasting(err error) bool {
	var l lastingError
	return errors.As(err, &l)
}

// ReadProcZone is the zone the process pid writes its wall clock in, read
// under procRoot ("/proc"): TZ from the environment it started with
// (<pid>/environ), else its /etc/localtime, both read in its own root
// (<pid>/root, so a container's files, never keploy's), as ReadZone reads
// them, except that a TZ naming a zone its root holds no file of is an
// error. An error where keploy cannot read the process's files (another
// user's process, one gone, a pid of none), its zone, or its environment
// (an exiting process's reads empty: procExiting). An empty environment of
// a live process (env -i) names no TZ. A JVM's user.timezone, which Java
// reads over TZ, is its zone where one of its options sets it
// (javaZoneID: -Duser.timezone= in JAVA_TOOL_OPTIONS, JDK_JAVA_OPTIONS,
// its command line or _JAVA_OPTIONS, the last the JVM reads winning), read
// as Java reads it (javaZone), and an error where keploy cannot read it; a
// JVM whose options set none reads its zone as the JDK does, not as the C
// library: TZ, else /etc/timezone, else the zone /etc/localtime links to or
// copies, in Java's own zone data (javaPlatformZone). It
// is the zone the process started with, as its environment, command line
// and files name it: a zone the application sets as it runs (Django's
// TIME_ZONE, process.env.TZ in Node, Java's TimeZone.setDefault) is not
// read, nor a JVM option in a file it names (@argfiles,
// -XX:VMOptionsFile), nor a TZ an environment overwritten in place
// (setproctitle: gunicorn, celery) no longer holds. That is a stated limit:
// a value such an application writes in its own zone is read in the one it
// started with, and fails where the two differ.
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
	// The first of each, as getenv reads it.
	tz, set := "", false
	opts := map[string]string{}
	for _, kv := range bytes.Split(env, []byte{0}) {
		k, v, ok := bytes.Cut(kv, []byte("="))
		if !ok {
			continue
		}
		switch name := string(k); name {
		case "TZ":
			if !set {
				tz, set = string(v), true
			}
		case "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "OPENJ9_JAVA_OPTIONS", "IBM_JAVA_OPTIONS":
			if _, seen := opts[name]; !seen {
				opts[name] = string(v)
			}
		}
	}
	id, javaSet, jvm, err := javaZoneID(procRoot, pid, dir, opts)
	if err != nil {
		return ProcZone{}, err
	}
	var loc *time.Location
	switch {
	case javaSet:
		loc, err = javaZone(id)
	case jvm:
		loc, err = javaPlatformZone(filepath.Join(dir, "root"), tz, set)
	default:
		loc, err = readZone(filepath.Join(dir, "root"), zoneMounts{table: filepath.Join(dir, "mountinfo"), proc: true}, tz, set, false)
	}
	if err != nil {
		return ProcZone{}, err
	}
	return ProcZone{Loc: loc, Name: models.ZoneNameOf(loc, time.Now())}, nil
}

// javaZoneID is the user.timezone the JVM process under dir (a /proc/<pid>
// entry) starts with, where one of its options sets it: the last
// -Duser.timezone in the order the JVM reads them, a later one overriding
// an earlier: JAVA_TOOL_OPTIONS (env, the opts of its environment), then
// JDK_JAVA_OPTIONS (which the java launcher alone reads, from Java 9,
// before its command line), then its command line's options (before its
// main class, -jar or -m: what follows is the application's; an @argfile
// among them read in its place, from the process's own files), then
// _JAVA_OPTIONS. set false where none sets one, where the last sets it
// empty (-Duser.timezone= or -Duser.timezone: Java then reads its
// platform's zone, as it does one unset: javaPlatformZone), or where the
// process is no JVM (it maps no libjvm.so: another program's options are
// no JVM's). jvm reports whether the process is a JVM. An error where
// keploy cannot read whether the process is a JVM, its command line, an
// argfile, or an OpenJ9 option variable (OPENJ9_JAVA_OPTIONS,
// IBM_JAVA_OPTIONS) that sets it, whose order keploy does not read; and
// where the process is exiting (its command line or its maps read empty).
func javaZoneID(procRoot string, pid uint32, dir string, env map[string]string) (id string, set, jvm bool, err error) {
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return "", false, false, fmt.Errorf("keploy cannot read the process's command line, which may set a JVM's zone: %w", err)
	}
	if len(cmdline) == 0 && procExiting(procRoot, pid) {
		return "", false, false, errors.New("the process is exiting, so its command line reads empty and keploy cannot tell whether it sets a JVM's zone")
	}
	lib, jvmRead := "", false
	isJVM := func() (bool, error) {
		if !jvmRead {
			l, err := mapsJVM(dir)
			if err != nil {
				return false, fmt.Errorf("keploy cannot tell whether the process is a JVM, whose options may set its zone: %w", err)
			}
			if l == "" && procExiting(procRoot, pid) {
				return false, errors.New("the process is exiting, so its maps read empty and keploy cannot tell whether it is a JVM")
			}
			lib, jvmRead = l, true
		}
		return lib != "", nil
	}
	type option struct{ from, id string }
	var sets []option
	add := func(from string, args []string) {
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "-Duser.timezone="); ok {
				sets = append(sets, option{from, v})
			} else if a == "-Duser.timezone" {
				sets = append(sets, option{from, ""})
			}
		}
	}
	for _, v := range []string{"OPENJ9_JAVA_OPTIONS", "IBM_JAVA_OPTIONS"} {
		if add(v, javaOptions(env[v])); len(sets) > 0 {
			j, err := isJVM()
			refused := fmt.Errorf("%s sets a JVM's user.timezone, which keploy does not read (OpenJ9's options' order)", v)
			switch {
			case err != nil:
				return "", false, j, errors.Join(refused, err)
			case j:
				return "", false, j, lasting(refused)
			}
			sets = nil
		}
	}
	add("JAVA_TOOL_OPTIONS", javaOptions(env["JAVA_TOOL_OPTIONS"]))
	add("JDK_JAVA_OPTIONS", javaOptions(env["JDK_JAVA_OPTIONS"]))
	argv := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	args, err := jvmArgs(argv, func(file string) ([]string, error) {
		// An argfile is read only of a JVM: "@..." is no file to any other
		// program.
		if j, err := isJVM(); err != nil {
			return nil, err
		} else if !j {
			return nil, errNoJVM
		}
		return readArgfile(dir, file)
	})
	switch {
	case errors.Is(err, errNoJVM):
		return "", false, false, nil
	case err != nil:
		return "", false, jvmRead && lib != "", err
	}
	add("the command line", args)
	add("_JAVA_OPTIONS", javaOptions(env["_JAVA_OPTIONS"]))
	if j, err := isJVM(); err != nil || !j || len(sets) == 0 {
		return "", false, j, err
	}
	for i := len(sets) - 1; i >= 0; i-- {
		if sets[i].from == "JDK_JAVA_OPTIONS" {
			exe, err := os.Readlink(filepath.Join(dir, "exe"))
			if err != nil {
				return "", false, true, fmt.Errorf("JDK_JAVA_OPTIONS sets a JVM's user.timezone, which the java launcher alone reads, but keploy cannot read the process's executable: %w", err)
			}
			// The java launcher reads it from Java 9; Java 8's libjvm.so
			// sits under jre/lib.
			if path.Base(strings.TrimSuffix(exe, " (deleted)")) != "java" || strings.Contains(lib, "/jre/lib/") {
				continue
			}
		}
		return sets[i].id, sets[i].id != "", true, nil
	}
	return "", false, true, nil
}

// errNoJVM: the process is no JVM, so its arguments are no JVM's.
var errNoJVM = errors.New("the process is no JVM")

// javaOptions splits a JVM options variable (JAVA_TOOL_OPTIONS,
// _JAVA_OPTIONS, JDK_JAVA_OPTIONS) into its options as the JVM does: at
// white space outside quotes, a pair of ' or " quoting the space it holds,
// the quotes themselves dropped.
func javaOptions(v string) []string {
	return splitJava(v, false)
}

// splitJava splits v at white space outside quotes, a pair of ' or "
// quoting the space it holds, the quotes dropped; of an argfile (file), a
// # that starts an argument comments out the rest of its line, and a
// backslash in quotes escapes the next character (\n, \t, \r, \f as
// they name).
func splitJava(v string, file bool) []string {
	var (
		out     []string
		cur     strings.Builder
		quote   rune
		in, esc bool
		comment bool
	)
	for _, r := range v {
		switch {
		case comment:
			if r == '\n' || r == '\r' {
				comment = false
			}
		case esc:
			esc = false
			switch r {
			case 'n':
				r = '\n'
			case 't':
				r = '\t'
			case 'r':
				r = '\r'
			case 'f':
				r = '\f'
			}
			cur.WriteRune(r)
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case file && r == '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case file && r == '#' && !in:
			comment = true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// readArgfile is the arguments of the java launcher's argfile name (an
// @argfile's path) of the process under dir, read in its own view: an
// absolute path under its root, a relative one under its working
// directory (splitJava).
func readArgfile(dir, name string) ([]string, error) {
	p := name
	if !path.IsAbs(p) {
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		if err != nil {
			return nil, fmt.Errorf("keploy cannot read the JVM's working directory, where its argfile @%s is: %w", name, err)
		}
		p = path.Join(cwd, p)
	}
	data, err := readUnder(filepath.Join(dir, "root"), p)
	if err != nil {
		err = fmt.Errorf("keploy cannot read the JVM's argfile @%s, which may set its zone: %w", name, err)
		if absent(err) {
			// None there (a build tool's argfile, removed once the JVM
			// read it): none there at the next read either.
			err = lasting(err)
		}
		return nil, err
	}
	return splitJava(string(data), true), nil
}

// javaArgOptions are the java launcher's options whose value is the next
// argument (its own -cp, -p, --add-modules and the like).
var javaArgOptions = map[string]bool{
	"-cp": true, "-classpath": true, "--class-path": true,
	"-p": true, "--module-path": true, "--upgrade-module-path": true,
	"--add-modules": true, "--enable-native-access": true, "--limit-modules": true,
	"--add-exports": true, "--add-opens": true, "--add-reads": true, "--patch-module": true,
	"--source": true,
}

// jvmArgs is the JVM's own options of argv, a process's command line: the
// arguments after argv[0] up to its main class, its source file, -jar or
// -m (--module, --module=<module>), past which they are the application's,
// never the JVM's, the value of an option that takes the next argument
// (javaArgOptions) skipped. An @argfile among them (until
// --disable-@files; @@ is a literal @) is the arguments argfile reads of
// it, in its place, as the java launcher expands it; an error where it
// cannot read one.
func jvmArgs(argv []string, argfile func(string) ([]string, error)) ([]string, error) {
	var out []string
	q := append([]string(nil), argv...)
	files := true
	// expand puts the arguments of the argfile at i in its place, as the
	// launcher does before it reads them, a value's place too (-cp @file).
	expand := func(i int) error {
		if i >= len(q) || !files || !strings.HasPrefix(q[i], "@") || strings.HasPrefix(q[i], "@@") {
			return nil
		}
		more, err := argfile(q[i][1:])
		if err != nil {
			return err
		}
		q = append(q[:i:i], append(more, q[i+1:]...)...)
		return nil
	}
	for i := 1; i < len(q); i++ {
		if err := expand(i); err != nil {
			return nil, err
		}
		if i >= len(q) {
			break
		}
		a := q[i]
		switch {
		case a == "--disable-@files":
			files = false
		case a == "-jar" || a == "-m" || a == "--module" || strings.HasPrefix(a, "--module=") || !strings.HasPrefix(a, "-"):
			return out, nil
		case javaArgOptions[a]:
			if err := expand(i + 1); err != nil {
				return nil, err
			}
			i++
		default:
			out = append(out, a)
		}
	}
	return out, nil
}

// mapsJVM is the path of the JVM's libjvm.so the process under dir maps
// (HotSpot's and OpenJ9's), read from its maps; "" where it maps none.
func mapsJVM(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "maps"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSuffix(sc.Text(), " (deleted)")
		if strings.HasSuffix(l, "/libjvm.so") {
			return l[strings.IndexByte(l, '/'):], nil
		}
	}
	return "", sc.Err()
}

// javaZone is the zone a JVM reads its user.timezone id in: UTC of "UTC"
// and "GMT"; a fixed offset of a custom id ("GMT+05:30", "GMT-8",
// "GMT+0530"), as Java parses it; else an IANA name or link ("Asia/Kolkata",
// "CET", "Japan"), read as keploy reads a stored zone (models.ZoneLocation:
// Java reads its own zone data, never the process's files). An error for
// any other id, never another zone (an old three-letter id is the region
// or offset Java maps it to, javaShortIDs: IST is Asia/Kolkata), such as a name
// only a host's zone directory holds (localtime, posixrules, Factory,
// posix/ and right/ ones), which Java does not know and reads at the
// platform's offset.
func javaZone(id string) (*time.Location, error) {
	if id == "UTC" || id == "GMT" {
		return time.UTC, nil
	}
	if r, ok := javaShortIDs[id]; ok {
		id = r
	}
	if r, ok := strings.CutPrefix(id, "GMT"); ok {
		off, ok := javaCustomOffset(r)
		if !ok {
			return nil, lasting(fmt.Errorf("a JVM's user.timezone %q is no zone keploy reads", id))
		}
		if off == 0 {
			return time.UTC, nil
		}
		return time.FixedZone("", off), nil
	}
	switch {
	case id == "localtime" || id == "posixrules" || id == "Factory" || strings.HasPrefix(id, "posix/") || strings.HasPrefix(id, "right/") || strings.Contains(id, ".."),
		!javaRegionID(id):
		return nil, lasting(fmt.Errorf("a JVM's user.timezone %q is no zone a JVM reads", id))
	}
	loc, err := models.ZoneLocation(id)
	if err != nil {
		return nil, lasting(fmt.Errorf("a JVM's user.timezone %q is no zone keploy reads: %w", id, err))
	}
	return loc, nil
}

// javaRegionID reports whether id is spelt as a region of Java's zone data
// is: names joined by "/", each a letter and then letters, digits, "_",
// "-" or "+" ("America/Port-au-Prince", "Etc/GMT+5"). A spelling the
// host's zone files read too ("Europe//Berlin", "./UTC") is no id Java
// knows, nor an offset with no "GMT" before it ("+05:30"), which is no
// custom id to Java's TimeZone.
func javaRegionID(id string) bool {
	letter := func(r rune) bool { return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' }
	for _, n := range strings.Split(id, "/") {
		if n == "" || !letter(rune(n[0])) {
			return false
		}
		for _, r := range n {
			if ok := letter(r) || '0' <= r && r <= '9' || r == '_' || r == '-' || r == '+'; !ok {
				return false
			}
		}
	}
	return true
}

// javaPlatformZone is the zone a JVM whose options set no user.timezone
// reads, under root (its /proc/<pid>/root, so a container's files are
// read, never keploy's), as the JDK reads it on Linux (TimeZone_md.c:
// findJavaTZ_md, getPlatformTimeZoneID), not as the C library does: TZ
// where it is set and not empty (an empty TZ is unset to Java, where the C
// library reads UTC); else the zone /etc/localtime's own link names, after
// its first "zoneinfo/" (read as a name, so a link to a file the image
// lacks names it too); else, of a copy (or a link naming no zoneinfo/),
// the zone file under /usr/share/zoneinfo of the same bytes
// (javaZoneinfoMatch); else GMT, which Java reads as UTC: no
// /etc/localtime, or a copy of no zone file there (an image that copied
// one in without its zone directory). JDK 8 and 11, and 17, 21 and 22-24
// before JDK-8345213 (JDK 25; 17 and 21 updates from late 2025), read the
// first line of Debian's /etc/timezone before /etc/localtime; keploy does
// not read which JDK the process runs, so where /etc/timezone names a zone
// it must be the one /etc/localtime gives, else an error. Likewise JDK 21
// and later clean a link's "//", "." and ".." before reading its name and
// older ones do not, so a link those change is an error. The id, a leading
// ":" and then "posix/" dropped, is read as Java reads it (javaZone), in
// keploy's zone data, which stands for Java's own (an id one knows and the
// other does not, a zone newer than the JVM's, is read in keploy's). An
// error where keploy cannot read one of those files or the process's root
// (an exited process's reads nothing), and where Java does not know the id
// (a POSIX rule "IST-5:30", a path, an empty one): Java then reads the C
// library's offset at its start, a fixed one, which keploy does not.
func javaPlatformZone(root, tz string, set bool) (*time.Location, error) {
	id, from := tz, "TZ"
	if !set || tz == "" {
		var err error
		if id, from, err = javaPlatformID(root); err != nil {
			return nil, err
		}
		if from == "" {
			return time.UTC, nil
		}
	}
	id = javaStrip(id)
	loc, err := javaZone(id)
	if err != nil {
		// Lasting, as javaZone's err is.
		return nil, fmt.Errorf("a JVM with no user.timezone reads its zone from %s, %q, which Java does not know, so it reads the C library's offset at its start, which keploy does not: %w", from, id, err)
	}
	return loc, nil
}

// javaStrip is a platform zone id as the JDK reads it: a leading ":" and
// then "posix/" dropped.
func javaStrip(id string) string {
	return strings.TrimPrefix(strings.TrimPrefix(id, ":"), "posix/")
}

// javaPlatformID is the zone id a JVM reads where TZ names none
// (javaPlatformZone), and the file it is read from; from "" where Java
// reads GMT.
func javaPlatformID(root string) (id, from string, err error) {
	// Each absence below is read in a root keploy can read: an exited
	// process's root reads as absent everywhere, never GMT.
	alive := func() error {
		if _, err := os.Stat(root); err != nil {
			return fmt.Errorf("keploy cannot read the process's root, where a JVM reads its zone: %w", err)
		}
		return nil
	}
	if err := alive(); err != nil {
		return "", "", err
	}
	id, from, err = javaLocaltimeID(root)
	if err == nil && from == "" {
		err = alive()
	}
	tzid, ok, tzerr := javaTimezoneLine(root)
	switch {
	case tzerr != nil:
		return "", "", tzerr
	case !ok:
		return id, from, err
	case err != nil:
		return "", "", fmt.Errorf("the process's /etc/timezone names %q, which JDKs before JDK-8345213 read as its zone, and keploy cannot read the zone later ones read from /etc/localtime: %w", tzid, err)
	case javaStrip(tzid) != javaStrip(id):
		other := id
		if from == "" {
			other = "GMT"
		}
		return "", "", lasting(fmt.Errorf("the process's /etc/timezone names %q, which JDKs before JDK-8345213 read as its zone, and its /etc/localtime %q, which later ones read; keploy does not read which JDK the process runs", tzid, other))
	}
	return id, from, nil
}

// javaTimezoneLine is the first line of root's /etc/timezone, as fgets
// reads it into 256 bytes, to its newline, then as strlen reads it, to a
// NUL; false where there is none, it is empty, or it is no file (a
// directory, which docker makes of a bind of a host file that is not
// there: fgets reads nothing of it). An error where keploy cannot read it,
// or a JVM may not (other users cannot read it).
func javaTimezoneLine(root string) (string, bool, error) {
	b, err := readUnder(root, "/etc/timezone")
	var nr errNotRegular
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.As(err, &nr) && nr.mode.IsDir():
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("keploy cannot read the process's /etc/timezone, which a JVM may read its zone from: %w", err)
	}
	if r, err := resolveUnder(root, "/etc/timezone"); err != nil {
		return "", false, err
	} else if fi, err := os.Stat(under(root, r)); err != nil {
		return "", false, fmt.Errorf("keploy cannot read the process's /etc/timezone, which a JVM may read its zone from: %w", err)
	} else if fi.Mode().Perm()&0o004 == 0 {
		return "", false, lasting(errors.New("the process's /etc/timezone, which a JVM may read its zone from, is not readable by every user, so keploy cannot tell whether the JVM reads it"))
	}
	line := b[:min(len(b), 255)]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	}
	return string(line), len(line) > 0, nil
}

// javaLocaltimeID is the zone id a JVM reads from root's /etc/localtime
// (javaPlatformZone), and where it read it; from "" where Java reads GMT.
// Of a link, it is the zone the link names, whatever rules the file there
// holds (a host's /etc/localtime bind-mounted on it, which the C library
// reads and loadZone refuses): a JVM reads that name in its own zone data,
// and writes its wall clock in that zone.
func javaLocaltimeID(root string) (id, from string, err error) {
	etc, err := resolveUnder(root, "/etc")
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("keploy cannot read the process's /etc, where a JVM reads its zone: %w", err)
	}
	lt := under(root, path.Join(etc, "localtime"))
	fi, err := os.Lstat(lt)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("keploy cannot read the process's /etc/localtime, which a JVM reads its zone from: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(lt)
		if err != nil {
			return "", "", fmt.Errorf("keploy cannot read the process's /etc/localtime link, which a JVM reads its zone from: %w", err)
		}
		// The part after the first "zoneinfo/", as JDK 21 and later read
		// it (its "//", "." and ".." cleaned) and as older ones do (not).
		_, raw, rawOK := strings.Cut(target, "zoneinfo/")
		_, clean, cleanOK := strings.Cut(path.Clean(target), "zoneinfo/")
		switch {
		case rawOK && cleanOK && raw == clean:
			return clean, "the /etc/localtime link", nil
		case rawOK || cleanOK:
			return "", "", lasting(fmt.Errorf("the process's /etc/localtime links to %q, which JDK 21 and later read as zone %q and older ones as %q; keploy does not read which JDK the process runs", target, clean, raw))
		}
	}
	data, err := readUnder(root, "/etc/localtime")
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("keploy cannot read the process's /etc/localtime, which a JVM reads its zone from: %w", err)
	}
	n, err := javaZoneinfoMatch(root, data)
	if err != nil || n == "" {
		return "", "", err
	}
	return n, "the zone file /etc/localtime copies", nil
}

// javaZoneinfoMatchCap bounds the entries javaZoneinfoMatch lists.
var javaZoneinfoMatchCap = 20000 // a var, which a test lowers

// javaZoneinfoMatch is the zone id the JDK finds for a copied
// /etc/localtime holding data (findZoneinfoFile): the name, after
// "zoneinfo/", of a file under root's /usr/share/zoneinfo holding the same
// bytes, its UTC and then its GMT first, then each file its directories
// list, links followed, names starting with "." and ROC, posixrules and
// localtime skipped; "" where none does (Java then reads GMT). The JDK
// takes the first its directory listing gives, in an order keploy does not
// read, so where another file holds the same bytes too, each must be a zone
// Java knows (javaZone, a "posix/" dropped), else an error: of the same
// bytes, they are one zone's rules. Where a directory links to one already
// listed (posix -> ., in some images' zone directories), the JDK lists it
// again under the link's name, deeper each time, and which name it finds
// first turns on that order too: an error where anything matched. An error,
// too, where keploy cannot read an entry for any reason but its absence.
func javaZoneinfoMatch(root string, data []byte) (string, error) {
	const dir = "/usr/share/zoneinfo"
	if _, err := resolveUnder(root, dir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("keploy cannot read the process's %s, where a JVM finds the zone its /etc/localtime copies: %w", dir, err)
	}
	unread := func(p string, err error) error {
		return fmt.Errorf("keploy cannot read %s in the process's zone directory, where a JVM finds the zone its /etc/localtime copies: %w", p, err)
	}
	same := func(p string) (ok, isDir bool, err error) {
		r, err := resolveUnder(root, p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		} else if err != nil {
			return false, false, unread(p, err)
		}
		fi, err := os.Stat(under(root, r))
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		} else if err != nil {
			return false, false, unread(p, err)
		}
		if fi.IsDir() {
			return false, true, nil
		}
		if !fi.Mode().IsRegular() || fi.Size() != int64(len(data)) {
			return false, false, nil
		}
		b, err := readUnder(root, r)
		if err != nil {
			return false, false, unread(p, err)
		}
		return bytes.Equal(b, data), false, nil
	}
	for _, n := range []string{"UTC", "GMT"} {
		if ok, _, err := same(dir + "/" + n); err != nil {
			return "", err
		} else if ok {
			return n, nil
		}
	}
	var found []string
	seen, files, aliased := map[string]bool{}, 0, false
	var walk func(p string) error
	walk = func(p string) error {
		r, err := resolveUnder(root, p)
		if err != nil {
			return unread(p, err)
		}
		if seen[r] {
			aliased = true
			return nil
		}
		seen[r] = true
		// os.ReadDir opens with O_DIRECTORY: a device or a FIFO in a
		// directory's place is refused (ENOTDIR) before its open runs.
		ents, err := os.ReadDir(under(root, r))
		if err != nil {
			return unread(p, err)
		}
		for _, e := range ents {
			n := e.Name()
			if strings.HasPrefix(n, ".") || n == "ROC" || n == "posixrules" || n == "localtime" {
				continue
			}
			if files++; files > javaZoneinfoMatchCap {
				return lasting(fmt.Errorf("the process's %s holds more than %d files, so keploy does not search it for the zone its /etc/localtime copies", dir, javaZoneinfoMatchCap))
			}
			q := p + "/" + n
			ok, isDir, err := same(q)
			switch {
			case err != nil:
				return err
			case isDir:
				if err := walk(q); err != nil {
					return err
				}
			case ok:
				found = append(found, strings.TrimPrefix(q, dir+"/"))
			}
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return "", err
	}
	if aliased && len(found) > 0 {
		return "", lasting(fmt.Errorf("the process's %s links a directory to one a JVM lists already, so the name it finds first for the zone its /etc/localtime copies (%s) turns on its directory listing's order", dir, found[0]))
	}
	for _, n := range found {
		if _, err := javaZone(strings.TrimPrefix(n, "posix/")); err != nil {
			// Lasting, as javaZone's err is.
			return "", fmt.Errorf("the process's /etc/localtime copies its zone file %s, which Java does not know, and a JVM may find that one first: %w", n, err)
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	return found[0], nil
}

// javaShortIDs is how Java reads its old three-letter ids (ZoneId.SHORT_IDS,
// which TimeZone reads too): each a region, or a fixed offset for EST, MST
// and HST.
var javaShortIDs = map[string]string{
	"ACT": "Australia/Darwin", "AET": "Australia/Sydney", "AGT": "America/Argentina/Buenos_Aires",
	"ART": "Africa/Cairo", "AST": "America/Anchorage", "BET": "America/Sao_Paulo", "BST": "Asia/Dhaka",
	"CAT": "Africa/Harare", "CNT": "America/St_Johns", "CST": "America/Chicago", "CTT": "Asia/Shanghai",
	"EAT": "Africa/Addis_Ababa", "ECT": "Europe/Paris", "IET": "America/Indiana/Indianapolis",
	"IST": "Asia/Kolkata", "JST": "Asia/Tokyo", "MIT": "Pacific/Apia", "NET": "Asia/Yerevan",
	"NST": "Pacific/Auckland", "PLT": "Asia/Karachi", "PNT": "America/Phoenix", "PRT": "America/Puerto_Rico",
	"PST": "America/Los_Angeles", "SST": "Pacific/Guadalcanal", "VST": "Asia/Ho_Chi_Minh",
	"EST": "GMT-05:00", "MST": "GMT-07:00", "HST": "GMT-10:00",
}

// javaCustomOffset is the offset, in seconds east of UTC, of the part of a
// custom zone id after "GMT", as Java's TimeZone parses it: a sign, then
// hours of one or two digits and minutes of two after a colon, or one to
// four digits with no colon (hours, or hours and minutes, HHMM); hours up
// to 23, minutes up to 59. false for any other.
func javaCustomOffset(r string) (int, bool) {
	if len(r) < 2 || r[0] != '+' && r[0] != '-' {
		return 0, false
	}
	sign := 1
	if r[0] == '-' {
		sign = -1
	}
	hs, ms, colon := strings.Cut(r[1:], ":")
	if !isDigitsOnly(hs) || colon && (len(hs) > 2 || len(ms) != 2 || !isDigitsOnly(ms)) || !colon && len(hs) > 4 {
		return 0, false
	}
	n, _ := strconv.Atoi(hs)
	h, m := n, 0
	if colon {
		m, _ = strconv.Atoi(ms)
	} else if len(hs) > 2 {
		h, m = n/100, n%100
	}
	if h > 23 || m > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60), true
}

// isDigitsOnly reports whether s is one or more ASCII digits.
func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// procZoneCap bounds ProcZones: a cache holding it starts again.
const procZoneCap = 4096

// ProcZones reads each process's zone once (ReadProcZone), at its first
// call, as a JVM and the C library read their zone once: a pid that has
// since run another program (procID: another process took its number, or
// it exec'd one) is read again. An error the process's own files give
// (lastingError) is kept as a zone is; one keploy could not read (a file it
// cannot read, a process exiting or gone) is read again at each call. It
// holds at most procZoneCap processes. It reads none where keploy runs in a
// PID namespace of its own (utils.KernelPidsLocalIn: a container without
// --pid=host, keploy's own docker and compose modes), where a kernel pid
// names another process or none: every zone is then unknown, never another
// process's.
type ProcZones struct {
	root  string
	local bool
	read  func(procRoot string, pid uint32) (ProcZone, error) // ReadProcZone
	mu    sync.Mutex
	m     map[uint32]procZone
}

type procZone struct {
	id  procID
	z   ProcZone
	err error // lasting (lastingError); z is then unknown
}

// NewProcZones reads processes under procRoot ("/proc").
func NewProcZones(procRoot string) *ProcZones {
	return &ProcZones{root: procRoot, local: utils.KernelPidsLocalIn(procRoot), read: ReadProcZone, m: map[uint32]procZone{}}
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
	id, _, ok := procIdent(c.root, pid)
	if !ok {
		return ProcZone{}, errors.New("keploy cannot tell which program the process runs: its stat cannot be read (it exited) or shows no field 51 (a kernel before 3.5)")
	}
	c.mu.Lock()
	e, hit := c.m[pid]
	c.mu.Unlock()
	if hit && e.id == id {
		return e.z, e.err
	}
	z, err := c.read(c.root, pid)
	if err != nil && !isLasting(err) {
		return ProcZone{}, err
	}
	after, exiting, okAfter := procIdent(c.root, pid)
	if !okAfter || after != id {
		// The process exited, ran another program or another took its
		// number as keploy read it: what it read may be either's, or
		// none's.
		return ProcZone{}, errors.New("the process exited, ran another program or another took its number while keploy read its zone")
	}
	if exiting {
		// It lets go of its files as it exits, so an error keploy read may
		// be their absence: never kept. A zone it read is its own (an
		// absence read as a zone, no /etc/localtime, is read in a root
		// keploy could still read after it: localtimeAbsentIn,
		// javaPlatformID), so it is this call's, never kept either: the
		// pid is soon another's.
		return z, err
	}
	c.mu.Lock()
	if len(c.m) >= procZoneCap {
		c.m = map[uint32]procZone{}
	}
	c.m[pid] = procZone{id: id, z: z, err: err}
	c.mu.Unlock()
	return z, err
}

// pfExiting is the kernel's PF_EXITING, a process's flag (field 9 of
// <pid>/stat) from the moment it starts to exit.
const pfExiting = 0x4

// procStat is the fields of pid's <pid>/stat after its name (which may hold
// spaces and parentheses): field n of proc(5) at [n-3]; ok false where it
// cannot be read.
func procStat(root string, pid uint32) ([]string, bool) {
	if pid == 0 {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return nil, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil, false
	}
	return strings.Fields(string(b[i+1:])), true
}

// statExiting reports whether the stat fields f (procStat) show a process
// exiting or gone: a zombie (state Z) or a dead one (X), one flagged
// PF_EXITING, or fields too few to tell.
func statExiting(f []string) bool {
	if len(f) < 7 {
		return true
	}
	if st := f[0]; st == "Z" || st == "X" || st == "x" {
		return true
	}
	flags, err := strconv.ParseUint(f[6], 10, 64)
	return err != nil || flags&pfExiting != 0
}

// procExiting reports whether pid is exiting or gone as <pid>/stat shows
// it (statExiting), or its stat cannot be read.
func procExiting(root string, pid uint32) bool {
	f, ok := procStat(root, pid)
	return !ok || statExiting(f)
}

// procID tells apart the programs a pid has run, for ProcZones: its start
// time (field 22 of <pid>/stat), which another process taking the pid
// changes; where its arguments and environment lie in its memory (fields
// 48-51, arg_start to env_end, which the kernel shows a reader that may
// trace the process, as keploy reading its environment may), which an
// exec lays out anew, at a random place (ASLR), while the pid and its start
// stay (a wrapper that execs the JVM: berglas, chamber, a shell's exec);
// and the program it runs (the <pid>/exe link). A program that execs
// itself again with ASLR off (setarch -R) and arguments and an environment
// of the same sizes is not told apart: a stated limit.
type procID struct {
	start, mem, exe string
}

// procIdent is pid's procID, and whether it is exiting (statExiting); ok
// false where its stat cannot be read or holds no field 51 (a kernel
// before 3.5).
func procIdent(root string, pid uint32) (id procID, exiting, ok bool) {
	f, ok := procStat(root, pid)
	if !ok || len(f) < 49 {
		return procID{}, false, false
	}
	exe, _ := os.Readlink(filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "exe"))
	return procID{start: f[19], mem: strings.Join(f[45:49], " "), exe: exe}, statExiting(f), true
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

// zoneFileIn is ReadZone of the zone file at name, a path in the process's
// own view, in f's root: its bytes, read through its links there
// (readZoneFileIn), named by the zoneinfo path its last link names, else by
// name's own, else by /etc/timezone where the bytes are that zone's file
// under root's /usr/share/zoneinfo; with no name otherwise. A file that is
// not there (a link to nothing) is an error, as Go and the C library read
// UTC there, never the zone its link names. So is a file that names its
// zone by its path but is mounted from another zone's (mountedZone).
func zoneFileIn(f ZoneFiles, name string) (*time.Location, error) {
	data, final, err := readZoneFileIn(f, name)
	if absent(err) {
		// A link to nothing: lasting (lastingError).
		return nil, lasting(err)
	} else if err != nil {
		return nil, err
	}
	for _, p := range []string{final, name} {
		if zn := models.ZoneinfoName(p); zn != "" {
			if err := mountedZone(f, final, zn); err != nil {
				return nil, err
			}
			return loadZone(zn, data)
		}
	}
	if b, err := readUnderIn(f, "/etc/timezone"); err == nil {
		zn := strings.TrimSpace(string(b))
		if zn != "" && !strings.Contains(zn, "..") {
			if same, at, err := readZoneFileIn(f, "/usr/share/zoneinfo/"+zn); err == nil && bytes.Equal(same, data) {
				if err := mountedZone(f, at, zn); err != nil {
					return nil, err
				}
				return loadZone(zn, data)
			}
		}
	}
	return loadZone("", data)
}

// utcZone is the zone of a TZ of "UTC" or "" (tz, its ":" cut), which Go,
// Node and a JVM read as UTC with no file, as do some C libraries (Ubuntu
// 24.04's glibc writes UTC for TZ=UTC where the file below is another
// zone's, and musl reads no file), while Debian's glibc (and so Python on
// it) reads the file /usr/share/zoneinfo/UTC under root,
// /usr/share/zoneinfo/Universal for an empty TZ, where root holds it, and
// UTC where it does not. Debian's tzdata links both to Etc/UTC, so a host's
// /etc/localtime bind-mounted over an image's link to Etc/UTC
// (`-v /etc/localtime:/etc/localtime:ro`) is the file Debian's glibc reads
// for TZ=UTC too (bookworm's writes the host's +05:30 there, 2026-10-29).
// UTC where the file is not there (in a root keploy can still read: an
// exiting process's root reads every file as absent) or is UTC's: mounted
// from no other zone's file (zoneMountOf) and holding UTC's rules. A lasting
// error otherwise, naming the file: keploy cannot tell which C library the
// application's is, so its zone is unknown; a C library that reads UTC
// there is refused too, saying why (a stated limit).
func utcZone(files ZoneFiles, tz string) (*time.Location, error) {
	f := "/usr/share/zoneinfo/UTC"
	if tz == "" {
		f = "/usr/share/zoneinfo/Universal"
	}
	who := fmt.Sprintf("TZ=%s is UTC to Go, Node and a JVM, and to a C library that reads no file for it (Ubuntu 24.04's glibc, musl), but Debian's glibc, and Python on it, read the zone file %s", tz, f)
	data, final, err := readZoneFileIn(files, f)
	if absent(err) {
		if serr := files.Readable(); serr != nil {
			return nil, fmt.Errorf("keploy cannot read the process's root (%v), so it cannot tell whether it holds %s, which the C library may read for TZ=%s", serr, f, tz)
		}
		return time.UTC, nil
	} else if err != nil {
		return nil, fmt.Errorf("keploy cannot read the zone file %s, which the C library may read for TZ=%s: %w", f, tz, err)
	}
	zn := models.ZoneinfoName(final)
	m, err := files.ZoneMount(final, zn)
	if err != nil {
		return nil, err
	}
	if m != nil {
		what, remedy := fmt.Sprintf("the zone file of %q", m.Zone), "TZ="+m.Zone
		if m.Zone == "" {
			what, remedy = "a file whose path names no zone (a copy)", "TZ=<the IANA name of the zone the mounted file holds>"
		}
		if m.Replaced {
			what += ", since replaced on the host"
		}
		return nil, lasting(fmt.Errorf("%s, which is %s, a file mounted from %s, %s, so keploy cannot tell the zone the application writes in, and reads it as unknown. A host's /etc/localtime bind-mounted over an image's link to Etc/UTC (-v /etc/localtime:/etc/localtime:ro lands on the file the link names) gives this: drop the mount, or set TZ to the mounted file's zone (%s), which every runtime reads alike",
			who, final, m.Source, what, remedy))
	}
	loc, err := time.LoadLocationFromTZData(zn, data)
	if err != nil {
		return nil, lasting(fmt.Errorf("%s, which is no zone file keploy reads (%w), so keploy reads the application's zone as unknown", who, err))
	}
	if at, differ := zoneRulesDiffer(loc, time.UTC); differ {
		_, off := at.In(loc).Zone()
		return nil, lasting(fmt.Errorf("%s, which holds other rules than UTC's (UTC%s on %s), so keploy reads the application's zone as unknown. Make %s UTC's file, or set TZ to the zone the application runs in",
			who, utcOffset(off), at.UTC().Format(time.RFC3339), f))
	}
	return time.UTC, nil
}

// mountedZone is nil where the zone file at p (a path in the process's own
// view, its links resolved, in f's root) is, as far as the mounts it is
// read through tell (ZoneFiles.ZoneMount), the file of the zone zn its path
// names.
// A lasting error naming both where the file is read from another zone's,
// or one whose path names no zone (a host's /etc/localtime that is a
// copy): Node and a JVM read the zone by zn, the C library, Go and Python
// by the mounted file's rules, so the zone is unknown. A host's
// /etc/localtime bind-mounted over an image's link to its zone
// (`-v /etc/localtime:/etc/localtime:ro`, which lands on the file the link
// names) gives that shape. loadZone's check of the file's rules against
// keploy's own zone data cannot see it where the process shares keploy's
// root (native mode, and keploy's own zone): keploy reads the very file
// there. zoneMountOf's error, not lasting, where keploy cannot tell.
func mountedZone(f ZoneFiles, p, zn string) error {
	m, err := f.ZoneMount(p, zn)
	if err != nil || m == nil {
		return err
	}
	what, remedy := fmt.Sprintf("the zone file of %q", m.Zone), "TZ="+m.Zone
	if m.Zone == "" {
		what, remedy = "a file whose path names no zone (a copy)", "TZ=<the IANA name of the zone the mounted file holds>"
	}
	if m.Replaced {
		what += ", since replaced on the host"
	}
	return lasting(fmt.Errorf("the zone file %s, which names %q by its path, is a file mounted from %s, %s. Node and a JVM read the zone by the name %q, the C library, Go and Python by the mounted file's rules, so keploy reads the application's zone as unknown. A host's /etc/localtime bind-mounted over an image's link to its zone (-v /etc/localtime:/etc/localtime:ro lands on the file the link names) gives this: set TZ to the mounted file's zone (%s), which the C library, Go and Python run in now, or drop the mount (a TZ of the zone the link names reads the mounted file too, and so does TZ=UTC where the C library reads a file for it: Debian's glibc)",
		p, zn, m.Source, what, zn, remedy))
}

// zoneMountOf is the mount the zone file at p (a path in the process's own
// view, its links resolved, under root), which names the zone zn, is read
// from another zone's file through (models.ZoneFileMount: none where no
// file or directory is mounted inside its zone's name, or the one mounted
// is zn's, or of another of IANA's names of UTC where zn is one), read
// from mounts. A mounted file since replaced on the host (a host's tzdata
// upgrade renames its files over: the mount's root then ends in
// "//deleted") still holds the rules it held, and is read by its path. An
// error, not lasting, where keploy cannot read the mounts: a process's
// zone is read again at its next call (ProcZones), keploy's own, read once
// at its start, stays unnamed for the run. Off Linux no mount table is
// read: macOS's zone files are no container's.
func zoneMountOf(mounts zoneMounts, root, p, zn string) (*models.ZoneMount, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	at, err := mounts.point(root, p)
	if err != nil {
		return nil, fmt.Errorf("keploy cannot resolve the zone file %s, so it cannot tell whether it is a file mounted from another zone's: %w", p, err)
	}
	m, err := models.ZoneFileMount(mounts.table, at, zn)
	if err != nil {
		return nil, fmt.Errorf("keploy cannot read the mounts the zone file %s is read through (%s), so it cannot tell whether it is a file mounted from another zone's: %w", p, mounts.table, err)
	}
	return m, nil
}

// zoneMounts is the mount table a root's zone files are read through
// (mountedZone), and the view it writes its mount points in: a process's
// own (proc: its /proc/<pid>/mountinfo, of its /proc/<pid>/root, as
// ReadProcZone reads it), or keploy's (keploy's own /proc/self/mountinfo,
// of "/" or a directory keploy reads in its own view, as ReadZone reads
// it), never told from root's path.
type zoneMounts struct {
	table string
	proc  bool
}

// ownMounts are keploy's own mounts (ReadZone).
var ownMounts = zoneMounts{table: "/proc/self/mountinfo"}

// point is p, a path in the process's own view under root with its links
// resolved there, as m's table writes it: p itself in the process's view;
// in keploy's, the path keploy reads p at (root's own links resolved:
// /proc/self/root is "/").
func (m zoneMounts) point(root, p string) (string, error) {
	if m.proc {
		return path.Clean("/" + p), nil
	}
	return filepath.EvalSymlinks(under(root, p))
}

// zoneLocCap bounds zoneLocs: a cache holding it starts again.
const zoneLocCap = 256

// zoneLocs are the zones zoneFileIn loaded, by their name and their file's
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
// holds where it holds one of that name and bytes. A name whose rules in
// keploy's own zone data (models.ZoneLocation) are not data's is a lasting
// error that names both (zoneRulesDiffer): the zone is unknown, as runtimes
// disagree on it. Node (ICU) and a JVM read the zone by the name, in their
// own zone data; the C library, Go and Python read the file's rules. A
// host's /etc/localtime bind-mounted over an image's link to its zone
// (`-v /etc/localtime:/etc/localtime:ro`) lands on the file the link
// names, and gives that shape; so does a file of another release of IANA's
// data than keploy's, for a zone whose rules changed between the two (a
// stated limit: a right value of such a zone is refused, saying why). A
// reference that is the very file read (keploy's own zone files, where the
// process shares keploy's root) finds no difference: zoneFileIn reads the
// bind mount itself there (mountedZone). A name keploy's zone
// data does not hold (nor "Local", which names keploy's own zone) is
// dropped: the file's rules with no name, as keploy cannot tell whether
// they are the name's.
func loadZone(name string, data []byte) (*time.Location, error) {
	var own *time.Location
	if name != "" {
		var err error
		if own, err = models.ZoneLocation(name); models.IsOwnZoneMount(err) {
			// keploy's own file of the name is another zone's: it cannot
			// check the name against the file's rules.
			return nil, lasting(fmt.Errorf("the zone file named %q (by TZ, the /etc/localtime link or /etc/timezone) cannot be checked against %q's rules: %w", name, name, err))
		} else if err != nil {
			name = ""
		}
	}
	k := zoneLocKey{name: name, sum: sha256.Sum256(data)}
	zoneLocs.mu.Lock()
	loc, ok := zoneLocs.m[k]
	zoneLocs.mu.Unlock()
	if ok {
		return loc, nil
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		// Bytes that are no zone file's: lasting (lastingError).
		return nil, lasting(err)
	}
	if own != nil {
		if at, ok := zoneRulesDiffer(loc, own); ok {
			_, off := at.In(loc).Zone()
			_, ownOff := at.In(own).Zone()
			return nil, lasting(fmt.Errorf("the zone file named %q (by TZ, the /etc/localtime link or /etc/timezone) does not hold %q's rules: their offsets first differ on %s, where it reads UTC%s and %q UTC%s. Node and a JVM read the zone by its name, the C library, Go and Python by the file's rules, so keploy reads the application's zone as unknown. A host's /etc/localtime bind-mounted over an image's link to its zone (-v /etc/localtime:/etc/localtime:ro lands on the file the link names) gives this: set TZ to the zone the application runs in. So does zone data (tzdata) of another release than keploy's, for a zone whose rules changed between them: update the image's tzdata",
				name, name, at.UTC().Format(time.RFC3339), utcOffset(off), name, utcOffset(ownOff)))
		}
	}
	zoneLocs.mu.Lock()
	if len(zoneLocs.m) >= zoneLocCap {
		zoneLocs.m = map[zoneLocKey]*time.Location{}
	}
	zoneLocs.m[k] = loc
	zoneLocs.mu.Unlock()
	return loc, nil
}

// zoneRulesFrom and zoneRulesTo bound the instants zoneRulesDiffer reads:
// from 1970, as builds of one release of IANA's data differ only before it
// (built with backzone or without, which drops the history of zones merged
// for agreeing since 1970), to 2038, as a version 1 or "fat" zone file
// lists no change of offset after 2037, where a version 2 file's footer
// rule goes on.
var (
	zoneRulesFrom = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	zoneRulesTo   = time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
)

// zoneRulesDiffer is the first instant from zoneRulesFrom to zoneRulesTo at
// which a and b read different offsets from UTC, and whether there is one:
// each is read at every change of offset either makes
// (time.Time.ZoneBounds), so no change of either between two reads is
// missed.
func zoneRulesDiffer(a, b *time.Location) (time.Time, bool) {
	for t := zoneRulesFrom; t.Before(zoneRulesTo); {
		ta, tb := t.In(a), t.In(b)
		_, oa := ta.Zone()
		_, ob := tb.Zone()
		if oa != ob {
			return t, true
		}
		next := zoneRulesTo
		for _, tt := range []time.Time{ta, tb} {
			if _, end := tt.ZoneBounds(); !end.IsZero() && end.Before(next) {
				next = end
			}
		}
		t = next.UTC()
	}
	return time.Time{}, false
}

// utcOffset is off, in seconds east of UTC, as "+05:30" (seconds where it
// holds some).
func utcOffset(off int) string {
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	if off%60 != 0 {
		return fmt.Sprintf("%c%02d:%02d:%02d", sign, off/3600, off%3600/60, off%60)
	}
	return fmt.Sprintf("%c%02d:%02d", sign, off/3600, off%3600/60)
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
	return readZoneFileIn(dirFiles{root: root}, p)
}

// readZoneFileIn is readZoneFile of the root whose files f gives.
func readZoneFileIn(f ZoneFiles, p string) ([]byte, string, error) {
	real, err := resolveIn(f, p)
	if err != nil {
		return nil, "", err
	}
	data, err := readRegularIn(f, real)
	return data, real, err
}

// zoneFileCap bounds the bytes readRegular reads: a zone file holds a few
// KiB.
const zoneFileCap = 1 << 20

// errNotRegular is readRegular's error of a file that is no regular one.
type errNotRegular struct{ mode fs.FileMode }

func (e errNotRegular) Error() string {
	return "no regular file (" + e.mode.Type().String() + ")"
}

// readRegular reads the regular file at p, a path in the process's own
// view with its links resolved (resolveUnder), under root, of at most
// zoneFileCap bytes: an error of any other (a FIFO, which would hold the
// read, a device, whose driver's open would run, a directory), never
// opened for reading (openRegular), or of a longer one.
func readRegular(root, p string) ([]byte, error) {
	return readRegularIn(dirFiles{root: root}, p)
}

// readRegularIn is readRegular of the root whose files f gives.
func readRegularIn(f ZoneFiles, p string) ([]byte, error) {
	r, err := f.OpenRegular(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, zoneFileCap+1))
	if err == nil && len(data) > zoneFileCap {
		err = lasting(fmt.Errorf("more than %d bytes, more than keploy reads of a zone file, an /etc/timezone or an argfile", zoneFileCap))
	}
	return data, err
}

// readUnder reads the file at p, a path in the process's own view, under
// root (readZoneFile).
func readUnder(root, p string) ([]byte, error) {
	return readUnderIn(dirFiles{root: root}, p)
}

// readUnderIn is readUnder of the root whose files f gives.
func readUnderIn(f ZoneFiles, p string) ([]byte, error) {
	data, _, err := readZoneFileIn(f, p)
	return data, err
}

// localtimeAbsentIn reports whether the root whose files f gives holds no
// /etc/localtime at all (not a link to nothing, which the C library reads
// as UTC too, but keploy refuses as a zone it cannot tell).
func localtimeAbsentIn(f ZoneFiles) bool {
	etc, err := resolveIn(f, "/etc")
	if err != nil {
		return false
	}
	_, err = f.Lstat(path.Join(etc, "localtime"))
	if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	// An absence read in a root keploy can still read: an exited
	// process's root reads as absent everywhere.
	return f.Readable() == nil
}

// resolveUnder is p, a path in the process's own view, with each link on
// it, at any of its components, resolved in that view, never keploy's: an
// absolute link's target is read from root, and ".." stops at root. The
// path it gives names no link, so the kernel follows none reading it under
// root (under), where it would read an absolute link of a directory on the
// way from keploy's own /. An error where a link names nothing, or there
// are too many.
func resolveUnder(root, p string) (string, error) {
	return resolveIn(dirFiles{root: root}, p)
}

// resolveIn is resolveUnder of the root whose files f gives.
func resolveIn(f ZoneFiles, p string) (string, error) {
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
		mode, err := f.Lstat(next)
		if err != nil {
			return "", err
		}
		if mode&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", lasting(errors.New("too many links on the path " + p))
		}
		target, err := f.Readlink(next)
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
