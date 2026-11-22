package memoryguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

const (
	defaultCheckInterval = 500 * time.Millisecond
	// bandCheckInterval is how often the guard samples while recording with
	// the working set over the pause point, waiting for the runtime's next GC
	// (sample): it sees the ceiling crossed, or a GC that found the memory live,
	// within it.
	bandCheckInterval    = 50 * time.Millisecond
	reclaimCooldown      = 5 * time.Second
	pauseThresholdRatio  = 0.80
	resumeThresholdRatio = 0.70 // Lower than pause to avoid rapid toggle (hysteresis)

	// minGoMemLimitBytes floors GOMEMLIMIT. Below this the Go runtime is told to
	// hold the whole agent heap in a few tens of MiB and GCs nearly continuously
	// (see runtime/debug.SetMemoryLimit). k8s-proxy settled on the same 64 MiB
	// floor on the replay path (pkg/platform/k8s/k8s.go, minGoMemLimitBytes) for
	// this exact reason — keep the two in lockstep.
	minGoMemLimitBytes = 64 << 20 // 64 MiB

	// minPauseHeadroomBytes is the absolute memory margin the guard keeps below
	// the OOM ceiling. The percentage thresholds alone leave a margin
	// proportional to the (limit − overhead) budget, which shrinks to a
	// dangerously small ABSOLUTE number for small budgets; the guard polls every
	// 500ms and debug.FreeOSMemory cannot reclaim live heap, so a fast burst can
	// cross a thin margin between ticks and get the agent OOM-killed inside the
	// app's pod. This clamps the pause point to leave at least this much room
	// regardless of budget size. resume keeps 1.5× to preserve pause<resume
	// hysteresis ordering.
	minPauseHeadroomBytes  = 128 << 20 // 128 MiB
	minResumeHeadroomBytes = 192 << 20 // 1.5 × pause headroom

	// minViableEffLimitBytes is the smallest post-overhead budget a low-latency
	// recording will run within. Below it the band between the pause and resume
	// points is too thin for the guard to ever fall back far enough to resume — a
	// one-way pause. Set to the sum of the two headroom floors so, at the floor,
	// resume sits a full pause-headroom below pause (pause=effLimit−128,
	// resume=effLimit−192). Applies to low-latency (overhead>0) only.
	minViableEffLimitBytes = minPauseHeadroomBytes + minResumeHeadroomBytes // 320 MiB

	// softLimitLowerStep and softLimitRaiseStep are the least moves of a
	// GOMEMLIMIT derived from the budget (deriveSoftLimit) that the guard
	// installs: it lowers the limit on the next sample once the derivation is
	// softLimitLowerStep under it, and raises it once the derivation has been
	// softLimitRaiseStep over it for reclaimCooldown, so that memory Go does not
	// manage that comes and goes (socket buffers, page tables) never moves it
	// up and down sample by sample.
	softLimitLowerStep = 4 << 20  // 4 MiB
	softLimitRaiseStep = 16 << 20 // 16 MiB
	// softLimitSlack is what the derived GOMEMLIMIT leaves under the guard's
	// ceiling (deriveSoftLimit): the softLimitLowerStep the installed limit
	// may lag the derivation by, and 12 MiB more of memory Go does not manage
	// grown between two samples.
	softLimitSlack = 16 << 20 // 16 MiB

	// unlimitedCgroupBytes: a cgroup limit at or over this is no limit (cgroup
	// v1 writes "no limit" as 9223372036854771712; v2 as "max").
	unlimitedCgroupBytes = int64(1) << 52 // 4 PiB
)

// MinViableLowLatencyLimitBytes returns the smallest container memory limit (in
// bytes) a low-latency recording needs for a given fixed capture-buffer
// overhead: the overhead plus the minimum usable post-overhead budget. Callers
// that validate a memory limit before the agent starts (e.g. k8s-proxy's
// record-start check) can derive their floor from this instead of duplicating
// the guard's constants. A negative overhead is treated as zero.
func MinViableLowLatencyLimitBytes(overheadBytes int64) int64 {
	if overheadBytes < 0 {
		overheadBytes = 0
	}
	return overheadBytes + minViableEffLimitBytes
}

var recordingPaused atomic.Bool

// fixedOverheadBytes is memory the agent allocates up-front and that pausing
// recording cannot reclaim — chiefly the eBPF capture ring buffer in
// low-latency mode, which is charged to the container cgroup as kernel memory.
// The enterprise ringbuf loader sets this to the ACTUAL configured buffer size
// (record.ringbufSizeMB), never a hardcoded constant. It is zero in proxy mode.
// The guard discounts it from both the usage and the limit so the pressure
// signal tracks only the growing capture buffers — the memory a pause can
// actually free — budgeted against the room left above the fixed floor.
var fixedOverheadBytes atomic.Int64

// SetFixedOverheadMB records the fixed, non-reclaimable memory overhead (in MB)
// the guard should discount when evaluating memory pressure, and returns the
// number of bytes actually applied so the caller can log it (this setter is the
// only thing that turns the discount on, so a silent no-op is indistinguishable
// from working correctly). Safe to call before or after Start; the running
// guard picks up the new value on its next tick (the ring buffer is allocated
// after the guard has already started). Callers MUST pass the actual configured
// buffer size (e.g. the enterprise low-latency capture ring buffer's
// record.ringbufSizeMB), not a hardcoded value. A non-positive or implausibly
// large value clears the discount (returns 0) rather than overflowing — the
// proxy-mode default.
func SetFixedOverheadMB(mb int) int64 {
	var b int64
	if mb > 0 && int64(mb) <= math.MaxInt64/(1024*1024) {
		b = int64(mb) * 1024 * 1024
	}
	fixedOverheadBytes.Store(b)
	return b
}

// FixedOverheadBytes returns the currently configured fixed overhead in bytes.
// Exposed for diagnostics and tests.
func FixedOverheadBytes() int64 {
	return fixedOverheadBytes.Load()
}

type guard struct {
	logger            *zap.Logger
	memoryCurrentPath string
	limitBytes        int64
	memoryLimitMB     uint64
	lastReclaim       time.Time
	underPressure     bool
	readFailCount     int
	prevMemLimit      int64

	// limitPath is the container cgroup's memory limit file: memory.max
	// (v2) or memory.limit_in_bytes (v1), beside memoryCurrentPath.
	limitPath string

	// readCgroup, readGoMemory, reclaim and setMemoryLimit default to
	// readCgroupSample, readGoMemoryStats, debug.FreeOSMemory and
	// debug.SetMemoryLimit; clock and wait default to time.Now and a timer
	// (run). Rows replace them.
	readCgroup     func() (cgroupSample, error)
	readGoMemory   func() goMemory
	reclaim        func()
	setMemoryLimit func(int64) int64
	clock          func() time.Time
	wait           func(time.Duration) <-chan time.Time
	// overLogged: the crossing of the pause point that did not pause has
	// been logged; cleared once the working set is back under it.
	overLogged bool
	// pausedAt is when recording last paused.
	pausedAt time.Time

	// The GOMEMLIMIT installed (applySoftLimit): its bytes, when it last
	// changed, whether it was derived from the budget, and whether the
	// budget has been reported too small for the agent (once each way).
	softLimit        int64
	softLimitAt      time.Time
	softLimitDerived bool
	tooSmall         bool
	// Where no limit is derived (deriveSoftLimit: memory.stat or the
	// runtime's figures unreadable, or a budget too small to record), the
	// budget's 90% is installed on the first such sample (goMemAligned) and
	// again only when the ring buffer's size (lastOverhead) changes.
	goMemAligned bool
	lastOverhead int64
	// One-time log latches, so a persistent misconfiguration is reported
	// once, not on every sample.
	goMemFlooredLogged, nonViableLogged, containerUnderFlagLogged bool
}

// goMemory is what the Go runtime reports of its own memory (runtime/metrics,
// which never stops the world). The runtime keeps garbage in check itself:
// GOGC pacing and GOMEMLIMIT set when its next collection is due, and every
// collection, its own or forced, measures the live heap anew.
type goMemory struct {
	objects int64  // /memory/classes/heap/objects: live objects and dead ones no GC has freed yet
	free    int64  // /memory/classes/heap/free: free heap still backed by memory
	live    int64  // /gc/heap/live: what the last completed GC marked live
	goal    int64  // /gc/heap/goal: the heap size by which the runtime's next GC is due
	cycles  uint64 // /gc/cycles/total: completed GC cycles
	mapped  int64  // /memory/classes/total less /memory/classes/heap/released: what GOMEMLIMIT limits
	ok      bool   // false: unreadable
}

var goMemorySampleNames = [...]string{
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/free:bytes",
	"/gc/heap/live:bytes",
	"/gc/heap/goal:bytes",
	"/gc/cycles/total:gc-cycles",
	"/memory/classes/total:bytes",
	"/memory/classes/heap/released:bytes",
}

// cgroupSample is one reading of the container's cgroup.
type cgroupSample struct {
	workingSet int64 // memory.current less inactive_file, as kubectl top reports it
	activeFile int64 // active_file: page cache, which the kernel reclaims before it OOM-kills
	statOK     bool  // memory.stat was read: activeFile is known
	limit      int64 // the cgroup's memory limit; 0: none (or unreadable)
}

// readGoMemoryStats reads goMemory from runtime/metrics; any figure the
// runtime does not report leaves it not ok.
func readGoMemoryStats() goMemory {
	var s [len(goMemorySampleNames)]metrics.Sample
	for i, name := range goMemorySampleNames {
		s[i].Name = name
	}
	metrics.Read(s[:])
	var v [len(goMemorySampleNames)]uint64
	for i := range s {
		if s[i].Value.Kind() != metrics.KindUint64 {
			return goMemory{}
		}
		v[i] = s[i].Value.Uint64()
	}
	bytes := func(u uint64) int64 { return int64(min(u, math.MaxInt64)) }
	return goMemory{objects: bytes(v[0]), free: bytes(v[1]), live: bytes(v[2]), goal: bytes(v[3]), cycles: v[4],
		mapped: bytes(v[5] - min(v[6], v[5])), ok: true}
}

// unmarked is the heap no GC has found live: the last GC's garbage and
// everything allocated since it. Before the first GC nothing has been found
// live, so it is 0.
func (m goMemory) unmarked() int64 {
	if m.cycles > 0 && m.objects > m.live {
		return m.objects - m.live
	}
	return 0
}

// heldAndNext judges a working-set sample by what the Go runtime reports of
// its heap:
//   - held is the working set less what no collection has found live: the heap
//     objects beyond what the last GC marked live (its garbage, and everything
//     allocated since it) and the free heap. What is left is what the last GC
//     found live plus all the memory a collection cannot free: Go's own
//     non-heap memory, heap fragmentation, and memory Go does not manage. Every
//     GC, the runtime's own or forced, brings it up to date.
//   - next is the memory the kernel cannot reclaim (the working set less its
//     active page cache, activeFile) by the time the runtime's next GC is due:
//     the heap grows into its free memory first, then on to its goal.
//
// Before the first GC nothing has been found live, so every object counts as
// held. Unreadable figures leave the working set alone, as the guard judged
// it before (held = next = working set).
func heldAndNext(workingSet, activeFile int64, m goMemory) (held, next int64) {
	if !m.ok {
		return workingSet, workingSet
	}
	held = max(workingSet-m.unmarked()-m.free, 0)
	next = max(workingSet-activeFile, 0) + max(m.goal-m.objects-m.free, 0)
	return held, next
}

// softLimit is the GOMEMLIMIT for one sample (deriveSoftLimit) and the sizes
// it comes from.
type softLimit struct {
	bytes   int64 // the limit to install
	derived bool  // from the budget; false: goMemTarget, 90% of the budget
	hard    int64 // the budget: --memory-limit, or the container's limit where that is lower
	nonGo   int64 // memory in the container the Go runtime does not manage, page cache aside
	room    int64 // hard less nonGo less the margin: what Go may use
	need    int64 // Go memory no collection can free now
	// floorBytes is need plus minGoMemLimitBytes: the least soft limit, so
	// that the GC has at least 64 MiB to allocate between its collections.
	floorBytes int64
	// short: the room is under floorBytes and under 90% of the budget: the
	// memory Go does not manage, not the budget's 90%, leaves Go less than
	// its floor, and the limit installed is over the room.
	short bool
	// source and raise: where hard came from and what the lines advise
	// raising (limitSource).
	source, raise string
}

// deriveSoftLimit is the GOMEMLIMIT for one sample: the room the budget
// leaves Go, so that the runtime's own GC collects its garbage before the
// budget:
//
//	room = budget - memory Go does not manage - margin
//
// Its terms:
//   - The budget is the one the guard pauses against (budget, dec.limit):
//     --memory-limit, or the container's cgroup limit (memory.max, or
//     memory.limit_in_bytes on cgroup v1) where that is lower. A
//     container with no memory limit (an
//     agent container docker starts) leaves it --memory-limit: 90% of that,
//     blind to the memory Go does not manage, would let the runtime's next
//     GC fall due over the ceiling on garbage alone, since a GC counts live
//     all it allocates while it marks (more, the longer a loaded host keeps
//     it marking) and GOGC lets the heap grow to twice what it marked; the
//     guard would then pause recording on garbage.
//   - Memory Go does not manage is measured, every sample: the working set less
//     its active page cache (the kernel reclaims page cache before it
//     OOM-kills; inactive page cache is already out of the working set) less
//     what the Go runtime has mapped and not released (what GOMEMLIMIT
//     limits). It is the capture ring buffer, BPF maps, cgo and other
//     processes' memory, socket buffers. Pages Go has mapped but not yet
//     touched make it read low, by the little the runtime has mapped ahead.
//   - The margin is the room the guard's ceiling leaves under the budget (a
//     quarter of what the pause point leaves: 5% of the budget where the pause
//     point is at 80%, 48 MiB on a 1 GiB sidecar with a 64 MiB ring buffer;
//     32 MiB where the 128 MiB headroom floor binds) plus softLimitSlack (16
//     MiB): 64 MiB on that sidecar. With Go at its soft limit, the memory the
//     kernel cannot reclaim is then due 16 MiB, and the 3% of the heap goal
//     the runtime keeps under its soft limit, under the ceiling, so the
//     guard's ceiling rule (sample) does not fire on a heap the soft limit holds
//     unless the memory Go does not manage has grown more than that since the
//     last sample.
//
// The limit is never over 90% of the budget (goMemTarget, the limit where
// none is derived): where that already leaves room under the budget, a higher
// soft limit would only let the runtime keep more garbage, and take the
// working set nearer the budget. Under that, it is never under the Go memory no
// collection can free (need: the last GC's live heap and Go's own memory
// outside the heap) plus minGoMemLimitBytes (floorBytes): a soft limit at
// what Go holds makes the GC run back to back. The floor is over the room
// only when Go's live memory is within 64 MiB of it: with the budget at
// --memory-limit, that is past the pause point on every low-latency budget
// and every proxy-mode one of 534 MiB or more, so recording pauses there.
// Where the room is under the floor (short), the floor is installed, up to
// 90% of the budget, and applySoftLimit reports the budget too small if
// pausing recording does not bring Go's live memory back under it.
//
// With no memory.stat, unreadable runtime figures, or a budget too small to
// record, it is goMemTarget.
func deriveSoftLimit(dec memoryDecision, cs cgroupSample, m goMemory) softLimit {
	s := softLimit{bytes: dec.goMemTarget}
	if !cs.statOK || !m.ok || !dec.viable {
		return s
	}
	s.derived = true
	s.hard, s.source, s.raise = dec.limit, dec.source, dec.raise
	s.nonGo = max(cs.workingSet-cs.activeFile-m.mapped, 0)
	margin := (dec.effLimit-dec.pauseThreshold)/4 + softLimitSlack
	s.room = s.hard - s.nonGo - margin
	s.need = max(m.mapped-m.free-m.unmarked(), 0)
	s.floorBytes = s.need + minGoMemLimitBytes
	s.bytes = min(max(s.room, s.floorBytes), dec.goMemTarget)
	s.short = s.room < s.floorBytes && s.room < dec.goMemTarget
	return s
}

func (g *guard) cgroup() (cgroupSample, error) {
	if g.readCgroup != nil {
		return g.readCgroup()
	}
	return readCgroupSample(g.memoryCurrentPath, g.limitPath)
}

func (g *guard) installMemoryLimit(b int64) {
	if g.setMemoryLimit != nil {
		g.setMemoryLimit(b)
		return
	}
	debug.SetMemoryLimit(b)
}

func (g *guard) goMemory() goMemory {
	if g.readGoMemory != nil {
		return g.readGoMemory()
	}
	return readGoMemoryStats()
}

func (g *guard) freeOSMemory() {
	if g.reclaim != nil {
		g.reclaim()
		return
	}
	debug.FreeOSMemory()
}

type cgroupLayout struct {
	version    int
	mountPoint string
	mountRoot  string
	controller string
	usageFile  string
}

const (
	cgroupV1 = 1
	cgroupV2 = 2
)

// LimitBytes validates the configured memory limit in MB and converts it to bytes.
func LimitBytes(limitMB uint64) (int64, error) {
	if limitMB == 0 {
		return 0, nil
	}
	if limitMB > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("memory limit %dMB is too large", limitMB)
	}
	return int64(limitMB) * 1024 * 1024, nil
}

// goMemoryBudget is GoMemoryBudget's figure, as the guard's last sample (or
// Start) found it; 0 while no guard runs.
var goMemoryBudget atomic.Int64

// GoMemoryBudget returns the Go memory budget the guard holds the agent to:
// 90% of its memory budget (--memory-limit, or the container's limit where
// that is lower) left after the capture ring buffer, floored at 64 MiB: the
// GOMEMLIMIT the guard sets where it cannot measure the memory Go does not
// manage. It is 0 when no guard runs. Where it can, GOMEMLIMIT is fitted
// lower, to the room the budget leaves Go once that memory is measured
// (deriveSoftLimit). A reader that sizes a buffer as a
// share of the agent's memory, such as the enterprise agent's capture
// budget, sizes it from this, so that the share is what it was before
// GOMEMLIMIT was fitted: on a 1 GiB sidecar with a 64 MiB ring buffer,
// 864 MiB where GOMEMLIMIT is 708. A reader sized against the Go heap's own
// limit (pkg/matcher/learn's set budget) keeps reading GOMEMLIMIT.
func GoMemoryBudget() int64 {
	return goMemoryBudget.Load()
}

// IsRecordingPaused reports whether the agent is currently dropping captured
// tests and mocks due to memory pressure.
func IsRecordingPaused() bool {
	return recordingPaused.Load()
}

// SetRecordingPausedForTest sets what IsRecordingPaused reports, as memory
// pressure does, without telling the mock managers, and returns what puts the
// previous value back. Tests only.
func SetRecordingPausedForTest(paused bool) (restore func()) {
	pauseEdgeMu.Lock()
	defer pauseEdgeMu.Unlock()
	was := recordingPaused.Swap(paused)
	notePauseEdgeLocked(paused)
	return func() {
		pauseEdgeMu.Lock()
		defer pauseEdgeMu.Unlock()
		recordingPaused.Store(was)
		notePauseEdgeLocked(was)
	}
}

// SetGoMemoryBudgetForTest sets what GoMemoryBudget reports, as a running
// guard does, and returns what puts the previous value back. Tests only.
func SetGoMemoryBudgetForTest(b int64) (restore func()) {
	was := goMemoryBudget.Swap(b)
	return func() { goMemoryBudget.Store(was) }
}

// Start enables the memory guard when a Docker agent is running with a limit.
func Start(ctx context.Context, logger *zap.Logger, isDocker bool, memoryLimitMB uint64) error {
	resetAllPressure()
	// The debug knob that forces a pause by request count, read whether or
	// not the guard itself runs (ForcePauseEnv).
	startForcedPause(logger)

	limitBytes, err := LimitBytes(memoryLimitMB)
	if err != nil {
		return err
	}
	if limitBytes == 0 || !isDocker {
		goMemoryBudget.Store(0)
		return nil
	}

	memoryCurrentPath, layout, err := resolveMemoryUsagePath("/proc/mounts", "/proc/self/cgroup", "/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("failed to resolve container memory usage path: %w", err)
	}

	// Inform the Go runtime about the memory constraint so the GC becomes
	// more aggressive well before the cgroup hard-limit is hit.  We use
	// 90% of the container limit because the remaining 10% is needed for
	// non-Go memory (kernel buffers, page cache, OS overhead).  Without
	// this headroom the GC won't kick in until the cgroup is already at
	// the OOM boundary, causing connection drops and I/O disruptions.
	// Floored (see minGoMemLimitBytes) so a small limit can't drive the GC
	// nearly continuous; run() realigns this once the overhead is known, and
	// sets it from the budget less the memory Go does not manage, as
	// measured (deriveSoftLimit).
	goMemLimit := int64(float64(limitBytes) * 0.9)
	if goMemLimit < minGoMemLimitBytes {
		goMemLimit = minGoMemLimitBytes
	}
	prevMemLimit := debug.SetMemoryLimit(goMemLimit)

	g := &guard{
		logger:            logger,
		memoryCurrentPath: memoryCurrentPath,
		limitPath:         limitPathFor(memoryCurrentPath, layout.version),
		limitBytes:        limitBytes,
		memoryLimitMB:     memoryLimitMB,
		prevMemLimit:      prevMemLimit,
		softLimit:         goMemLimit,
	}

	logger.Info("Enabled keploy-agent memory guard",
		zap.Uint64("memory_limit_mb", g.memoryLimitMB))
	logger.Debug("Memory guard cgroup details",
		zap.Int("cgroup_version", layout.version),
		zap.String("memory_current_path", g.memoryCurrentPath))
	g.sampleSoftLimit()

	go g.run(ctx)
	return nil
}

func (g *guard) run(ctx context.Context) {
	defer goMemoryBudget.Store(0)
	defer g.resetPressure()
	defer debug.SetMemoryLimit(g.prevMemLimit)

	wait := g.wait
	if wait == nil {
		timer := time.NewTimer(defaultCheckInterval)
		defer timer.Stop()
		wait = func(d time.Duration) <-chan time.Time {
			timer.Reset(d)
			return timer.C
		}
	}
	for next, more := defaultCheckInterval, true; more; {
		select {
		case <-ctx.Done():
			return
		case <-wait(next):
			next, more = g.sample()
		}
	}
}

// timeNow is the guard's clock: time.Now unless a row sets clock.
func (g *guard) timeNow() time.Time {
	if g.clock != nil {
		return g.clock()
	}
	return time.Now()
}

// sample takes one sample of the container's and the runtime's memory and
// acts on it: GOMEMLIMIT, and pausing or resuming recording. It returns how
// long run waits for the next one, and false once the guard gives up after
// persistent read failures.
func (g *guard) sample() (next time.Duration, more bool) {
	now := g.timeNow()
	// The runtime's figures first: memory Go allocates between the two
	// reads then makes the memory Go does not manage read high
	// (deriveSoftLimit), never low.
	m := g.goMemory()
	// readCgroupSample subtracts inactive_file from memory.current so
	// that page-cache does not count toward the pressure threshold.  This
	// matches what kubectl top / Kubernetes eviction logic uses and avoids
	// false-positive pauses caused by reclaimable cache pages.
	cs, err := g.cgroup()
	if err != nil {
		g.readFailCount++
		if g.readFailCount == 1 {
			g.logger.Debug("failed to read keploy-agent memory usage; "+
				"ensure /sys/fs/cgroup is mounted in the container or set --memory-limit=0 to disable",
				zap.String("path", g.memoryCurrentPath),
				zap.Error(err))
		}
		// After ~10s of consecutive failures, disable the guard entirely
		if g.readFailCount >= 20 {
			g.logger.Info("Disabling memory guard after persistent read failures",
				zap.Int("consecutive_failures", g.readFailCount))
			g.resetPressure()
			return 0, false
		}
		return defaultCheckInterval, true
	}
	g.readFailCount = 0
	currentBytes := cs.workingSet

	// Discount the fixed, non-reclaimable overhead (the low-latency eBPF
	// ring buffer) from BOTH usage and limit, then pause when the
	// reclaimable remainder fills the ratio of the room left above the
	// floor — but never closer than an absolute headroom to the ceiling.
	// See evaluateMemory. In proxy mode overhead is 0, so this reduces to
	// the previous working-set-vs-limit behaviour. The limit is the
	// container's where that is under --memory-limit (budget).
	dec := g.decide(cs)
	goMemoryBudget.Store(dec.goMemTarget)

	// GOMEMLIMIT: the room the budget leaves Go once the memory Go does
	// not manage is measured (deriveSoftLimit, applySoftLimit), so that the
	// runtime collects its garbage before the budget, whether or not the
	// container has a memory limit. Where that cannot be measured, as
	// before: realign it with the Go-heap budget (the room left after the
	// non-Go fixed overhead), recomputing only when the overhead changes so
	// we don't thrash debug.SetMemoryLimit every tick. Floored (see
	// evaluateMemory) so a small budget can't drive near-continuous GC.
	if sl := deriveSoftLimit(dec, cs, m); sl.derived {
		g.applySoftLimit(sl, now)
		g.goMemAligned = false
	} else if !g.goMemAligned || dec.overhead != g.lastOverhead {
		g.installMemoryLimit(dec.goMemTarget)
		g.softLimit, g.softLimitDerived = dec.goMemTarget, false
		g.goMemAligned, g.lastOverhead = true, dec.overhead
	}
	// Re-log if a later change re-floors GOMEMLIMIT; reset the latch when
	// it is no longer floored so the two states stay in sync.
	if dec.goMemFloored {
		if !g.goMemFlooredLogged {
			g.logger.Warn("keploy-agent GOMEMLIMIT floored — the heap budget left "+
				"after the capture ring buffer is very small; raise "+dec.raise+
				" or lower record.ringbufSizeMB",
				zap.Int64("go_mem_limit_bytes", g.softLimit),
				zap.Int64("effective_limit_bytes", dec.effLimit),
				zap.Int64("fixed_overhead_bytes", dec.overhead),
				zap.Int64("limit_bytes", dec.limit),
				zap.String("limit_source", dec.source))
			g.goMemFlooredLogged = true
		}
	} else {
		g.goMemFlooredLogged = false
	}

	// Non-viable budget (low-latency only): the fixed ring buffer leaves
	// too little room above it for a usable pause/resume band, so there is
	// no way to record without courting an OOM-kill. Hold paused and say so
	// loudly ONCE — silently falling back to raw guarding would reintroduce
	// the startup-pause bug this discount exists to fix. Proxy mode
	// (overhead 0) is never non-viable, so this and its ring-buffer message
	// only fire when a ring buffer is actually present. The real fix is
	// upstream (reject the record-start); this is the in-agent backstop.
	if !dec.viable {
		if !g.nonViableLogged {
			g.logger.Error("capture ring buffer leaves too little memory below the "+
				"limit for a usable recording budget; recording cannot proceed — "+
				"raise "+dec.raise+" or lower record.ringbufSizeMB",
				zap.Int64("fixed_overhead_bytes", dec.overhead),
				zap.Int64("effective_limit_bytes", dec.effLimit),
				zap.Int64("limit_bytes", dec.limit),
				zap.String("limit_source", dec.source),
				zap.Int64("min_viable_budget_bytes", int64(minViableEffLimitBytes)))
			g.nonViableLogged = true
		}
		g.enterPressure(now, dec)
		return defaultCheckInterval, true
	}
	g.nonViableLogged = false

	// Garbage is not pressure. Between its collections the Go runtime
	// lets garbage build up (GOGC pacing; at a recorded app's exec the
	// agent allocates about 1.5 GB in a second, and a GC there started
	// at 630 MB with 163 MB live), and it keeps freed heap until its
	// scavenger returns it, so the working set crosses the pause point
	// on memory the runtime is about to collect. The guard forces no
	// collection to find out: it judges what the runtime's own last GC
	// found live plus all memory a collection cannot free (held), and
	// pauses when that reaches the pause point. A natural GC refreshes
	// it, and one is due by the heap goal; so when the working set is
	// over the pause point and the next GC is not due until the memory
	// the kernel cannot reclaim reaches the ceiling (next), waiting for
	// it would let memory that may be live come within a quarter of the
	// pause point's headroom of the limit unchecked, and the guard
	// pauses on the working set as before. The soft limit derived from
	// the budget has the runtime's GC due under the ceiling
	// (deriveSoftLimit), so that pause is left to where it cannot hold:
	// no memory.stat, a GC that runs long while live memory grows past
	// its goal, a budget too small for Go's live memory, or memory Go
	// does not manage grown since the last sample. While it
	// waits, the guard samples every bandCheckInterval.
	held, nextGC := heldAndNext(currentBytes, cs.activeFile, m)
	heldEff, nextEff := max(held-dec.overhead, 0), max(nextGC-dec.overhead, 0)
	if heldEff >= dec.pauseThreshold || (dec.effCurrent >= dec.pauseThreshold && nextEff >= dec.ceiling) {
		g.enterPressure(now, dec,
			zap.Int64("held_memory_bytes", heldEff),
			zap.Int64("working_set_at_next_gc_bytes", nextEff),
			zap.Int64("ceiling_bytes", dec.ceiling))
		return defaultCheckInterval, true
	}

	if dec.effCurrent >= dec.pauseThreshold {
		if !g.overLogged && !g.underPressure {
			g.logger.Debug("memory guard: the working set is over the pause point, but what the "+
				"runtime's last GC found live is not, and its next GC is due under the ceiling; recording goes on",
				zap.Int64("memory_usage_bytes", dec.effCurrent),
				zap.Int64("held_memory_bytes", heldEff),
				zap.Int64("working_set_at_next_gc_bytes", nextEff),
				zap.Int64("pause_threshold_bytes", dec.pauseThreshold),
				zap.Int64("ceiling_bytes", dec.ceiling))
			g.overLogged = true
		}
	} else {
		g.overLogged = false
	}

	// Resume once held memory is back at the resume point, and the
	// working set too unless the next GC is due under the ceiling.
	if g.underPressure && heldEff <= dec.resumeThreshold &&
		(dec.effCurrent <= dec.resumeThreshold || nextEff < dec.ceiling) {
		g.resetPressure()
		g.logger.Info("Cleared keploy-agent memory pressure after memory recovered",
			zap.Int64("memory_usage_bytes", dec.effCurrent),
			zap.Int64("held_memory_bytes", heldEff),
			zap.Int64("resume_threshold_bytes", dec.resumeThreshold),
			zap.Int64("fixed_overhead_bytes", dec.overhead),
			zap.Int64("effective_limit_bytes", dec.effLimit))
	}

	// Recording with the working set over the pause point, the sample that
	// resumed included: the guard is waiting for the runtime's next GC.
	if dec.effCurrent >= dec.pauseThreshold && !g.underPressure {
		return bandCheckInterval, true
	}
	return defaultCheckInterval, true
}

// enterPressure pauses recording. On pausing, and every reclaimCooldown while
// paused, it collects (FreeOSMemory), so that the buffers the pause drops are
// returned and the next sample judges up-to-date figures. It is the guard's
// only collection: it never collects to decide whether to pause. Its line
// names the limit dec judged against, where that came from and what to
// raise (limitSource).
func (g *guard) enterPressure(now time.Time, dec memoryDecision, why ...zap.Field) {
	alreadyPaused := g.underPressure
	g.underPressure = true
	applyPausedState(true)

	if !alreadyPaused {
		g.pausedAt = now
		g.logger.Info("Pausing keploy-agent recording due to memory pressure. "+
			"Consider increasing "+dec.raise+", enabling sampling, or reducing request concurrency",
			append([]zap.Field{
				zap.Int64("memory_usage_bytes", dec.effCurrent),
				zap.Int64("pause_threshold_bytes", dec.pauseThreshold),
				zap.Int64("fixed_overhead_bytes", dec.overhead),
				zap.Int64("effective_limit_bytes", dec.effLimit),
				zap.Int64("limit_bytes", dec.limit),
				zap.String("limit_source", dec.source)}, why...)...)
	}
	if !alreadyPaused || now.Sub(g.lastReclaim) >= reclaimCooldown {
		g.lastReclaim = now
		g.freeOSMemory()
	}
}

// sampleSoftLimit takes one sample and installs the GOMEMLIMIT derived from
// it (applySoftLimit), where it can be derived: Start calls it so that the
// derived limit is in force from the start, not from run's first sample.
func (g *guard) sampleSoftLimit() {
	m := g.goMemory()
	cs, err := g.cgroup()
	if err != nil {
		return
	}
	dec := g.decide(cs)
	goMemoryBudget.Store(dec.goMemTarget)
	if sl := deriveSoftLimit(dec, cs, m); sl.derived {
		g.applySoftLimit(sl, g.timeNow())
	}
}

// budget is the memory limit the guard pauses, resumes and sets its ceiling
// against: --memory-limit, or the container's limit where that is lower, so
// that the guard can pause before the container's limit, not only before
// one the container never lets the agent reach. The first sample that finds
// the container's limit under --memory-limit says so once, until it is not.
func (g *guard) budget(cs cgroupSample) int64 {
	if cs.limit <= 0 || cs.limit >= g.limitBytes {
		g.containerUnderFlagLogged = false
		return g.limitBytes
	}
	if !g.containerUnderFlagLogged {
		g.containerUnderFlagLogged = true
		g.logger.Warn("the container's memory limit is under --memory-limit; the keploy-agent memory guard "+
			"pauses recording against the container's limit",
			zap.Int64("container_limit_bytes", cs.limit),
			zap.Int64("memory_limit_bytes", g.limitBytes))
	}
	return cs.limit
}

// decide is evaluateMemory of the working set against the budget (budget),
// with where the budget came from (limitSource).
func (g *guard) decide(cs cgroupSample) memoryDecision {
	dec := evaluateMemory(cs.workingSet, g.budget(cs), fixedOverheadBytes.Load())
	dec.source, dec.raise = g.limitSource(dec.limit, cs.limit)
	return dec
}

// limitSource is where a budget the guard judged against (budget) came
// from, as the lines that report it name it, and the limit those lines
// advise raising, given the container's limit as read (0: none):
//   - the container's, where that is under --memory-limit: raising
//     --memory-limit then changes nothing;
//   - both, where the container's limit is --memory-limit to the MiB
//     (--memory-limit's unit), as in every sidecar k8s-proxy injects (it
//     sets the two equal): raising either alone leaves the other binding,
//     and the budget where it was;
//   - --memory-limit, where the container has no limit or one a MiB or more
//     over it.
func (g *guard) limitSource(limit, containerLimit int64) (source, raise string) {
	switch {
	case limit < g.limitBytes:
		return "container", "the container's memory limit"
	case containerLimit > 0 && containerLimit < g.limitBytes+1<<20:
		return "--memory-limit and container", "--memory-limit and the container's memory limit"
	}
	return "--memory-limit", "--memory-limit"
}

// applySoftLimit installs a GOMEMLIMIT derived from the budget
// (deriveSoftLimit) over the one installed (Start's 90% of --memory-limit
// at first): lower once the derivation is softLimitLowerStep under it, higher
// at once where Go's live memory with its floor is over it, and otherwise
// higher only once the derivation has been softLimitRaiseStep over it
// reclaimCooldown after the last change.
//
// It reports the budget too small for the agent once, and once again when
// it has room: too small only where Go's live memory with its floor is over
// the room the budget leaves Go (short) with recording paused, after the
// guard's collection reclaimCooldown into the pause. Until then, live memory
// over the room may be recording's own, which a pause drops: capture
// buffers, at the GC that first finds a burst live, are as live as the rest.
func (g *guard) applySoftLimit(s softLimit, now time.Time) {
	first := !g.softLimitDerived
	g.softLimitDerived = true
	changed := s.bytes <= g.softLimit-softLimitLowerStep || (s.bytes > g.softLimit && s.floorBytes > g.softLimit) ||
		(s.bytes >= g.softLimit+softLimitRaiseStep && now.Sub(g.softLimitAt) >= reclaimCooldown)
	if changed {
		g.installMemoryLimit(s.bytes)
		g.softLimit, g.softLimitAt = s.bytes, now
	}
	if first || changed {
		log := g.logger.Debug
		if first {
			log = g.logger.Info
		}
		log("keploy-agent GOMEMLIMIT set from the memory limit",
			zap.Int64("go_mem_limit_bytes", g.softLimit),
			zap.Int64("go_room_bytes", s.room),
			zap.Int64("limit_bytes", s.hard),
			zap.String("limit_source", s.source),
			zap.Int64("non_go_memory_bytes", s.nonGo),
			zap.Int64("go_live_memory_bytes", s.need))
	}
	collectedWhilePaused := g.underPressure && g.lastReclaim.Sub(g.pausedAt) >= reclaimCooldown
	switch {
	case s.short && collectedWhilePaused && !g.tooSmall:
		g.tooSmall = true
		g.logger.Error("the memory limit is too small for the keploy-agent: with recording paused "+
			"and its memory collected, the agent's live Go memory plus 64 MiB is over the room the limit leaves Go, "+
			"so GOMEMLIMIT is set over that room; raise "+s.raise,
			zap.Int64("limit_bytes", s.hard),
			zap.String("limit_source", s.source),
			zap.Int64("non_go_memory_bytes", s.nonGo),
			zap.Int64("go_room_bytes", s.room),
			zap.Int64("go_live_memory_bytes", s.need),
			zap.Int64("go_mem_limit_bytes", g.softLimit))
	case g.tooSmall && s.room >= s.floorBytes+softLimitRaiseStep:
		g.tooSmall = false
		g.logger.Info("the memory limit has room for the keploy-agent's live Go memory plus 64 MiB again",
			zap.Int64("limit_bytes", s.hard),
			zap.String("limit_source", s.source),
			zap.Int64("non_go_memory_bytes", s.nonGo),
			zap.Int64("go_room_bytes", s.room),
			zap.Int64("go_live_memory_bytes", s.need),
			zap.Int64("go_mem_limit_bytes", g.softLimit))
	}
}

func (g *guard) resetPressure() {
	g.underPressure = false
	applyPausedState(false)
}

func resetAllPressure() {
	applyPausedState(false)
}

// pressureHooks are additional sinks for the memory-pressure signal,
// registered via RegisterPressureHook. The decision to pause is global
// (pod-level cgroup memory), but the buffered mocks that consume that memory
// can live in many sync-mock managers — e.g. the multi-app agent runs one
// manager per app and the package-global Get() manager is then unused. A
// composer registers a hook that fans the pressure out to all its live
// managers so the relief actually reaches the buffers.
var (
	pressureHookMu  sync.RWMutex
	pressureHooks   = map[uint64]func(paused bool){}
	pressureHookSeq uint64
)

// RegisterPressureHook adds fn to the set invoked by applyPausedState
// alongside the package-global manager and returns an unregister func that
// removes it again. A multi-app composer that registers one hook per app/
// session MUST call the returned func when that session ends, otherwise the
// hook — and everything its closure captures (the session's SyncMockManager
// and its buffers) — is pinned for the life of the process and re-invoked on
// every pressure transition. The returned func is idempotent and safe to call
// from any goroutine; calling it more than once is a no-op. Safe for
// concurrent use.
func RegisterPressureHook(fn func(paused bool)) (unregister func()) {
	if fn == nil {
		return func() {}
	}
	pressureHookMu.Lock()
	pressureHookSeq++
	id := pressureHookSeq
	pressureHooks[id] = fn
	pressureHookMu.Unlock()
	return func() {
		pressureHookMu.Lock()
		delete(pressureHooks, id)
		pressureHookMu.Unlock()
	}
}

func applyPausedState(paused bool) {
	// The flag and the request count read at its change move together, so
	// each open PauseTally sees the pauses in the order the ingress does.
	pauseEdgeMu.Lock()
	recordingPaused.Store(paused)
	notePauseEdgeLocked(paused)
	pauseEdgeMu.Unlock()
	// Global manager: the buffering manager in single-app mode.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(paused)
	}
	// Fan out to registered managers (multi-app: one per app). The global
	// trigger stays global; only the action reaches every live buffer.
	pressureHookMu.RLock()
	hooks := make([]func(paused bool), 0, len(pressureHooks))
	for _, fn := range pressureHooks {
		hooks = append(hooks, fn)
	}
	pressureHookMu.RUnlock()
	for _, fn := range hooks {
		fn(paused)
	}
}

// readMemoryCurrent reads the raw cgroup memory.current value (total bytes
// including page cache).  Callers that need the working-set should use
// readCgroupSample instead.
func readMemoryCurrent(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return 0, fmt.Errorf("empty cgroup memory usage file: %s", path)
	}
	currentBytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cgroup memory usage file %s: %w", path, err)
	}
	return currentBytes, nil
}

// readPageCache returns the cgroup's inactive_file and active_file from the
// memory.stat beside memCurrentPath (both cgroup versions name them so);
// ok is false when memory.stat cannot be read.
func readPageCache(memCurrentPath string) (inactiveFile, activeFile int64, ok bool) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(memCurrentPath), "memory.stat"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || (name != "inactive_file" && name != "active_file") {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if name == "inactive_file" {
			inactiveFile = n
		} else {
			activeFile = n
		}
	}
	return inactiveFile, activeFile, true
}

// readCgroupSample reads the container's working set:
//
//	working_set = memory.current − inactive_file
//
// This matches what kubectl top and the Kubernetes eviction manager report,
// and avoids false-positive pressure events caused by reclaimable page cache.
// If memory.stat is unavailable the raw memory.current value is the working
// set, so the guard still functions (conservatively), and statOK is false.
//
// It also reads the active page cache, and the memory limit from limitPath: a
// byte count, "max" (cgroup v2: none), or a count at or over
// unlimitedCgroupBytes (cgroup v1's "none") reads as no limit (0), as does an
// unreadable file.
func readCgroupSample(memCurrentPath, limitPath string) (cgroupSample, error) {
	currentBytes, err := readMemoryCurrent(memCurrentPath)
	if err != nil {
		return cgroupSample{}, err
	}
	inactiveFile, activeFile, statOK := readPageCache(memCurrentPath)
	s := cgroupSample{workingSet: max(currentBytes-inactiveFile, 0), activeFile: activeFile, statOK: statOK}
	if data, err := os.ReadFile(limitPath); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && n > 0 && n < unlimitedCgroupBytes {
			s.limit = n
		}
	}
	return s, nil
}

// limitPathFor is the memory limit file beside a cgroup's usage file:
// memory.limit_in_bytes on cgroup v1, memory.max on v2.
func limitPathFor(memCurrentPath string, version int) string {
	name := "memory.max"
	if version == cgroupV1 {
		name = "memory.limit_in_bytes"
	}
	return filepath.Join(filepath.Dir(memCurrentPath), name)
}

func resolveMemoryUsagePath(procMountsPath, procSelfCgroupPath, procMountInfoPath string) (string, cgroupLayout, error) {
	layouts, err := detectCgroupLayouts(procMountsPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}

	// Primary path: derive the container's cgroup from /proc/self/cgroup.
	// This works on Linux (including Linux kind clusters) where cgroup namespace
	// isolation is in effect and /proc/self/cgroup reports the container-relative
	// path rather than "/".
	for _, layout := range layouts {
		candidate, err := resolveFromSelfCgroup(layout, procSelfCgroupPath)
		if err == nil {
			return candidate, layout, nil
		}
	}

	// Secondary path: walk every cgroup.procs file under the mount point and
	// find the scope that actually owns this process (matched by its
	// root-namespace PID read from NSpid in /proc/self/status). This is
	// unambiguous — exactly one cgroup contains our PID — so we prefer it
	// over identifier matching, which can fuzzy-match sibling container
	// scopes when multiple pod cgroups are visible (kind, nested Docker).
	var resolutionErrs []string
	for _, layout := range layouts {
		candidate, err := findCgroupByOwnPID(layout)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, fmt.Sprintf("pid-walk: %v", err))
	}

	// Tertiary path: scan identifiers extracted from the environment, hostname,
	// /proc/self/cgroup, and /proc/self/mountinfo, then walk the cgroup tree
	// looking for a scope directory whose name contains one of those identifiers.
	// Used only when PID-based resolution fails (e.g. environments where
	// /proc/self/status NSpid is hidden or cgroup.procs is not readable).
	identifiers, err := collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}
	for _, layout := range layouts {
		candidate, err := findMemoryUsagePathByIdentifier(layout, identifiers)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, err.Error())
	}

	return "", cgroupLayout{}, fmt.Errorf("no container-specific cgroup memory file found (%s)", strings.Join(resolutionErrs, "; "))
}

func detectCgroupLayouts(procMountsPath, procMountInfoPath string) ([]cgroupLayout, error) {
	mountRoots, err := readMountRoots(procMountInfoPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(procMountsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var layouts []cgroupLayout
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		mountPoint := fields[1]
		fsType := fields[2]
		options := strings.Split(fields[3], ",")

		switch {
		case fsType == "cgroup" && hasController(options, "memory"):
			key := "v1:" + mountPoint
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			layouts = append(layouts, cgroupLayout{
				version:    cgroupV1,
				mountPoint: mountPoint,
				mountRoot:  mountRoots[mountPoint],
				controller: "memory",
				usageFile:  "memory.usage_in_bytes",
			})
		case fsType == "cgroup2":
			key := "v2:" + mountPoint
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			layouts = append(layouts, cgroupLayout{
				version:    cgroupV2,
				mountPoint: mountPoint,
				mountRoot:  mountRoots[mountPoint],
				usageFile:  "memory.current",
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(layouts) == 0 {
		return nil, fmt.Errorf("no cgroup v1 memory or cgroup v2 mounts found")
	}

	sort.SliceStable(layouts, func(i, j int) bool {
		if layouts[i].version == layouts[j].version {
			return layouts[i].mountPoint < layouts[j].mountPoint
		}
		return layouts[i].version < layouts[j].version
	})
	return layouts, nil
}

func readSelfCgroupPath(procSelfCgroupPath string, layout cgroupLayout) (string, error) {
	data, err := os.ReadFile(procSelfCgroupPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch layout.version {
		case cgroupV2:
			if parts[1] == "" {
				if parts[2] == "" {
					return "/", nil
				}
				return parts[2], nil
			}
		case cgroupV1:
			if hasController(strings.Split(parts[1], ","), layout.controller) {
				if parts[2] == "" {
					return "/", nil
				}
				return parts[2], nil
			}
		}
	}
	return "", fmt.Errorf("container cgroup path not found in %s for cgroup v%d", procSelfCgroupPath, layout.version)
}

func readMountRoots(procMountInfoPath string) (map[string]string, error) {
	data, err := os.ReadFile(procMountInfoPath)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		roots[fields[4]] = fields[3]
	}
	return roots, nil
}

func collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath string) ([]string, error) {
	candidates := make(map[string]struct{})

	for _, value := range []string{os.Getenv("HOSTNAME")} {
		for _, id := range extractHexIdentifiers(value) {
			candidates[id] = struct{}{}
		}
	}
	if hostname, err := os.Hostname(); err == nil {
		for _, id := range extractHexIdentifiers(hostname) {
			candidates[id] = struct{}{}
		}
	}
	for _, path := range []string{procSelfCgroupPath, procMountInfoPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, id := range extractHexIdentifiers(string(data)) {
			candidates[id] = struct{}{}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("failed to derive container identifier for cgroup lookup")
	}

	identifiers := make([]string, 0, len(candidates))
	for id := range candidates {
		identifiers = append(identifiers, id)
	}
	sort.Slice(identifiers, func(i, j int) bool {
		if len(identifiers[i]) == len(identifiers[j]) {
			return identifiers[i] < identifiers[j]
		}
		return len(identifiers[i]) > len(identifiers[j])
	})
	return identifiers, nil
}

func extractHexIdentifiers(value string) []string {
	re := regexp.MustCompile(`(?i)[a-f0-9]{12,64}`)
	matches := re.FindAllString(strings.ToLower(value), -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		result = append(result, match)
	}
	return result
}

// resolveFromSelfCgroup derives the container's cgroup memory file path by
// reading /proc/self/cgroup.  On Linux with proper cgroup namespace isolation
// this reports the container-relative path (e.g.
// /kubepods/burstable/pod.../cri-containerd-<id>) and is the fastest,
// most reliable resolution method.
//
// When the resolved path is the root ("/"), the mounted candidate may point
// at an ancestor cgroup rather than the container's own: macOS Docker Desktop
// exposes the VM's root cgroup, and kind nodes expose the node-level tree.
// For the root case we therefore require proof that the candidate cgroup
// actually owns this process (its cgroup.procs contains our PID); otherwise
// the caller falls through to PID-based or identifier-based resolution.
// If cgroup.procs cannot be read (e.g. unit tests with a fake tree), the
// candidate is accepted optimistically to preserve previous behavior.
func resolveFromSelfCgroup(layout cgroupLayout, procSelfCgroupPath string) (string, error) {
	cgroupPath, err := readSelfCgroupPath(procSelfCgroupPath, layout)
	if err != nil {
		return "", err
	}

	candidate, ok := buildMountedCgroupPath(layout.mountPoint, layout.mountRoot, cgroupPath, layout.usageFile)
	if !ok {
		return "", fmt.Errorf("unable to map cgroup path %q to mount %q", cgroupPath, layout.mountPoint)
	}
	if !fileExists(candidate) {
		return "", fmt.Errorf("cgroup memory file %q not found", candidate)
	}

	if cgroupPath == "/" || cgroupPath == "" {
		if owns, checked := cgroupOwnsSelf(filepath.Dir(candidate)); checked && !owns {
			return "", fmt.Errorf("cgroup %q is not container-specific (self not listed in cgroup.procs)", filepath.Dir(candidate))
		}
	}

	return candidate, nil
}

// cgroupOwnsSelf reports whether the cgroup at cgroupDir owns this process.
// The second return value indicates whether we could actually read
// cgroup.procs — callers use it to distinguish "verified not ours" from
// "unable to verify" so missing-file environments (e.g. unit tests) don't
// incorrectly fail resolution.
func cgroupOwnsSelf(cgroupDir string) (owns, checked bool) {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.procs"))
	if err != nil {
		return false, false
	}
	pid, err := getRootNSPID()
	if err != nil {
		return false, false
	}
	pidStr := strconv.Itoa(pid)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == pidStr {
			return true, true
		}
	}
	return false, true
}

func buildMountedCgroupPath(mountPoint, mountRoot, cgroupPath, usageFile string) (string, bool) {
	cleanMountRoot := filepath.Clean(mountRoot)
	if cleanMountRoot == "." || cleanMountRoot == "" {
		cleanMountRoot = "/"
	}
	cleanCgroupPath := filepath.Clean(cgroupPath)
	if cleanCgroupPath == "." || cleanCgroupPath == "" {
		cleanCgroupPath = "/"
	}

	var relativePath string
	switch {
	case cleanMountRoot == "/" && cleanCgroupPath == "/":
		relativePath = ""
	case cleanMountRoot == "/":
		relativePath = strings.TrimPrefix(cleanCgroupPath, "/")
	case cleanCgroupPath == cleanMountRoot:
		relativePath = ""
	case strings.HasPrefix(cleanCgroupPath, cleanMountRoot+"/"):
		relativePath = strings.TrimPrefix(cleanCgroupPath, cleanMountRoot+"/")
	default:
		return "", false
	}

	if relativePath == "" {
		return filepath.Join(mountPoint, usageFile), true
	}
	return filepath.Join(mountPoint, relativePath, usageFile), true
}

func findMemoryUsagePathByIdentifier(layout cgroupLayout, identifiers []string) (string, error) {
	const maxWalkDepth = 8 // bound the search to avoid slow walks over large cgroup trees

	type match struct {
		path  string
		idLen int
		depth int
	}

	mountDepth := strings.Count(filepath.Clean(layout.mountPoint), string(os.PathSeparator))
	best := match{}

	err := filepath.WalkDir(layout.mountPoint, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		currentDepth := strings.Count(path, string(os.PathSeparator)) - mountDepth
		if d.IsDir() && currentDepth > maxWalkDepth {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != layout.usageFile {
			return nil
		}
		dir := filepath.Dir(path)
		if dir == layout.mountPoint {
			return nil
		}
		dirLower := strings.ToLower(dir)
		for _, identifier := range identifiers {
			if !strings.Contains(dirLower, identifier) {
				continue
			}
			depth := strings.Count(dirLower, string(os.PathSeparator))
			if len(identifier) > best.idLen || (len(identifier) == best.idLen && depth > best.depth) {
				best = match{
					path:  path,
					idLen: len(identifier),
					depth: depth,
				}
			}
			break
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if best.path == "" {
		return "", fmt.Errorf("failed to find container-specific %s under %s", layout.usageFile, layout.mountPoint)
	}
	return best.path, nil
}

// getRootNSPID returns this process's PID in the root (initial) PID namespace
// by reading the NSpid field from /proc/self/status.  NSpid lists namespaces
// innermost-first, so the last entry is the outermost / host-level PID — the
// one written into cgroup.procs files by the container runtime.
func getRootNSPID() (int, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		// Last field = outermost (root) namespace PID
		return strconv.Atoi(fields[len(fields)-1])
	}
	return 0, fmt.Errorf("NSpid field not found in /proc/self/status")
}

// findCgroupByOwnPID walks the cgroup tree under layout.mountPoint looking for
// the leaf scope whose cgroup.procs file contains this process's root-namespace
// PID.  It is used as a last-resort fallback for environments where
// /proc/self/cgroup reports "/" and no container identifier can be found in
// mountinfo — specifically macOS + Docker Desktop + kind clusters where the
// cgroup namespace is not propagated into kind-node containers.
//
// This function is never reached on a standard Linux setup (the primary
// resolveFromSelfCgroup path succeeds there), so it has zero impact on the
// existing Linux behaviour.
func findCgroupByOwnPID(layout cgroupLayout) (string, error) {
	rootPID, err := getRootNSPID()
	if err != nil {
		return "", fmt.Errorf("could not determine root namespace PID: %w", err)
	}
	pidStr := strconv.Itoa(rootPID)

	const maxWalkDepth = 8
	mountDepth := strings.Count(filepath.Clean(layout.mountPoint), string(os.PathSeparator))

	var foundPath string
	// sentinelErr signals a successful early-exit from WalkDir.
	sentinelErr := errors.New("found")

	walkErr := filepath.WalkDir(layout.mountPoint, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't abort the walk
		}
		currentDepth := strings.Count(path, string(os.PathSeparator)) - mountDepth
		if d.IsDir() && currentDepth > maxWalkDepth {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(line) != pidStr {
				continue
			}
			candidate := filepath.Join(filepath.Dir(path), layout.usageFile)
			if fileExists(candidate) {
				foundPath = candidate
				return sentinelErr // signal early exit
			}
		}
		return nil
	})

	// WalkDir returns sentinelErr when we found the path — that is success.
	if walkErr != nil && !errors.Is(walkErr, sentinelErr) {
		return "", walkErr
	}
	if foundPath == "" {
		return "", fmt.Errorf("no cgroup scope found containing PID %d (root NS) under %s", rootPID, layout.mountPoint)
	}
	return foundPath, nil
}

func hasController(controllers []string, target string) bool {
	for _, controller := range controllers {
		if controller == target {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func thresholdBytes(limit int64, ratio float64) int64 {
	if limit <= 0 {
		return 0
	}
	threshold := int64(float64(limit) * ratio)
	if threshold == 0 {
		return limit
	}
	return threshold
}

// memoryDecision is the pure result of evaluating one memory sample against the
// limit and the fixed overhead. run() applies it (pause/resume, GOMEMLIMIT,
// logging); keeping the arithmetic here makes it unit-testable without a cgroup.
type memoryDecision struct {
	limit           int64 // the limit judged against (guard.budget)
	effCurrent      int64 // reclaimable usage = working set − overhead, floored at 0
	effLimit        int64 // budget for reclaimable memory = limit − overhead
	overhead        int64 // sanitised overhead actually applied
	pauseThreshold  int64 // pause when effCurrent ≥ this (viable only)
	resumeThreshold int64 // resume when effCurrent ≤ this (viable only)
	ceiling         int64 // pause at the pause point when the next GC is due only at this (viable only)
	goMemTarget     int64 // GOMEMLIMIT to install (floored at minGoMemLimitBytes)
	goMemFloored    bool  // goMemTarget hit its floor
	viable          bool  // false ⇒ budget too small to record; hold paused
	// source and raise: where limit came from and what the lines advise
	// raising (guard.limitSource); set by guard.decide, empty from
	// evaluateMemory alone.
	source, raise string
}

// evaluateMemory discounts the fixed, non-reclaimable overhead (the low-latency
// eBPF ring buffer, charged to the cgroup as kernel memory) from both the
// working-set usage and the limit — pausing cannot reclaim it, so the guard
// budgets only the reclaimable remainder against the room above the floor.
//
// Two safety rules on top of the percentage thresholds:
//   - GOMEMLIMIT is floored at minGoMemLimitBytes so a tiny budget can't drive
//     the runtime into near-continuous GC.
//   - The pause/resume points never leave less than an absolute headroom below
//     the ceiling (minPause/minResumeHeadroomBytes); the percentage margin alone
//     shrinks to a dangerously small ABSOLUTE number for small budgets, and the
//     500ms poll + FreeOSMemory-can't-reclaim-live-heap means a thin margin gets
//     the agent OOM-killed between ticks. If the floor leaves less than the
//     resume headroom, the budget is non-viable and the guard holds paused.
//
// In proxy mode overhead is 0, so effLimit == limit and the ratios apply as
// before (the absolute floor only binds for small budgets). A negative overhead
// means "unset/bad" and is treated as no discount.
func evaluateMemory(currentBytes, limitBytes, overhead int64) memoryDecision {
	if overhead < 0 {
		overhead = 0
	}
	effLimit := limitBytes - overhead
	effCurrent := currentBytes - overhead
	if effCurrent < 0 {
		effCurrent = 0
	}
	// effCurrent is computed even on the non-viable path so the pause log carries
	// the real usage, not a zero.
	d := memoryDecision{limit: limitBytes, overhead: overhead, effLimit: effLimit, effCurrent: effCurrent}

	// GOMEMLIMIT floored so a tiny budget can't drive near-continuous GC. effLimit
	// may be negative here when overhead > limit; 0.9×negative is still below the
	// floor, so it lands on the floor and is safe.
	goTarget := int64(float64(effLimit) * 0.9)
	if goTarget < minGoMemLimitBytes {
		goTarget = minGoMemLimitBytes
		d.goMemFloored = true
	}
	d.goMemTarget = goTarget

	// Proxy mode (overhead == 0): there is no fixed floor to compensate for, so
	// guard the raw working set against the plain ratios exactly as before — no
	// absolute headroom, no non-viability. Those rules exist only to stop a large
	// ring buffer from eating the low-latency budget; imposing them on proxy mode
	// would wrongly declare any small --memory-limit unrecordable.
	if overhead == 0 {
		d.viable = true
		d.pauseThreshold = thresholdBytes(effLimit, pauseThresholdRatio)
		d.resumeThreshold = thresholdBytes(effLimit, resumeThresholdRatio)
		d.ceiling = ceilingBytes(effLimit, d.pauseThreshold)
		return d
	}

	// Low-latency: the ring buffer is a fixed floor. Require a usable budget above
	// it — pause and resume must be separated by a real band — else hold paused
	// and report the config as too small (also catches overhead ≥ limit ⇒
	// effLimit ≤ 0).
	if effLimit < minViableEffLimitBytes {
		d.viable = false
		return d
	}
	d.viable = true
	pauseHeadroom := max(effLimit-thresholdBytes(effLimit, pauseThresholdRatio), int64(minPauseHeadroomBytes))
	resumeHeadroom := max(effLimit-thresholdBytes(effLimit, resumeThresholdRatio), int64(minResumeHeadroomBytes))
	d.pauseThreshold = effLimit - pauseHeadroom
	d.resumeThreshold = effLimit - resumeHeadroom
	d.ceiling = ceilingBytes(effLimit, d.pauseThreshold)
	return d
}

// ceilingBytes is how high the working set may be due to reach by the
// runtime's next GC while the guard waits for that GC to tell its garbage
// from live memory: a quarter of the room the pause point leaves under the
// budget stays free (5% of the budget where the pause point is at 80%, 32 MiB
// where the 128 MiB headroom floor binds). Garbage measured at a recorded
// app's exec reached 88% of a 1 GiB sidecar's budget.
func ceilingBytes(effLimit, pauseThreshold int64) int64 {
	return effLimit - (effLimit-pauseThreshold)/4
}
