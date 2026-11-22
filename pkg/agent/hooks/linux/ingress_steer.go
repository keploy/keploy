//go:build linux

package linux

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// Steering: how keploy takes the application's calls while the application
// keeps its port.
//
// With the hold on (a kernel with BPF sk_lookup), the bind hooks move no bind
// (structs.FlagIngressSteer). The application listens on the port it asks
// for, P, and its bind is noted, by address and family, as a moved one is. The
// lookup steers each connection that socket would take to keploy's park, and
// the ingress forwards it to the application (SteeredDialTarget), from a
// socket whose connection the kernel notes in ingress_own as it begins it
// (OwnDialControl), so the lookup leaves that connection to the kernel, and
// so to the application's socket. Only keploy's own sockets stand between the
// application and its callers: when the agent dies (a crash, an OOM kill), they go with it, the
// lookup has nothing to steer to, and the application's socket takes every
// call, at once. Moving the bind, keploy's forwarder alone listened on P, and
// the agent's death left the application unreachable until it was restarted.
//
// A restarted agent finds the application listening already: its binds were
// made before the agent's hooks were there. So each agent keeps the notes it
// steers by in a steer table, in the pod's /dev/shm (keepSteerTable), which
// outlives a restarted container and goes with the pod, as the recording
// start does (recording_start.go). The agent after it reads the table at its
// start (seedSteering), finds which of the noted sockets listen now (the
// kernel's socket table, with each socket's cookie), and notes each again as
// the bind hooks would have, so the lookup steers them and the socket's
// release takes its note back. A noted socket the application closed while no
// agent ran is not in the socket table, and is not noted again; one it bound
// again on the same address and port is. The table names the pod's PID
// namespace and its init, and is read only by an agent of the same ones, so
// another pod's (a namespace inode the kernel gave out again) is never taken.
// Every container of the pod can write /dev/shm: a table is read only if it is
// a regular file of this agent's user (readOwnShmFile), and only a socket one
// of the application's processes holds is steered again (seedSteering), never
// one of another container's that listens where the table says; a table can
// never make keploy refuse a call.

// steerTableFile is the steer table's name in /dev/shm, followed by the PID
// namespace's inode.
const steerTableFile = "keploy-ingress-steer-"

// maxSteerTableSize bounds what is read of a steer table: app_port_binds has
// 8192 entries, each under 80 bytes as JSON.
const maxSteerTableSize = 1 << 20

// steerTable is a steer table's content.
type steerTable struct {
	Version int          `json:"keployIngressSteer"`
	PIDNS   uint64       `json:"pidns"`
	Init    uint64       `json:"init"`
	Notes   []appBindKey `json:"notes"`
}

// IngressSteers reports whether keploy steers the application's ports: the
// hold is on and the kernel has structs.FlagIngressSteer, so the bind hooks
// move no bind (ingressHoldFlag). The ingress then forwards what the park
// takes to the application (SteeredDialTarget), and dials it with
// OwnDialControl.
func (h *Hooks) IngressSteers() bool {
	return h.steering.Load() && h.IngressHeld()
}

// ownConnKey mirrors struct ingress_own_key (keploy/ebpf ingress_lookup.c):
// one of the agent's own connections to the application, by its four-tuple.
// A zero Family is a socket named before its connect, not yet begun.
type ownConnKey struct {
	Family       uint32
	Saddr, Daddr [16]byte
	Sport, Dport uint16
}

// OwnDialControl is the Control of the dialer the ingress reaches the
// application with: while the hold is on, it names the socket (its cookie)
// in ingress_own_socks before it connects, and k_ingress_own_connect writes
// the connection's four-tuple to ingress_own as the kernel begins it, so the
// lookup leaves the connection to the application's socket instead of
// steering it back to keploy. Nothing is bound first: the kernel picks the
// source port as for any connect(), by the whole four-tuple, and reuses one
// another connection left in TIME_WAIT where it allows that (the loopback,
// net.ipv4.tcp_tw_reuse), as it does for every other client. k_ingress_release
// takes both entries out as the socket goes. A no-op with the hold off. An
// error fails the dial: a connection the lookup would steer back to keploy is
// never made. OwnConnNoted checks the connection once it is made.
func (h *Hooks) OwnDialControl(_, _ string, c syscall.RawConn) error {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.ownSocks == nil {
		return nil
	}
	var nerr error
	if err := c.Control(func(fd uintptr) {
		ck, err := unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE)
		if err != nil {
			nerr = fmt.Errorf("read the cookie of keploy's connection to the application: %w", err)
			return
		}
		if err := hold.ownSocks.Update(ck, ownConnKey{}, ebpf.UpdateNoExist); err != nil {
			nerr = fmt.Errorf("note keploy's own connection to the application (ingress_own_socks): %w", err)
		}
	}); err != nil {
		return err
	}
	return nerr
}

// OwnConnNoted reports, for a connection a dialer with OwnDialControl made,
// whether k_ingress_own_connect noted its four-tuple before its SYN: an error
// when it could not (the kernel could not allocate the entry), and then the
// lookup may have steered the connection back to keploy's own park, so the
// caller closes it and does not forward the call over it. Nil with the hold
// off, or for a connection that is not a TCP one.
func (h *Hooks) OwnConnNoted(conn net.Conn) error {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.ownSocks == nil {
		return nil
	}
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var ck uint64
	var cerr error
	if err := rc.Control(func(fd uintptr) { ck, cerr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE) }); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("read the cookie of keploy's connection to the application: %w", cerr)
	}
	var k ownConnKey
	if err := hold.ownSocks.Lookup(ck, &k); err != nil {
		return fmt.Errorf("keploy's connection to the application from %s is not noted as its own (ingress_own_socks): %w", conn.LocalAddr(), err)
	}
	if k.Family == 0 {
		return fmt.Errorf("the kernel could not note keploy's connection to the application from %s as its own (ingress_own), so it may have been steered back to keploy", conn.LocalAddr())
	}
	return nil
}

// SteeredDialTarget is the address the ingress forwards a call steered to it
// to, for a call sent to to: the loopback of to's family, on to's port, where
// the application's socket that takes the call is one bound to any address
// (its note: 0.0.0.0, or :: with or without IPV6_V6ONLY as to's family
// needs) and none of its sockets is bound to to's own address or to that
// loopback address on the port; to itself otherwise (to already a loopback
// address, or a socket bound to it alone). A connection over the loopback
// whose client closes first leaves its TIME_WAIT where the kernel reuses it
// (net.ipv4.tcp_tw_reuse, on the loopback), so the ingress never runs out of
// source ports however many calls it forwards; one to another address of the
// host does not. The kernel gives a connection to the most specific socket
// bound to its address, and another socket can listen on the loopback
// address of a port an application's socket listens on at any address only
// with SO_REUSEPORT under the same user, as one more of the application's
// own; so the application's notes are what decide it.
func (h *Hooks) SteeredDialTarget(to netip.AddrPort) netip.AddrPort {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.appBinds == nil {
		return netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	}
	return steerDialTarget(to, func(k appBindKey) bool {
		var c appBindCount
		return hold.appBinds.Lookup(k, &c) == nil && c.N > 0
	})
}

// steerDialTarget is SteeredDialTarget's rule, with noted saying whether the
// application holds a socket of a note (app_port_binds).
func steerDialTarget(to netip.AddrPort, noted func(appBindKey) bool) netip.AddrPort {
	a := to.Addr().Unmap()
	to = netip.AddrPortFrom(a, to.Port())
	if a.IsLoopback() {
		return to
	}
	port := to.Port()
	exact := appBindKey{Port: port, Kind: appBindV6, Addr: a.WithZone("").As16()}
	loop := appBindKey{Port: port, Kind: appBindV6, Addr: netip.IPv6Loopback().As16()}
	anyOf := [2]appBindKey{{Port: port, Kind: appBindV6}, {Port: port, Kind: appBindV6Dual}}
	lo := netip.IPv6Loopback()
	if a.Is4() {
		exact = appBindKey{Port: port, Kind: appBindV4}
		a4 := a.As4()
		copy(exact.Addr[:4], a4[:])
		loop = appBindKey{Port: port, Kind: appBindV4}
		copy(loop.Addr[:4], []byte{127, 0, 0, 1})
		anyOf = [2]appBindKey{{Port: port, Kind: appBindV4}, {Port: port, Kind: appBindV6Dual}}
		lo = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	if noted(exact) || noted(loop) || !noted(anyOf[0]) && !noted(anyOf[1]) {
		return to
	}
	return netip.AddrPortFrom(lo, port)
}

// steerTablePath is where this pod's steer table is, or "" where none is
// kept: the host's PID or IPC namespace, whose /dev/shm outlives any pod
// (recording_start.go), or one that cannot be read.
func steerTablePath(src recordingStartSource) (path string, mine steerTable, err error) {
	pidNS, err := src.pidNS()
	if err != nil {
		return "", mine, err
	}
	ipcNS, err := src.ipcNS()
	if err != nil {
		return "", mine, err
	}
	if pidNS == procPIDInitInode || ipcNS == procIPCInitInode {
		return "", mine, nil
	}
	initStart, err := src.initStart()
	if err != nil {
		return "", mine, err
	}
	mine = steerTable{Version: 1, PIDNS: pidNS, Init: initStart}
	return filepath.Join(src.shmDir, steerTableFile+strconv.FormatUint(pidNS, 10)), mine, nil
}

// keepSteerTable writes the notes keploy steers by now to this pod's steer
// table, for an agent restarted in the pod to steer the same sockets. It is
// written to a temporary file and renamed into place (utils.WriteShmFile), so
// a reader never sees half a table, and a rename replaces whatever was there
// (a link included) without writing through it.
func (h *Hooks) keepSteerTable(src recordingStartSource) error {
	notes, err := h.steerNotes()
	if err != nil {
		return err
	}
	return writeSteerTable(src, notes)
}

// writeSteerTable writes notes as this pod's steer table (keepSteerTable).
func writeSteerTable(src recordingStartSource, notes []appBindKey) error {
	path, tbl, err := steerTablePath(src)
	if err != nil || path == "" {
		return err
	}
	tbl.Notes = notes
	b, err := json.Marshal(tbl)
	if err != nil {
		return err
	}
	return utils.WriteShmFile(path, b)
}

// steerNotes is the notes of the application's sockets keploy steers now
// (app_port_binds, each held by one socket or more), in a stable order.
func (h *Hooks) steerNotes() ([]appBindKey, error) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.appBinds == nil {
		return nil, errors.New("the ingress hold is off")
	}
	var (
		notes []appBindKey
		k     appBindKey
		c     appBindCount
	)
	it := hold.appBinds.Iterate()
	for it.Next(&k, &c) {
		if c.N > 0 {
			notes = append(notes, k)
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(notes, func(a, b appBindKey) int {
		if a.Port != b.Port {
			return int(a.Port) - int(b.Port)
		}
		if a.Kind != b.Kind {
			return int(a.Kind) - int(b.Kind)
		}
		return slices.Compare(a.Addr[:], b.Addr[:])
	})
	return notes, nil
}

// readSteerTable is the steer table an earlier agent of this pod kept at
// path: one of the same PID namespace and init (mine).
func readSteerTable(path string, mine steerTable) (steerTable, error) {
	var tbl steerTable
	b, err := readOwnShmFile(path, maxSteerTableSize)
	if err != nil {
		return tbl, err
	}
	if err := json.Unmarshal(b, &tbl); err != nil {
		return tbl, fmt.Errorf("%s is not a steer table: %w", path, err)
	}
	if tbl.Version != 1 || tbl.PIDNS != mine.PIDNS || tbl.Init != mine.Init {
		return tbl, fmt.Errorf("%s is another pod's steer table", path)
	}
	return tbl, nil
}

// listenerNote is the note the bind hooks write for a socket of port that
// listens as s (ingress_hold_note, keploy_ebpf.c): an IPv4 socket, or an
// IPv6 one on a v4-mapped address, as IPv4 to its address; an IPv6 one on ::
// by its IPV6_V6ONLY; any other IPv6 one as IPv6 to its address. ok is false
// for a socket on :: whose IPV6_V6ONLY the table does not say, which the bind
// hooks do not note either.
func listenerNote(port uint16, s util.ListenSocket) (appBindKey, bool) {
	k := appBindKey{Port: port}
	switch {
	case s.Addr.Is4():
		k.Kind = appBindV4
		a := s.Addr.As4()
		copy(k.Addr[:4], a[:])
	case s.Family != unix.AF_INET6:
		return k, false
	case s.Addr.IsUnspecified():
		if !s.V6OnlyKnown {
			return k, false
		}
		k.Kind = appBindV6Dual
		if s.V6Only {
			k.Kind = appBindV6
		}
	default:
		k.Kind = appBindV6
		k.Addr = s.Addr.WithZone("").As16()
	}
	return k, true
}

// seedSteering steers again, for an agent restarted in the pod, the sockets
// of the application that an earlier agent steered and that listen now: each
// listening socket whose note is in the pod's steer table, and that one of
// the application's processes holds (appSockets: the processes the hooks
// take as the application's), is noted as the bind hooks would have noted it
// (by its cookie, in app_bind_socks, and counted in app_port_binds), unless
// it is noted already (a bind since this agent's hooks attached): in the
// kernel (seedNote), under the count's lock as each bind and release, so
// none of their counts is lost to a seeding beside them. A socket
// of another container of the pod that listens where the table says is never
// steered: every container can write /dev/shm, and a table can name any
// address. A socket that went between the read of the socket table and its
// note is not left counted (unseedGone). It returns the ports it steers
// again, for the ingress to start on as it does for a bind event, and
// whether there was a table. It runs once the hold is on and the bind hooks
// steer (sendFinalAgentInfo), so no bind of the application's is moved
// meanwhile.
func (h *Hooks) seedSteering(src recordingStartSource) (seeded []uint16, found bool, err error) {
	path, mine, err := steerTablePath(src)
	if err != nil || path == "" {
		return nil, false, err
	}
	tbl, err := readSteerTable(path, mine)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil // the pod's first agent, or a table that was lost
	}
	if err != nil {
		return nil, true, err
	}
	noted := map[appBindKey]bool{}
	var ports []uint16
	for _, k := range tbl.Notes {
		noted[k] = true
		if !slices.Contains(ports, k.Port) {
			ports = append(ports, k.Port)
		}
	}
	if len(ports) == 0 {
		return nil, true, nil
	}
	owned, err := h.appSockets()
	if err != nil {
		return nil, true, fmt.Errorf("read which sockets the application holds: %w", err)
	}
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.appBinds == nil || hold.appSocks == nil {
		return nil, true, errors.New("the ingress hold is off")
	}
	for _, port := range ports {
		socks, err := util.TCPListenSockets(port)
		if err != nil {
			return seeded, true, fmt.Errorf("read which sockets listen on port %d: %w", port, err)
		}
		already := false
		var added []seededSock
		for _, s := range socks {
			k, ok := listenerNote(port, s)
			if !ok || !noted[k] || !owned[s.Inode] {
				continue
			}
			switch r, err := seedNote(hold, seedOpNote, s.Cookie, k); {
			case err != nil:
				return seeded, true, fmt.Errorf("note the application's socket on port %d again: %w", port, err)
			case r == seedNoted:
				already = true // noted by its bind since this agent's hooks attached
				continue
			case r == seedNoRoom:
				return seeded, true, fmt.Errorf("note the application's socket on port %d again: app_bind_socks or app_port_binds has no room", port)
			case r != seedDone:
				return seeded, true, fmt.Errorf("note the application's socket on port %d again: the seeding answered %d", port, r)
			}
			added = append(added, seededSock{cookie: s.Cookie, note: k})
		}
		kept := len(added)
		if kept > 0 {
			kept -= unseedGone(hold, port, added)
		}
		if already || kept > 0 {
			seeded = append(seeded, port)
		}
	}
	return seeded, true, nil
}

// The seeding's operations and answers (k_ingress_seed, keploy/ebpf
// ingress_lookup.c).
const (
	seedOpNote   = 1 // note a socket as the bind hooks would have
	seedOpUnnote = 2 // take back a note the seeding wrote, for a socket that went

	seedDone   = 0 // noted, or taken back
	seedNoted  = 1 // noted already: nothing done
	seedGone   = 2 // nothing to take back: its release took it back
	seedNoRoom = 3 // no room in app_bind_socks or app_port_binds
)

// seedNote runs k_ingress_seed once for the socket of cookie and its note k:
// the note and its count are written in the kernel, under the count's lock,
// as every bind and release of the same note is, so none of their counts is
// lost to a read and write from here.
func seedNote(hold *ingressHold, op uint32, cookie uint64, k appBindKey) (uint32, error) {
	var b bytes.Buffer
	b.Write(make([]byte, 14)) // the Ethernet header's room
	if err := binary.Write(&b, binary.LittleEndian, struct {
		Cookie uint64
		Key    appBindKey
		Op     uint32
	}{cookie, k, op}); err != nil {
		return 0, err
	}
	r, err := hold.seed.Run(&ebpf.RunOptions{Data: b.Bytes()})
	if err != nil {
		return 0, fmt.Errorf("run the seeding program: %w", err)
	}
	return r, nil
}

// seededSock is a socket seedSteering noted, by its cookie, and its note.
type seededSock struct {
	cookie uint64
	note   appBindKey
}

// unseedGone takes back the notes seedSteering just wrote on port for
// sockets that have gone since it read the socket table: a socket that went
// before its note was written had no release to take it back. A socket that
// goes later is taken back by its release, which deletes its note first, so
// whichever deletes the note takes the count back (both in the kernel, under
// the count's lock). It returns how many it took back.
func unseedGone(hold *ingressHold, port uint16, added []seededSock) int {
	socks, err := util.TCPListenSockets(port)
	if err != nil {
		return 0
	}
	live := map[uint64]bool{}
	for _, s := range socks {
		live[s.Cookie] = true
	}
	gone := 0
	for _, s := range added {
		if live[s.cookie] {
			continue
		}
		if r, err := seedNote(hold, seedOpUnnote, s.cookie, s.note); err != nil || r != seedDone {
			continue // its release took it back
		}
		gone++
	}
	return gone
}

// appSockets is the inodes of the sockets the application's processes hold
// (appSocketInodes, from the recording's start), or the test's.
func (h *Hooks) appSockets() (map[uint32]bool, error) {
	if h.appSocketsFn != nil {
		return h.appSocketsFn()
	}
	h.agentInfoMu.Lock()
	start := h.agentInfo.RecordingStartTime
	h.agentInfoMu.Unlock()
	if start == 0 {
		return nil, errors.New("the recording's start is not known, so neither are the application's processes")
	}
	return appSocketInodes("/proc", start)
}

// startSteering, for the hold that is on now, steers again what an earlier
// agent of the pod steered (seedSteering), keeps the table for the agent
// after this one, and keeps the ports it steers again for WatchBindEvents to
// give the ingress first. A table that cannot be read or kept is said once at
// WARN, and so is a restarted agent's missing one: this agent records what
// the application binds from now on, and the sockets it steered before its
// restart go to the application unrecorded.
func (h *Hooks) startSteering(src recordingStartSource) {
	if !h.IngressSteers() {
		return
	}
	seeded, found, err := h.seedSteering(src)
	switch {
	case err != nil:
		h.logger.Warn("keploy could not steer again the application's ports an earlier agent of this pod steered; calls to them reach the application unrecorded until it binds them again",
			zap.Error(err))
	case !found && h.restartedInPod():
		h.logger.Warn("this keploy agent was restarted in a pod whose application runs on, and the earlier agent's steer table is not in the pod's /dev/shm (it could not be written, or /dev/shm was full); calls to the ports the application bound before reach it unrecorded until it binds them again",
			zap.String("table", steerTableFile+"<pid namespace>"))
	}
	if len(seeded) > 0 {
		h.logger.Info("keploy steers again the application's ports an earlier agent of this pod steered (this agent was restarted)",
			zap.Any("ports", seeded))
		h.keepSteerTableOrSay(src)
	}
	h.ingressMu.Lock()
	h.steeredAgain = seeded
	h.ingressMu.Unlock()
}

// restartedInPod reports whether an earlier agent of this pod set the
// recording's start (recording_start.go): it is older than this process.
func (h *Hooks) restartedInPod() bool {
	h.agentInfoMu.Lock()
	start := h.agentInfo.RecordingStartTime
	h.agentInfoMu.Unlock()
	mine, err := procStartTicks("/proc/self/stat")
	return err == nil && start > 0 && start/uint64(clockTick.Nanoseconds()) < mine
}

// keepSteerTableOrSay keeps the steer table (keepSteerTable), and says at
// WARN, once, when it cannot: an agent restarted in the pod then steers none
// of the ports the application bound before.
func (h *Hooks) keepSteerTableOrSay(src recordingStartSource) {
	if err := h.keepSteerTable(src); err != nil {
		h.steerTableWarn.Do(func() {
			h.logger.Warn("keploy could not keep the application's ports in its steer table in the pod's /dev/shm; if this agent is restarted, calls to those ports reach the application unrecorded until it binds them again",
				zap.Error(err))
		})
	}
}

// steeredAgainEvents is a bind event of each port startSteering steers again,
// as the bind hooks give a bind of a port they steer: the port to itself.
func (h *Hooks) steeredAgainEvents() []models.IngressEvent {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	evs := make([]models.IngressEvent, 0, len(h.steeredAgain))
	for _, p := range h.steeredAgain {
		evs = append(evs, models.IngressEvent{OrigAppPort: p, NewAppPort: p})
	}
	return evs
}

// KeepSteerTable keeps the notes keploy steers by now in the pod's steer
// table (keepSteerTable), for an agent restarted in the pod: the ingress
// manager calls it as it starts on a port the hooks steer (a bind event of
// the port to itself). A no-op unless keploy steers.
func (h *Hooks) KeepSteerTable() {
	if !h.IngressSteers() {
		return
	}
	h.keepSteerTableOrSay(h.shmSource())
}

// shmSource is where the steer table is kept: this process's /dev/shm and
// namespaces, or a test's.
func (h *Hooks) shmSource() recordingStartSource {
	if h.shmSrc != nil {
		return *h.shmSrc
	}
	return hostRecordingStart()
}
