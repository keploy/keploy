//go:build linux

package linux

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// objectSpecs are keploy's objects' specs, as the agent loads them: the main
// object's k_sockops is a sock_ops program (its section name reads to
// cilium/ebpf as a cgroup sock one), as hooks.go sets it.
func objectSpecs(t *testing.T) map[string]*ebpf.CollectionSpec {
	t.Helper()
	main, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := main.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
	}
	uc, err := loadUnixconn()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*ebpf.CollectionSpec{"bpf": main, "unixconn": uc}
}

// Every program of keploy's objects is in the coverage map, and the map names
// no program they do not have: a program added to an object without saying
// what its skipped runs cost fails here.
func TestEveryProgramIsInTheSkipCoverageMap(t *testing.T) {
	seen := map[string]bool{}
	for object, spec := range objectSpecs(t) {
		for name := range spec.Programs {
			seen[name] = true
			if _, ok := programSkips[name]; !ok {
				t.Errorf("%s's program %s is not in programSkips (skipwatch.go): say whether the kernel can skip its runs and what a skipped run loses", object, name)
			}
		}
	}
	for name := range programSkips {
		if !seen[name] {
			t.Errorf("programSkips names %s, which no object of keploy's has", name)
		}
	}
}

// A program's class agrees with its type: the kernel runs a cgroup or
// sock_ops program for every event, and every other type keploy uses has the
// recursion guard. A covered program's backstop is named, and is made of
// programs and maps the objects have; a program a backstop counts on is
// guard-free itself.
func TestTheSkipCoverageMapAgreesWithTheProgramTypes(t *testing.T) {
	guardFreeTypes := map[ebpf.ProgramType]bool{
		ebpf.CGroupSockAddr: true, ebpf.CGroupSock: true, ebpf.CGroupSKB: true, ebpf.SockOps: true,
	}
	guardedTypes := map[ebpf.ProgramType]bool{
		ebpf.TracePoint: true, ebpf.RawTracepoint: true, ebpf.Tracing: true, ebpf.Kprobe: true,
	}
	programs := map[string]*ebpf.ProgramSpec{}
	maps := map[string]bool{}
	for _, spec := range objectSpecs(t) {
		for n, p := range spec.Programs {
			programs[n] = p
		}
		for n := range spec.Maps {
			maps[n] = true
		}
	}
	for name, s := range programSkips {
		p := programs[name]
		if p == nil {
			continue // TestEveryProgramIsInTheSkipCoverageMap says so
		}
		switch s.class {
		case skipGuardFree:
			if !guardFreeTypes[p.Type] {
				t.Errorf("%s is %v, which has the kernel's recursion guard: it is not guard-free", name, p.Type)
			}
		case skipCovered, skipLoses, skipUnsupported:
			if !guardedTypes[p.Type] {
				t.Errorf("%s is %v: a type this test does not know to have the recursion guard; say which it is", name, p.Type)
			}
			if s.does == "" {
				t.Errorf("%s does not say what a run of it does", name)
			}
			if s.class == skipCovered && s.backstop == "" {
				t.Errorf("%s is covered but names no backstop", name)
			}
			for _, b := range s.by {
				bp := programs[b]
				switch {
				case bp != nil && !guardFreeTypes[bp.Type]:
					t.Errorf("%s's backstop %s is %v: the kernel can skip it too", name, b, bp.Type)
				case bp == nil && !maps[b]:
					t.Errorf("%s's backstop %s is no program or map of keploy's objects", name, b)
				}
			}
		default:
			t.Errorf("%s has no class", name)
		}
	}
}

// fakeMisses makes programMisses read counts from a map, on a kernel that
// counts them, for the test's life.
func fakeMisses(t *testing.T) map[*ebpf.Program]uint64 {
	t.Helper()
	counted(t, true)
	counts := map[*ebpf.Program]uint64{}
	was := programMisses
	programMisses = func(p *ebpf.Program) (uint64, error) { return counts[p], nil }
	t.Cleanup(func() { programMisses = was })
	return counts
}

// counted makes recursionMissesCounted say on, for the test's life.
func counted(t *testing.T, on bool) {
	t.Helper()
	was := recursionMissesCounted
	recursionMissesCounted = func(*ebpf.Program) (bool, error) { return on, nil }
	t.Cleanup(func() { recursionMissesCounted = was })
}

// Under the proven switch, the skipped runs of a program that loses calls are
// a kernel_skipped loss, counted from the watch's start and naming the
// program; a covered program's are logged, never charged; a guard-free one is
// not read. With the switch off nothing is charged, and both are logged.
func TestTheSkipsOfALosingProgramAreChargedUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			provenswitch.Pin(on)
			t.Cleanup(provenswitch.Unpin)
			counts := fakeMisses(t)
			loses, covered, free := new(ebpf.Program), new(ebpf.Program), new(ebpf.Program)
			counts[loses], counts[covered] = 5, 7 // between the programs' attach and the watch: this session's
			core, logs := observer.New(zapcore.WarnLevel)
			w := newSkipWatch(zap.New(core), nil)
			w.watch(map[string]*ebpf.Program{"k_unnamed_program": loses, "syscall_probe_entry_socket": covered, "k_connect4": free})
			for _, wp := range w.progs {
				if wp.prog == free {
					t.Fatal("the watch reads a guard-free program")
				}
			}
			counts[loses], counts[covered] = 8, 1007
			got := w.losses()
			if !on {
				if len(got) != 0 {
					t.Fatalf("with the switch off: %v, want nothing charged", got)
				}
			} else if len(got) != 1 || got[0].Kind != models.CaptureLossKernelSkip || got[0].Count != 8 || got[0].What != "k_unnamed_program" {
				t.Fatalf("under the switch: %v, want one kernel_skipped of 8 (counted from the programs' load) naming k_unnamed_program", got)
			}
			// The first skip of each is logged, once.
			counts[loses], counts[covered] = 9, 1008
			w.losses()
			var lost, backed int
			for _, e := range logs.All() {
				prog := e.ContextMap()["program"]
				switch {
				case prog == "k_unnamed_program" && strings.Contains(e.Message, "may have been a call"):
					lost++
					if e.ContextMap()["next_step"] != models.KernelSkipRemedy {
						t.Errorf("the losing program's WARN gives no remedy: %v", e.ContextMap())
					}
				case prog == "syscall_probe_entry_socket" && strings.Contains(e.Message, "keploy captures nothing less for it: k_sock_create") && strings.Contains(e.Message, "; not covered: "):
					backed++
				default:
					t.Errorf("unexpected log: %s %v", e.Message, e.ContextMap())
				}
			}
			if lost != 1 || backed != 1 {
				t.Fatalf("logged the losing program's first skip %d time(s) and the covered one's %d, want once each", lost, backed)
			}
			// The session's end says what each skipped in it.
			w.close()
			var summary int
			for _, e := range logs.All() {
				if strings.HasPrefix(e.Message, "in this session") {
					summary++
					if n := e.ContextMap()["skipped_runs"]; n != uint64(9) && n != uint64(1008) {
						t.Errorf("the session's summary says %v skipped runs: %v", n, e.ContextMap())
					}
				}
			}
			if summary != 2 {
				t.Fatalf("the session's end logged %d summaries, want one per program that skipped", summary)
			}
			// What was skipped before the end stays charged; nothing later is.
			counts[loses] = 50
			got = w.losses()
			if on && (len(got) != 1 || got[0].Count != 9) {
				t.Fatalf("after the end: %v, want the 9 skipped before it", got)
			}
		})
	}
}

// k_process_exit's skips are what start a sweep of the PID maps
// (sweepGonePIDs): one per growth of its count, none without.
func TestASkippedExitStartsASweepOnce(t *testing.T) {
	counts := fakeMisses(t)
	exit := new(ebpf.Program)
	w := newSkipWatch(zap.NewNop(), nil)
	w.watch(map[string]*ebpf.Program{"k_process_exit": exit})
	if sweep, _ := w.exitsSkipped(); sweep {
		t.Fatal("no exit was skipped since the program's load, but it asks for a sweep")
	}
	counts[exit] = 3
	if sweep, only := w.exitsSkipped(); !sweep || only != nil {
		t.Fatal("an exit was skipped, and it asks for no full sweep")
	}
	if sweep, _ := w.exitsSkipped(); sweep {
		t.Fatal("it asks for a second sweep for the same skip")
	}
}

// A program the coverage map does not name is taken as losing calls.
func TestAProgramTheCoverageMapDoesNotNameLosesCalls(t *testing.T) {
	if s := skipOf("k_no_such_program"); s.class != skipLoses {
		t.Fatalf("an unknown program is class %d, want skipLoses", s.class)
	}
}

// The processes k_sock_create could not classify are charged from the
// watch's start, under the switch.
func TestAProcessTheBackstopCouldNotClassifyIsCharged(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	w := newSkipWatch(zap.NewNop(), nil)
	w.failBase, w.failLast = 2, pidMapInsertFailure{Failures: 5, LastTGID: 77}
	got := w.losses()
	if len(got) != 1 || got[0].Kind != models.CaptureLossUnclassified || got[0].Count != 3 || !strings.Contains(got[0].What, "77") {
		t.Fatalf("got %v, want one process_unclassified of 3 naming TGID 77", got)
	}
}

// The agent asks the kernel to classify processes (structs.FlagClassifies)
// only where it attached sys_enter_socket: in DaemonSet and per-container
// targeting mode the flag is clear, whatever a customizer set, and the
// other flags are kept either way.
func TestTheKernelClassifiesOnlyWhereTheAgentAttachedTheSocketHook(t *testing.T) {
	other := structs.FlagKernelPortAlloc | structs.FlagProvenReady
	if got := classifyFlag(other|structs.FlagClassifies, false); got != other {
		t.Errorf("without sys_enter_socket: flags %#x, want %#x", got, other)
	}
	if got := classifyFlag(other, true); got != other|structs.FlagClassifies {
		t.Errorf("with sys_enter_socket: flags %#x, want %#x", got, other|structs.FlagClassifies)
	}
}

// What the hooks' sweep loop does after each sweep (sweepIfExitsSkipped): a
// sweep that found zombies asks for one more, of them alone; one that failed
// asks for one more, whole; one that found neither asks for none; and a
// sweep the agent's PID namespace refuses is said at WARN, once.
func TestTheSweepLoopSweepsAgainAfterZombiesOrAFailure(t *testing.T) {
	counts := fakeMisses(t)
	exit := new(ebpf.Program)
	core, logs := observer.New(zapcore.WarnLevel)
	h := &Hooks{logger: zap.New(core), skips: newSkipWatch(zap.NewNop(), nil)}
	h.skips.watch(map[string]*ebpf.Program{"k_process_exit": exit})
	type sweep struct {
		only    []uint32
		zombies []uint32
		err     error
	}
	var calls []sweep
	var next []sweep
	was := sweepPIDs
	sweepPIDs = func(_, _, _ *ebpf.Map, only []uint32) (int, []uint32, error) {
		r := next[0]
		next = next[1:]
		r.only = only
		calls = append(calls, r)
		return 0, r.zombies, r.err
	}
	t.Cleanup(func() { sweepPIDs = was })
	h.sweepIfExitsSkipped()
	if len(calls) != 0 {
		t.Fatalf("swept %d times with no exit skipped", len(calls))
	}
	counts[exit] = 1
	next = []sweep{{zombies: []uint32{77}}, {err: errors.New("iteration aborted")}, {}, {err: errNotHostPIDs}, {err: errNotHostPIDs}}
	for i := 0; i < 3; i++ {
		h.sweepIfExitsSkipped()
	}
	if len(calls) != 3 || calls[0].only != nil || fmt.Sprint(calls[1].only) != "[77]" || calls[2].only != nil {
		t.Fatalf("sweeps %+v, want a full one, then TGID 77's alone (a zombie), then a full one (the last failed)", calls)
	}
	h.sweepIfExitsSkipped()
	if len(calls) != 3 {
		t.Fatalf("swept again after a sweep that left nothing: %+v", calls)
	}
	counts[exit] = 2
	h.sweepIfExitsSkipped()
	counts[exit] = 3
	h.sweepIfExitsSkipped()
	if n := len(logs.FilterMessageSnippet("cannot tell which processes are gone").All()); n != 1 {
		t.Fatalf("%d WARNs that the sweep is refused, want 1", n)
	}
}

// The hooks' capture losses carry what their skip watch charges, and their
// error what it could not read: deleting either wiring fails this.
func TestTheHooksChargeTheirSkipWatchs(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	counts := fakeMisses(t)
	unix := new(ebpf.Program)
	h := &Hooks{logger: zap.NewNop(), skips: newSkipWatch(zap.NewNop(), nil)}
	h.skips.watch(map[string]*ebpf.Program{"k_unnamed_program": unix})
	counts[unix] = 2
	got := h.CaptureLosses()
	if len(got) != 1 || got[0].Kind != models.CaptureLossKernelSkip || got[0].Count != 2 {
		t.Fatalf("the hooks' capture losses %v, want the skip watch's kernel_skipped of 2", got)
	}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("CaptureLossesErr = %v, want nil", err)
	}
	// A skip count that cannot be read is evidence missed, never an error;
	// socket_classify_failures that cannot be read is the hooks' error, and
	// kept: a read that works again does not take it away.
	was := programMisses
	programMisses = func(*ebpf.Program) (uint64, error) { return 0, errors.New("bad file descriptor") }
	h.CaptureLosses()
	programMisses = was
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("CaptureLossesErr = %v for an unread skip count, want nil: evidence only", err)
	}
	h = &Hooks{logger: zap.NewNop(), skips: newSkipWatch(zap.NewNop(), new(ebpf.Map))}
	wasRead := readClassifyFailures
	readClassifyFailures = func(*ebpf.Map) (pidMapInsertFailure, error) {
		return pidMapInsertFailure{}, errors.New("bad file descriptor")
	}
	h.CaptureLosses()
	readClassifyFailures = wasRead
	if err := h.CaptureLossesErr(); err == nil {
		t.Fatal("socket_classify_failures the hooks could not read is not their error")
	}
	h.CaptureLosses()
	if err := h.CaptureLossesErr(); err == nil {
		t.Fatal("a later read that worked took away the error of the one that did not")
	}
}

// The sweep's refusal is logged once.
func TestTheSweepsRefusalIsLoggedOnce(t *testing.T) {
	w := newSkipWatch(zap.NewNop(), nil)
	if !w.refusedOnce() || w.refusedOnce() {
		t.Fatal("the sweep's refusal is logged once")
	}
}

// Under the proven switch, the skips of every covered program in the
// coverage map, however many (on a loaded 7.2 host, other tenants' socket()
// calls make sys_enter_socket skip by the million, and k_process_exit skips
// in about one session in six), charge nothing; the skips of every losing
// program are a kernel_skipped loss, evidence only: neither is a verdict.
func TestOnlyALosingProgramsSkipsAreALoss(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	for name, s := range programSkips {
		if s.class == skipGuardFree {
			continue
		}
		t.Run(name, func(t *testing.T) {
			counts := fakeMisses(t)
			p := new(ebpf.Program)
			w := newSkipWatch(zap.NewNop(), nil)
			w.watch(map[string]*ebpf.Program{name: p})
			counts[p] = 2_540_000
			got := w.losses()
			switch s.class {
			case skipCovered:
				if len(got) != 0 {
					t.Fatalf("%s is covered (%s), and its skips charged %v", name, s.backstop, got)
				}
			case skipLoses:
				if len(got) != 1 || got[0].Kind != models.CaptureLossKernelSkip || got[0].Count != 2_540_000 || got[0].What != name {
					t.Fatalf("%s loses calls, and its skips charged %v, want one kernel_skipped of 2540000 naming it", name, got)
				}
			case skipUnsupported:
				if len(got) != 0 {
					t.Fatalf("%s reports what keploy does not support, and its skips charged %v", name, got)
				}
			}
		})
	}
}

// A skip count that cannot be read (a losing program's or a covered one's)
// leaves the capture losses known: the count is evidence, which judges
// nothing, so a read it missed is no verdict. A socket_classify_failures
// count that cannot be read (k_sock_create's, on every kernel) leaves them
// unknown under the proven switch, never 0; with the switch off nothing is
// unknown.
func TestAnUnreadableSkipCountIsNoVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		program  string
		failures bool
		on       bool
		unknown  bool
	}{
		{"a losing program's skips, the switch on", "k_unnamed_program", false, true, false},
		{"a covered program's skips, the switch on", "syscall_probe_entry_socket", false, true, false},
		{"socket_classify_failures, the switch on", "syscall_probe_entry_socket", true, true, true},
		{"socket_classify_failures, the switch off", "syscall_probe_entry_socket", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenswitch.Pin(tc.on)
			t.Cleanup(provenswitch.Unpin)
			counted(t, true)
			was := programMisses
			programMisses = func(*ebpf.Program) (uint64, error) { return 0, errors.New("bad file descriptor") }
			t.Cleanup(func() { programMisses = was })
			var failures *ebpf.Map
			if tc.failures {
				failures = new(ebpf.Map)
				wasRead := readClassifyFailures
				readClassifyFailures = func(*ebpf.Map) (pidMapInsertFailure, error) {
					return pidMapInsertFailure{}, errors.New("bad file descriptor")
				}
				t.Cleanup(func() { readClassifyFailures = wasRead })
			}
			w := newSkipWatch(zap.NewNop(), failures)
			w.watch(map[string]*ebpf.Program{tc.program: new(ebpf.Program)})
			w.losses()
			if err := w.err(); (err != nil) != tc.unknown {
				t.Fatalf("err() = %v, want unknown: %v", err, tc.unknown)
			}
		})
	}
}

// A kernel that counts no skipped runs (before 5.12: no tracing program has
// a per-program recursion guard to skip a run by, and bpf_prog_info has no
// recursion_misses) leaves every run judged as it would be without the
// watch: no program is read, nothing is charged, nothing is unknown, and one
// INFO line says so. The Flipkart hosts run 5.10.
func TestAKernelThatCountsNoSkipsJudgesTheRunNormally(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	counted(t, false)
	was := programMisses
	programMisses = func(*ebpf.Program) (uint64, error) {
		t.Fatal("a program was read on a kernel that counts no skipped runs")
		return 0, nil
	}
	t.Cleanup(func() { programMisses = was })
	core, logs := observer.New(zapcore.DebugLevel)
	w := newSkipWatch(zap.New(core), nil)
	progs := map[string]*ebpf.Program{}
	for name, s := range programSkips {
		if s.class != skipGuardFree {
			progs[name] = new(ebpf.Program)
		}
	}
	w.watch(progs)
	if got := w.losses(); len(got) != 0 {
		t.Fatalf("charged %v, want nothing", got)
	}
	if err := w.err(); err != nil {
		t.Fatalf("err() = %v, want nil: the run is judged", err)
	}
	if sweep, _ := w.exitsSkipped(); sweep {
		t.Fatal("asked for a sweep with no skip to sweep after")
	}
	w.close()
	info := logs.FilterMessageSnippet("does not count the runs of BPF programs it skips").All()
	if len(info) != 1 || info[0].Level != zapcore.InfoLevel || logs.Len() != 1 {
		t.Fatalf("logged %+v, want exactly one INFO line", logs.All())
	}
}

// A failed ask of whether the kernel counts skipped runs says nothing either
// way: the programs are read, and a losing program's skips are charged as
// evidence. Taking the failure for "not counted" would read nothing for the
// session.
func TestAFailedAskOfTheKernelLeavesTheSkipsRead(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	counts := fakeMisses(t)
	recursionMissesCounted = func(*ebpf.Program) (bool, error) { return false, unix.EPERM }
	core, logs := observer.New(zapcore.InfoLevel)
	w := newSkipWatch(zap.New(core), nil)
	loses := new(ebpf.Program)
	w.watch(map[string]*ebpf.Program{"k_unnamed_program": loses})
	counts[loses] = 3
	if got := w.losses(); len(got) != 1 || got[0].Kind != models.CaptureLossKernelSkip || got[0].Count != 3 {
		t.Fatalf("charged %v, want the losing program's 3 skipped runs", got)
	}
	if n := logs.FilterMessageSnippet("does not count the runs of BPF programs it skips").Len(); n != 0 {
		t.Fatalf("said %d times that the kernel counts no skipped runs, on a failed ask", n)
	}
}

// A skipped run of a program that reports what keploy does not support (the
// unixconn report) charges nothing, under the switch or not, and is said at
// INFO in the session's summary only.
func TestAnUnsupportedProgramsSkipsAreSaidAtInfoOnly(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	counts := fakeMisses(t)
	p := new(ebpf.Program)
	core, logs := observer.New(zapcore.InfoLevel)
	w := newSkipWatch(zap.New(core), nil)
	w.watch(map[string]*ebpf.Program{"k_unix_dgram_sendmsg": p})
	counts[p] = 161
	if got := w.losses(); len(got) != 0 {
		t.Fatalf("charged %v, want nothing", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %+v before the summary, want nothing", logs.All())
	}
	w.close()
	all := logs.All()
	if len(all) != 1 || all[0].Level != zapcore.InfoLevel || all[0].ContextMap()["skipped_runs"] != uint64(161) {
		t.Fatalf("logged %+v, want one INFO in the summary with the 161 skipped runs", all)
	}
}
