//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// ExtraBPFPinPatterns lets callers extend CleanOrphanedBPFPins with
// additional glob patterns (for example, a downstream build that pins
// extra BPF maps under /sys/fs/bpf). Set before the first call.
var ExtraBPFPinPatterns []string

// cgroupOwnerMapName is the unpinned kernel BPF map name held by every live
// keploy Hooks instance for the lifetime of its cgroup attachments. Its length
// is exactly BPF_OBJ_NAME_LEN-1 (15 bytes) so the kernel keeps it verbatim.
// Because the map is unpinned, the kernel destroys it immediately when the
// owning process exits or is killed (including SIGKILL/OOM), and because BPF
// map IDs are kernel-global, ownership is visible across PID and mount
// namespaces without scanning /proc or writing files to the host filesystem.
const cgroupOwnerMapName = "keploy_cg_owner"

var (
	selfOwnerMapIDsMu sync.Mutex
	selfOwnerMapIDs   = map[ebpf.MapID]struct{}{}
)

func registerSelfOwnerMapID(id ebpf.MapID) {
	if id == 0 {
		return
	}
	selfOwnerMapIDsMu.Lock()
	selfOwnerMapIDs[id] = struct{}{}
	selfOwnerMapIDsMu.Unlock()
}

func unregisterSelfOwnerMapID(id ebpf.MapID) {
	if id == 0 {
		return
	}
	selfOwnerMapIDsMu.Lock()
	delete(selfOwnerMapIDs, id)
	selfOwnerMapIDsMu.Unlock()
}

func snapshotSelfOwnerMapIDs() map[ebpf.MapID]struct{} {
	selfOwnerMapIDsMu.Lock()
	defer selfOwnerMapIDsMu.Unlock()
	if len(selfOwnerMapIDs) == 0 {
		return nil
	}
	out := make(map[ebpf.MapID]struct{}, len(selfOwnerMapIDs))
	for id := range selfOwnerMapIDs {
		out[id] = struct{}{}
	}
	return out
}

// cgroupOwnerToken represents this Hooks instance's live, kernel-enforced
// ownership claim on a cgroup hierarchy. Keeping the unpinned BPF map FD open
// signals to any other keploy process on the same host — in any PID namespace
// (including hostPID: false sidecar/replay pods mounting the host cgroupfs) —
// that this cgroup has a live owner. Closing it (or process death / SIGKILL)
// automatically releases the token in the kernel.
type cgroupOwnerToken struct {
	m  *ebpf.Map
	id ebpf.MapID
}

func (t *cgroupOwnerToken) mapID() ebpf.MapID {
	if t == nil {
		return 0
	}
	return t.id
}

func (t *cgroupOwnerToken) Close() {
	if t == nil {
		return
	}
	unregisterSelfOwnerMapID(t.id)
	if t.m != nil {
		_ = t.m.Close()
		t.m = nil
	}
	t.id = 0
}

// acquireCgroupOwnerToken creates an unpinned 1-entry BPF array map recording
// the inode (cgroup ID) of cgroupPath. Callers must hold the returned token
// from before orphan cleanup/attach until after all cgroup links are closed.
func acquireCgroupOwnerToken(cgroupPath string) (*cgroupOwnerToken, error) {
	cgroupID, err := cgroupIDOfPath(cgroupPath)
	if err != nil {
		return nil, fmt.Errorf("stat cgroup %q: %w", cgroupPath, err)
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       cgroupOwnerMapName,
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("create cgroup owner map: %w", err)
	}
	if err := m.Update(uint32(0), cgroupID, ebpf.UpdateAny); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("record cgroup ID %d in owner map: %w", cgroupID, err)
	}
	info, err := m.Info()
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("inspect cgroup owner map: %w", err)
	}
	id, ok := info.ID()
	if !ok || id == 0 {
		_ = m.Close()
		return nil, errors.New("cgroup owner map did not report a kernel ID")
	}
	registerSelfOwnerMapID(id)
	return &cgroupOwnerToken{m: m, id: id}, nil
}

func cgroupIDOfPath(cgroupPath string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(cgroupPath, &st); err != nil {
		return 0, err
	}
	if st.Ino == 0 {
		return 0, fmt.Errorf("cgroup %q reported zero inode", cgroupPath)
	}
	return st.Ino, nil
}

func cgroupIDOfFD(cgroupFD uintptr) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(cgroupFD), &st); err != nil {
		return 0, err
	}
	if st.Ino == 0 {
		return 0, errors.New("cgroup FD reported zero inode")
	}
	return st.Ino, nil
}

// keployBPFProgNames lists the BPF program names that keploy attaches to cgroups.
// Used to identify orphaned legacy (non-bpf_link) programs from crashed/killed
// previous runs.
var keployBPFProgNames = map[string]ebpf.AttachType{
	"k_connect4":     ebpf.AttachCGroupInet4Connect,
	"k_connect6":     ebpf.AttachCGroupInet6Connect,
	"k_bind4":        ebpf.AttachCGroupInet4Bind,
	"k_bind6":        ebpf.AttachCGroupInet6Bind,
	"k_post_bind4":   ebpf.AttachCGroupInet4PostBind,
	"k_post_bind6":   ebpf.AttachCGroupInet6PostBind,
	"k_getpeername4": ebpf.AttachCgroupInet4GetPeername,
	"k_getpeername6": ebpf.AttachCgroupInet6GetPeername,
	"k_sockops":      ebpf.AttachCGroupSockOps,
	"k_sock_create":  ebpf.AttachCGroupInetSockCreate,
	"k_udp4_sendmsg": ebpf.AttachCGroupUDP4Sendmsg,
	"k_udp6_sendmsg": ebpf.AttachCGroupUDP6Sendmsg,
	"k_udp4_recvmsg": ebpf.AttachCGroupUDP4Recvmsg,
	"k_udp6_recvmsg": ebpf.AttachCGroupUDP6Recvmsg,
	// Attached only under the proven switch (udpwatch.go).
	"k_udp_bind4":        ebpf.AttachCGroupInet4Bind,
	"k_udp_bind6":        ebpf.AttachCGroupInet6Bind,
	"k_udp_ingress":      ebpf.AttachCGroupInetIngress,
	"k_udp_egress":       ebpf.AttachCGroupInetEgress,
	"k_udp_sock_release": ebpf.AttachCgroupInetSockRelease,
	// Attached only under the proven switch (readywatch.go).
	"k_ready_bind4":    ebpf.AttachCGroupInet4Bind,
	"k_ready_bind6":    ebpf.AttachCGroupInet6Bind,
	"k_ready_connect4": ebpf.AttachCGroupInet4Connect,
	"k_ready_connect6": ebpf.AttachCGroupInet6Connect,
	// Attached in every mode (hooks.go).
	"k_app_conn_release": ebpf.AttachCgroupInetSockRelease,
}

// kernelProgName is the name the kernel keeps for a program loaded as name:
// at most BPF_OBJ_NAME_LEN-1 (15) bytes, the rest cut (k_udp_sock_release is
// k_udp_sock_rele there, and k_ready_connect4 and 6 are both k_ready_connect,
// told apart by their attach types).
func kernelProgName(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

// orphanNamed says whether a program the kernel reports as name (either its
// full BTF name on modern kernels or the 15-byte truncated bpf_prog_info name)
// is keploy's progName.
func orphanNamed(name, progName string) bool {
	return name == progName || name == kernelProgName(progName)
}

type attachedCgroupProg struct {
	id     ebpf.ProgramID
	linkID link.ID
}

type cgroupAttachKey struct {
	attachType ebpf.AttachType
	progID     ebpf.ProgramID
}

type cgroupLinkSnapshot struct {
	// exact holds (attachType, progID) pairs confirmed backed by a bpf_link on
	// the target cgroup.
	exact map[cgroupAttachKey]bool
	// anyCgroup holds progIDs backed by a cgroup bpf_link whose CgroupInfo
	// could not be read (fail-safe: never treat as an unowned legacy attach).
	anyCgroup map[ebpf.ProgramID]bool
}

func (s cgroupLinkSnapshot) isLinked(attachType ebpf.AttachType, ap attachedCgroupProg) bool {
	if ap.linkID != 0 {
		return true
	}
	if s.exact[cgroupAttachKey{attachType: attachType, progID: ap.id}] {
		return true
	}
	return s.anyCgroup[ap.id]
}

type cgroupCleanupCandidate struct {
	progID     ebpf.ProgramID
	name       string
	attachType ebpf.AttachType
}

type cgroupCleanupDeps struct {
	hasOtherOwner  func(cgroupID uint64, exclude map[ebpf.MapID]struct{}) (bool, error)
	hasOtherProc   func(selfPID int) bool
	queryPrograms  func(cgroupFD uintptr, attachType ebpf.AttachType) ([]attachedCgroupProg, error)
	linkedPrograms func(cgroupID uint64) (cgroupLinkSnapshot, error)
	progNameByID   func(progID ebpf.ProgramID) (string, error)
	detachProgram  func(cgroupFD uintptr, progID ebpf.ProgramID, attachType ebpf.AttachType) error
	selfPID        int
}

func defaultCgroupCleanupDeps() cgroupCleanupDeps {
	return cgroupCleanupDeps{
		hasOtherOwner:  realHasOtherCgroupOwnerMap,
		hasOtherProc:   hasOtherKeployProcInProcfs,
		queryPrograms:  queryAttachedPrograms,
		linkedPrograms: realLinkedPrograms,
		progNameByID:   progNameByID,
		detachProgram:  detachProgramFromCgroup,
		selfPID:        os.Getpid(),
	}
}

// DetectAndCleanOrphanedCgroupPrograms finds orphaned legacy (non-bpf_link)
// BPF programs attached to cgroupPath that match keploy's known program names
// and detaches them.
//
// Ownership safety does NOT rely solely on /proc, because containers running
// with hostPID: false (such as Kubernetes sidecar/replay pods mounting host
// /sys/fs/cgroup) cannot see host or sibling keploy processes in /proc:
//
//  1. Programs attached via bpf_link are automatically detached by the kernel
//     when their owning process exits or is killed (unless pinned; keploy does
//     not pin cgroup links). Any program backed by a live bpf_link belongs to
//     a live process and is never touched.
//  2. Live keploy instances hold an unpinned kernel BPF map (keploy_cg_owner)
//     carrying the target cgroup's inode. Kernel BPF map IDs are visible across
//     PID and mount namespaces, and the kernel destroys the map immediately on
//     process death / SIGKILL. If another live owner token exists for this
//     cgroup (or another keploy process is visible in /proc for backward
//     compatibility with older binaries in the same PID namespace), cleanup is
//     skipped.
//  3. After querying candidate legacy attachments, ownership and bpf_link
//     status are re-verified before calling BPF_PROG_DETACH, preventing races
//     with concurrently starting agents.
//
// Capability note: inspecting kernel-global BPF maps/links via
// BPF_MAP_GET_NEXT_ID / BPF_LINK_GET_NEXT_ID requires CAP_SYS_ADMIN (unlike
// BPF_MAP_CREATE, which only needs CAP_BPF). Containers running with CAP_BPF
// without CAP_SYS_ADMIN receive EPERM from those queries and fail closed here
// (skipping orphan detach rather than risking detaching a live sibling's
// programs across PID namespaces).
func DetectAndCleanOrphanedCgroupPrograms(logger *zap.Logger, cgroupPath string) int {
	return detectAndCleanOrphanedCgroupPrograms(logger, cgroupPath, 0)
}

func detectAndCleanOrphanedCgroupPrograms(logger *zap.Logger, cgroupPath string, selfOwnerMapID ebpf.MapID) int {
	cgroupFD, err := os.Open(cgroupPath)
	if err != nil {
		logger.Debug("Cannot open cgroup for orphan detection",
			zap.String("path", cgroupPath), zap.Error(err))
		return 0
	}
	defer func() { _ = cgroupFD.Close() }()

	cgroupID, err := cgroupIDOfFD(cgroupFD.Fd())
	if err != nil {
		logger.Debug("Cannot stat cgroup for orphan detection; skipping cleanup",
			zap.String("path", cgroupPath), zap.Error(err))
		return 0
	}

	var exclude map[ebpf.MapID]struct{}
	if selfOwnerMapID != 0 {
		exclude = map[ebpf.MapID]struct{}{selfOwnerMapID: {}}
	}

	return cleanOrphanedCgroupProgramsWithDeps(logger, cgroupFD.Fd(), cgroupPath, cgroupID, exclude, defaultCgroupCleanupDeps())
}

func cleanOrphanedCgroupProgramsWithDeps(
	logger *zap.Logger,
	cgroupFD uintptr,
	cgroupPath string,
	cgroupID uint64,
	excludeOwnerIDs map[ebpf.MapID]struct{},
	deps cgroupCleanupDeps,
) int {
	// Pre-check: skip immediately if another live keploy owns this cgroup
	// (in any PID namespace) or if we lack permission to inspect live owners
	// (fail-closed).
	otherOwner, err := deps.hasOtherOwner(cgroupID, excludeOwnerIDs)
	if err != nil {
		logger.Debug("Skipping orphaned cgroup program cleanup — cannot verify live cgroup owners",
			zap.String("cgroupPath", cgroupPath), zap.Uint64("cgroupID", cgroupID), zap.Error(err))
		return 0
	}
	if otherOwner {
		logger.Debug("Skipping orphaned cgroup program cleanup — another live keploy instance owns this cgroup",
			zap.String("cgroupPath", cgroupPath), zap.Uint64("cgroupID", cgroupID))
		return 0
	}
	if deps.hasOtherProc(deps.selfPID) {
		logger.Debug("Skipping orphaned cgroup program cleanup — another keploy process is alive",
			zap.Int("selfPID", deps.selfPID))
		return 0
	}

	// Group known keploy program names by AttachType so we issue one
	// BPF_PROG_QUERY per attach type instead of repeating the syscall for
	// attach types shared by multiple keploy programs.
	namesByAttach := make(map[ebpf.AttachType][]string, len(keployBPFProgNames))
	for progName, attachType := range keployBPFProgNames {
		namesByAttach[attachType] = append(namesByAttach[attachType], progName)
	}

	// Collect candidate keploy programs whose query result did not already
	// report a bpf_link ID.
	seen := make(map[cgroupAttachKey]struct{})
	var candidates []cgroupCleanupCandidate
	for attachType, progNames := range namesByAttach {
		progs, err := deps.queryPrograms(cgroupFD, attachType)
		if err != nil {
			logger.Debug("cannot query cgroup-attached BPF programs for orphan cleanup; the helper will become a no-op for this attach type — verify kernel support for BPF_PROG_QUERY on this attach type, ensure CAP_SYS_ADMIN/CAP_BPF are granted, check the cgroup v2 mount, and use bpftool to clear stale attachments manually if the subsequent attach fails with 'program already attached'",
				zap.String("cgroupPath", cgroupPath),
				zap.String("attachType", attachType.String()),
				zap.Error(err))
			continue
		}
		for _, ap := range progs {
			if ap.linkID != 0 {
				continue
			}
			key := cgroupAttachKey{attachType: attachType, progID: ap.id}
			if _, dup := seen[key]; dup {
				continue
			}
			name, err := deps.progNameByID(ap.id)
			if err != nil {
				continue
			}
			matched := false
			for _, progName := range progNames {
				if orphanNamed(name, progName) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, cgroupCleanupCandidate{
				progID:     ap.id,
				name:       name,
				attachType: attachType,
			})
		}
	}

	if len(candidates) == 0 {
		return 0
	}

	// Post-query bpf_link filter: BPF_PROG_QUERY on cgroups does not populate
	// link IDs on all kernels, so cross-check against the kernel's live
	// cgroup bpf_link table. Any program backed by a bpf_link belongs to a
	// live process and is never detached (fail-closed if links cannot be read).
	links, err := deps.linkedPrograms(cgroupID)
	if err != nil {
		logger.Debug("Skipping orphaned cgroup program cleanup — cannot inspect live cgroup bpf_links",
			zap.String("cgroupPath", cgroupPath), zap.Uint64("cgroupID", cgroupID), zap.Error(err))
		return 0
	}
	legacy := candidates[:0]
	for _, c := range candidates {
		if links.isLinked(c.attachType, attachedCgroupProg{id: c.progID}) {
			continue
		}
		legacy = append(legacy, c)
	}
	if len(legacy) == 0 {
		return 0
	}

	// Post-query ownership re-check (happens-before barrier): every live agent
	// creates its keploy_cg_owner map BEFORE attaching cgroup programs, so if
	// any candidate queried above belongs to an agent that started concurrently
	// after our pre-check, its owner map is guaranteed to be visible now.
	otherOwner, err = deps.hasOtherOwner(cgroupID, excludeOwnerIDs)
	if err != nil {
		logger.Debug("Skipping orphaned cgroup program detach — cannot re-verify live cgroup owners",
			zap.String("cgroupPath", cgroupPath), zap.Uint64("cgroupID", cgroupID), zap.Error(err))
		return 0
	}
	if otherOwner {
		logger.Debug("Skipping orphaned cgroup program detach — another live keploy instance became active for this cgroup",
			zap.String("cgroupPath", cgroupPath), zap.Uint64("cgroupID", cgroupID))
		return 0
	}
	if deps.hasOtherProc(deps.selfPID) {
		logger.Debug("Skipping orphaned cgroup program detach — another keploy process became active",
			zap.Int("selfPID", deps.selfPID))
		return 0
	}

	cleaned := 0
	for _, c := range legacy {
		logger.Info("Detaching orphaned BPF program from cgroup",
			zap.String("program", c.name),
			zap.Uint32("progID", uint32(c.progID)),
			zap.String("attachType", c.attachType.String()))

		if err := deps.detachProgram(cgroupFD, c.progID, c.attachType); err != nil {
			logger.Debug("Failed to detach orphaned BPF program; re-run with elevated privileges or check cgroup permissions and retry, otherwise remove the stale attachment manually with bpftool",
				zap.String("program", c.name),
				zap.Uint32("progID", uint32(c.progID)),
				zap.String("cgroupPath", cgroupPath),
				zap.String("attachType", c.attachType.String()),
				zap.Error(err))
			continue
		}
		cleaned++
	}

	if cleaned > 0 {
		logger.Info("Cleaned orphaned BPF cgroup programs from previous crashed run",
			zap.Int("count", cleaned))
	}

	return cleaned
}

func progNameByID(progID ebpf.ProgramID) (string, error) {
	prog, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		return "", err
	}
	defer func() { _ = prog.Close() }()
	info, err := prog.Info()
	if err != nil {
		return "", err
	}
	return info.Name, nil
}

// queryAttachedPrograms queries the cgroup for attached BPF programs of the
// given attach type. Returns program IDs together with link IDs when the
// kernel reports them.
func queryAttachedPrograms(cgroupFD uintptr, attachType ebpf.AttachType) ([]attachedCgroupProg, error) {
	qr, err := link.QueryPrograms(link.QueryOptions{
		Target: int(cgroupFD),
		Attach: attachType,
	})
	if err != nil {
		return nil, err
	}

	result := make([]attachedCgroupProg, 0, len(qr.Programs))
	for _, p := range qr.Programs {
		lid, _ := p.LinkID()
		result = append(result, attachedCgroupProg{
			id:     p.ID,
			linkID: lid,
		})
	}
	return result, nil
}

// realLinkedPrograms enumerates live cgroup bpf_links in the kernel so that
// bpf_link-backed attachments are never mistaken for legacy BPF_PROG_ATTACH
// orphans. On kernels < 5.7 without bpf_link support (EINVAL/ENOSYS), returns
// an empty snapshot and nil error. Any permission/unexpected error is returned
// so the caller fails closed.
//
// cilium/ebpf's link.Iterator opens each link FD and exposes its type only via
// it.Link.Info() (one BPF_OBJ_GET_INFO_BY_FD syscall per link); non-cgroup
// links are skipped immediately once info.Type != link.CgroupType, ENOENT from
// concurrently closed links is ignored, and unexpected errors fail closed.
func realLinkedPrograms(cgroupID uint64) (cgroupLinkSnapshot, error) {
	snap := cgroupLinkSnapshot{
		exact:     make(map[cgroupAttachKey]bool),
		anyCgroup: make(map[ebpf.ProgramID]bool),
	}
	var it link.Iterator
	defer it.Close()
	for it.Next() {
		info, err := it.Link.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
				continue
			}
			return snap, fmt.Errorf("read bpf_link %d info: %w", it.ID, err)
		}
		if info.Type != link.CgroupType {
			continue
		}
		cg := info.Cgroup()
		if cg == nil {
			snap.anyCgroup[info.Program] = true
			continue
		}
		if cgroupID == 0 || cg.CgroupId == cgroupID {
			snap.exact[cgroupAttachKey{
				attachType: ebpf.AttachType(cg.AttachType),
				progID:     info.Program,
			}] = true
		}
	}
	if err := it.Err(); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, ebpf.ErrNotSupported) {
			return snap, nil
		}
		return snap, fmt.Errorf("iterate bpf_links: %w", err)
	}
	return snap, nil
}

// realHasOtherCgroupOwnerMap checks whether any live keploy_cg_owner BPF map
// (other than IDs in exclude) exists in the kernel for targetCgroupID (or for
// any cgroup when targetCgroupID == 0). Returns (false, nil) on ancient
// kernels lacking BPF_MAP_GET_NEXT_ID, and returns a non-nil error on
// permission errors so callers can fail closed.
func realHasOtherCgroupOwnerMap(targetCgroupID uint64, exclude map[ebpf.MapID]struct{}) (bool, error) {
	var id ebpf.MapID
	for {
		next, err := ebpf.MapGetNextID(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
				return false, nil
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, ebpf.ErrNotSupported) {
				return false, nil
			}
			return false, fmt.Errorf("iterate BPF maps: %w", err)
		}
		id = next
		if _, skip := exclude[id]; skip {
			continue
		}
		m, err := ebpf.NewMapFromID(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
				continue
			}
			return false, fmt.Errorf("open BPF map %d: %w", id, err)
		}
		matched, err := isMatchingCgroupOwnerMap(m, targetCgroupID)
		_ = m.Close()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
				continue
			}
			return false, err
		}
		if matched {
			return true, nil
		}
	}
}

func isMatchingCgroupOwnerMap(m *ebpf.Map, targetCgroupID uint64) (bool, error) {
	info, err := m.Info()
	if err != nil {
		return false, err
	}
	if info.Name != cgroupOwnerMapName ||
		info.Type != ebpf.Array ||
		info.KeySize != 4 ||
		info.ValueSize != 8 ||
		info.MaxEntries != 1 {
		return false, nil
	}
	if targetCgroupID == 0 {
		return true, nil
	}
	var ownerCgroupID uint64
	if err := m.Lookup(uint32(0), &ownerCgroupID); err != nil {
		return false, err
	}
	// ownerCgroupID == 0 indicates a concurrently starting agent between
	// ebpf.NewMap and m.Update(0, cgroupID); treat it conservatively as live.
	return ownerCgroupID == 0 || ownerCgroupID == targetCgroupID, nil
}

// detachProgramFromCgroup detaches a specific legacy program from a cgroup
// using BPF_PROG_DETACH.
func detachProgramFromCgroup(cgroupFD uintptr, progID ebpf.ProgramID, attachType ebpf.AttachType) error {
	prog, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		return fmt.Errorf("open program %d: %w", progID, err)
	}
	defer func() { _ = prog.Close() }()

	return link.RawDetachProgram(link.RawDetachProgramOptions{
		Target:  int(cgroupFD),
		Program: prog,
		Attach:  attachType,
	})
}

// CleanOrphanedBPFPins removes stale pinned BPF objects from /sys/fs/bpf/.
// Only removes keploy-specific pins. Gated on both /proc and kernel-global
// keploy_cg_owner maps so a non-hostPID container sharing host /sys/fs/bpf
// cannot yank pins belonging to a live keploy instance in another PID
// namespace.
//
// Fails closed when BPF_MAP_GET_NEXT_ID returns an error (for example, EPERM
// in a container with CAP_BPF but without CAP_SYS_ADMIN), preserving pins
// rather than risking removing a live sibling's pinned maps.
func CleanOrphanedBPFPins(logger *zap.Logger) {
	cleanOrphanedBPFPinsWithDeps(logger, os.Getpid(), hasOtherKeployProcInProcfs, func() (bool, error) {
		return realHasOtherCgroupOwnerMap(0, snapshotSelfOwnerMapIDs())
	}, nil)
}

func cleanOrphanedBPFPinsWithDeps(
	logger *zap.Logger,
	selfPID int,
	hasOtherProc func(int) bool,
	hasOtherOwner func() (bool, error),
	overridePatterns []string,
) {
	if hasOtherProc(selfPID) {
		logger.Debug("Skipping BPF pin cleanup because another keploy process is still running")
		return
	}
	otherOwner, err := hasOtherOwner()
	if err != nil {
		logger.Debug("Skipping BPF pin cleanup because live keploy ownership could not be verified",
			zap.Error(err))
		return
	}
	if otherOwner {
		logger.Debug("Skipping BPF pin cleanup because another keploy instance holds a live cgroup owner token")
		return
	}
	patterns := overridePatterns
	if patterns == nil {
		patterns = []string{
			"/sys/fs/bpf/keploy_sock*",
			"/sys/fs/bpf/keploy_capture_rb",
			"/sys/fs/bpf/keploy_redirect_proxy_map",
			"/sys/fs/bpf/keploy_target_namespace_pids",
			"/sys/fs/bpf/keploy_ssl_*",
			"/sys/fs/bpf/keploy_gotls_*",
		}
		patterns = append(patterns, ExtraBPFPinPatterns...)
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			info, statErr := os.Stat(m)
			if statErr != nil {
				continue
			}
			var removeErr error
			if info.IsDir() {
				removeErr = os.RemoveAll(m)
			} else {
				removeErr = os.Remove(m)
			}
			if removeErr != nil {
				logger.Error("failed to remove stale BPF pin during orphan cleanup; verify /sys/fs/bpf is writable, this process has CAP_SYS_ADMIN/CAP_BPF, and no other keploy instance has the pin in use, then retry startup",
					zap.String("path", m),
					zap.Error(removeErr))
				continue
			}
			logger.Debug("Removed stale BPF pin", zap.String("path", m))
		}
	}
}

// HasOtherKeployProcesses reports whether any keploy instance other than us
// is currently running — checking both /proc in the current PID namespace and
// kernel-global keploy_cg_owner maps across PID namespaces. Exported for use
// by downstream cleanup code.
//
// Fails closed (returns true) if BPF_MAP_GET_NEXT_ID cannot be queried (for
// example, EPERM under CAP_BPF without CAP_SYS_ADMIN), so callers such as
// pinned-map flush guards never wipe a live sibling's state across PID
// namespaces when ownership cannot be verified.
func HasOtherKeployProcesses(selfPID int) bool {
	return hasOtherKeployProcessesWithDeps(selfPID, hasOtherKeployProcInProcfs, func() (bool, error) {
		return realHasOtherCgroupOwnerMap(0, snapshotSelfOwnerMapIDs())
	})
}

func hasOtherKeployProcessesWithDeps(
	selfPID int,
	hasOtherProc func(int) bool,
	hasOtherOwner func() (bool, error),
) bool {
	if hasOtherProc(selfPID) {
		return true
	}
	other, err := hasOtherOwner()
	if err != nil {
		return true
	}
	return other
}

func hasOtherKeployProcInProcfs(selfPID int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // assume yes if we can't check
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		_, _ = fmt.Sscanf(e.Name(), "%d", &pid)
		if pid <= 1 || pid == selfPID {
			continue
		}
		exePath, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		exePath = strings.TrimSuffix(exePath, " (deleted)")
		exeBase := strings.ToLower(filepath.Base(exePath))
		if strings.Contains(exeBase, "keploy") {
			return true
		}
	}
	return false
}
