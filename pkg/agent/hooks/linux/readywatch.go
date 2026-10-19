//go:build linux

package linux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// The proven switch's readiness watch (keploy/ebpf k_ready.h; design v14
// §8.4, row W6). keploy test fires its first test once the application
// listens where that test dials, never after a time, and calls a start that
// makes no progress across its watchdog's count a stall. The kernel reports
// each TCP listen of the application's (a member's socket entering LISTEN: a
// bound socket that does not listen refuses a connect), and counts its
// listens, connects, process forks and execs; the watch adds the CPU time
// its processes used, so a start that only computes (a JIT warming up, a
// migration run in-process) is going on too. The members are a native app's
// processes (the keploy client's descendants) or, in Docker mode, each
// container's main process and its descendants, of the containers in the
// agent's network namespace, which the application shares, but the
// agent's own: what an exec into a container starts (a docker exec, a
// healthcheck) is no member. Only under KEPLOY_PROVEN: the agent then sets
// FlagProvenReady and attaches the bind and connect programs and the fork,
// exec and exit tracepoints, which today's users never run.

// readyListenEventSize is struct ready_listen_event's size: a report of any
// other size is one keploy cannot read.
const readyListenEventSize = 40

// readyListenEvent mirrors struct ready_listen_event (k_ready.h).
type readyListenEvent struct {
	TGID   uint32
	Netns  uint32
	Addr   [4]uint32 // network order, IPv4 as ::ffff:a.b.c.d
	Port   uint16
	Family uint16
	_      uint32
	Cookie uint64 // the socket's (bpf_get_socket_cookie's, SO_COOKIE's)
}

// readyCfg mirrors struct ready_cfg_t: user space writes it, before the
// flag. The agent's mount namespace, which the kernel records at the agent's
// own TCP bind (registerSelf), is ready_agent_mntns, the kernel its only
// writer.
type readyCfg struct {
	Netns uint32
}

func (e readyListenEvent) addr() netip.Addr {
	var b [16]byte
	for i, w := range e.Addr {
		binary.LittleEndian.PutUint32(b[4*i:], w) // the words hold network-order bytes
	}
	return netip.AddrFrom16(b).Unmap()
}

type readyWatch struct {
	links []link.Link
	rd    *ringbuf.Reader
	maps  *bpfObjects
	// netns is the agent's network namespace (inode): a listen in it is
	// where keploy test's requests reach the application.
	netns  uint32
	docker bool

	// selfOnce runs registerSelf once, at the hooks' load, once the flag is
	// set (selfRegistered).
	selfOnce sync.Once
	selfErr  error

	mu      sync.Mutex
	listens []models.AppListen // in order of Seq, as many of each socket address as listensPerAddress
	told    uint64             // how many listens the kernel reported in all
	// err is why the watch's reports cannot be told any more; it stays.
	err error
	// cpu is the application's CPU time as progress (cpuTally).
	cpu cpuTally
}

// startProvenReady starts the readiness watch under the proven switch, for
// keploy test only, and returns the agent-info flag that turns the kernel's
// side on: the programs first, then the flag. With the switch off nothing is
// attached and no flag is returned.
func (h *Hooks) startProvenReady(cg string, docker bool, mode models.Mode) (uint32, error) {
	if !provenswitch.On() || mode != models.MODE_TEST {
		// Only keploy test waits on it: elsewhere its fork and exec
		// tracepoints would run on every process for no reader.
		return 0, nil
	}
	w, err := startReadyWatch(&h.objects, cg, docker)
	if err != nil {
		return 0, err
	}
	h.readyMu.Lock()
	h.ready = w
	h.readyMu.Unlock()
	go func() {
		defer utils.Recover(h.logger)
		w.watch(h.logger)
	}()
	return structs.FlagProvenReady, nil
}

// startReadyWatch attaches the readiness watch's programs (the bind and
// connect programs to the cgroup at cg, beside keploy's own) and reads its
// reports. An error leaves nothing
// attached.
func startReadyWatch(objs *bpfObjects, cg string, docker bool) (*readyWatch, error) {
	if objs.ReadyCfg == nil || objs.ReadyAgentMntns == nil || objs.ReadyMains == nil || objs.ReadyLive == nil || objs.ReadySeen == nil || objs.ReadyHolders == nil || objs.ReadyLastRestart == nil || objs.ReadySocks == nil || objs.ReadyListenEvents == nil ||
		objs.ReadyListenEventsLost == nil || objs.ReadyProgress == nil || objs.K_readyBind4 == nil || objs.K_readyBind6 == nil ||
		objs.K_readyConnect4 == nil || objs.K_readyConnect6 == nil || objs.K_readyFork == nil || objs.K_readyExec == nil || objs.K_readyExit == nil {
		return nil, errors.New("keploy's BPF objects are not loaded: the readiness watch's programs and maps are missing")
	}
	netns, err := threadNetns()
	if err != nil {
		return nil, err
	}
	w := &readyWatch{maps: objs, netns: netns, docker: docker}
	if err := objs.ReadyCfg.Update(uint32(0), readyCfg{Netns: netns}, ebpf.UpdateAny); err != nil {
		return nil, fmt.Errorf("set the readiness watch's network namespace: %w", err)
	}
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
		name   string
	}{
		{objs.K_readyBind4, ebpf.AttachCGroupInet4Bind, "k_ready_bind4"},
		{objs.K_readyBind6, ebpf.AttachCGroupInet6Bind, "k_ready_bind6"},
		{objs.K_readyConnect4, ebpf.AttachCGroupInet4Connect, "k_ready_connect4"},
		{objs.K_readyConnect6, ebpf.AttachCGroupInet6Connect, "k_ready_connect6"},
	} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: p.attach, Program: p.prog})
		if err != nil {
			w.close()
			return nil, fmt.Errorf("attach %s: %w", p.name, err)
		}
		w.links = append(w.links, l)
	}
	for _, p := range []struct {
		prog *ebpf.Program
		tp   string
	}{{objs.K_readyFork, "sched_process_fork"}, {objs.K_readyExec, "sched_process_exec"}, {objs.K_readyExit, "sched_process_exit"}} {
		l, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: p.tp, Program: p.prog})
		if err != nil {
			w.close()
			return nil, fmt.Errorf("attach the %s tracepoint: %w", p.tp, err)
		}
		w.links = append(w.links, l)
	}
	rd, err := ringbuf.NewReader(objs.ReadyListenEvents)
	if err != nil {
		w.close()
		return nil, fmt.Errorf("read ready_listen_events: %w", err)
	}
	w.rd = rd
	return w, nil
}

// threadNetns is the inode of the calling thread's network namespace: of a
// thread, not of the process's leader (/proc/self), which a goroutine locked
// to it may have moved elsewhere.
func threadNetns() (uint32, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/thread-self/ns/net", &st); err != nil {
		return 0, fmt.Errorf("read the agent's network namespace: %w", err)
	}
	return uint32(st.Ino), nil
}

func (w *readyWatch) close() {
	if w == nil {
		return
	}
	if w.rd != nil {
		_ = w.rd.Close()
	}
	for _, l := range w.links {
		_ = l.Close()
	}
}

// watch keeps each reported listen until the reader is closed.
func (w *readyWatch) watch(logger *zap.Logger) {
	for {
		rec, err := w.rd.Read()
		switch {
		case errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed):
			w.fail(errors.New("the readiness watch was closed"))
			return
		case err != nil:
			w.fail(fmt.Errorf("read a listen report: %w", err))
			return
		}
		if len(rec.RawSample) != readyListenEventSize {
			w.fail(fmt.Errorf("a listen report of %d bytes, want %d: keploy's agent and its BPF objects disagree", len(rec.RawSample), readyListenEventSize))
			continue
		}
		var e readyListenEvent
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			w.fail(fmt.Errorf("decode a listen report: %w", err))
			continue
		}
		l := w.handle(e)
		logger.Debug("the application listens", zap.Uint16("port", l.Port), zap.String("addr", l.Addr), zap.Bool("inTheAgentsNetworkNamespace", l.Here), zap.Uint32("tgid", l.TGID))
	}
}

// listensPerAddress is how many listens the watch keeps of one socket
// address (port, address, namespace), the latest: a listen and close loop,
// a kept-alive application read from its first listen, cost no more than
// the sockets it listens on (as many as one SO_REUSEPORT pool has).
const listensPerAddress = 64

func (w *readyWatch) handle(e readyListenEvent) models.AppListen {
	l := models.AppListen{Port: e.Port, Addr: e.addr().String(), Here: e.Netns == w.netns, TGID: e.TGID, Cookie: e.Cookie}
	w.mu.Lock()
	defer w.mu.Unlock()
	l.Seq = w.told
	w.told++
	same, oldest := 0, -1
	for i, k := range w.listens {
		if k.Port == l.Port && k.Addr == l.Addr && k.Here == l.Here {
			if same++; oldest < 0 {
				oldest = i
			}
		}
	}
	if same >= listensPerAddress {
		w.listens = append(w.listens[:oldest], w.listens[oldest+1:]...)
	}
	w.listens = append(w.listens, l)
	return l
}

// listensFrom is the count of listens the watch was told, and those it
// keeps from since on (Seq since or later), in order; or why it can no
// longer watch.
func (w *readyWatch) listensFrom(since uint64) (uint64, []models.AppListen, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, nil, w.err
	}
	var out []models.AppListen
	for _, l := range w.listens {
		if l.Seq >= since {
			out = append(out, l)
		}
	}
	return w.told, out, nil
}

func (w *readyWatch) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

// snapshot is what the watch knows now (models.AppReadiness), of the listens
// from since on.
func (w *readyWatch) snapshot(clientPID uint32, since uint64) (models.AppReadiness, error) {
	told, listens, err := w.listensFrom(since)
	if err != nil {
		return models.AppReadiness{}, err
	}
	out := models.AppReadiness{ListenCount: told, Listens: listens}
	// The CPU time first, then the restarts: a restart after the CPU read is
	// in the count read after it, so its process's CPU time is never weighed
	// under the count before it (keploy test weighs CPU time until the start
	// restarts).
	if members, ok := memberCPU("/proc", os.Getpid(), w.docker, clientPID); ok {
		out.CPU, out.CPUKnown = w.cpu.add(members), true
	}
	if err := w.maps.ReadyListenEventsLost.Lookup(uint32(0), &out.ListensLost); err != nil {
		return models.AppReadiness{}, fmt.Errorf("read the listens the kernel had no room to report: %w", err)
	}
	var n [5]uint64
	for i := range n {
		if err := w.maps.ReadyProgress.Lookup(uint32(i), &n[i]); err != nil {
			return models.AppReadiness{}, fmt.Errorf("read the application's progress: %w", err)
		}
	}
	out.Progress = models.ReadyProgress{Listens: n[0], Connects: n[1], Forks: n[2], Execs: n[3], Restarts: n[4]}
	if n[4] > 0 {
		var r struct{ Program [64]byte }
		if err := w.maps.ReadyLastRestart.Lookup(uint32(0), &r); err == nil {
			out.LastRestart = cString(r.Program[:])
		}
	}
	// A listen reported holds while its own socket still listens: the
	// kernel keeps each reported listen's cookie in ready_live until the
	// socket leaves LISTEN.
	for i := range out.Listens {
		var tgid uint32
		switch err := w.maps.ReadyLive.Lookup(out.Listens[i].Cookie, &tgid); {
		case err == nil:
			out.Listens[i].Live = true
		case !errors.Is(err, ebpf.ErrKeyNotExist):
			return models.AppReadiness{}, fmt.Errorf("read whether the application's listen on :%d still listens: %w", out.Listens[i].Port, err)
		}
	}
	out.Listens = latestOfEach(out.Listens)
	return out, nil
}

// registerSelf has the kernel record the agent's mount namespace
// (ready_agent_mntns): one TCP bind of the agent's own, once the switch's
// flag is set, in Docker mode (native mode's members are the client's
// descendants, whatever their namespaces). There what runs in the agent's
// container (keploy-agent's healthcheck, an exec every interval) shares the
// application's network namespace, and until the kernel knows the agent's
// container it takes nothing for a member.
func (w *readyWatch) registerSelf() error {
	if !w.docker {
		return nil
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("record the agent's mount namespace: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return fmt.Errorf("record the agent's mount namespace: %w", err)
	}
	var mnt uint32
	if err := w.maps.ReadyAgentMntns.Lookup(uint32(0), &mnt); err != nil {
		return fmt.Errorf("read the agent's mount namespace back: %w", err)
	}
	if mnt == 0 {
		return errors.New("the kernel recorded no mount namespace for the agent's own bind")
	}
	// The kernel records a container's main at its first exec from the flag
	// on: one that already runs in this network namespace has none, and the
	// first exec into it (a healthcheck) would take it.
	pid, ok, err := strangerInNetns("/proc", os.Getpid())
	if err != nil {
		return fmt.Errorf("tell whether another container already runs in the agent's network namespace: %w", err)
	}
	if ok {
		return fmt.Errorf("process %d already runs in the agent's network namespace from another container, before the agent watches: the kernel knows no main of its container (an application's container must start after keploy's agent is ready)", pid)
	}
	return nil
}

// strangerInNetns is a process, in the /proc at root, in self's network
// namespace and another mount namespace (another container's): the lowest
// pid, if any. What cannot be read is an error, never "none" (but a process
// gone since the listing).
func strangerInNetns(root string, self int) (int, bool, error) {
	net, err := os.Readlink(fmt.Sprintf("%s/%d/ns/net", root, self))
	if err != nil {
		return 0, false, err
	}
	mnt, err := os.Readlink(fmt.Sprintf("%s/%d/ns/mnt", root, self))
	if err != nil {
		return 0, false, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0, false, err
	}
	found := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		n, err := os.Readlink(fmt.Sprintf("%s/%d/ns/net", root, pid))
		if err == nil {
			var m string
			if m, err = os.Readlink(fmt.Sprintf("%s/%d/ns/mnt", root, pid)); err == nil {
				if n == net && m != mnt && (found == 0 || pid < found) {
					found = pid
				}
				continue
			}
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue // gone since the listing
		}
		return 0, false, fmt.Errorf("read process %d's namespaces: %w", pid, err)
	}
	return found, found != 0, nil
}

// cString is the NUL-terminated string at the start of b: the kernel writes
// a shorter name over a longer one in place, leaving the longer one's tail
// after the NUL.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// clockTick is the unit of /proc/<pid>/stat's CPU times (USER_HZ, 100 on
// every Linux architecture keploy runs on).
const clockTick = 10 * time.Millisecond

// procStat is what memberCPU reads of /proc/<pid>/stat (and, in Docker
// mode, of /proc/<pid>/ns/mnt).
type procStat struct {
	ppid  int
	cpu   uint64 // utime + stime, its own, in clock ticks (a reaped child's is not: a crashed restart's would come back as its supervisor's)
	start uint64 // starttime, in clock ticks after boot
	mnt   string // its mount namespace (Docker mode)
}

// memberCPU lists the application's processes, with their own CPU time,
// from the /proc at root: in Docker mode each container's
// main process and its descendants, of the containers in the agent's PID
// namespace (the application shares it) but the agent's own (its mount
// namespace); else the keploy client's descendants; never the agent or
// those above it. A container's main is its oldest process whose parent is
// outside the namespace (the runtime's shim started it, ppid 0 here): what
// an exec into the container starts (a healthcheck, a docker exec) has the
// shim for its parent too, but started later, and is no member, as the
// kernel's ready_mains has it (its first exec there). ok is false when it
// cannot be read.
func memberCPU(root string, self int, docker bool, client uint32) (map[int]procStat, bool) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, false
	}
	var agentMntns string
	if docker {
		if agentMntns, err = os.Readlink(fmt.Sprintf("%s/%d/ns/mnt", root, self)); err != nil {
			return nil, false
		}
	}
	stats := map[int]procStat{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		s, ok := readProcStat(root, pid)
		if !ok {
			continue
		}
		if docker {
			if s.mnt, err = os.Readlink(fmt.Sprintf("%s/%d/ns/mnt", root, pid)); err != nil {
				continue // gone, or not ours to read: no member known
			}
		}
		stats[pid] = s
	}
	if _, ok := stats[self]; !ok && len(stats) > 0 && docker {
		return nil, false // the agent cannot see itself: not its PID namespace's /proc
	}
	// The agent and those above it are never the application's: a native
	// agent the keploy client started (through sudo) is one of its
	// descendants.
	notMembers := map[int]bool{}
	for p := self; p > 0 && !notMembers[p]; p = stats[p].ppid {
		notMembers[p] = true
	}
	if !docker && client == 0 {
		return nil, false
	}
	// Docker mode: each container's main, by its mount namespace.
	mains := map[string]int{}
	if docker {
		for pid, s := range stats {
			if s.ppid != 0 || s.mnt == agentMntns || notMembers[pid] {
				continue
			}
			if m, ok := mains[s.mnt]; !ok || s.start < stats[m].start || (s.start == stats[m].start && pid < m) {
				mains[s.mnt] = pid
			}
		}
	}
	member := map[int]bool{}
	var isMember func(pid int, depth int) bool
	isMember = func(pid int, depth int) bool {
		if m, ok := member[pid]; ok {
			return m
		}
		s, ok := stats[pid]
		m := false
		root := int(client)
		if docker {
			root = -1
			if main, ok := mains[s.mnt]; ok && s.mnt != agentMntns {
				root = main
			}
		}
		switch {
		case !ok || depth > 64 || notMembers[pid] || root < 0:
		case docker && pid == root:
			m = true
		case !docker && pid == root:
		case s.ppid == root:
			m = true
		case s.ppid > 1:
			m = isMember(s.ppid, depth+1)
		}
		member[pid] = m
		return m
	}
	out := map[int]procStat{}
	for pid, s := range stats {
		if isMember(pid, 0) {
			out[pid] = s
		}
	}
	return out, true
}

// cpuTally is the application's CPU time as the watchdog's progress: what
// its processes used, each its own, read at each snapshot and only ever
// growing. A process is its pid and its start time, so a pid used again is
// another process. keploy test weighs it until the start first restarts
// (the kernel's READY_RESTART): a supervisor re-exec'ing a crashing
// application, a little CPU time at each round, is a loop going round, not
// a start going on. A daemon is no restart (it forks, and runs on when its
// parent exits), and counts.
type cpuTally struct {
	mu    sync.Mutex
	total uint64
	last  map[procID]uint64
}

// procID is one process: its pid, and when it started.
type procID struct {
	pid   int
	start uint64
}

// add weighs members' CPU time since the last read, and returns the total.
func (c *cpuTally) add(members map[int]procStat) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[procID]uint64{}
	}
	now := make(map[procID]bool, len(members))
	for pid, m := range members {
		id := procID{pid, m.start}
		now[id] = true
		prev := c.last[id]
		c.last[id] = m.cpu
		if m.cpu > prev {
			c.total += m.cpu - prev
		}
	}
	for id := range c.last {
		if !now[id] {
			delete(c.last, id)
		}
	}
	return time.Duration(c.total) * clockTick
}

func readProcStat(root string, pid int) (procStat, bool) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", root, pid))
	if err != nil {
		return procStat{}, false // gone, or not ours to read
	}
	// "pid (comm) state ppid ..."; comm may hold spaces and parentheses.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(string(b[i+1:]))
	// f[0] is the state (field 3): ppid is field 4, utime..cstime 14..17,
	// starttime 22.
	if len(f) < 20 {
		return procStat{}, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procStat{}, false
	}
	var cpu uint64
	for _, x := range f[11:13] {
		v, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return procStat{}, false
		}
		if v > 0 {
			cpu += uint64(v)
		}
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return procStat{}, false
	}
	return procStat{ppid: ppid, cpu: cpu, start: start}, true
}

// latestOfEach keeps, of listens (in order), the latest for each port,
// address, namespace and liveness, in order: the answer stays as small as the
// sockets an application listens on, however often it listens.
func latestOfEach(listens []models.AppListen) []models.AppListen {
	type key struct {
		port       uint16
		addr       string
		here, live bool
	}
	latest := map[key]int{}
	for i, l := range listens {
		latest[key{l.Port, l.Addr, l.Here, l.Live}] = i
	}
	var out []models.AppListen
	for i, l := range listens {
		if latest[key{l.Port, l.Addr, l.Here, l.Live}] == i {
			out = append(out, l)
		}
	}
	return out
}

// MarkReadiness is the agent's mark for a set whose application keploy
// starts anew: what the set's processes did is forgotten (the kernel's
// ready_seen, and the command lines its processes hold, ready_holders, so
// one left from before exits without letting go of the new start's), so
// the new start's first connects, forks and execs are
// progress again, and what it answers (its ListenCount, and its restarts so
// far) is where the set's listens and restarts begin.
func (h *Hooks) MarkReadiness() (models.AppReadiness, error) {
	h.readyMu.Lock()
	w := h.ready
	h.readyMu.Unlock()
	if w == nil {
		return models.AppReadiness{}, fmt.Errorf("%w: this agent watches no application's readiness", models.ErrReadinessUnsupported)
	}
	// ready_holders first, then ready_seen. A member's exec between the two
	// clears notes its command line in both (the holder, and a live run);
	// cleared this way, ready_seen's live count goes with the second clear,
	// so the command line is new to the set when it is run again, and a
	// restart once that run exited. The other way round, the live count
	// stayed with no holder to let it go, and every later run of the command
	// line was a repeat, never a restart. What is left (pre-flip
	// W6-ready-holders-generation): the window's run is still a holder, and
	// its exit, or its exec of another command line, after a fresh run of its
	// command line started, lets go of the fresh run's live count; the count
	// then stays one short until every run of the command line has exited
	// (ready_release stops at 0), and until then a run beside a single live
	// run is a false restart, each time; a start not judged, never a false
	// pass.
	for i, m := range []*ebpf.Map{w.maps.ReadyHolders, w.maps.ReadySeen} {
		if err := clearMap(m); err != nil {
			return models.AppReadiness{}, fmt.Errorf("forget what the last set's processes did: %w", err)
		}
		if i == 0 && markBetweenClears != nil {
			markBetweenClears()
		}
	}
	return h.AppReadiness(math.MaxUint64)
}

// markBetweenClears, when a test sets it, runs between MarkReadiness's two
// clears: an exec there is the window the clears' order is for.
var markBetweenClears func()

// clearMap deletes every entry of m, whatever its key and value sizes. An
// iteration the kernel's own deletes abort (ready_holders loses an entry at
// every member's exit) deletes what it read and goes round again, a bounded
// number of times; each round leaves fewer entries.
func clearMap(m *ebpf.Map) error {
	k, v := make([]byte, m.KeySize()), make([]byte, m.ValueSize())
	for round := 0; ; round++ {
		var keys [][]byte
		it := m.Iterate()
		for it.Next(k, v) {
			keys = append(keys, append([]byte(nil), k...))
		}
		err := it.Err()
		for _, key := range keys {
			if derr := m.Delete(key); derr != nil && !errors.Is(derr, ebpf.ErrKeyNotExist) {
				return fmt.Errorf("delete from %s: %w", m, derr)
			}
		}
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, ebpf.ErrIterationAborted) || round == 16:
			return fmt.Errorf("read %s: %w", m, err)
		}
	}
}

// AppReadiness is what the readiness watch knows now (models.AppReadiness),
// of the listens from since on: under the proven switch only; an error is
// "unknown".
func (h *Hooks) AppReadiness(since uint64) (models.AppReadiness, error) {
	h.readyMu.Lock()
	w := h.ready
	h.readyMu.Unlock()
	if w == nil {
		return models.AppReadiness{}, fmt.Errorf("%w: this agent watches no application's readiness (the proven model is off in it, or it does not run keploy test)", models.ErrReadinessUnsupported)
	}
	if err := w.selfRegistered(); err != nil {
		// Without the agent's container the kernel takes nothing for a
		// member: an agent that cannot tell (the first test's own connect
		// decides), not a stall.
		return models.AppReadiness{}, fmt.Errorf("%w: %v", models.ErrReadinessUnsupported, err)
	}
	// The keploy client by its pid in the agent's PID namespace, as the
	// client registered it: the numbers its /proc shows (the kernel's own,
	// keploy_client_kernel_pid_map, are another namespace's when the agent
	// runs in a container of its own).
	var client uint32
	if !w.docker {
		h.objectsMutex.RLock()
		m := h.objects.KeployClientRegistrationMap
		var info structs.ClientInfo
		var err error
		if m == nil {
			err = errors.New("not loaded")
		} else {
			err = m.Lookup(uint64(0), &info)
		}
		h.objectsMutex.RUnlock()
		if err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return models.AppReadiness{}, fmt.Errorf("read the keploy client's pid: %w", err)
		}
		client = info.ClientNSPID
	}
	return w.snapshot(client, since)
}

// selfRegistered is registerSelf, once: its error stays.
func (w *readyWatch) selfRegistered() error {
	w.selfOnce.Do(func() { w.selfErr = w.registerSelf() })
	return w.selfErr
}
