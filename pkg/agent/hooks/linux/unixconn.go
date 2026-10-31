//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/captureloss"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// The AF_UNIX report (keploy/ebpf unixconn.c, decision 20): the application's
// calls over Unix sockets, which go nowhere keploy's proxy sees, so keploy
// cannot capture them at all. The kernel reports each successful connect, and
// each process's first datagram to an address; the agent drops the
// allow-listed destinations (unixAllowlist) and the ones a member of the
// application, or keploy's agent, owns, and counts the rest as
// models.CaptureLossUnixSocket, which leaves the test open at the time and
// every later one NOT JUDGED under the proven switch, never failed.
//
// Two ways to the same reports: fexit, where a BPF trampoline attaches, and
// otherwise (arm64 before 6.0) the syscall tracepoints, which know no peer:
// there the agent finds who listens on the path. An owner that cannot be
// found, a path the kernel could not read, and a report that found no room
// are each counted: unknown is never safe. A kernel that loads the report
// neither way gets one WARN and no report (startUnixConn): Unix sockets are
// not supported.

// unixConnectEvent mirrors struct unix_connect_event (keploy/ebpf k_unix.h).
type unixConnectEvent struct {
	TGID     uint32
	PeerTGID uint32
	PeerIno  uint64
	Kind     uint32
	PathLen  uint32
	Path     [108]byte
	Flags    uint32
}

// The report's kinds (k_unix.h).
const (
	unixConnectStream  = 1 // fexit: a stream connect; a member's or the agent's peer is not reported
	unixConnectDgram   = 2 // fexit: a datagram connect, with the peer's inode
	unixSendDgram      = 3 // fexit: a process's first datagram to an address
	unixSyscallConnect = 4 // tracepoints: connect(2) that succeeded
	unixSyscallSend    = 5 // tracepoints: a process's first sendto/sendmsg(2) to an address
)

// unixPathUnread flags a report whose address the kernel could not read.
const unixPathUnread = 1

// path is the address connected to: a path, or "@name" for an abstract one.
func (e unixConnectEvent) path() string {
	n := int(e.PathLen)
	if n > len(e.Path) {
		n = len(e.Path)
	}
	p := e.Path[:n]
	if len(p) > 0 && p[0] == 0 {
		return "@" + string(bytes.TrimRight(p[1:], "\x00"))
	}
	return string(bytes.TrimRight(p, "\x00"))
}

// unixAllowlist are the Unix sockets the application may call without keploy
// leaving a test unjudged (decision 20): ones that carry no dependency
// traffic of the application's. Both paths, fexit and the tracepoints, read
// this one table, and so do the tests. A pattern is a path.Match pattern; "@"
// starts an abstract name. Not the Docker daemon's socket: an application
// that drives Docker (testcontainers) has a real dependency there. keploy's
// own sockets are excused by their owner, keploy's agent, not by path.
var unixAllowlist = []string{
	// name services glibc asks on a host lookup: nscd, SSSD,
	// systemd-resolved, systemd's userdb, avahi
	"/var/run/nscd/socket", "/run/nscd/socket", "/var/lib/sss/pipes/*",
	"/run/systemd/resolve/io.systemd.Resolve", "/run/systemd/resolve/io.systemd.Resolve.*",
	"/run/systemd/userdb/*", "/run/avahi-daemon/socket", "/var/run/avahi-daemon/socket",
	// syslog and journald
	"/dev/log", "/run/systemd/journal/*", "/var/run/systemd/journal/*",
	// D-Bus, the system and the session bus
	"/run/dbus/system_bus_socket", "/var/run/dbus/system_bus_socket", "/run/user/*/bus", "@/tmp/dbus-*",
	// X11 and Wayland
	"/tmp/.X11-unix/X*", "@/tmp/.X11-unix/X*", "/run/user/*/wayland-*",
}

// unixAllowed reports whether the application may connect to p unreported.
func unixAllowed(p string) bool {
	for _, pat := range unixAllowlist {
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
	}
	return false
}

// unixConn is the loaded report: its object, links and reader, and what it
// counted.
type unixConn struct {
	coll   *ebpf.Collection
	links  []link.Link
	rd     *ringbuf.Reader
	lost   *ebpf.Map
	losses captureloss.Counter
	// fallback: the syscall tracepoints report, not fexit (no trampoline);
	// why is the attach error that chose it.
	fallback bool
	why      string
	// self is keploy's agent's TGID: a socket it holds is keploy's own.
	self uint32

	mu    sync.Mutex
	known map[uint64]bool // dgram peer inode -> owned by a member

	// drain lets a read wait for every report before it (DrainCaptureLosses).
	drain *reportDrain
}

// sharedMaps are the main object's maps by their BPF names, which unixconn
// uses in place of its own copies (pid_filter reads them).
func sharedMaps(objs *bpfObjects) map[string]*ebpf.Map {
	out := map[string]*ebpf.Map{}
	v := reflect.ValueOf(objs.bpfMaps)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("ebpf")
		if m, ok := v.Field(i).Interface().(*ebpf.Map); ok && m != nil && name != "" {
			out[name] = m
		}
	}
	return out
}

// unixFallbackOnly makes loadUnixConn take the syscall tracepoints on any
// kernel: for tests, which prove the fallback where fexit would attach.
var unixFallbackOnly = false

// unixFexitPrograms are unixconn's fexit programs; unixFallbackPrograms its
// syscall tracepoint programs, by tracepoint, for a kernel where those cannot
// attach.
var (
	unixFexitPrograms    = []string{"k_unix_stream_connect", "k_unix_dgram_connect", "k_unix_dgram_sendmsg"}
	unixFallbackPrograms = map[string]string{
		"k_unix_sys_enter_connect": "sys_enter_connect",
		"k_unix_sys_exit_connect":  "sys_exit_connect",
		"k_unix_sys_enter_sendto":  "sys_enter_sendto",
		"k_unix_sys_exit_sendto":   "sys_exit_sendto",
		"k_unix_sys_enter_sendmsg": "sys_enter_sendmsg",
		"k_unix_sys_exit_sendmsg":  "sys_exit_sendmsg",
	}
)

// loadUnixConn loads unixconn beside objs, sharing its maps, and attaches its
// fexit programs; where those cannot load or attach (arm64 before 6.0 has no
// BPF trampoline), it loads unixconn again without them and attaches its
// syscall tracepoints instead. A kernel whose verifier refuses those too
// loads neither: startUnixConn says so, and starts no report.
func loadUnixConn(logger *zap.Logger, objs *bpfObjects) (*unixConn, error) {
	if unixFallbackOnly {
		u, err := loadUnixConnWith(logger, objs, true)
		if err == nil {
			u.why = "the fallback was asked for"
		}
		return u, err
	}
	u, ferr := loadUnixConnWith(logger, objs, false)
	if ferr == nil {
		return u, nil
	}
	u, err := loadUnixConnWith(logger, objs, true)
	if err != nil {
		return nil, fmt.Errorf("%w (and the fallback to the syscall tracepoints: %v)", ferr, err)
	}
	u.why = ferr.Error()
	return u, nil
}

func loadUnixConnWith(logger *zap.Logger, objs *bpfObjects, fallback bool) (*unixConn, error) {
	spec, err := loadUnixconn()
	if err != nil {
		return nil, fmt.Errorf("read the unixconn object: %w", err)
	}
	if fallback {
		for name, ps := range spec.Programs {
			if ps.Type == ebpf.Tracing { // a kernel without trampolines may not even load them
				delete(spec.Programs, name)
			}
		}
	}
	shared := sharedMaps(objs)
	repl := map[string]*ebpf.Map{}
	for name := range spec.Maps {
		if strings.HasPrefix(name, ".") { // its own data sections
			continue
		}
		if m := shared[name]; m != nil {
			repl[name] = m
		}
	}
	// The load, and each attach below, waits in the kernel as the hooks' do
	// (watchedCall): a perf-event attach on the tracing event_mutex, a
	// trampoline's on its own locks.
	var coll *ebpf.Collection
	err = watchedCall(logger, "bpf(BPF_PROG_LOAD) of unixconn via ebpf.NewCollectionWithOptions", func() error {
		var err error
		coll, err = ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{MapReplacements: repl})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("load the unixconn object: %w", err)
	}
	u := &unixConn{coll: coll, lost: coll.Maps["unix_connect_events_lost"], known: map[uint64]bool{}, fallback: fallback, self: uint32(os.Getpid()), drain: newReportDrain()}
	if fallback {
		for name, tp := range unixFallbackPrograms {
			var l link.Link
			err := watchedCall(logger, "perf_event_open (tracepoint syscalls/"+tp+") via link.Tracepoint", func() error {
				var err error
				l, err = link.Tracepoint("syscalls", tp, coll.Programs[name], nil)
				return err
			})
			if err != nil {
				u.close()
				return nil, fmt.Errorf("attach %s to syscalls:%s: %w", name, tp, err)
			}
			u.links = append(u.links, l)
		}
	} else {
		for _, name := range unixFexitPrograms {
			var l link.Link
			err := watchedCall(logger, "attach of fexit program "+name+" via link.AttachTracing", func() error {
				var err error
				l, err = link.AttachTracing(link.TracingOptions{Program: coll.Programs[name]})
				return err
			})
			if err != nil {
				u.close()
				return nil, fmt.Errorf("attach %s (a kernel without BPF trampolines?): %w", name, err)
			}
			u.links = append(u.links, l)
		}
	}
	if u.rd, err = ringbuf.NewReader(coll.Maps["unix_connect_events"]); err != nil {
		u.close()
		return nil, fmt.Errorf("read unix_connect_events: %w", err)
	}
	return u, nil
}

func (u *unixConn) close() {
	if u == nil {
		return
	}
	if u.rd != nil {
		_ = u.rd.Close()
	}
	for _, l := range u.links {
		_ = l.Close()
	}
	if u.coll != nil {
		u.coll.Close()
	}
}

// watch reads the reports until the reader is closed, and counts each one the
// allow-list and the owner do not excuse.
func (u *unixConn) watch(logger *zap.Logger, member func(tgid uint32) bool) {
	// However it ends (a panic too): a drain waiting on it ends with it.
	defer u.drain.stop(errors.New("keploy's AF_UNIX report ended"))
	warned := false
	for {
		rec, err := u.rd.Read()
		switch {
		case errors.Is(err, ringbuf.ErrFlushed):
			// Every report before a read's flush is handled (drain).
			u.drain.flushed()
			continue
		case errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed):
			u.drain.stop(errReportClosed)
			return
		case err != nil:
			// The ring cannot be read past here: nothing after can be
			// judged.
			u.drain.stop(fmt.Errorf("read an AF_UNIX report: %w", err))
			return
		}
		var e unixConnectEvent
		if len(rec.RawSample) != binary.Size(e) {
			// A report of another size: keploy's agent and its BPF
			// objects disagree. A call keploy cannot judge: counted.
			u.losses.Add(models.CaptureLossUnixSocket, "a report keploy could not read")
			continue
		}
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			// A call keploy cannot judge: counted, never dropped.
			u.losses.Add(models.CaptureLossUnixSocket, "a report keploy could not read")
			continue
		}
		if !u.judge(e, member) {
			continue
		}
		if !warned {
			warned = true
			fields := []zap.Field{zap.String("socket", unixWhat(e)), zap.Uint32("tgid", e.TGID),
				zap.String("next_step", "reach that dependency over a TCP address (mocked, or listed in mock.real)")}
			if u.fallback {
				fields = append(fields, zap.String("seen_by", "the syscall tracepoints: this kernel ("+kernelName()+") has no BPF trampoline"))
			}
			logger.Warn("the application called a Unix socket keploy cannot capture: the test open at the time and every later one are not judged", fields...)
		}
	}
}

// unixWhat names e's destination for the reason: its path, or why there is
// none.
func unixWhat(e unixConnectEvent) string {
	switch p := e.path(); {
	case e.Flags&unixPathUnread != 0:
		return "an address keploy could not read"
	case p == "" || p == "@":
		return "an unnamed socket"
	default:
		return p
	}
}

// judge counts e unless its destination is allow-listed or owned by a member
// of the application or by keploy's agent, and reports whether it counted it.
// An owner that cannot be found is no excuse: unknown is never safe.
func (u *unixConn) judge(e unixConnectEvent, member func(tgid uint32) bool) bool {
	p := e.path()
	readable := e.Flags&unixPathUnread == 0
	if readable && unixAllowed(p) {
		return false
	}
	switch {
	case !readable:
	case e.Kind == unixConnectStream:
		// The kernel left out a member's or the agent's listener.
	case e.Kind == unixConnectDgram && e.PeerIno != 0:
		if u.ownedByMember(e.PeerIno, member) {
			return false
		}
	default:
		// No peer in the report: whoever is bound to the path.
		if u.pathOwnedByMember(p, member) {
			return false
		}
	}
	u.losses.Add(models.CaptureLossUnixSocket, unixWhat(e))
	return true
}

// pathOwnedByMember reports whether every socket bound to p (a path, or
// "@name" for an abstract one) is held by a member of the application or by
// keploy's agent; false when none is found, or one is held by anyone else.
func (u *unixConn) pathOwnedByMember(p string, member func(tgid uint32) bool) bool {
	inos := unixInodesBoundTo(p)
	if len(inos) == 0 {
		return false
	}
	for _, ino := range inos {
		if !u.ownedByMember(ino, member) {
			return false
		}
	}
	return true
}

// unixInodesBoundTo are the inodes of the sockets /proc/net/unix lists at p:
// the listener or the bound datagram socket, and a listener's accepted ends.
func unixInodesBoundTo(p string) []uint64 {
	b, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil
	}
	var out []uint64
	for _, line := range strings.Split(string(b), "\n")[1:] {
		// Num RefCount Protocol Flags Type St Inode Path
		f := strings.Fields(line)
		if len(f) < 8 || f[7] != p {
			continue
		}
		if ino, err := strconv.ParseUint(f[6], 10, 64); err == nil && ino != 0 {
			out = append(out, ino)
		}
	}
	return out
}

// ownedByMember reports whether a member of the application holds the socket
// whose inode is ino (a datagram peer, which the kernel keeps no owner for):
// one of its open files is that socket. Members are those member says are.
func (u *unixConn) ownedByMember(ino uint64, member func(tgid uint32) bool) bool {
	u.mu.Lock()
	if v, ok := u.known[ino]; ok {
		u.mu.Unlock()
		return v
	}
	u.mu.Unlock()
	owned := false
	want := "socket:[" + strconv.FormatUint(ino, 10) + "]"
	procs, _ := os.ReadDir("/proc")
	for _, d := range procs {
		tgid, err := strconv.ParseUint(d.Name(), 10, 32)
		if err != nil || (u.self == 0 || uint32(tgid) != u.self) && !member(uint32(tgid)) {
			continue
		}
		fds, _ := os.ReadDir("/proc/" + d.Name() + "/fd")
		for _, fd := range fds {
			if l, err := os.Readlink("/proc/" + d.Name() + "/fd/" + fd.Name()); err == nil && l == want {
				owned = true
				break
			}
		}
		if owned {
			break
		}
	}
	u.mu.Lock()
	u.known[ino] = owned
	u.mu.Unlock()
	return owned
}

// snapshot is what the report counted, the reports that found no room
// included: each was a connect the agent could not judge.
func (u *unixConn) snapshot() models.CaptureLosses {
	out := u.losses.Snapshot()
	var lost uint64
	if u.lost != nil && u.lost.Lookup(uint32(0), &lost) == nil && lost > 0 {
		for i := range out {
			if out[i].Kind == models.CaptureLossUnixSocket {
				out[i].Count += lost
				return out
			}
		}
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossUnixSocket, Count: lost, What: "calls whose reports found no room"})
	}
	return out
}

// isMember reports whether tgid is a process of the application: a target,
// not excluded, as the PID maps hold it. Unlike pid_filter it does not
// compare start times (pid_start_times), so an entry a dead process left
// under the TGID counts: Unix sockets are not supported, and this owner
// check is left as it was.
func (h *Hooks) isMember(tgid uint32) bool {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	var v uint8
	if h.objects.TargetNamespacePids == nil || h.objects.TargetNamespacePids.Lookup(tgid, &v) != nil {
		return false
	}
	return h.objects.ExcludedPids == nil || h.objects.ExcludedPids.Lookup(tgid, &v) != nil
}

// loadUnixConnFn is loadUnixConn, swapped by tests.
var loadUnixConnFn = loadUnixConn

// startUnixConn loads the report and watches it until ctx ends, under the
// proven switch only: nothing reads it without the switch (as with the
// application's connection count, markProxyListener), so nothing attaches.
// The report is a detector: keploy does not support Unix sockets, and a test
// it sees make such a call is said so, not judged. A kernel that can load it
// neither way gets one WARN, and no report: keploy cannot see such calls
// there, and judges every test as it would without one.
func (h *Hooks) startUnixConn(ctx context.Context) {
	if !provenswitch.On() {
		return
	}
	h.objectsMutex.RLock()
	u, err := loadUnixConnFn(h.logger, &h.objects)
	h.objectsMutex.RUnlock()
	if err != nil {
		h.logger.Warn("keploy could not load its report of the application's calls over Unix sockets, and Unix sockets are not supported: a call over one is neither recorded nor mocked, and no test is said to have made one",
			zap.Error(err), zap.String("kernel", kernelName()),
			zap.String("next_step", "reach every dependency over a TCP address (mocked, or listed in mock.real): on this kernel keploy cannot say which tests call one over a Unix socket; to have this report here, report this error to keploy"))
		return
	}
	if u.fallback {
		h.logger.Info("keploy sees the application's calls over Unix sockets at the syscall tracepoints: this kernel has no BPF trampoline",
			zap.String("kernel", kernelName()), zap.String("fexit", u.why))
	}
	h.unixMu.Lock()
	h.unix = u
	h.unixMu.Unlock()
	go func() {
		defer utils.Recover(h.logger)
		u.watch(h.logger, h.isMember)
	}()
	// Its programs' skipped runs are read with the hooks' (skipwatch.go):
	// the counts as it closes stay counted.
	h.skipsWatch(u.coll.Programs)
	go func() {
		<-ctx.Done()
		h.skipsForget(u.coll.Programs)
		u.close()
	}()
}

// kernelName is this kernel's release and architecture, as uname gives them.
func kernelName() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown"
	}
	return unix.ByteSliceToString(u.Machine[:]) + " " + unix.ByteSliceToString(u.Release[:])
}

// unixLosses are the application's AF_UNIX connects to sockets no member of
// it owns (CaptureLosses), and, once the report stopped (it loaded, and
// then could not read on), a standing unix_unseen: every test from then on
// is not judged, as one that made such a call is. Empty with the report not
// started (the switch off, or a kernel that loads it neither way).
func (h *Hooks) unixLosses() models.CaptureLosses {
	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	if u == nil {
		return nil
	}
	out := u.snapshot()
	if u.drain != nil {
		if err := u.drain.failure(); err != nil {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossUnixUnseen, Count: 1, What: "keploy's report stopped: " + err.Error()})
		}
	}
	return out
}

// DrainCaptureLosses waits until the hooks' watches have handled every
// report the kernel made before it was called, so the read that follows
// counts a call the application made before it, on a loaded host too: the
// report of its calls over Unix sockets and, under the proven switch, the
// watch on its non-DNS UDP. No clock (reportDrain); ctx ends it. An error
// leaves what they counted unknown.
func (h *Hooks) DrainCaptureLosses(ctx context.Context) error {
	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	// A report that stopped is said by unixLosses (unix_unseen), not here.
	if u != nil && u.rd != nil {
		if err := u.drain.wait(ctx, u.rd.Flush); err != nil && u.drain.failure() == nil {
			return fmt.Errorf("wait for keploy's report of the application's calls over Unix sockets: %w", err)
		}
	}
	h.udpMu.Lock()
	w := h.udp
	h.udpMu.Unlock()
	if w != nil && w.rd != nil {
		if err := w.drain.wait(ctx, w.rd.Flush); err != nil {
			return fmt.Errorf("wait for keploy's watch on the application's UDP: %w", err)
		}
	}
	return nil
}

// CaptureLossesErr is why a count the hooks keep under the proven switch
// cannot be told: the watch on the application's non-DNS UDP that could not
// move its epoch on or read a report (udpwatch.go), or the count of
// processes k_sock_create could not classify that could not be read
// (skipwatch.go; a skip count that cannot be read is evidence missed, no
// error). What it would count is unknown, never 0, so the agent reads no
// capture losses at all while it is (Agent.CaptureLosses). (A Unix-socket
// report that stopped is a standing unix_unseen instead: unixLosses.)
func (h *Hooks) CaptureLossesErr() error {
	h.udpMu.Lock()
	w := h.udp
	h.udpMu.Unlock()
	if w != nil {
		if err := w.failure(); err != nil {
			return fmt.Errorf("keploy cannot tell whether the application's UDP got a reply from a real destination: %w", err)
		}
	}
	h.objectsMutex.RLock()
	s := h.skips
	h.objectsMutex.RUnlock()
	if s != nil {
		if err := s.err(); err != nil {
			return fmt.Errorf("keploy cannot tell whether it could classify every process it saw make a socket: %w", err)
		}
	}
	return nil
}

// udpLosses are, under the proven switch, the replies the application's
// non-DNS UDP got (udpwatch.go, CaptureLosses). Empty while not attached.
func (h *Hooks) udpLosses() models.CaptureLosses {
	h.udpMu.Lock()
	w := h.udp
	h.udpMu.Unlock()
	if w == nil {
		return nil
	}
	return w.snapshot()
}
