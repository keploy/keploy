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
// are each counted: unknown is never safe.

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
// counted. off says why it is not loaded ("" when it is).
type unixConn struct {
	coll   *ebpf.Collection
	links  []link.Link
	rd     *ringbuf.Reader
	lost   *ebpf.Map
	losses captureloss.Counter
	off    string
	// fallback: the syscall tracepoints report, not fexit (no trampoline);
	// why is the attach error that chose it.
	fallback bool
	why      string
	// self is keploy's agent's TGID: a socket it holds is keploy's own.
	self uint32

	mu    sync.Mutex
	known map[uint64]bool // dgram peer inode -> owned by a member
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
// syscall tracepoints instead, which every kernel has.
func loadUnixConn(objs *bpfObjects) (*unixConn, error) {
	if unixFallbackOnly {
		u, err := loadUnixConnWith(objs, true)
		if err == nil {
			u.why = "the fallback was asked for"
		}
		return u, err
	}
	u, ferr := loadUnixConnWith(objs, false)
	if ferr == nil {
		return u, nil
	}
	u, err := loadUnixConnWith(objs, true)
	if err != nil {
		return nil, fmt.Errorf("%w (and the fallback to the syscall tracepoints: %v)", ferr, err)
	}
	u.why = ferr.Error()
	return u, nil
}

func loadUnixConnWith(objs *bpfObjects, fallback bool) (*unixConn, error) {
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
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{MapReplacements: repl})
	if err != nil {
		return nil, fmt.Errorf("load the unixconn object: %w", err)
	}
	u := &unixConn{coll: coll, lost: coll.Maps["unix_connect_events_lost"], known: map[uint64]bool{}, fallback: fallback, self: uint32(os.Getpid())}
	if fallback {
		for name, tp := range unixFallbackPrograms {
			l, err := link.Tracepoint("syscalls", tp, coll.Programs[name], nil)
			if err != nil {
				u.close()
				return nil, fmt.Errorf("attach %s to syscalls:%s: %w", name, tp, err)
			}
			u.links = append(u.links, l)
		}
	} else {
		for _, name := range unixFexitPrograms {
			l, err := link.AttachTracing(link.TracingOptions{Program: coll.Programs[name]})
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
	warned := false
	for {
		rec, err := u.rd.Read()
		if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		if err != nil {
			logger.Debug("failed to read an AF_UNIX connect report", zap.Error(err))
			continue
		}
		var e unixConnectEvent
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			logger.Debug("failed to decode an AF_UNIX connect report", zap.Error(err))
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

// isMember reports whether tgid is a process of the application, as
// pid_filter decides it: a target, not excluded.
func (h *Hooks) isMember(tgid uint32) bool {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	var v uint8
	if h.objects.TargetNamespacePids == nil || h.objects.TargetNamespacePids.Lookup(tgid, &v) != nil {
		return false
	}
	return h.objects.ExcludedPids == nil || h.objects.ExcludedPids.Lookup(tgid, &v) != nil
}

// startUnixConn loads the report and watches it until ctx ends, under the
// proven switch only: nothing reads it without the switch (as with the
// application's connection count, markProxyListener), so nothing attaches. A
// kernel that can load it neither way gets a WARN, once, and the report stays
// off: what it would count is unknown (CaptureLossesErr).
func (h *Hooks) startUnixConn(ctx context.Context) {
	if !provenswitch.On() {
		return
	}
	h.objectsMutex.RLock()
	u, err := loadUnixConn(&h.objects)
	h.objectsMutex.RUnlock()
	if err != nil {
		h.logger.Warn("keploy cannot see the application's calls over Unix sockets on this kernel: a call over one to a dependency is neither recorded nor mocked, and keploy cannot say so",
			zap.Error(err), zap.String("next_step", "reach dependencies over TCP addresses"))
		h.unixMu.Lock()
		h.unix = &unixConn{off: err.Error()}
		h.unixMu.Unlock()
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
	go func() {
		<-ctx.Done()
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
// it owns (CaptureLosses). Empty while the report is off or not loaded.
func (h *Hooks) unixLosses() models.CaptureLosses {
	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	if u == nil || u.off != "" {
		return nil
	}
	return u.snapshot()
}

// CaptureLossesErr is why a count the hooks keep under the proven switch could
// not start: the report of the application's calls over Unix sockets, on a
// kernel that loads it neither way. What it would count is unknown, never 0,
// so the agent reads no capture losses at all while it is (Agent.CaptureLosses).
func (h *Hooks) CaptureLossesErr() error {
	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	if u != nil && u.off != "" {
		return fmt.Errorf("keploy cannot see the application's calls over Unix sockets on this kernel, so whether a test made one cannot be told: %s", u.off)
	}
	// Under the proven switch, the watch on the application's non-DNS UDP
	// (udpwatch.go).
	h.udpMu.Lock()
	w := h.udp
	h.udpMu.Unlock()
	if w != nil {
		if err := w.failure(); err != nil {
			return fmt.Errorf("keploy cannot tell whether the application's UDP got a reply from a real destination: %w", err)
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
