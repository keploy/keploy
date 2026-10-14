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

	"go.keploy.io/server/v3/pkg/agent/captureloss"
	"go.keploy.io/server/v3/pkg/models"
)

// The AF_UNIX connect report (keploy/ebpf unixconn.c): the application's
// connects to Unix sockets no member of it owns, a call that goes nowhere the
// proxy sees, neither recorded nor mocked (design v14 §3.2 (a)). The kernel
// reports each successful one; the agent drops the allowlisted ones and
// counts the rest as capture losses (unix_socket), which a test that ends
// after one is charged with under the proven switch.

// unixConnectEvent mirrors struct unix_connect_event (keploy/ebpf k_unix.h).
type unixConnectEvent struct {
	TGID     uint32
	PeerTGID uint32
	PeerIno  uint64
	Kind     uint32
	PathLen  uint32
	Path     [108]byte
}

const unixConnectDgram = 2

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

// unixAllowlist are the Unix sockets the application may reach without a
// loss: the desktop and system buses, the journal, the Docker daemon, and the
// name services glibc asks on a host lookup (design v14 §3.2 (a); 08's
// allowlist). A pattern is a path.Match pattern; "@" starts an abstract name.
var unixAllowlist = []string{
	// D-Bus
	"/run/dbus/system_bus_socket", "/var/run/dbus/system_bus_socket", "/run/user/*/bus", "@/tmp/dbus-*",
	// X11 and Wayland
	"/tmp/.X11-unix/X*", "@/tmp/.X11-unix/X*", "/run/user/*/wayland-*",
	// journald and syslog
	"/run/systemd/journal/*", "/var/run/systemd/journal/*", "/dev/log",
	// the Docker daemon (rootful and rootless)
	"/var/run/docker.sock", "/run/docker.sock", "/run/user/*/docker.sock",
	// name services: nscd, SSSD, systemd-resolved, avahi, systemd's userdb
	"/var/run/nscd/socket", "/run/nscd/socket", "/var/lib/sss/pipes/*",
	"/run/systemd/resolve/io.systemd.Resolve", "/run/avahi-daemon/socket", "/var/run/avahi-daemon/socket",
	"/run/systemd/userdb/*",
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

// loadUnixConn loads unixconn beside objs, sharing its maps, and attaches its
// fexit programs. A kernel that cannot (arm64 before 6.0 has no BPF
// trampoline) leaves the report off, and says why.
func loadUnixConn(objs *bpfObjects) (*unixConn, error) {
	spec, err := loadUnixconn()
	if err != nil {
		return nil, fmt.Errorf("read the unixconn object: %w", err)
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
	u := &unixConn{coll: coll, lost: coll.Maps["unix_connect_events_lost"], known: map[uint64]bool{}}
	for _, name := range []string{"k_unix_stream_connect", "k_unix_dgram_connect"} {
		l, err := link.AttachTracing(link.TracingOptions{Program: coll.Programs[name]})
		if err != nil {
			u.close()
			return nil, fmt.Errorf("attach %s (a kernel without BPF trampolines?): %w", name, err)
		}
		u.links = append(u.links, l)
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
// allowlist and the peer's owner do not excuse.
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
			logger.Warn("the application connected to a Unix socket keploy does not see: a call over it is neither recorded nor mocked",
				zap.String("socket", e.path()), zap.Uint32("tgid", e.TGID),
				zap.String("next_step", "reach that dependency over a TCP address (mocked, or listed in mock.real)"))
		}
	}
}

// judge counts e unless it is allowlisted or its peer is a member's, and
// reports whether it counted it.
func (u *unixConn) judge(e unixConnectEvent, member func(tgid uint32) bool) bool {
	p := e.path()
	if unixAllowed(p) {
		return false
	}
	if e.Kind == unixConnectDgram && e.PeerIno != 0 && u.ownedByMember(e.PeerIno, member) {
		return false
	}
	what := p
	if what == "" {
		what = "an unnamed socket"
	}
	u.losses.Add(models.CaptureLossUnixSocket, what)
	return true
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
		if err != nil || !member(uint32(tgid)) {
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
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossUnixSocket, Count: lost, What: "connects whose reports found no room"})
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

// startUnixConn loads the report and watches it until ctx ends; a kernel that
// cannot load it gets a WARN, once, and the report stays off.
func (h *Hooks) startUnixConn(ctx context.Context) {
	h.objectsMutex.RLock()
	u, err := loadUnixConn(&h.objects)
	h.objectsMutex.RUnlock()
	if err != nil {
		h.logger.Warn("keploy cannot see the application's AF_UNIX connects on this kernel: a call over a Unix socket to a dependency is neither recorded nor mocked, and keploy cannot say so",
			zap.Error(err), zap.String("next_step", "reach dependencies over TCP addresses; on arm64, a 6.0 or later kernel lets keploy see these connects"))
		h.unixMu.Lock()
		h.unix = &unixConn{off: err.Error()}
		h.unixMu.Unlock()
		return
	}
	h.unixMu.Lock()
	h.unix = u
	h.unixMu.Unlock()
	go u.watch(h.logger, h.isMember)
	go func() {
		<-ctx.Done()
		u.close()
	}()
}

// CaptureLosses is what the hooks saw the application do that keploy could
// not capture, besides the PID-map refusals: its AF_UNIX connects to sockets
// no member owns, and (under the proven switch) replies its non-DNS UDP got.
func (h *Hooks) CaptureLosses() models.CaptureLosses {
	var out models.CaptureLosses
	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	if u != nil && u.off == "" {
		out = append(out, u.snapshot()...)
	}
	// Under the proven switch, replies to the application's non-DNS UDP
	// (udpwatch.go).
	h.udpMu.Lock()
	w := h.udp
	h.udpMu.Unlock()
	if w != nil {
		out = append(out, w.snapshot()...)
	}
	return out
}
