//go:build linux

package linux

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The runs of keploy's kernel programs the kernel skipped.
//
// A tracing program (a tracepoint's, a raw tracepoint's, a BPF trampoline's
// fentry or fexit) has a per-CPU recursion guard: the kernel does not run it
// on a CPU where a run of it is in progress, and counts the run it skipped in
// the program's recursion_misses (5.12 onwards: before, there is no such
// guard and no such count, and a kernel that has none is asked once,
// recursionMissesCounted, and judged as without the watch). A perf-event
// program's run skipped behind the CPU's shared bpf_prog_active (another
// tracing program running there, with preemption off: from an interrupt) is
// counted only from about 6.7, and is rare. Where a run can be preempted
// (fentry and fexit since 5.11, raw tracepoints' programs since 7.0, a
// syscall tracepoint's since 7.2, under full or lazy preemption) a task
// preempted inside one holds the guard while it is off the CPU, and every
// other task's run of it on that CPU is skipped meanwhile. On 7.2.7
// (preemption full, lazy), with four other processes calling socket() on
// every CPU, sys_enter_socket missed 2,738 of a process's 200,000 socket()
// calls; on 6.12 (voluntary) none. A cgroup or sock_ops
// program has no such guard: the kernel runs it for every event.
//
// programSkips says, for every program of keploy's objects, which class it
// is in; a test fails on a program it does not name:
//   - guard-free: a cgroup or sock_ops program, which no skip reaches;
//   - covered: what a skipped run leaves undone is done by something the
//     kernel does not skip, or the skip loses no call (backstop says which).
//     Its skips are logged at WARN, and charge no test;
//   - loses: a skipped run may have been a call of the application's that
//     keploy then never sees, or its connection. It is logged at WARN, and,
//     under the proven switch, charged as evidence, never a verdict
//     (models.CaptureLossKernelSkip, models.CaptureLossKind.Evidence): the
//     count is the kernel's recursion_misses (5.12+), so it fails no test
//     and leaves none not judged, and a kernel without it (5.10) judges the
//     same. No program of keploy's own is in it today; one the map does not
//     name is (skipOf);
//   - unsupported: the program reports what keploy does not support (the
//     unixconn report: Unix sockets). Its skips are said at INFO in the
//     session's summary, and charge nothing.
//
// The counts are taken from 0 at each program's load (its own, the agent's
// load), and read at every read of the capture losses, at each tick of the
// sweep loop (sweepIfExitsSkipped, which also reads k_process_exit's), and
// once more as the hooks unload, which logs what each program skipped in the
// session. A skip count that cannot be read keeps its last value: it is
// evidence, and never makes the capture losses unknown. A
// socket_classify_failures count that cannot be read does (err): that
// count is k_sock_create's, on every kernel.

// skipClass is which of the classes a program is (programSkips).
type skipClass int

const (
	skipGuardFree skipClass = iota + 1
	skipCovered
	skipLoses
	// skipUnsupported: the program reports what keploy does not support
	// (Unix sockets): a skipped run of it is said at INFO in the session's
	// summary, and charges nothing.
	skipUnsupported
)

// programSkip is what keploy knows of the skips of one program.
type programSkip struct {
	class skipClass
	// does is what a run of the program does: what a skipped run left undone.
	does string
	// backstop (skipCovered) is what does a skipped run's work, or why a
	// skip loses no call; by names the programs and maps of keploy's
	// objects it consists of, which the test finds there.
	backstop string
	by       []string
	// gap (skipCovered) is what the backstop does not cover, "" for
	// nothing: said with the program's skips, never charged.
	gap string
}

var guardFree = programSkip{class: skipGuardFree}

// unixReport is what each of the unixconn report's programs does: detect
// calls keploy does not support.
const unixReport = "reports a member's AF_UNIX connect or datagram send to an address (unixconn.go), a call keploy does not support"

// readinessBackstop is why a skipped run of a readiness program loses no call.
const readinessBackstop = "the readiness watch calls a start ready only at a member's listen; a skipped run hides progress, a restart or a container's main process, which can delay the first test or make a start look stalled (not judged), never make it look ready, and never captures less"

// programSkips is the coverage map: every program of keploy's objects (the
// main object's and unixconn's), by name.
var programSkips = map[string]programSkip{
	// cgroup and sock_ops programs: the kernel runs them for every event.
	"k_bind4":            guardFree,
	"k_bind6":            guardFree,
	"k_connect4":         guardFree,
	"k_connect6":         guardFree,
	"k_getpeername4":     guardFree,
	"k_getpeername6":     guardFree,
	"k_post_bind4":       guardFree,
	"k_post_bind6":       guardFree,
	"k_sock_create":      guardFree,
	"k_sockops":          guardFree,
	"k_udp4_recvmsg":     guardFree,
	"k_udp4_sendmsg":     guardFree,
	"k_udp6_recvmsg":     guardFree,
	"k_udp6_sendmsg":     guardFree,
	"k_udp_bind4":        guardFree,
	"k_udp_bind6":        guardFree,
	"k_udp_egress":       guardFree,
	"k_udp_ingress":      guardFree,
	"k_udp_sock_release": guardFree,
	"k_ready_bind4":      guardFree,
	"k_ready_bind6":      guardFree,
	"k_ready_connect4":   guardFree,
	"k_ready_connect6":   guardFree,

	"syscall_probe_entry_socket": {
		class:    skipCovered,
		does:     "classifies a process at its socket() (a target, or excluded as older than the recording) and registers keploy's agent and client",
		backstop: "k_sock_create classifies by the same rule at each inet socket's creation, before the connect, send or bind pid_filter sees; a process k_sock_create could not classify is counted (socket_classify_failures, models.CaptureLossUnclassified)",
		by:       []string{"k_sock_create", "socket_classify_failures"},
		gap:      "a process every socket() run of which was skipped, that makes no inet socket of its own and uses one it did not make (inherited, or passed to it), is no target: its calls on that socket go to the real destination, neither recorded nor mocked; and one older than the recording that makes only sockets of other families, every socket() run of which was skipped, is not excluded, so its children started during the recording are targets: their calls are captured, and mocked in a replay, as the application's",
	},
	"k_process_exit": {
		class:    skipCovered,
		does:     "removes an exited process's PID-map entries",
		backstop: "every entry keploy's kernel programs make records its process's start time (pid_start_times; one whose start time cannot be read, or that finds the map full, is taken as it is), so none of pid_filter, its parent check and the readiness watch takes a dead process's entry for the next process the kernel gives its TGID, whose own classification replaces it; and an agent in the initial PID namespace deletes the entries of processes that are gone (sweepGonePIDs). An entry user space wrote without a start time is taken as it is",
		by:       []string{"pid_start_times"},
	},
	"k_ready_fork": {class: skipCovered, does: "counts a member's fork as progress of the application's start", backstop: readinessBackstop},
	"k_ready_exec": {class: skipCovered, does: "counts a member's exec as progress of the application's start, and records a container's main process", backstop: readinessBackstop},
	"k_ready_exit": {class: skipCovered, does: "forgets a container's main process when it exits", backstop: readinessBackstop},

	"k_unix_stream_connect":    {class: skipUnsupported, does: unixReport},
	"k_unix_dgram_connect":     {class: skipUnsupported, does: unixReport},
	"k_unix_dgram_sendmsg":     {class: skipUnsupported, does: unixReport},
	"k_unix_sys_enter_connect": {class: skipUnsupported, does: unixReport},
	"k_unix_sys_exit_connect":  {class: skipUnsupported, does: unixReport},
	"k_unix_sys_enter_sendto":  {class: skipUnsupported, does: unixReport},
	"k_unix_sys_exit_sendto":   {class: skipUnsupported, does: unixReport},
	"k_unix_sys_enter_sendmsg": {class: skipUnsupported, does: unixReport},
	"k_unix_sys_exit_sendmsg":  {class: skipUnsupported, does: unixReport},
}

// skipOf is name's entry in programSkips. A program it does not name (the
// test fails on one) is taken as losing calls: nothing is known to do its
// work.
func skipOf(name string) programSkip {
	if s, ok := programSkips[name]; ok {
		return s
	}
	return programSkip{class: skipLoses, does: "is a kernel program keploy's coverage map does not name"}
}

// recursionMissesCounted says the kernel counts the runs of p it skips: it
// fills struct bpf_prog_info up to recursion_misses (5.12 onwards; before,
// no tracing program has a per-program recursion guard to skip a run by,
// and the field is not there). An error is a failed ask, which says nothing
// either way. Swapped by tests.
var recursionMissesCounted = func(p *ebpf.Program) (bool, error) {
	info := new(progInfoToRecursionMisses)
	attr := struct {
		fd, infoLen uint32
		info        uint64
	}{uint32(p.FD()), uint32(unsafe.Sizeof(*info)), uint64(uintptr(unsafe.Pointer(info)))}
	_, _, errno := unix.Syscall(unix.SYS_BPF, bpfObjGetInfoByFD, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr))
	runtime.KeepAlive(info)
	if errno != 0 {
		return false, errno
	}
	return attr.infoLen >= uint32(unsafe.Sizeof(*info)), nil
}

// bpfObjGetInfoByFD is BPF_OBJ_GET_INFO_BY_FD.
const bpfObjGetInfoByFD = 15

// progInfoToRecursionMisses is struct bpf_prog_info (include/uapi/linux/bpf.h)
// up to and with recursion_misses: the kernel fills as much of it as it has,
// and says how much in info_len.
type progInfoToRecursionMisses struct {
	Type, ID                    uint32
	Tag                         [8]uint8
	JitedProgLen, XlatedProgLen uint32
	JitedProgInsns              uint64
	XlatedProgInsns             uint64
	LoadTime                    uint64
	CreatedByUID, NrMapIDs      uint32
	MapIDs                      uint64
	Name                        [16]byte
	Ifindex                     uint32
	GPLCompatible               uint32 // a bitfield, and padding
	NetnsDev, NetnsIno          uint64
	NrJitedKsyms                uint32
	NrJitedFuncLens             uint32
	JitedKsyms, JitedFuncLens   uint64
	BTFID, FuncInfoRecSize      uint32
	FuncInfo                    uint64
	NrFuncInfo, NrLineInfo      uint32
	LineInfo, JitedLineInfo     uint64
	NrJitedLineInfo             uint32
	LineInfoRecSize             uint32
	JitedLineInfoRecSize        uint32
	NrProgTags                  uint32
	ProgTags                    uint64
	RunTimeNs, RunCnt           uint64
	RecursionMisses             uint64
}

// programMisses reads a program's recursion_misses; swapped by tests.
var programMisses = func(p *ebpf.Program) (uint64, error) {
	st, err := p.Stats()
	if err != nil {
		return 0, err
	}
	return st.RecursionMisses, nil
}

// watchedProgram is one program a skipWatch reads.
type watchedProgram struct {
	name   string
	prog   *ebpf.Program
	skip   programSkip
	last   uint64 // its misses at the last read, from 0 at its load
	warned bool   // its first skip was logged
}

// skipWatch reads the recursion_misses of every covered and losing program
// the hooks load (watch), from their load on, and of socket_classify_failures.
type skipWatch struct {
	logger *zap.Logger
	mu     sync.Mutex
	progs  []*watchedProgram
	// failures is socket_classify_failures, nil in an object without it;
	// failBase its count when the watch began.
	failures *ebpf.Map
	failBase uint64
	failLast pidMapInsertFailure
	// readErr is why a read of socket_classify_failures failed (err):
	// kept for the session, as a count missed once leaves every later one
	// short by what it missed.
	readErr error
	// probed says the kernel was asked whether it counts skipped runs;
	// uncounted, that it does not (recursionMissesCounted): no program is
	// read then.
	probed, uncounted bool
	// exitSwept is k_process_exit's misses at the last sweep (exitsSkipped);
	// again asks for one more full sweep (a sweep failed: sweepAgain), and
	// zombies are the TGIDs with entries a sweep found zombies, which the
	// next looks at alone (zombiesLeft); refused says the sweep's refusal
	// was logged (refusedOnce).
	exitSwept uint64
	again     bool
	zombies   []uint32
	refused   bool
	// closed: the hooks unloaded, and nothing is read any more (close).
	closed bool
}

// sweepAgain asks exitsSkipped for one more full sweep: the last failed.
func (w *skipWatch) sweepAgain() {
	w.mu.Lock()
	w.again = true
	w.mu.Unlock()
}

// zombiesLeft asks exitsSkipped for one more sweep of tgids alone: processes
// with entries that were zombies, not yet gone.
func (w *skipWatch) zombiesLeft(tgids []uint32) {
	w.mu.Lock()
	w.zombies = tgids
	w.mu.Unlock()
}

// refusedOnce says true the first time it is called.
func (w *skipWatch) refusedOnce() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	first := !w.refused
	w.refused = true
	return first
}

func newSkipWatch(logger *zap.Logger, failures *ebpf.Map) *skipWatch {
	w := &skipWatch{logger: logger, failures: failures}
	if f, err := readClassifyFailures(failures); err == nil {
		w.failBase, w.failLast = f.Failures, f
	}
	return w
}

// watch adds progs, by name, to what w reads: programs the agent just loaded,
// whose misses are all this session's.
func (w *skipWatch) watch(progs map[string]*ebpf.Program) {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(progs))
	for n := range progs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := progs[n]
		s := skipOf(n)
		if p == nil || s.class == skipGuardFree {
			continue
		}
		if !w.probed {
			w.probed = true
			counted, err := recursionMissesCounted(p)
			// A failed ask says nothing: the programs are read.
			w.uncounted = err == nil && !counted
			if err != nil {
				w.logger.Debug("could not ask the kernel whether it counts the runs of BPF programs it skips: reading them anyway", zap.String("program", n), zap.Error(err))
			}
			if w.uncounted {
				w.logger.Info("this kernel does not count the runs of BPF programs it skips (bpf_prog_info.recursion_misses, Linux 5.12 onwards), and none of keploy's programs has a per-program recursion guard here to skip a run by: keploy has no skipped runs to say",
					zap.String("kernel", kernelName()))
			}
		}
		if w.uncounted {
			continue // nothing to read, and nothing a skip could lose
		}
		// From 0: the programs were just loaded, and a skip between their
		// attach and this is theirs too.
		w.progs = append(w.progs, &watchedProgram{name: n, prog: p, skip: s})
	}
}

// forget stops reading progs (unixconn's, as it closes): what they skipped
// so far stays counted.
func (w *skipWatch) forget(progs map[string]*ebpf.Program) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, wp := range w.progs {
		if p, ok := progs[wp.name]; ok && p == wp.prog {
			wp.prog = nil
		}
	}
}

// read reads every program's misses (one that cannot be read keeps its last
// count) and logs, once per program, its first skip.
func (w *skipWatch) read() {
	for _, wp := range w.progs {
		if wp.prog == nil {
			continue
		}
		m, err := programMisses(wp.prog)
		if err != nil {
			// Evidence only: a count missed once is no verdict.
			w.logger.Debug("could not read the skipped runs of a kernel program of keploy's", zap.String("program", wp.name), zap.Error(err))
			continue
		}
		if m < wp.last {
			continue
		}
		wp.last = m
		if !wp.warned && m > 0 && wp.skip.class != skipUnsupported {
			wp.warned = true
			w.logSkips(wp, "the kernel skipped runs of a kernel program of keploy's")
		}
	}
	f, err := readClassifyFailures(w.failures)
	if err != nil && w.failures != nil && w.readErr == nil {
		w.readErr = fmt.Errorf("read socket_classify_failures: %w", err)
	}
	if err == nil && f.Failures > w.failLast.Failures {
		if w.failLast.Failures == w.failBase {
			w.logger.Warn("keploy could not tell whether a process was the application's: a read of its PID namespace failed, so its calls are neither intercepted nor left out on purpose",
				zap.Uint32("tgid", f.LastTGID), zap.String("error", unix.ErrnoName(unix.Errno(-f.LastErr))), zap.String("comm", commOfHostTGID(f.LastTGID)))
		}
		w.failLast = f
	}
}

// logSkips logs what wp skipped since the watch began, with what the skip
// means for its class.
func (w *skipWatch) logSkips(wp *watchedProgram, lead string) {
	fields := []zap.Field{zap.String("program", wp.name), zap.Uint64("skipped_runs", wp.last), zap.String("kernel", kernelName()),
		zap.String("why", "a run of a tracing program is skipped on a CPU where another run of it is in progress, preempted (full or lazy preemption)")}
	switch wp.skip.class {
	case skipCovered:
		msg := lead + " that " + wp.skip.does + "; keploy captures nothing less for it: " + wp.skip.backstop
		if wp.skip.gap != "" {
			msg += "; not covered: " + wp.skip.gap
		}
		w.logger.Warn(msg, fields...)
	case skipUnsupported:
		w.logger.Info(lead+" that "+wp.skip.does+": nothing is charged for it", fields...)
	default:
		consequence := "a skipped run may have been a call of the application's, or its connection, that keploy did not see"
		if provenswitch.On() {
			consequence += "; it is evidence, not a verdict: in a recording the tests from it on carry it, for the replay that judges them, and in a replay it judges nothing"
		}
		w.logger.Warn(lead+" that "+wp.skip.does+": "+consequence, append(fields, zap.String("next_step", models.KernelSkipRemedy))...)
	}
}

// losses is what the skips charge, under the proven switch only (nothing
// without it): the losing programs' skipped runs since the watch began, one
// kernel_skipped count naming them (evidence only), and the processes
// k_sock_create could not classify (not judged: its count is the cgroup
// program's own, on every kernel).
func (w *skipWatch) losses() models.CaptureLosses {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.read()
	}
	if !provenswitch.On() {
		return nil
	}
	var out models.CaptureLosses
	var total uint64
	var named []string
	for _, wp := range w.progs {
		if wp.skip.class != skipLoses || wp.last == 0 {
			continue
		}
		total += wp.last
		named = append(named, wp.name)
	}
	if total > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossKernelSkip, Count: total, What: strings.Join(named, ", ")})
	}
	if w.failLast.Failures > w.failBase {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossUnclassified, Count: w.failLast.Failures - w.failBase, What: "the latest TGID " + strconv.FormatUint(uint64(w.failLast.LastTGID), 10)})
	}
	return out
}

// err is why a read of socket_classify_failures failed, under the proven
// switch: the counts are then unknown, never 0 (Hooks.CaptureLossesErr),
// for the rest of the session. A skip count that cannot be read is no error
// (evidence only).
func (w *skipWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !provenswitch.On() {
		return nil
	}
	return w.readErr
}

// exitsSkipped says whether k_process_exit skipped runs since the last time
// it said so: the entries of processes whose exit it missed are then left in
// the PID maps (sweepGonePIDs).
//
// It says which sweep: a full one (sweep, only nil) after a skip or a failed
// sweep, or one of the zombies the last found (only), which costs a look at
// each of them, not at every entry.
func (w *skipWatch) exitsSkipped() (sweep bool, only []uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false, nil
	}
	w.read()
	for _, wp := range w.progs {
		if wp.name == "k_process_exit" && wp.last > w.exitSwept {
			w.exitSwept = wp.last
			w.again, w.zombies = false, nil
			return true, nil
		}
	}
	if w.again {
		w.again, w.zombies = false, nil
		return true, nil
	}
	if len(w.zombies) > 0 {
		only, w.zombies = w.zombies, nil
		return true, only
	}
	return false, nil
}

// close reads the counts a last time and logs, for every program that skipped
// runs in the session, how many; nothing is read after it. Called before the
// programs close.
func (w *skipWatch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.read()
	w.closed = true
	for _, wp := range w.progs {
		if wp.last > 0 {
			w.logSkips(wp, "in this session the kernel skipped runs of a kernel program of keploy's")
		}
		wp.prog = nil
	}
	w.failures = nil
}

// readClassifyFailures reads socket_classify_failures (struct
// pid_map_insert_failure_t's shape). Swapped by tests.
var readClassifyFailures = func(m *ebpf.Map) (pidMapInsertFailure, error) {
	var f pidMapInsertFailure
	if m == nil {
		return f, errors.New("no socket_classify_failures")
	}
	err := m.Lookup(uint32(0), &f)
	return f, err
}

// sweepPIDs is sweepGonePIDs, swapped by tests.
var sweepPIDs = sweepGonePIDs

// errNotHostPIDs is sweepGonePIDs's refusal where /proc does not number
// processes as the PID maps do.
var errNotHostPIDs = errors.New("the agent is not in the initial PID namespace, whose numbers the PID maps hold")

// sweepGonePIDs deletes the PID-map entries (target_namespace_pids,
// excluded_pids, pid_start_times) of every TGID no process has: what
// k_process_exit leaves when the kernel skips its run for a process's exit.
// Once another process has the TGID, pid_filter, its parent check and the
// readiness watch take such an entry for none (pid_entry_stale; the unixconn
// report's owner check does not compare start times). But it holds a slot,
// and enough of them fill the map, which then refuses new processes. Only
// from the initial PID namespace, whose numbers the maps hold; elsewhere (an
// agent in a PID namespace of its own) it refuses (errNotHostPIDs).
//
// A user-space writer of target_namespace_pids or excluded_pids (a
// downstream build that arms processes itself) writes no start time, and its
// entry is taken as it is, unless a dead process's start time is still under
// the TGID (pid_start_times): then pid_filter, its parent check and the
// readiness watch take the entry for that dead process's, and none (the
// unixconn report's owner check, Hooks.isMember, takes it as it is). Such a
// writer deletes pid_start_times' entry for the TGID as it inserts (or
// writes there the process's own start_boottime, its thread-group
// leader's).
//
// A process is gone when kill(tgid, 0) says ESRCH: once it is reaped (a
// zombie's TGID still answers). zombies are the TGIDs with entries that are
// zombies, whose entries a later sweep of them alone (only) can delete. A
// TGID the kernel gives a new process between the look and the delete loses
// the new process's entries: where the agent classifies processes itself,
// its next socket() or inet socket makes them again; where user space writes
// target_namespace_pids (DaemonSet, per-container targeting), they stay lost
// until it writes them again. For an entry with a start time the delete is
// skipped when the start time is no longer the one read, which a
// classification of the new process, or a user-space writer (which deletes
// it), changes.
func sweepGonePIDs(targets, excluded, starts *ebpf.Map, only []uint32) (cleared int, zombies []uint32, err error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return 0, nil, err
	}
	if st.Ino != initialPIDNSInode {
		return 0, nil, errNotHostPIDs
	}
	tgids := map[uint32]uint64{} // TGID: its start time, 0 for none
	if only != nil {
		for _, tgid := range only {
			var v8 uint8
			var start uint64
			held := false
			for _, m := range []*ebpf.Map{targets, excluded} {
				if m != nil && m.Lookup(tgid, &v8) == nil {
					held = true
				}
			}
			if starts != nil && starts.Lookup(tgid, &start) == nil {
				held = true
			}
			if held {
				tgids[tgid] = start
			}
		}
	}
	for _, m := range []*ebpf.Map{targets, excluded} {
		if only != nil {
			break
		}
		if m == nil {
			continue
		}
		var k uint32
		var v uint8
		it := m.Iterate()
		for it.Next(&k, &v) {
			tgids[k] = 0
		}
		if err := it.Err(); err != nil {
			return 0, nil, fmt.Errorf("iterate the PID map %s: %w", m.String(), err)
		}
	}
	if starts != nil && only == nil {
		var k uint32
		var v uint64
		it := starts.Iterate()
		for it.Next(&k, &v) {
			tgids[k] = v
		}
		if err := it.Err(); err != nil {
			return 0, nil, fmt.Errorf("iterate pid_start_times: %w", err)
		}
	}
	for tgid, start := range tgids {
		if tgid == 0 || tgid > math.MaxInt32 {
			continue
		}
		if err := unix.Kill(int(tgid), 0); !errors.Is(err, unix.ESRCH) {
			if zombieProcess(tgid) {
				zombies = append(zombies, tgid)
			}
			continue // alive, or not known to be gone
		}
		if starts != nil && start != 0 {
			var now uint64
			if err := starts.Lookup(tgid, &now); err != nil || now != start {
				continue // classified again since: a new process's
			}
		}
		for _, m := range []*ebpf.Map{targets, excluded, starts} {
			if m != nil {
				_ = m.Delete(tgid)
			}
		}
		cleared++
	}
	return cleared, zombies, nil
}

// zombieProcess says the process tgid (initial PID namespace) has exited and
// waits to be reaped: its leader is a zombie and has no thread left. A
// leader that exited while other threads of its process live is a zombie
// too, but the process is not: that one keeps no sweep going.
func zombieProcess(tgid uint32) bool {
	if processState(tgid) != 'Z' {
		return false
	}
	tasks, err := os.ReadDir("/proc/" + strconv.FormatUint(uint64(tgid), 10) + "/task")
	return err == nil && len(tasks) == 1
}

// processState is the state letter of the process tgid (initial PID
// namespace) in /proc/<tgid>/stat, 0 when it cannot be read.
func processState(tgid uint32) byte {
	b, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(tgid), 10) + "/stat")
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return 0
	}
	return b[i+2]
}

// sweepIfExitsSkipped sweeps the PID maps (sweepGonePIDs) when k_process_exit
// skipped runs since the last sweep, and again while a process whose entries
// it found is a zombie, not yet gone. Where it cannot sweep (an agent in a
// container) it says so at WARN, once.
func (h *Hooks) sweepIfExitsSkipped() {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	if h.skips == nil {
		return
	}
	sweep, only := h.skips.exitsSkipped()
	if !sweep {
		return
	}
	n, zombies, err := sweepPIDs(h.objects.TargetNamespacePids, h.objects.ExcludedPids, h.objects.PidStartTimes, only)
	switch {
	case err != nil && !errors.Is(err, errNotHostPIDs):
		// A sweep a map's churn cut short (an iteration aborted): the
		// next tick sweeps again, whole.
		h.skips.sweepAgain()
	case len(zombies) > 0:
		// Processes not reaped yet: the next tick looks at them alone.
		h.skips.zombiesLeft(zombies)
	}
	switch {
	case errors.Is(err, errNotHostPIDs):
		if h.skips.refusedOnce() {
			h.logger.Warn("the kernel skipped runs of the program that removes exited processes from keploy's PID maps, and this agent, in a PID namespace of its own, cannot tell which processes are gone: their entries stay, and enough of them fill the map, which then refuses new processes (a refusal is reported)",
				zap.String("kernel", kernelName()))
		}
	case err != nil:
		h.logger.Debug("the PID maps keep the entries of processes whose exit the kernel skipped", zap.Error(err))
	default:
		h.logger.Debug(fmt.Sprintf("deleted the PID-map entries of %d processes whose exit the kernel skipped (%d zombies left for the next sweep)", n, len(zombies)))
	}
}

// skipsWatch adds progs to the hooks' skip watch, if it runs.
func (h *Hooks) skipsWatch(progs map[string]*ebpf.Program) {
	if h.skips != nil {
		h.skips.watch(progs)
	}
}

// skipsForget reads progs' counts a last time and stops reading them, before
// they close.
func (h *Hooks) skipsForget(progs map[string]*ebpf.Program) {
	if h.skips == nil {
		return
	}
	h.skips.mu.Lock()
	if !h.skips.closed {
		h.skips.read()
	}
	h.skips.mu.Unlock()
	h.skips.forget(progs)
}

// skipLosses is what the kernel's skips of keploy's programs charge
// (skipWatch.losses), under the proven switch.
func (h *Hooks) skipLosses() models.CaptureLosses {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	if h.skips == nil {
		return nil
	}
	return h.skips.losses()
}

// programsByName is every program field of progs, a pointer to a bpf2go
// programs struct, by the name its ebpf tag gives.
func programsByName(progs any) map[string]*ebpf.Program {
	out := map[string]*ebpf.Program{}
	v := reflect.ValueOf(progs)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Tag.Get("ebpf")
		if p, ok := v.Field(i).Interface().(*ebpf.Program); ok && name != "" {
			out[name] = p
		}
	}
	return out
}

// classifyFlag is flags with structs.FlagClassifies set where the agent
// classifies processes itself, having attached sys_enter_socket and
// k_sock_create (classifies), and clear everywhere else: k_sock_create runs
// only where it is set. Never where something else owns
// target_namespace_pids (DaemonSet, per-container targeting), whatever a
// downstream build's customizer set.
func classifyFlag(flags uint32, classifies bool) uint32 {
	flags &^= structs.FlagClassifies
	if classifies {
		flags |= structs.FlagClassifies
	}
	return flags
}
