//go:build linux

package bpfsafety

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// resolveTrampolineTarget resolves a tracing/LSM program's (attach_btf_obj_id,
// attach_btf_id) to its trampoline key and kernel function name.
//
// Returns:
//   - (key, funcName, true, false, nil) for trampoline-attached functions (BTF_KIND_FUNC) on Linux 6.0+
//   - (0, "", false, true, nil) on pre-6.0 kernels where bpf_prog_info omits attach_btf_id
//   - (0, "", false, false, nil) for non-trampoline programs (e.g. tp_btf / unloaded modules)
//   - error if BTF inspection failed (fail closed).
func resolveTrampolineTarget(info uapiProgInfo) (key uint64, funcName string, isTramp bool, pre60Unknown bool, err error) {
	progType := ebpf.ProgramType(info.Type)
	if progType != ebpf.Tracing && progType != ebpf.LSM {
		return 0, "", false, false, nil
	}
	if info.AttachBtfID == 0 {
		// Linux 5.10..5.19 do not populate attach_btf_id in bpf_prog_info.
		return 0, "", false, true, nil
	}

	vmlinux, err := loadKernelBTF()
	if err != nil {
		return 0, "", false, false, fmt.Errorf("load kernel BTF: %w", err)
	}

	spec := vmlinux
	if info.AttachBtfObjID != 0 {
		h, herr := btf.NewHandleFromID(btf.ID(info.AttachBtfObjID))
		if herr != nil {
			if errors.Is(herr, os.ErrNotExist) {
				// Kernel module was unloaded; its trampoline cannot exist anymore.
				return 0, "", false, false, nil
			}
			return 0, "", false, false, fmt.Errorf("open BTF handle %d: %w", info.AttachBtfObjID, herr)
		}
		defer func() { _ = h.Close() }()

		hi, ierr := h.Info()
		if ierr != nil {
			return 0, "", false, false, fmt.Errorf("inspect BTF handle %d: %w", info.AttachBtfObjID, ierr)
		}
		if hi.IsModule() {
			modSpec, serr := h.Spec(vmlinux)
			if serr != nil {
				return 0, "", false, false, fmt.Errorf("load module BTF %q: %w", hi.Name, serr)
			}
			spec = modSpec
		}
	}

	typ, err := spec.TypeByID(btf.TypeID(info.AttachBtfID))
	if err != nil {
		return 0, "", false, false, fmt.Errorf("resolve BTF type %d (obj %d): %w", info.AttachBtfID, info.AttachBtfObjID, err)
	}
	fn, ok := typ.(*btf.Func)
	if !ok {
		// e.g. tp_btf attaches to btf_trace_* typedef, not a bpf_trampoline.
		return 0, "", false, false, nil
	}
	return trampolineKey(info.AttachBtfObjID, info.AttachBtfID), fn.Name, true, false, nil
}

type dirTargets struct {
	keyToFunc    map[uint64]string
	hasUnknown5x bool
}

// inspectDirTrampolineTargets scans every pinned program in a dead grace
// directory and returns the unique trampoline key -> kernel function name map.
func (r *graceReaper) inspectDirTrampolineTargets(dirPath string) (dirTargets, error) {
	out := dirTargets{keyToFunc: make(map[uint64]string)}
	if !autoStateStore.isBPFFS(autoStateStore{}, dirPath) {
		return out, nil
	}
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
		key, fnName, isTramp, pre60, rerr := resolveTrampolineTarget(info)
		if rerr != nil {
			return out, fmt.Errorf("resolve trampoline target for %s: %w", pinPath, rerr)
		}
		if pre60 {
			out.hasUnknown5x = true
			continue
		}
		if isTramp && key != 0 && fnName != "" {
			out.keyToFunc[key] = fnName
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

	l, err := attachPinnedReaperNoop(noop)
	if err != nil {
		return fmt.Errorf("attach noop fentry/%s: %w", funcName, err)
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
// bpf_jit_harden!=0, kptr_restrict hides addresses, or pre-6.0 kernel without
// attach_btf_id in bpf_prog_info):
//  1. Forces a trampoline rebuild for every known target function in targets
//     (detecting permanent last-link EBUSY and failing closed on transient errors).
//  2. Enables BPF_ENABLE_STATS(BPF_STATS_RUN_TIME) for r.statsProbeWait (~2s)
//     and verifies cumulative run_cnt across all pinned programs stays flat.
//  3. If BPF_ENABLE_STATS fails or run_cnt increases, fails closed (keeps pins).
func (r *graceReaper) checkReapViaStatsFallback(dirPath string, targets dirTargets) (safeToUnpin bool, permanentEBUSY bool, ebusyFunc string, err error) {
	for _, fnName := range targets.keyToFunc {
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
