//go:build linux

package bpfsafety

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// uapiProgInfo matches Linux UAPI struct bpf_prog_info (through attach_btf_id,
// Linux 6.0+). On older 5.10..5.19 kernels, the kernel copies only its shorter
// struct prefix and leaves AttachBtfObjId / AttachBtfId zeroed.
type uapiProgInfo struct {
	Type                 uint32
	ID                   uint32
	Tag                  [8]uint8
	JitedProgLen         uint32
	XlatedProgLen        uint32
	JitedProgInsns       uint64
	XlatedProgInsns      uint64
	LoadTime             uint64
	CreatedByUID         uint32
	NrMapIDs             uint32
	MapIDs               uint64
	Name                 [16]byte
	Ifindex              uint32
	_                    [4]byte
	NetnsDev             uint64
	NetnsIno             uint64
	NrJitedKsyms         uint32
	NrJitedFuncLens      uint32
	JitedKsyms           uint64
	JitedFuncLens        uint64
	BtfID                uint32
	FuncInfoRecSize      uint32
	FuncInfo             uint64
	NrFuncInfo           uint32
	NrLineInfo           uint32
	LineInfo             uint64
	JitedLineInfo        uint64
	NrJitedLineInfo      uint32
	LineInfoRecSize      uint32
	JitedLineInfoRecSize uint32
	NrProgTags           uint32
	ProgTags             uint64
	RunTimeNs            uint64
	RunCnt               uint64
	RecursionMisses      uint64
	VerifiedInsns        uint32
	AttachBtfObjID       uint32
	AttachBtfID          uint32
	_                    [4]byte
}

type uapiObjGetInfoAttr struct {
	BpfFD   uint32
	InfoLen uint32
	Info    uint64
}

func queryProgInfo(fd int) (uapiProgInfo, error) {
	var info uapiProgInfo
	attr := uapiObjGetInfoAttr{
		BpfFD:   uint32(fd),
		InfoLen: uint32(unsafe.Sizeof(info)),
		Info:    uint64(uintptr(unsafe.Pointer(&info))),
	}
	_, _, errno := unix.Syscall(
		unix.SYS_BPF,
		uintptr(unix.BPF_OBJ_GET_INFO_BY_FD),
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
	)
	if errno != 0 {
		return uapiProgInfo{}, errno
	}
	return info, nil
}

var (
	kernelBTFOnce sync.Once
	kernelBTFSpec *btf.Spec
	kernelBTFErr  error
)

func loadKernelBTF() (*btf.Spec, error) {
	kernelBTFOnce.Do(func() {
		kernelBTFSpec, kernelBTFErr = btf.LoadKernelSpec()
	})
	return kernelBTFSpec, kernelBTFErr
}

var (
	dynamicTrampImageOnce sync.Once
	dynamicTrampImageVal  bool
)

// kernelHasDynamicTrampImage reports whether this kernel allocates a distinct
// `struct bpf_tramp_image` per trampoline update (Linux 5.12+, or any distro
// kernel carrying that commit). On Linux 5.10..5.11, a single static trampoline
// page is rewritten in place at the same /proc/kallsyms address for the life of
// the trampoline while any anchor or neighbor link remains attached, so
// kallsyms address changes cannot distinguish old vs rebuilt trampolines there;
// the reaper instead forces a synchronous rebuild + BPF_ENABLE_STATS run_cnt
// verification on those kernels.
func kernelHasDynamicTrampImage() bool {
	dynamicTrampImageOnce.Do(func() {
		spec, err := loadKernelBTF()
		if err != nil {
			dynamicTrampImageVal = false
			return
		}
		_, err = spec.AnyTypeByName("bpf_tramp_image")
		dynamicTrampImageVal = (err == nil)
	})
	return dynamicTrampImageVal
}

// resolveTrampolineTarget resolves a tracing/LSM program's (attach_btf_obj_id,
// attach_btf_id) to its (btfObjID, btfID, funcName).
//
// For vmlinux targets (AttachBtfObjID <= 1), uses the loaded vmlinux BTF spec
// directly without calling btf.NewHandleFromID (BPF_BTF_GET_FD_BY_ID requires
// CAP_SYS_ADMIN, whereas CAP_BPF + CAP_PERFMON agents can read /sys/kernel/btf/vmlinux).
//
// Returns:
//   - (objID, btfID, funcName, true, false, nil) for trampoline-attached functions on Linux 6.0+
//   - (0, 0, "", false, true, nil) on pre-6.0 kernels where bpf_prog_info omits attach_btf_id
//   - (0, 0, "", false, false, nil) for non-trampoline programs (e.g. tp_btf / unloaded modules)
//   - error if BTF inspection failed (fail closed).
func resolveTrampolineTarget(info uapiProgInfo) (btfObjID, btfID uint32, funcName string, isTramp bool, pre60Unknown bool, err error) {
	progType := ebpf.ProgramType(info.Type)
	if progType != ebpf.Tracing && progType != ebpf.LSM {
		return 0, 0, "", false, false, nil
	}
	if info.AttachBtfID == 0 {
		// Linux 5.10..5.19 do not populate attach_btf_id in bpf_prog_info.
		return 0, 0, "", false, true, nil
	}

	vmlinux, err := loadKernelBTF()
	if err != nil {
		return 0, 0, "", false, false, fmt.Errorf("load kernel BTF: %w", err)
	}

	spec := vmlinux
	objID := info.AttachBtfObjID
	if objID > 1 {
		h, herr := btf.NewHandleFromID(btf.ID(objID))
		if herr != nil {
			if errors.Is(herr, os.ErrNotExist) {
				// Kernel module was unloaded; its trampoline cannot exist anymore.
				return 0, 0, "", false, false, nil
			}
			if errors.Is(herr, unix.EPERM) || errors.Is(herr, unix.EACCES) {
				// CAP_BPF-only agent (without CAP_SYS_ADMIN) cannot call
				// BPF_BTF_GET_FD_BY_ID; signal fallback to world-readable
				// /sys/kernel/btf/<mod> using pin-time target_funcs.
				return objID, info.AttachBtfID, "", false, true, nil
			}
			return 0, 0, "", false, false, fmt.Errorf("open BTF handle %d: %w", objID, herr)
		}
		defer func() { _ = h.Close() }()

		hi, ierr := h.Info()
		if ierr != nil {
			return 0, 0, "", false, false, fmt.Errorf("inspect BTF handle %d: %w", objID, ierr)
		}
		if hi.IsModule() {
			modSpec, serr := h.Spec(vmlinux)
			if serr != nil {
				return 0, 0, "", false, false, fmt.Errorf("load module BTF %q: %w", hi.Name, serr)
			}
			spec = modSpec
		}
	} else if objID == 0 {
		objID = 1
	}

	typ, err := spec.TypeByID(btf.TypeID(info.AttachBtfID))
	if err != nil {
		return 0, 0, "", false, false, fmt.Errorf("resolve BTF type %d (obj %d): %w", info.AttachBtfID, objID, err)
	}
	fn, ok := typ.(*btf.Func)
	if !ok {
		// e.g. tp_btf attaches to btf_trace_* typedef, not a bpf_trampoline.
		return 0, 0, "", false, false, nil
	}
	return objID, info.AttachBtfID, fn.Name, true, false, nil
}

// resolveFuncNameInBTF resolves a kernel function name (vmlinux or loaded kernel
// module such as tls.ko) using /sys/kernel/btf/*. Used on Linux 5.10..5.19
// (where bpf_prog_info omits attach_btf_id) and for CAP_BPF-only agents
// inspecting module targets on 6.0+.
func resolveFuncNameInBTF(funcName string) (btfObjID, btfID uint32, isModule, ok bool, err error) {
	if funcName == "" {
		return 0, 0, false, false, nil
	}
	vmlinux, err := loadKernelBTF()
	if err != nil {
		return 0, 0, false, false, fmt.Errorf("load kernel BTF: %w", err)
	}
	var fn *btf.Func
	if err := vmlinux.TypeByName(funcName, &fn); err == nil && fn != nil {
		if id, ierr := vmlinux.TypeID(fn); ierr == nil && id != 0 {
			return 1, uint32(id), false, true, nil
		}
	}
	// Check loaded kernel modules (e.g. tls.ko for tls_sw_sendmsg/recvmsg/tls_init).
	if entries, rerr := os.ReadDir("/sys/kernel/btf"); rerr == nil {
		for _, e := range entries {
			modName := e.Name()
			if modName == "vmlinux" || e.IsDir() {
				continue
			}
			modSpec, merr := btf.LoadKernelModuleSpec(modName)
			if merr != nil {
				continue
			}
			var mfn *btf.Func
			if err := modSpec.TypeByName(funcName, &mfn); err == nil && mfn != nil {
				if id, ierr := modSpec.TypeID(mfn); ierr == nil && id != 0 {
					return 0, uint32(id), true, true, nil
				}
			}
		}
	}
	return 0, 0, false, false, nil
}

// isRawBTFTracepointName reports whether name corresponds to a raw BTF
// tracepoint (`tp_btf/<name>` attaches to `btf_trace_<name>` typedef in
// vmlinux BTF, not a bpf_trampoline).
func isRawBTFTracepointName(name string) bool {
	if name == "" {
		return false
	}
	vmlinux, err := loadKernelBTF()
	if err != nil {
		return false
	}
	_, terr := vmlinux.AnyTypeByName("btf_trace_" + name)
	return terr == nil
}

type dirTargets struct {
	keyToFunc   map[uint64]string
	modIDToFunc map[uint32]string // module BTF type ID -> funcName (matches any module obj>=2 in kallsyms)
	funcs       map[string]struct{}
}

func (d dirTargets) modBTFIDSet() map[uint32]struct{} {
	if len(d.modIDToFunc) == 0 {
		return nil
	}
	out := make(map[uint32]struct{}, len(d.modIDToFunc))
	for id := range d.modIDToFunc {
		out[id] = struct{}{}
	}
	return out
}

func (d dirTargets) funcForKey(k uint64) (string, bool) {
	if fn, ok := d.keyToFunc[k]; ok && fn != "" {
		return fn, true
	}
	if len(d.modIDToFunc) > 0 && (k>>32) != 1 {
		if fn, ok := d.modIDToFunc[uint32(k&0x7fffffff)]; ok && fn != "" {
			return fn, true
		}
	}
	return "", false
}

func (d dirTargets) hasKallsymsMatchers() bool {
	return len(d.keyToFunc) > 0 || len(d.modIDToFunc) > 0
}

// parsePinTargetMap decodes per-pin `pinName=targetFunc` entries from
// target_funcs. Legacy entries without `=` do not satisfy per-pin resolution
// (so a legacy dir with unresolved pre-6.0 or EPERM pins stays safely pinned).
func parsePinTargetMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if idx := strings.IndexByte(e, '='); idx > 0 && idx+1 < len(e) {
			out[e[:idx]] = e[idx+1:]
		}
	}
	return out
}

// inspectDirTrampolineTargets scans EVERY pinned program in a dead grace
// directory and resolves its trampoline keys and kernel function name PER
// PROGRAM (not per directory).
//
// Works on BOTH Linux 6.0+ (from bpf_prog_info.attach_btf_id) and Linux
// 5.10..5.19 / CAP_BPF-only module targets (from per-pin target_funcs metadata
// resolved against /sys/kernel/btf/*). FAILS CLOSED (returns an error keeping
// the WHOLE directory pinned) if ANY Tracing/LSM program in the directory
// cannot be mapped to its concrete target — one resolved program can NEVER hide
// an unresolved program in the same directory.
func (r *graceReaper) inspectDirTrampolineTargets(dirPath string) (dirTargets, error) {
	out := dirTargets{
		keyToFunc:   make(map[uint64]string),
		modIDToFunc: make(map[uint32]string),
		funcs:       make(map[string]struct{}),
	}
	if !autoStateStore.isBPFFS(autoStateStore{}, dirPath) {
		return out, nil
	}
	recordedEntries, rerr := r.store.readTargets(dirPath)
	if rerr != nil {
		return out, fmt.Errorf("read recorded target_funcs in %s: %w", dirPath, rerr)
	}
	pinTargets := parsePinTargetMap(recordedEntries)

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return out, fmt.Errorf("read dead grace dir %s: %w", dirPath, err)
	}
	for _, e := range entries {
		if isMetadataFile(e.Name()) {
			continue
		}
		pinPath := filepath.Join(dirPath, e.Name())
		prog, lerr := ebpf.LoadPinnedProgram(pinPath, nil)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				continue
			}
			return out, fmt.Errorf("load pinned prog %s: %w", pinPath, lerr)
		}
		info, ierr := queryProgInfo(prog.FD())
		_ = prog.Close()
		if ierr != nil {
			return out, fmt.Errorf("query prog info %s: %w", pinPath, ierr)
		}
		progType := ebpf.ProgramType(info.Type)
		if progType != ebpf.Tracing && progType != ebpf.LSM {
			continue
		}

		objID, btfID, fnName, isTramp, pre60, rerr := resolveTrampolineTarget(info)
		if rerr != nil {
			return out, fmt.Errorf("resolve trampoline target for %s: %w", pinPath, rerr)
		}
		if !pre60 {
			if isTramp && fnName != "" {
				out.funcs[fnName] = struct{}{}
				for _, k := range trampolineCandidateKeys(objID, btfID) {
					out.keyToFunc[k] = fnName
				}
			}
			continue
		}

		// Per-program resolution for pre-6.0 kernels and CAP_BPF-only module
		// targets (6.0+ EPERM): EVERY such pin MUST have its own recorded
		// target in target_funcs, or we fail closed for the whole directory.
		recTarget, hasPin := pinTargets[e.Name()]
		if !hasPin || recTarget == "" {
			return out, fmt.Errorf("dead grace dir %s contains Tracing/LSM program %s with no recorded per-pin target (failing closed)", dirPath, e.Name())
		}
		if recTarget == nonTrampolineTargetSentinel {
			continue
		}
		bObjID, bID, isMod, ok, berr := resolveFuncNameInBTF(recTarget)
		if berr != nil {
			return out, fmt.Errorf("resolve recorded target %q for %s in BTF: %w", recTarget, pinPath, berr)
		}
		if !ok {
			if isRawBTFTracepointName(recTarget) {
				continue
			}
			return out, fmt.Errorf("dead grace dir %s contains Tracing/LSM program %s whose recorded target %q is not present in kernel BTF (failing closed)", dirPath, e.Name(), recTarget)
		}
		out.funcs[recTarget] = struct{}{}
		if isMod {
			out.modIDToFunc[bID] = recTarget
			if info.AttachBtfObjID > 1 && info.AttachBtfID != 0 {
				for _, k := range trampolineCandidateKeys(info.AttachBtfObjID, info.AttachBtfID) {
					out.keyToFunc[k] = recTarget
				}
			}
		} else {
			for _, k := range trampolineCandidateKeys(bObjID, bID) {
				out.keyToFunc[k] = recTarget
			}
		}
	}
	return out, nil
}

// forceRebuildTarget attaches and immediately detaches a cached 2-instruction
// no-op fentry program on funcName. The no-op program is pinned into this
// reaper's OWN active grace directory BEFORE its first attach so that even if
// its own detach hits -ENOMEM, it follows the exact same host-bpffs grace-pin
// invariant and can never be freed while still referenced by a stale image.
//
// Returns wrapped unix.EBUSY when the kernel rejects the attach because a prior
// last-link unlink failure left the ftrace direct entry orphaned until reboot.
func (r *graceReaper) forceRebuildTarget(funcName string) error {
	r.noopMu.Lock()
	defer r.noopMu.Unlock()

	noop := r.noopCache[funcName]
	if noop == nil {
		if r.pinNoop == nil {
			return errors.New("reaper has no active grace directory to pin no-op rebuild program")
		}
		p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
			Name:       "kp_grace_rb",
			Type:       ebpf.Tracing,
			AttachType: ebpf.AttachTraceFEntry,
			AttachTo:   funcName,
			License:    "GPL",
			Instructions: asm.Instructions{
				asm.Mov.Imm(asm.R0, 0),
				asm.Return(),
			},
		})
		if err != nil {
			return fmt.Errorf("load noop fentry/%s: %w", funcName, err)
		}
		if err := r.pinNoop(p, funcName); err != nil {
			_ = p.Close()
			return fmt.Errorf("pin noop fentry/%s in reaper grace dir: %w", funcName, err)
		}
		if r.noopCache == nil {
			r.noopCache = make(map[string]*ebpf.Program)
		}
		r.noopCache[funcName] = p
		noop = p
	}

	if r.onBeforeNoopAttach != nil {
		r.onBeforeNoopAttach()
	}
	l, err := attachPinnedReaperNoop(noop)
	if err != nil {
		return fmt.Errorf("attach noop fentry/%s: %w", funcName, err)
	}
	if r.onBeforeNoopClose != nil {
		r.onBeforeNoopClose()
	}
	_ = l.Close()
	return nil
}

func (r *graceReaper) closeNoopCache() {
	r.noopMu.Lock()
	cache := r.noopCache
	r.noopCache = nil
	r.noopMu.Unlock()
	for _, p := range cache {
		_ = p.Close()
	}
}

func (r *graceReaper) sumDirRunCnt(dirPath string) (uint64, error) {
	if !autoStateStore.isBPFFS(autoStateStore{}, dirPath) {
		return 0, nil
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, fmt.Errorf("read dead grace dir %s for run_cnt: %w", dirPath, err)
	}
	var sum uint64
	for _, e := range entries {
		if isMetadataFile(e.Name()) {
			continue
		}
		pinPath := filepath.Join(dirPath, e.Name())
		prog, lerr := ebpf.LoadPinnedProgram(pinPath, nil)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("load pinned prog %s for run_cnt: %w", pinPath, lerr)
		}
		info, ierr := queryProgInfo(prog.FD())
		_ = prog.Close()
		if ierr != nil {
			return 0, fmt.Errorf("query prog run_cnt %s: %w", pinPath, ierr)
		}
		sum += info.RunCnt
	}
	return sum, nil
}

func defaultEnableStats() (io.Closer, error) {
	return ebpf.EnableStats(unix.BPF_STATS_RUN_TIME)
}

// checkReapViaStatsFallback is the fallback liveness verifier when
// /proc/kallsyms addresses are unavailable (bpf_jit_kallsyms=0,
// bpf_jit_harden!=0, or kptr_restrict hides addresses):
//  1. Requires at least one resolved target function (failing closed if empty).
//  2. Forces a trampoline rebuild for EVERY target function in targets.funcs
//     (detecting permanent last-link EBUSY and failing closed on transient errors).
//  3. Enables BPF_ENABLE_STATS(BPF_STATS_RUN_TIME) for r.statsProbeWait (~2s)
//     and verifies cumulative run_cnt across all pinned programs stays flat.
//  4. If BPF_ENABLE_STATS fails or run_cnt increases, fails closed (keeps pins).
func (r *graceReaper) checkReapViaStatsFallback(dirPath string, targets dirTargets) (safeToUnpin bool, permanentEBUSY bool, ebusyFunc string, err error) {
	fnSet := targets.allFuncs()
	if len(fnSet) == 0 {
		return false, false, "", errors.New("cannot run stats fallback with 0 resolved trampoline targets (failing closed)")
	}
	for fnName := range fnSet {
		if rerr := r.rebuildTargetFn(fnName); rerr != nil {
			if errors.Is(rerr, unix.EBUSY) {
				return false, true, fnName, rerr
			}
			return false, false, "", rerr
		}
	}
	closer, serr := r.enableStatsFn()
	if serr != nil {
		return false, false, "", fmt.Errorf("BPF_ENABLE_STATS unavailable (failing closed, keeping pins): %w", serr)
	}
	defer func() { _ = closer.Close() }()

	c1, err := r.runCntFn(dirPath)
	if err != nil {
		return false, false, "", err
	}
	wait := r.statsProbeWait
	if wait <= 0 {
		wait = defaultStatsProbeWait
	}
	time.Sleep(wait)
	c2, err := r.runCntFn(dirPath)
	if err != nil {
		return false, false, "", err
	}
	if c2 > c1 {
		if r.logger != nil {
			r.logger.Warn("bpf-safety: pinned program still executing during BPF_ENABLE_STATS check; keeping pins",
				zap.String("graceDir", dirPath),
				zap.Uint64("runCntBefore", c1),
				zap.Uint64("runCntAfter", c2))
		}
		return false, false, "", nil
	}
	return true, false, "", nil
}

func (t dirTargets) allFuncs() map[string]struct{} {
	out := make(map[string]struct{}, len(t.funcs)+len(t.keyToFunc))
	for fn := range t.funcs {
		if fn != "" {
			out[fn] = struct{}{}
		}
	}
	for _, fn := range t.keyToFunc {
		if fn != "" {
			out[fn] = struct{}{}
		}
	}
	return out
}
