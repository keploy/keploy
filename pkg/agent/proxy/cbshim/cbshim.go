//go:build linux && (amd64 || arm64)

package cbshim

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
)

// CBShim is the eBPF-backed channel-binding shim. One instance per
// keploy agent. The lifecycle splits into:
//
//   - PID membership: RegisterPID / UnregisterPID maintain the kernel-
//     side allowlist (target_namespace_pids). External PID sources
//     (the agent's existing /proc walker, the proxyless ringbuf-based
//     promoter, etc.) drive this — cbshim itself never decides what
//     "the app" is.
//
//   - Libpq-range tracking: RegisterLibpqRanges populates the second-
//     stage filter so the BPF probe only intercepts X509_digest calls
//     coming from libpq, not from internal libcrypto callers (whose
//     smaller md buffers would crash on a 32-byte overwrite).
//
//   - Uprobe attachment: AttachToLibcrypto opens a libcrypto file and
//     attaches the uprobe + uretprobe globally (no kernel-side PID
//     filter). Idempotent per file; one global attach scales to N
//     allowlisted PIDs at zero extra cost.
//
//   - Convenience: AttachToProcess walks /proc/<pid>/maps and chains
//     the three primitives. AttachToProcessTree extends to descendants.
//     WatchProcessTree handles late-fork workers and lazy dlopen.
//
//   - Cert hash rendezvous: RegisterMITM / RegisterReal accept the
//     two halves of a connection at different times; whichever
//     arrives second triggers Publish into the cbmap BPF map.
type CBShim struct {
	log  *zap.Logger
	objs cbshimObjects

	mu sync.Mutex
	// libpqRangesByPID mirrors libpq_ranges so we know what's
	// registered per tgid (for replacement on re-register).
	libpqRangesByPID map[uint32][]cbshimLibpqRangeVal
	// attachedLibs holds the uprobe links per libcrypto file path.
	// Attaches are idempotent — second call for the same path is a
	// no-op. The kernel patches one breakpoint per (inode, offset)
	// regardless of how many PIDs hit it.
	attachedLibs map[string][]link.Link

	pendingMu sync.Mutex
	pending   map[string]*pendingHalf
}

// pendingHalf — one half of a (mitm, real) pair while the other half
// is in flight.
type pendingHalf struct {
	mitmDER []byte
	realDER []byte
	sigAlgo x509.SignatureAlgorithm
}

// LibpqRange is the userspace-friendly form of a single libpq mapping
// range. Callers populate this from /proc/<pid>/maps walks.
type LibpqRange struct {
	Start uint64
	End   uint64
}

// MaxRangesPerPID is the BPF program's per-tgid range slot count.
// Range slices longer than this are truncated at registration time.
const MaxRangesPerPID = 4

// Counters mirrors the counters BPF array.
type Counters struct {
	TotalFires  uint64
	TGIDMatched uint64
	LibpqFires  uint64
	LookupHit   uint64
	LookupMiss  uint64
	WriteOK     uint64
	WriteFailed uint64
}

// New loads the BPF program. It does not attach any probes yet —
// callers must invoke AttachToLibcrypto (or one of the convenience
// helpers built on it) and register the relevant PIDs.
func New(log *zap.Logger) (*CBShim, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Debug("cbshim: failed to bump RLIMIT_MEMLOCK", zap.Error(err))
	}

	c := &CBShim{
		log:              log,
		libpqRangesByPID: make(map[uint32][]cbshimLibpqRangeVal),
		attachedLibs:     make(map[string][]link.Link),
		pending:          make(map[string]*pendingHalf),
	}
	if err := loadCbshimObjects(&c.objs, nil); err != nil {
		return nil, fmt.Errorf("cbshim: load BPF object: %w", err)
	}
	log.Debug("cbshim: BPF program loaded")

	// Populate the BPF-side cbshim_agent_info_map with the keploy
	// agent's PID-namespace inode. The uprobe's Stage-1 filter
	// (task_in_agent_ns in cbshim.bpf.c) reads this inode and calls
	// bpf_get_ns_current_pid_tgid to verify the firing task is in the
	// agent's PID namespace. Same pattern the OSS hooks use (see
	// keploy/ebpf headers/k_helpers.h::check_and_register_agent), but
	// cbshim-owned so we have no cross-BPF-object coupling.
	//
	// Failure here is logged but non-fatal: cbshim's BPF will reject
	// every X509_digest fire because task_in_agent_ns returns 0 when
	// keploy_agent_inode is 0, so the shim becomes a safe no-op.
	if err := c.populateAgentInfo(); err != nil {
		log.Warn("cbshim: failed to populate agent_info; shim will be a no-op",
			zap.Error(err))
	}

	return c, nil
}

// populateAgentInfo writes the keploy agent's PID-namespace inode into
// the BPF cbshim_agent_info_map. Called once at New(); the BPF program
// reads it on every uprobe slow-path classification.
func (c *CBShim) populateAgentInfo() error {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/pid", &st); err != nil {
		return fmt.Errorf("stat /proc/self/ns/pid: %w", err)
	}
	key := uint32(0)
	info := cbshimCbshimAgentInfo{KeployAgentInode: st.Ino}
	if err := c.objs.CbshimAgentInfoMap.Update(&key, &info, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update cbshim_agent_info_map: %w", err)
	}
	c.log.Info("cbshim: agent_info registered",
		zap.Uint64("pid_ns_inode", st.Ino))
	return nil
}

// Close detaches all probes and releases BPF resources.
func (c *CBShim) Close() error {
	// Diagnostic counter dump BEFORE we tear down the maps. These are
	// the single most useful breadcrumb for "did cbshim do anything?"
	// — they tell us how many X509_digest calls fired, how many passed
	// each filter stage, and how many writes landed. Log unlocked
	// (Counters takes the lock internally) before grabbing c.mu below.
	counters := c.Counters()
	c.log.Info("cbshim: counter dump at shutdown",
		zap.Uint64("total_fires", counters.TotalFires),
		zap.Uint64("tgid_matched", counters.TGIDMatched),
		zap.Uint64("libpq_fires", counters.LibpqFires),
		zap.Uint64("lookup_hit", counters.LookupHit),
		zap.Uint64("lookup_miss", counters.LookupMiss),
		zap.Uint64("write_ok", counters.WriteOK),
		zap.Uint64("write_fail", counters.WriteFailed))

	c.mu.Lock()
	defer c.mu.Unlock()

	for path, links := range c.attachedLibs {
		for _, l := range links {
			if err := l.Close(); err != nil {
				c.log.Debug("cbshim: link close failed",
					zap.String("path", path), zap.Error(err))
			}
		}
	}
	c.attachedLibs = map[string][]link.Link{}

	// Best-effort wipe of map state; the BPF Maps Close below would
	// release the kernel-side maps anyway, but explicit deletes are
	// cleaner under pinned-map scenarios that survive process exit.
	// target_namespace_pids is now BPF-populated (lazy classification
	// via task_in_agent_ns), so we don't track its entries from
	// userspace — let kernel teardown handle it.
	for tgid, ranges := range c.libpqRangesByPID {
		for i := range ranges {
			_ = c.objs.LibpqRanges.Delete(cbshimLibpqRangeKey{Pid: tgid, Idx: uint32(i)})
		}
	}
	c.libpqRangesByPID = map[uint32][]cbshimLibpqRangeVal{}

	return c.objs.Close()
}

// -----------------------------------------------------------------
// Stage 2 — libpq range tracking
// -----------------------------------------------------------------

// RegisterLibpqRanges replaces any previously-registered libpq mapping
// ranges for tgid with the given slice. At most MaxRangesPerPID
// entries are kept (extras dropped with a warning). Pass an empty
// slice or nil to clear all ranges for tgid.
//
// Called after walking /proc/<tgid>/maps and finding the libpq
// executable mappings.
func (c *CBShim) RegisterLibpqRanges(tgid uint32, ranges []LibpqRange) error {
	if c == nil {
		return errors.New("cbshim: nil instance")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop any old entries first so a shrink doesn't leave stale
	// ranges around for the BPF program to false-positive on.
	if old, ok := c.libpqRangesByPID[tgid]; ok {
		for i := range old {
			_ = c.objs.LibpqRanges.Delete(cbshimLibpqRangeKey{Pid: tgid, Idx: uint32(i)})
		}
		delete(c.libpqRangesByPID, tgid)
	}

	if len(ranges) > MaxRangesPerPID {
		c.log.Warn("cbshim: more libpq ranges than slots, truncating",
			zap.Uint32("tgid", tgid),
			zap.Int("provided", len(ranges)),
			zap.Int("kept", MaxRangesPerPID))
		ranges = ranges[:MaxRangesPerPID]
	}

	stored := make([]cbshimLibpqRangeVal, 0, len(ranges))
	for i, r := range ranges {
		k := cbshimLibpqRangeKey{Pid: tgid, Idx: uint32(i)}
		v := cbshimLibpqRangeVal{Start: r.Start, End: r.End}
		if err := c.objs.LibpqRanges.Update(k, v, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("cbshim: libpq_ranges[%d/%d] update: %w", tgid, i, err)
		}
		stored = append(stored, v)
	}
	if len(stored) > 0 {
		c.libpqRangesByPID[tgid] = stored
	}
	return nil
}

// -----------------------------------------------------------------
// Uprobe attachment (per-libcrypto-file, global)
// -----------------------------------------------------------------

// AttachToLibcrypto attaches the uprobe + uretprobe pair to
// X509_digest in the given libcrypto file globally (no kernel-side
// PID filter — cbshim's two-stage in-BPF filter handles PID scoping).
//
// Idempotent. Multiple PIDs sharing the same inode pay nothing extra
// for re-registration.
func (c *CBShim) AttachToLibcrypto(libcryptoPath string) error {
	if c == nil {
		return errors.New("cbshim: nil instance")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.attachedLibs[libcryptoPath]; ok {
		return nil
	}
	if !hasELFSymbol(libcryptoPath, "X509_digest") {
		return fmt.Errorf("cbshim: X509_digest not exported from %s", libcryptoPath)
	}
	ex, err := openExecutableLenient(libcryptoPath)
	if err != nil {
		return fmt.Errorf("cbshim: open executable %s: %w", libcryptoPath, err)
	}
	opts := &link.UprobeOptions{} // PID=0 → fire for any process

	entry, err := ex.Uprobe("X509_digest", c.objs.CbX509DigestEntry, opts)
	if err != nil {
		return fmt.Errorf("cbshim: uprobe X509_digest in %s: %w", libcryptoPath, err)
	}
	ret, err := ex.Uretprobe("X509_digest", c.objs.CbX509DigestReturn, opts)
	if err != nil {
		_ = entry.Close()
		return fmt.Errorf("cbshim: uretprobe X509_digest in %s: %w", libcryptoPath, err)
	}
	c.attachedLibs[libcryptoPath] = []link.Link{entry, ret}
	c.log.Info("cbshim: attached uprobes to libcrypto",
		zap.String("path", libcryptoPath))
	return nil
}

// -----------------------------------------------------------------
// Convenience: process-tree integration (OSS standalone path)
// -----------------------------------------------------------------

// AttachToProcess does the full per-PID setup in one call:
//   - walks /proc/<tgid>/maps
//   - calls AttachToLibcrypto for each libcrypto found
//   - RegisterLibpqRanges for the libpq mappings found
//   - RegisterPID(tgid)
//
// Idempotent. Safe to call repeatedly from a rescan loop.
//
// Enterprise integrations that already have their own PID tracker
// can skip this method and call RegisterPID + RegisterLibpqRanges +
// AttachToLibcrypto separately for finer control.
func (c *CBShim) AttachToProcess(tgid int) error {
	libcryptos, libpqs, err := scanProcessMaps(tgid)
	if err != nil {
		return fmt.Errorf("cbshim: scan /proc/%d/maps: %w", tgid, err)
	}

	// Diagnostic: log what scanProcessMaps found per-PID. Without this,
	// a missing-libpq case (selectivity filter rejects everything →
	// no hash substitution → SCRAM-PLUS fails silently) looks the same
	// in logs as a working attach.
	libpqPaths := make([]string, 0, len(libpqs))
	for _, lp := range libpqs {
		libpqPaths = append(libpqPaths, lp.path)
	}
	libcryptoPaths := make([]string, 0, len(libcryptos))
	for _, lc := range libcryptos {
		libcryptoPaths = append(libcryptoPaths, lc.path)
	}
	c.log.Info("cbshim: scanProcessMaps result",
		zap.Int("tgid", tgid),
		zap.Int("libpq_count", len(libpqs)),
		zap.Strings("libpq_paths", libpqPaths),
		zap.Int("libcrypto_count", len(libcryptos)),
		zap.Strings("libcrypto_paths", libcryptoPaths))

	// Register libpq ranges (even if empty — clears stale state).
	ranges := make([]LibpqRange, 0, len(libpqs))
	for _, lp := range libpqs {
		ranges = append(ranges, LibpqRange{Start: lp.start, End: lp.end})
	}
	if err := c.RegisterLibpqRanges(uint32(tgid), ranges); err != nil {
		return err
	}

	// Attach uprobes to every libcrypto found (idempotent).
	for _, lc := range libcryptos {
		if err := c.AttachToLibcrypto(lc.path); err != nil {
			c.log.Warn("cbshim: attach to libcrypto failed",
				zap.Int("tgid", tgid),
				zap.String("path", lc.path),
				zap.Error(err))
			continue
		}
	}

	// PID membership is now handled BPF-side via task_in_agent_ns
	// (cbshim.bpf.c). Userspace no longer needs to call RegisterPID:
	// the uprobe's first fire from this tgid lazily classifies it via
	// the agent's PID-namespace inode and self-populates the cache.
	// We still need this function for the per-PID work the kernel
	// CANNOT do — libpq mapping ranges (Stage-2 filter) and
	// libcrypto uprobe attach (per-file, but discovered per-PID).
	return nil
}

// AttachToProcessTree does a ONE-SHOT scan of rootPID + descendants
// at startup. Each PID's libcrypto files get uprobed and libpq ranges
// registered. After this returns, the BPF program handles all
// subsequent PID membership decisions via lazy namespace classification
// (see cbshim.bpf.c::task_in_agent_ns). No userspace polling loop is
// kept — the old WatchProcessTree pattern was replaced by the BPF
// namespace check because:
//
//   - PID inheritance from namespace is automatic in the kernel; we
//     no longer have to re-walk /proc every 2s to catch forks.
//   - Late-spawned workers loading already-uprobed libraries get
//     covered for free (uprobes are file-scoped, not PID-scoped).
//
// Remaining caveat: a late-spawned worker that lazy-dlopens a
// NEVER-BEFORE-SEEN libcrypto/libpq file (e.g. a wheel-bundled
// version no other process has loaded yet) is NOT covered. For the
// canonical psycopg2-binary case this is rare in practice — the
// bundled libs are loaded at Python import time, which happens during
// worker fork-exec, well before any TLS handshake. Catching
// truly-late dlopen would need a sched_process_exec ringbuf event
// driving an on-demand walk; left as future work.
func (c *CBShim) AttachToProcessTree(rootPID int) error {
	pids := []int{rootPID}
	pids = append(pids, discoverDescendantPIDs(rootPID)...)
	var firstErr error
	for _, pid := range pids {
		if err := c.AttachToProcess(pid); err != nil {
			c.log.Debug("cbshim: AttachToProcess failed (continuing)",
				zap.Int("pid", pid), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// WatchLibraryMappings periodically re-scans rootPID's descendant tree
// and calls AttachToProcess for each PID it finds, refreshing libcrypto
// uprobe attachments and libpq range registrations.
//
// PID MEMBERSHIP is NOT this loop's job — that's handled BPF-side via
// task_in_agent_ns (cbshim.bpf.c). The loop exists ONLY to catch:
//
//   - Late-fork workers (gunicorn, uwsgi, puma) that map a BUNDLED
//     libcrypto file no other process has loaded yet
//   - psycopg2-binary's lazy dlopen of libcrypto-XXXXXX.so.1.1 on
//     first connect — happens AFTER worker startup, after the initial
//     AttachToProcessTree
//
// Because uprobes attach per FILE, once a process has mapped a given
// libcrypto file AND we've attached the uprobe, all peers/children
// mapping the same file are covered for free. The loop converges
// quickly: typically 1-2 iterations to find and uprobe the bundled
// libs gunicorn workers load.
//
// Loop stops when:
//   - ctx is cancelled, or
//   - rootPID no longer exists, or
//   - 10 consecutive scans saw no new libs AND we've attached to ≥1.
func (c *CBShim) WatchLibraryMappings(ctx context.Context, rootPID int) {
	go c.watchLibraryMappingsLoop(ctx, rootPID, 2*time.Second, 150)
}

func (c *CBShim) watchLibraryMappingsLoop(ctx context.Context, rootPID int, interval time.Duration, maxIterations int) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	consecutiveQuiet := 0
	for i := 0; i < maxIterations; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/status", rootPID)); err != nil {
			c.log.Debug("cbshim: root pid gone, stopping library refresh",
				zap.Int("rootPID", rootPID), zap.Error(err))
			return
		}

		c.mu.Lock()
		beforeLibs := len(c.attachedLibs)
		c.mu.Unlock()

		if err := c.AttachToProcessTree(rootPID); err != nil {
			c.log.Debug("cbshim: library refresh AttachToProcessTree returned error",
				zap.Int("rootPID", rootPID), zap.Error(err))
		}

		c.mu.Lock()
		afterLibs := len(c.attachedLibs)
		c.mu.Unlock()

		if afterLibs > beforeLibs {
			consecutiveQuiet = 0
			c.log.Debug("cbshim: library refresh found new lib mappings",
				zap.Int("rootPID", rootPID),
				zap.Int("libs_added", afterLibs-beforeLibs))
		} else {
			consecutiveQuiet++
		}

		// Periodic counter snapshot for diagnostics.
		if i%5 == 4 {
			cnt := c.Counters()
			c.log.Info("cbshim: counter snapshot",
				zap.Int("iter", i),
				zap.Uint64("total_fires", cnt.TotalFires),
				zap.Uint64("tgid_matched", cnt.TGIDMatched),
				zap.Uint64("libpq_fires", cnt.LibpqFires),
				zap.Uint64("lookup_hit", cnt.LookupHit),
				zap.Uint64("lookup_miss", cnt.LookupMiss),
				zap.Uint64("write_ok", cnt.WriteOK),
				zap.Uint64("write_failed", cnt.WriteFailed))
		}

		if afterLibs > 0 && consecutiveQuiet >= 10 {
			c.log.Debug("cbshim: library refresh converged, stopping",
				zap.Int("rootPID", rootPID), zap.Int("libs", afterLibs))
			return
		}
	}
}

// -----------------------------------------------------------------
// Cert hash rendezvous (unchanged from previous design)
// -----------------------------------------------------------------

// RegisterMITM is called when keploy mints (or reuses) a MITM cert
// for the client-facing half of a connection.
func (c *CBShim) RegisterMITM(connID string, mitmDER []byte) {
	if c == nil || connID == "" || len(mitmDER) == 0 {
		return
	}
	c.pendingMu.Lock()
	h, ok := c.pending[connID]
	if !ok {
		h = &pendingHalf{}
		c.pending[connID] = h
	}
	h.mitmDER = mitmDER
	ready := h.realDER != nil
	if ready {
		delete(c.pending, connID)
	}
	c.pendingMu.Unlock()
	if ready {
		if err := c.Publish(h.mitmDER, h.realDER, h.sigAlgo); err != nil {
			c.log.Debug("cbshim: publish failed", zap.String("conn", connID), zap.Error(err))
		}
	}
}

// RegisterReal is called when the proxy completes the upstream TLS
// handshake and has the real server cert in hand.
func (c *CBShim) RegisterReal(connID string, realDER []byte, sigAlgo x509.SignatureAlgorithm) {
	if c == nil || connID == "" || len(realDER) == 0 {
		return
	}
	c.pendingMu.Lock()
	h, ok := c.pending[connID]
	if !ok {
		h = &pendingHalf{}
		c.pending[connID] = h
	}
	h.realDER = realDER
	h.sigAlgo = sigAlgo
	ready := h.mitmDER != nil
	if ready {
		delete(c.pending, connID)
	}
	c.pendingMu.Unlock()
	if ready {
		if err := c.Publish(h.mitmDER, h.realDER, h.sigAlgo); err != nil {
			c.log.Debug("cbshim: publish failed", zap.String("conn", connID), zap.Error(err))
		}
	}
}

// CleanupConnection drops any half-arrived pending state for connID.
func (c *CBShim) CleanupConnection(connID string) {
	if c == nil || connID == "" {
		return
	}
	c.pendingMu.Lock()
	delete(c.pending, connID)
	c.pendingMu.Unlock()
}

// Publish records a (mitmCert, realCert) pair into the cbmap.
func (c *CBShim) Publish(mitmCertDER, realCertDER []byte, sigAlgo x509.SignatureAlgorithm) error {
	if c == nil {
		return errors.New("cbshim: nil instance")
	}
	mitmHash, ok := cbindHash(mitmCertDER, sigAlgo)
	if !ok {
		return fmt.Errorf("cbshim: unsupported signature algorithm %s for cbind hashing", sigAlgo)
	}
	realHash, ok := cbindHash(realCertDER, sigAlgo)
	if !ok {
		return fmt.Errorf("cbshim: unsupported signature algorithm %s for cbind hashing", sigAlgo)
	}
	if err := c.objs.Cbmap.Update(mitmHash, realHash, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("cbshim: update cbmap: %w", err)
	}
	c.log.Debug("cbshim: published mitm→real hash pair",
		zap.String("mitm_hash", fmt.Sprintf("%x", mitmHash[:8])),
		zap.String("real_hash", fmt.Sprintf("%x", realHash[:8])))
	return nil
}

// Counters snapshots the BPF counter array.
func (c *CBShim) Counters() Counters {
	var out Counters
	if c == nil {
		return out
	}
	fields := []*uint64{
		&out.TotalFires, &out.TGIDMatched, &out.LibpqFires,
		&out.LookupHit, &out.LookupMiss,
		&out.WriteOK, &out.WriteFailed,
	}
	for i, dst := range fields {
		k := uint32(i)
		var v uint64
		if err := c.objs.Counters.Lookup(k, &v); err == nil {
			*dst = v
		}
	}
	return out
}

// -----------------------------------------------------------------
// cbindHash + /proc/maps parsing helpers
// -----------------------------------------------------------------

func cbindHash(certDER []byte, sigAlgo x509.SignatureAlgorithm) ([32]byte, bool) {
	var out [32]byte
	switch sigAlgo {
	case x509.MD5WithRSA,
		x509.SHA1WithRSA,
		x509.ECDSAWithSHA1,
		x509.DSAWithSHA1,
		x509.SHA256WithRSA,
		x509.SHA256WithRSAPSS,
		x509.ECDSAWithSHA256:
		out = sha256.Sum256(certDER)
		return out, true
	}
	return out, false
}

type procMapping struct {
	start uint64
	end   uint64
	path  string
}

func scanProcessMaps(pid int) (libcryptos, libpqs []procMapping, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		m, ok := parseMapsLine(sc.Text())
		if !ok || m.path == "" || !strings.Contains(m.path, "/") {
			continue
		}
		base := m.path[strings.LastIndex(m.path, "/")+1:]
		isLibcrypto := strings.HasPrefix(base, "libcrypto")
		isLibpq := strings.HasPrefix(base, "libpq")
		if !isLibcrypto && !isLibpq {
			continue
		}
		// Mount-namespace bridge: keploy may be in a different
		// mount ns from the target (sidecar to a container app),
		// so the literal path in the target's /proc/<pid>/maps
		// may not resolve in our own filesystem view.
		m.path = hostVisiblePath(pid, m.path)
		key := m.path + ":exec"
		if seen[key] {
			continue
		}
		seen[key] = true
		if isLibcrypto {
			libcryptos = append(libcryptos, m)
		}
		if isLibpq {
			libpqs = append(libpqs, m)
		}
	}
	return libcryptos, libpqs, sc.Err()
}

func parseMapsLine(line string) (procMapping, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return procMapping{}, false
	}
	perms := fields[1]
	if !strings.Contains(perms, "x") {
		return procMapping{}, false
	}
	dash := strings.IndexByte(fields[0], '-')
	if dash < 0 {
		return procMapping{}, false
	}
	start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
	if err != nil {
		return procMapping{}, false
	}
	end, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
	if err != nil {
		return procMapping{}, false
	}
	path := ""
	if len(fields) >= 6 {
		path = fields[5]
	}
	return procMapping{start: start, end: end, path: path}, true
}

// discoverDescendantPIDs walks /proc and returns every TGID whose
// stat field PPid eventually traces back to parentPID. Mirrors the
// enterprise sockmap_proxy.discoverDescendantPIDs helper; kept local
// to OSS so cbshim is self-contained in standalone mode.
func discoverDescendantPIDs(parentPID int) []int {
	if parentPID <= 0 {
		return nil
	}
	entries, _ := os.ReadDir("/proc")
	// Build pid → ppid map in one pass, then traverse to find
	// transitive descendants. /proc has thousands of entries on
	// busy hosts so a two-phase scan is cheaper than the naïve
	// recursive readdir.
	pidToPPID := make(map[int]int, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// Format: pid (comm) state ppid ...
		// comm can contain spaces or parens; find the LAST ')' to
		// skip it safely.
		s := string(stat)
		closeIdx := strings.LastIndex(s, ")")
		if closeIdx < 0 || closeIdx+2 >= len(s) {
			continue
		}
		parts := strings.SplitN(s[closeIdx+2:], " ", 3)
		if len(parts) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		pidToPPID[pid] = ppid
	}

	descendants := make(map[int]bool)
	for pid := range pidToPPID {
		// Walk up the chain until we hit parentPID or run out.
		for cur := pid; cur != 0; {
			ppid := pidToPPID[cur]
			if ppid == parentPID {
				descendants[pid] = true
				break
			}
			if ppid == cur || ppid == 0 {
				break
			}
			cur = ppid
		}
	}
	out := make([]int, 0, len(descendants))
	for d := range descendants {
		out = append(out, d)
	}
	return out
}
