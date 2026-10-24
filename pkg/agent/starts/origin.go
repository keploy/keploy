package starts

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// THE PROCESS A RECORDED CALL CAME FROM.
//
// Under the proven switch every recorded mock names the process that made its
// call, as its origin's comm and argv[0] (models.Origin.Comm, Exe; a program a
// go run or go test built, by that go command: programOf): a reason
// line that routes or moves a call names it, and a miss or an answered call
// names it too (models.UnmatchedCall.Source). No committed file holds more of
// its command line: the whole of it, with its secrets redacted
// (containerkey.RedactArgs), goes to the record log once per process, at
// debug, beside its pid.
//
// The proxy notes each connection's process as it accepts the connection
// (NoteOrigin: the pid the kernel's redirect gave it), and forgets it once
// the connection's parsers are done (ForgetOrigin). A mock is stamped
// (StampOrigin) as its parser hands it over, while its connection is still
// open; a connection closed since is kept for a late mock, as the container
// keyer keeps one (keptClosedOrigins).
//
// That pid is a kernel pid: the initial PID namespace's. keploy reads a
// process by it only where it names that process in keploy's own /proc
// (KernelPids: keploy runs in the initial PID namespace). In a PID namespace
// of its own (a container without --pid=host) the same number names another
// process there, or none, so keploy names none: the mock's origin says why
// (models.UnnamedPIDNamespace, with the kernel pid), every line that would
// name the process says so (models.Origin.ProcessText), and the record log
// says it once, with the remedy. Naming a process from inside a PID
// namespace of its own is a follow-up (a BPF-stamped comm with the
// namespace-local pid, or a namespace-inode match). Only naming
// is so guarded: the registry's starts, which attribute a call by its pid to
// a worker the scope API registered (by number first, a direct worker's call
// in keploy's docker mode included), are read as before, whatever the
// switch.

// KernelPids is implemented by a Proc that tells whether a kernel pid (the
// initial PID namespace's, which keploy's proxy reports) names in it the
// process the kernel names by it (utils.KernelPidsLocalIn). The registry
// names a process by a kernel pid only when it does. A Proc that does not
// say is taken to: its numbers are its own (a test's fake).
type KernelPids interface {
	KernelPidsLocal() bool
}

// kernelPidsLocal reports whether r reads a process by a kernel pid
// (KernelPids).
func (r *Registry) kernelPidsLocal() bool {
	if k, ok := r.proc.(KernelPids); ok {
		return k.KernelPidsLocal()
	}
	return true
}

// Commander is implemented by a Proc that reads a process's name: its comm
// and its command line (argv). ok is false for a process it cannot read.
type Commander interface {
	Command(pid uint32) (comm string, argv []string, ok bool)
}

// keptClosedOrigins bounds how many closed connections the registry keeps the
// process of, for mocks stamped after their connection closed.
const keptClosedOrigins = 4096

// keptProcessNames bounds how many processes the registry keeps the name of
// (origins.named): an application that forks many short-lived callers reads
// a process's name again once the cache has been emptied, and logs its
// command line again then.
const keptProcessNames = 4096

// origins is the registry's table of each recorded connection's process.
type origins struct {
	mu     sync.Mutex
	conns  map[string]*connOrigin
	opened map[string]int
	closed []string
	// named caches, by process (pid and birth), its comm and argv[0], and
	// says its command line was logged.
	named map[procKey]procName
	// unborn is, by pid, the command line (redacted) last logged of a
	// process whose birth cannot be read, so not cached in named: it is
	// logged once per pid, and again only for another command line (the
	// pid reused).
	unborn map[uint32]string
	// logger is where a process's command line is logged, once (SetLogger).
	logger atomic.Pointer[zap.Logger]
	// foreignSaid: the record log said, once, that keploy names no process
	// because it runs in a PID namespace of its own.
	foreignSaid atomic.Bool
}

// connOrigin is one connection's process, named once, at its first stamp.
type connOrigin struct {
	pid   uint32
	name  procName
	named bool
}

// procName is a process's comm and argv[0]; ok is false when it could not
// be read. foreign: its pid is a kernel pid keploy does not read a process by
// (KernelPids), so nothing was read. pid is the pid it was asked by.
type procName struct {
	comm, exe string
	ok        bool
	foreign   bool
	pid       uint32
}

// SetLogger sets where the registry logs, once per process, the command line
// (redacted) of a process a recorded call came from. nil logs nothing.
func (r *Registry) SetLogger(l *zap.Logger) {
	r.origin.logger.Store(l)
}

// NoteOrigin notes that connection conn (the connection ID its mocks carry)
// came from process pid. A pid of 0 (the redirect gave none) notes nothing.
// A connection ID noted again while it is open is the later connection's;
// only its last ForgetOrigin closes it.
func (r *Registry) NoteOrigin(conn string, pid uint32) {
	if conn == "" || pid == 0 {
		return
	}
	o := &r.origin
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conns == nil {
		o.conns, o.opened = map[string]*connOrigin{}, map[string]int{}
	}
	cur, known := o.conns[conn]
	if !known || cur.pid != pid {
		o.conns[conn] = &connOrigin{pid: pid}
	}
	o.opened[conn]++
	// Noted afresh: no longer among the closed (a later close puts it
	// back, last). Only a connection ID known already can be among them:
	// a new one costs no scan.
	if known {
		for i, c := range o.closed {
			if c == conn {
				o.closed = append(o.closed[:i], o.closed[i+1:]...)
				break
			}
		}
	}
}

// ForgetOrigin closes connection conn: its process is kept for a mock stamped
// after, among the last keptClosedOrigins connections closed.
func (r *Registry) ForgetOrigin(conn string) {
	o := &r.origin
	o.mu.Lock()
	defer o.mu.Unlock()
	if n := o.opened[conn]; n > 1 {
		o.opened[conn] = n - 1
		return
	}
	delete(o.opened, conn)
	if _, ok := o.conns[conn]; !ok {
		return
	}
	o.closed = append(o.closed, conn)
	if len(o.closed) > keptClosedOrigins {
		delete(o.conns, o.closed[0])
		o.closed = o.closed[1:]
	}
}

// StampOrigin gives m, a recorded mock, the process its call came from
// (models.Origin.Comm, Exe), keeping the origin's clock: the process of its
// connection (NoteOrigin), else its SourcePID's. A mock whose origin names a
// process already, or whose process keploy cannot tell or read, is left as it
// is. The origin is replaced whole, never edited: a copy of the mock may share
// it (models.Mock.Origin).
func (r *Registry) StampOrigin(m *models.Mock) {
	if m == nil || (m.Origin != nil && (m.Origin.Comm != "" || m.Origin.Exe != "" || m.Origin.Unnamed != "")) {
		return
	}
	id := m.ConnectionID
	if id == "" && m.Spec.Metadata != nil {
		id = m.Spec.Metadata["connID"]
	}
	name, _ := r.connName(id, m.SourcePID)
	o := name.origin()
	if o == nil {
		return
	}
	if m.Origin != nil {
		o.Clock = m.Origin.Clock
	}
	m.Origin = o
}

// origin is the origin a mock or a call from n's process names it by: its
// comm and argv[0], or, for a process keploy would not read (foreign), why,
// with its kernel pid; nil when n names nothing.
func (n procName) origin() *models.Origin {
	switch {
	case n.ok:
		return &models.Origin{Comm: n.comm, Exe: n.exe}
	case n.foreign:
		return &models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: n.pid}
	}
	return nil
}

// ProcessOf is process pid's comm and argv[0], as a recorded call's origin
// names it (StampOrigin); ok is false when it cannot be read, and when pid
// is a kernel pid keploy does not read a process by (KernelPids).
func (r *Registry) ProcessOf(pid uint32) (comm, exe string, ok bool) {
	if pid == 0 {
		return "", "", false
	}
	n := r.nameOf(pid)
	return n.comm, n.exe, n.ok
}

// SourceOf is how a call from process pid (a kernel pid) names it
// (models.UnmatchedCall.Source): its comm and argv[0]; where keploy does not
// read a process by a kernel pid (KernelPids), no name, why, and the pid;
// nil for pid 0, or a process that cannot be read.
func (r *Registry) SourceOf(pid uint32) *models.Origin {
	if pid == 0 {
		return nil
	}
	n := r.nameOf(pid)
	n.pid = pid
	return n.origin()
}

// connName is the process of connection id, named once (at its first
// stamp), else the process pid (a mock's SourcePID).
func (r *Registry) connName(id string, pid uint32) (procName, bool) {
	o := &r.origin
	o.mu.Lock()
	c := o.conns[id]
	if c != nil && c.named {
		n := c.name
		o.mu.Unlock()
		return n, n.ok
	}
	if c != nil {
		pid = c.pid
	}
	o.mu.Unlock()
	if pid == 0 {
		return procName{}, false
	}
	n := r.nameOf(pid)
	n.pid = pid
	// Only a name read is kept: a read that failed (the process's /proc
	// entry not readable yet) is tried again at the next stamp.
	if c != nil && n.ok {
		o.mu.Lock()
		c.name, c.named = n, true
		o.mu.Unlock()
	}
	return n, n.ok
}

// nameOf reads process pid's name (Commander), cached by the process (its
// pid and birth: a pid reused is another process), and logs its whole
// command line, redacted, the first time.
func (r *Registry) nameOf(pid uint32) procName {
	cmd, ok := r.proc.(Commander)
	if !ok {
		return procName{}
	}
	// A kernel pid names another process in keploy's /proc, or none, when
	// keploy runs in a PID namespace of its own: nothing is read by it, and
	// no command line is logged.
	if !r.kernelPidsLocal() {
		r.origin.sayForeign(pid)
		return procName{foreign: true}
	}
	key := procKey{pid: pid}
	born, bornKnown := r.proc.Birth(pid)
	if bornKnown {
		key.born = born.UnixNano()
	}
	o := &r.origin
	// A process whose birth cannot be read is not told from a later one
	// that reuses its pid: its name is read each time, never kept.
	if bornKnown {
		o.mu.Lock()
		if n, ok := o.named[key]; ok {
			o.mu.Unlock()
			return n
		}
		o.mu.Unlock()
	}
	comm, argv, read := cmd.Command(pid)
	n := procName{ok: read && (comm != "" || len(argv) > 0)}
	if !n.ok {
		// Not kept: the next ask reads again.
		return n
	}
	n.comm = comm
	if len(argv) > 0 {
		n.exe = r.programOf(cmd, pid, argv[0])
	}
	line := strings.Join(containerkey.RedactArgs(argv), " ")
	if !bornKnown {
		// Not kept, but its command line is logged all the same, once per
		// pid: a routing line may name it.
		o.mu.Lock()
		prev, logged := o.unborn[pid]
		if !logged || prev != line {
			if o.unborn == nil || len(o.unborn) >= keptProcessNames {
				o.unborn = map[uint32]string{}
			}
			o.unborn[pid] = line
		}
		o.mu.Unlock()
		if !logged || prev != line {
			o.logProcess(pid, n, line)
		}
		return n
	}
	o.mu.Lock()
	if prev, ok := o.named[key]; ok {
		o.mu.Unlock()
		return prev
	}
	if o.named == nil || len(o.named) >= keptProcessNames {
		o.named = map[procKey]procName{}
	}
	o.named[key] = n
	o.mu.Unlock()
	o.logProcess(pid, n, line)
	return n
}

// sayForeign says in the record log, once, that keploy names no process a
// call came from because it runs in a PID namespace of its own, with the
// first kernel pid it was asked to name (pid) and the remedy.
func (o *origins) sayForeign(pid uint32) {
	l := o.logger.Load()
	if l == nil || !o.foreignSaid.CompareAndSwap(false, true) {
		return
	}
	l.Warn("keploy names no process its recorded calls came from, and its reason lines say so: "+models.PIDNamespaceReason(pid),
		zap.String("pid", strconv.FormatUint(uint64(pid), 10)),
		zap.String("next_step", "run keploy in the host PID namespace (docker run --pid=host, or hostPID: true in Kubernetes) and record again, to have each routed or moved call name its process; keploy's own docker and compose modes start its agent in the application's PID namespace, where it names no process yet: nothing to do there, the lines say why instead"))
}

// logProcess logs, at debug, that a recorded call came from process pid,
// named n, with its whole command line, redacted (line).
func (o *origins) logProcess(pid uint32, n procName, line string) {
	if l := o.logger.Load(); l != nil && n.ok {
		l.Debug("a recorded call came from this process",
			zap.String("pid", strconv.FormatUint(uint64(pid), 10)),
			zap.String("comm", n.comm),
			zap.String("cmdline", line))
	}
}

// programOf is what a committed file holds of process pid's program
// (models.Origin.Exe), argv0 its argv[0]. A program the go tool built for a
// go run or a go test and runs as its child (builtByGo) is named by that go
// command: `go run` and its target, or `go test` and its packages, without
// build or test flags (goCommandOf), read from its parent's command line. Its
// own argv[0] is the binary the go tool built, in a dir it makes for that one
// command (/tmp/go-build<random>/b001/exe/<name> for a go run,
// /tmp/go-build<random>/b001/<pkg>.test for a go test) or, for a go run's
// program once built, in the go build cache (<GOCACHE>/<xx>/<hash>-d/<name>):
// a path the user never typed, which changes from one record to the next, so
// a committed line naming it would change with each. Only that child is: the
// go tool's other children (git fetching a module, the compiler, vet, a
// go test -exec program) are named by their own argv[0], never as the go
// command. Any other program is its argv[0] (exeOf), as is one whose parent
// cannot be read.
func (r *Registry) programOf(cmd Commander, pid uint32, argv0 string) string {
	if !builtByGo(argv0) {
		return exeOf(argv0)
	}
	if parent, ok := r.proc.Parent(pid); ok && parent != 0 && parent != pid {
		if _, argv, read := cmd.Command(parent); read {
			if name, ok := goCommandOf(argv); ok {
				return name
			}
		}
	}
	return exeOf(argv0)
}

// builtByGo reports whether argv0 is a binary the go tool built to run: in a
// work dir it made for one command (go-build<digits>/b<digits>/..., under
// GOTMPDIR or the temp dir), or a cached executable (<xx>/<64 hex>-d/<name>
// in GOCACHE).
func builtByGo(argv0 string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(argv0)), "/")
	for i := 0; i+1 < len(parts); i++ {
		if digitsAfter(parts[i], "go-build") && digitsAfter(parts[i+1], "b") {
			return true
		}
	}
	if n := len(parts); n >= 3 {
		id, dir := parts[n-2], parts[n-3]
		return len(id) == 66 && strings.HasSuffix(id, "-d") && isHex(id[:64]) && len(dir) == 2 && isHex(dir)
	}
	return false
}

// digitsAfter reports whether w is prefix and then one or more digits.
func digitsAfter(w, prefix string) bool {
	if !strings.HasPrefix(w, prefix) || len(w) == len(prefix) {
		return false
	}
	for _, c := range w[len(prefix):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isHex(w string) bool {
	for _, c := range w {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return w != ""
}

// goCommandOf names the go command that runs a program it built, argv the go
// tool's command line: a go run (goRunOf) or a go test (goTestOf). ok is
// false for any other command line.
func goCommandOf(argv []string) (string, bool) {
	if name, ok := goRunOf(argv); ok {
		return name, true
	}
	return goTestOf(argv)
}

// goRunOf names the program a go run runs, argv the go tool's command line:
// "go run " and its target, the package (`.`, `./cmd/server`,
// `example.com/cmd@v1.2.0`) or the .go files it names, as the command gives
// them, redacted as argv[0] is (containerkey.RedactArgs). Never a build flag
// nor its value: a -ldflags value can carry a key. "go run" alone when keploy
// cannot tell which word is the target: a flag it does not know may take the
// next word as its value, so no word is named rather than a value. ok is
// false when argv is not a go run.
func goRunOf(argv []string) (string, bool) {
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || argv[1] != "run" {
		return "", false
	}
	i, known := skipGoFlags(argv, 2, goRunBoolFlags, goRunValueFlags)
	if !known || i >= len(argv) {
		return "go run", true
	}
	end := i + 1
	if strings.HasSuffix(argv[i], ".go") {
		for end < len(argv) && strings.HasSuffix(argv[end], ".go") {
			end++
		}
	}
	return "go run " + strings.Join(containerkey.RedactArgs(argv[i:end]), " "), true
}

// goTestOf names the test binaries a go test runs, argv the go tool's command
// line: "go test " and its packages (`./...`, `./pkg/store`, the _test.go
// files it names), as the command gives them, redacted as argv[0] is: the
// words after its leading flags, up to the next flag. Never a build or test
// flag nor its value (-ldflags, -run, -args and what follows it). "go test"
// alone when it names no package (the current dir's), or when keploy cannot
// tell which words are packages: a flag it does not know may take the next
// word as its value. ok is false when argv is not a go test.
func goTestOf(argv []string) (string, bool) {
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return "", false
	}
	// A flag it does not know ends the flags it reads (skipGoFlags), and
	// no package is read past a flag: no word after it is named.
	i, _ := skipGoFlags(argv, 2, goTestBoolFlags, goTestValueFlags)
	end := i
	for end < len(argv) && !strings.HasPrefix(argv[end], "-") {
		end++
	}
	if end == i {
		return "go test", true
	}
	return "go test " + strings.Join(containerkey.RedactArgs(argv[i:end]), " "), true
}

// skipGoFlags is the index of the first word of argv from i on that is not
// one of the go command's flags (bools: one word; values: two, unless written
// -flag=value; "--" ends them), and known false at a flag it does not know:
// the next word may be that flag's value. A test flag is known with its
// test. prefix too. A go test's -args is not known: every word after it is
// the test binary's, so no package follows it.
func skipGoFlags(argv []string, i int, bools, values map[string]bool) (int, bool) {
	for i < len(argv) {
		w := argv[i]
		if w == "--" {
			return i + 1, true
		}
		if len(w) < 2 || w[0] != '-' {
			return i, true
		}
		name := strings.TrimPrefix(strings.TrimPrefix(w[1:], "-"), "test.")
		switch {
		case strings.Contains(name, "="):
			i++
		case bools[name]:
			i++
		case values[name]:
			i += 2
		default:
			return i, false
		}
	}
	return i, true
}

// goRunBoolFlags and goRunValueFlags are go run's own flags (the go tool's
// build flags, its cover flags and -exec): a bool flag is one word, a flag
// that takes a value is two unless written -flag=value.
var (
	goRunBoolFlags = map[string]bool{
		"a": true, "asan": true, "buildvcs": true, "cover": true, "json": true,
		"linkshared": true, "modcacherw": true, "msan": true, "n": true,
		"race": true, "trimpath": true, "v": true, "work": true, "x": true,
	}
	goRunValueFlags = map[string]bool{
		"C": true, "asmflags": true, "buildmode": true, "compiler": true,
		"covermode": true, "coverpkg": true, "debug-actiongraph": true,
		"debug-runtime-trace": true, "debug-trace": true, "exec": true,
		"gccgoflags": true, "gcflags": true, "installsuffix": true,
		"ldflags": true, "mod": true, "modfile": true, "overlay": true,
		"p": true, "pgo": true, "pkgdir": true, "tags": true, "toolexec": true,
	}
)

// goTestBoolFlags and goTestValueFlags are go test's flags: its build flags
// (go run's), its own (-c, -json, -o, -vet, -exec) and the test binary's it
// takes on its command line (-run, -count, -v, ...).
var (
	goTestBoolFlags  = union(goRunBoolFlags, "artifacts", "benchmem", "c", "failfast", "fullpath", "short")
	goTestValueFlags = union(goRunValueFlags, "bench", "benchtime", "blockprofile", "blockprofilerate",
		"count", "coverprofile", "cpu", "cpuprofile", "fuzz", "fuzzcachedir", "fuzzminimizetime", "fuzztime",
		"list", "memprofile", "memprofilerate", "mutexprofile", "mutexprofilefraction", "o", "outputdir",
		"parallel", "run", "shuffle", "skip", "timeout", "trace", "vet")
)

// union is the flags of base and names.
func union(base map[string]bool, names ...string) map[string]bool {
	out := make(map[string]bool, len(base)+len(names))
	for n := range base {
		out[n] = true
	}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// exeOf is what a committed file may hold of a process's argv[0]
// (models.Origin.Exe): a process that sets its own title (setproctitle, a
// runtime's process.title) puts a whole command line there, with whatever
// secrets it carries, so each of its words is redacted as a command line's
// argument is (containerkey.RedactArgs); a path with a space in it reads as
// it is.
func exeOf(argv0 string) string {
	words := strings.Fields(argv0)
	if len(words) <= 1 {
		return containerkey.RedactArgs([]string{strings.TrimSpace(argv0)})[0]
	}
	return strings.Join(containerkey.RedactArgs(words), " ")
}

// resetOrigins forgets every connection's and process's name (Reset).
func (r *Registry) resetOrigins() {
	o := &r.origin
	o.mu.Lock()
	o.conns, o.opened, o.closed, o.named, o.unborn = nil, nil, nil, nil, nil
	o.mu.Unlock()
	o.foreignSaid.Store(false)
}
