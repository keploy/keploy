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
		loc, err = readZone(filepath.Join(dir, "root"), tz, set, false)
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
			if j, err := isJVM(); err != nil || j {
				return "", false, j, errors.Join(fmt.Errorf("%s sets a JVM's user.timezone, which keploy does not read (OpenJ9's options' order)", v), err)
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

// argfileCap bounds an argfile keploy reads.
const argfileCap = 1 << 20

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
		return nil, fmt.Errorf("keploy cannot read the JVM's argfile @%s, which may set its zone: %w", name, err)
	}
	if len(data) > argfileCap {
		return nil, fmt.Errorf("the JVM's argfile @%s is larger than keploy reads (%d bytes)", name, argfileCap)
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
			return nil, fmt.Errorf("a JVM's user.timezone %q is no zone keploy reads", id)
		}
		if off == 0 {
			return time.UTC, nil
		}
		return time.FixedZone("", off), nil
	}
	switch {
	case id == "localtime" || id == "posixrules" || id == "Factory" || strings.HasPrefix(id, "posix/") || strings.HasPrefix(id, "right/") || strings.Contains(id, ".."),
		!javaRegionID(id):
		return nil, fmt.Errorf("a JVM's user.timezone %q is no zone a JVM reads", id)
	}
	loc, err := models.ZoneLocation(id)
	if err != nil {
		return nil, fmt.Errorf("a JVM's user.timezone %q is no zone keploy reads: %w", id, err)
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
		return "", "", fmt.Errorf("the process's /etc/timezone names %q, which JDKs before JDK-8345213 read as its zone, and its /etc/localtime %q, which later ones read; keploy does not read which JDK the process runs", tzid, other)
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
	} else if fi, err := os.Stat(under(root, r)); err != nil || fi.Mode().Perm()&0o004 == 0 {
		return "", false, errors.New("the process's /etc/timezone, which a JVM may read its zone from, is not readable by every user, so keploy cannot tell whether the JVM reads it")
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
			return "", "", fmt.Errorf("the process's /etc/localtime links to %q, which JDK 21 and later read as zone %q and older ones as %q; keploy does not read which JDK the process runs", target, clean, raw)
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
const javaZoneinfoMatchCap = 20000

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
				return fmt.Errorf("the process's %s holds more than %d files, so keploy does not search it for the zone its /etc/localtime copies", dir, javaZoneinfoMatchCap)
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
		return "", fmt.Errorf("the process's %s links a directory to one a JVM lists already, so the name it finds first for the zone its /etc/localtime copies (%s) turns on its directory listing's order", dir, found[0])
	}
	for _, n := range found {
		if _, err := javaZone(strings.TrimPrefix(n, "posix/")); err != nil {
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
	if err != nil {
		return z, err
	}
	if start == "" || procStart(c.root, pid) != start {
		// The process exited, or another took its number, as keploy read
		// it: what it read may be either's, or none's.
		return ProcZone{}, errors.New("the process exited or another took its number while keploy read its zone")
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
	data, err := readRegular(under(root, real))
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

// readRegular reads the regular file at name, of at most zoneFileCap
// bytes: an error of any other (a FIFO, which would hold the read, a
// device, a directory), never read, or a longer one.
func readRegular(name string) ([]byte, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular{fi.Mode()}
	}
	data, err := io.ReadAll(io.LimitReader(f, zoneFileCap+1))
	if err == nil && len(data) > zoneFileCap {
		err = fmt.Errorf("more than %d bytes, which no zone file holds", zoneFileCap)
	}
	return data, err
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
