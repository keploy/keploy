package starts

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// THE PROCESS A RECORDED CALL CAME FROM (S12 v16 §4.4).
//
// Under the proven switch every recorded mock names the process that made its
// call, as its origin's comm and argv[0] (models.Origin.Comm, Exe): a reason
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
}

// connOrigin is one connection's process, named once, at its first stamp.
type connOrigin struct {
	pid   uint32
	name  procName
	named bool
}

// procName is a process's comm and argv[0]; ok is false when it could not
// be read.
type procName struct {
	comm, exe string
	ok        bool
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
	if m == nil || (m.Origin != nil && (m.Origin.Comm != "" || m.Origin.Exe != "")) {
		return
	}
	id := m.ConnectionID
	if id == "" && m.Spec.Metadata != nil {
		id = m.Spec.Metadata["connID"]
	}
	name, ok := r.connName(id, m.SourcePID)
	if !ok {
		return
	}
	o := models.Origin{Comm: name.comm, Exe: name.exe}
	if m.Origin != nil {
		o.Clock = m.Origin.Clock
	}
	m.Origin = &o
}

// ProcessOf is process pid's comm and argv[0], as a recorded call's origin
// names it (StampOrigin); ok is false when it cannot be read.
func (r *Registry) ProcessOf(pid uint32) (comm, exe string, ok bool) {
	if pid == 0 {
		return "", "", false
	}
	n := r.nameOf(pid)
	return n.comm, n.exe, n.ok
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
		n.exe = exeOf(argv[0])
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
}
