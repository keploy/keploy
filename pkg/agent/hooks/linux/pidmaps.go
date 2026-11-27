//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// The process tracking the hooks depend on can fail without a sound: when
// target_namespace_pids or excluded_pids is full, the kernel refuses the insert
// sys_enter_socket (or pid_filter) makes, and the process goes untracked. For
// target_namespace_pids that means its calls are not intercepted at all: they
// reach the real destination, neither recorded nor mocked. The BPF programs
// count every refused insert in pid_map_insert_failures (keploy/ebpf's
// track_pid); the agent reads it, says so at the first refusal, and answers it
// over GET /agent/pidmaps so that a mock or test session can fail on it
// (models.PIDMapRefusals).

// Slots of pid_map_insert_failures (PID_MAP_* in keploy/ebpf's
// headers/k_maps.h).
const (
	pidMapTarget   uint32 = 0
	pidMapExcluded uint32 = 1
)

// pidMapInsertFailure mirrors struct pid_map_insert_failure_t.
type pidMapInsertFailure struct {
	Failures uint64
	LastTGID uint32
	LastErr  int32
}

// pidMapCounts is one read of pid_map_insert_failures.
type pidMapCounts struct {
	target, excluded pidMapInsertFailure
	// targetCap and excludedCap are the maps' max_entries.
	targetCap, excludedCap uint32
}

func (c pidMapCounts) refusals() models.PIDMapRefusals {
	r := models.PIDMapRefusals{
		Target: c.target.Failures, Excluded: c.excluded.Failures,
		TargetCapacity: c.targetCap, ExcludedCapacity: c.excludedCap,
	}
	if r.Target > 0 {
		r.LastTargetTGID = c.target.LastTGID
		r.LastTargetErr = c.target.LastErr
	}
	if r.Excluded > 0 {
		r.LastExcludedErr = c.excluded.LastErr
	}
	return r
}

// readPIDMapCounts reads both slots of failures, the loaded
// pid_map_insert_failures; targets and excluded are the maps it counts for.
func readPIDMapCounts(failures, targets, excluded *ebpf.Map) (pidMapCounts, error) {
	var c pidMapCounts
	if failures == nil {
		return c, errors.New("pid_map_insert_failures is not loaded")
	}
	if err := failures.Lookup(pidMapTarget, &c.target); err != nil {
		return c, fmt.Errorf("read pid_map_insert_failures[target_namespace_pids]: %w", err)
	}
	if err := failures.Lookup(pidMapExcluded, &c.excluded); err != nil {
		return c, fmt.Errorf("read pid_map_insert_failures[excluded_pids]: %w", err)
	}
	if targets != nil {
		c.targetCap = targets.MaxEntries()
	}
	if excluded != nil {
		c.excludedCap = excluded.MaxEntries()
	}
	return c, nil
}

// pidMapCounts reads the loaded object's counters, under the lock unLoad
// takes to close them.
func (h *Hooks) pidMapCounts() (pidMapCounts, error) {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	return readPIDMapCounts(h.objects.PidMapInsertFailures, h.objects.TargetNamespacePids, h.objects.ExcludedPids)
}

// PIDMapRefusals counts the inserts into target_namespace_pids and
// excluded_pids the kernel has refused since Load, one for every socket() of
// a process these hooks could not track, with the size of each map.
func (h *Hooks) PIDMapRefusals(_ context.Context) (models.PIDMapRefusals, error) {
	c, err := h.pidMapCounts()
	if err != nil {
		return models.PIDMapRefusals{}, err
	}
	return c.refusals(), nil
}

// Compile-time proof that the Linux hooks count refused PID map inserts: were
// the method renamed, the agent's type assertion would stop matching and every
// session would read "unknown".
var _ agent.PIDMapReporter = (*Hooks)(nil)

// pidMapPollInterval is how often the agent reads pid_map_insert_failures. A
// session that ends reads the counts itself; this only decides how soon the
// log says so.
const pidMapPollInterval = time.Second

// watchPIDMapRefusals reads the counts every interval until ctx ends, and once
// more as it ends, and logs the first refusal into each map at WARN, once.
// Later refusals add to the count a session reads; they are not logged again,
// so that a full map, where every new process is refused, cannot flood the
// log. The WARN's refused_inserts is what had been refused by the read that
// found the first: inserts, of which one process makes one per socket().
//
// The last read is for a refusal in the final interval: a session that ends
// within a second of it, as a test command's last call often does, would
// otherwise leave no WARN. The caller closes the counters only after this
// returns.
//
// A read that fails is logged at Debug and tried again at the next tick.
func watchPIDMapRefusals(ctx context.Context, logger *zap.Logger, read func() (pidMapCounts, error), interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var warnedTarget, warnedExcluded bool
	check := func() {
		c, err := read()
		if err != nil {
			logger.Debug("failed to read the refused PID map inserts", zap.Error(err))
			return
		}
		if !warnedTarget && c.target.Failures > 0 {
			warnedTarget = true
			logger.Warn("the kernel refused a process a place in the agent's PID map target_namespace_pids"+refusedBecause(c.target.LastErr)+": that process's calls are not intercepted, so they go to the real destination, neither recorded nor mocked",
				pidMapFields("target_namespace_pids", c.target, c.targetCap)...)
		}
		if !warnedExcluded && c.excluded.Failures > 0 {
			warnedExcluded = true
			logger.Warn("the kernel refused a process a place in the agent's PID map excluded_pids"+refusedBecause(c.excluded.LastErr)+": that process started before the recording, or is a child of one that did, and its own children may be recorded or mocked as the app's",
				pidMapFields("excluded_pids", c.excluded, c.excludedCap)...)
		}
	}
	for {
		select {
		case <-ctx.Done():
			check()
			return
		case <-ticker.C:
			check()
		}
	}
}

// refusedBecause is why the kernel refused the insert, as a clause for the WARN
// ("" when the error is not known). It names the cause from the error rather
// than saying "full": from Linux 6.15 an insert can also fail on the map's lock.
func refusedBecause(lastErr int32) string {
	if c := models.PIDMapRefusalCause(lastErr); c != "" {
		return " (" + c + ")"
	}
	return ""
}

// pidMapFields describes the refusals into one map for the log.
func pidMapFields(name string, f pidMapInsertFailure, capacity uint32) []zap.Field {
	fields := []zap.Field{
		zap.String("map", name),
		zap.Uint64("refused_inserts", f.Failures),
		zap.Uint32("capacity", capacity),
		zap.Uint32("tgid", f.LastTGID),
	}
	if comm := commOfHostTGID(f.LastTGID); comm != "" {
		fields = append(fields, zap.String("comm", comm))
	}
	if f.LastErr != 0 {
		fields = append(fields, zap.String("error", unix.ErrnoName(unix.Errno(-f.LastErr))))
	}
	return append(fields, zap.String("next_step", models.PIDMapRefusalRemedy(f.LastErr)))
}

// initialPIDNSInode is the inode of the initial PID namespace
// (PROC_PID_INIT_INO), the namespace the BPF programs' TGIDs belong to.
const initialPIDNSInode = utils.InitialPIDNSInode

// commOfHostTGID is the command name of the process whose TGID, in the initial
// PID namespace, is tgid; "" when it cannot be told. It is read only by an
// agent in that namespace: elsewhere /proc numbers processes differently, and
// the same number would name another process.
func commOfHostTGID(tgid uint32) string {
	if tgid == 0 {
		return ""
	}
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil || st.Ino != initialPIDNSInode {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(tgid), 10) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
