package models

import (
	"errors"
	"fmt"
	"strings"
)

// PIDMapRefusals counts the inserts into the agent's per-process BPF maps that
// the kernel refused, since the agent loaded its hooks. Both counts only ever
// grow; a session's own are what they grew by while it ran (Since).
//
// The agent learns which processes it may intercept by adding each one that
// calls socket() to a BPF map, target_namespace_pids, or to excluded_pids when
// it predates the recording. Both maps have a fixed size; the agent removes a
// process when it exits, so they fill only with more LIVE socket-using
// processes than they hold, in the agent's PID namespace (for a native agent,
// the whole host). Then the kernel refuses the insert:
//   - Target: a refused process is not intercepted. Its calls reach the real
//     destination: in record nothing is recorded, in replay nothing is mocked,
//     and a call that a live dependency answers passes.
//   - Excluded: a process that should be left alone (it predates the
//     recording, or is a child of one that does) is not excluded, so its
//     children, which are excluded through it, are recorded or mocked as the
//     app's.
//
// Either way what the session captured is not what the run did:
// "capture_incomplete". GET /agent/pidmaps answers it.
//
// The counts are of refused INSERTS, not of processes. The agent tries again
// at every socket() of a process a map has no room for, so that the process
// gets its place once another exits, and every try the kernel refuses counts:
// one untracked process that opens fifty sockets counts fifty. A count above
// zero means at least one process went untracked; it does not say how many.
type PIDMapRefusals struct {
	// Target counts refused inserts into target_namespace_pids.
	Target uint64 `json:"target"`
	// Excluded counts refused inserts into excluded_pids.
	Excluded uint64 `json:"excluded"`
	// LastTargetTGID is the TGID (in the initial PID namespace) of the latest
	// process refused a place in target_namespace_pids; 0 when none was.
	// Only an example: two refused at once may leave either.
	LastTargetTGID uint32 `json:"lastTargetTgid,omitempty"`
	// TargetCapacity and ExcludedCapacity are how many processes each map
	// holds (its max_entries); 0 when the agent did not say.
	TargetCapacity   uint32 `json:"targetCapacity,omitempty"`
	ExcludedCapacity uint32 `json:"excludedCapacity,omitempty"`
	// LastTargetErr and LastExcludedErr are the kernel's (negative, Linux)
	// errno for the latest refusal into each map; 0 when none was refused or
	// the agent did not say (an older agent). They say why: a full map
	// (-E2BIG) is not the only cause -- from Linux 6.15 an insert can also fail
	// on the map's bucket lock -- so a report must not claim "full" without it.
	LastTargetErr   int32 `json:"lastTargetErr,omitempty"`
	LastExcludedErr int32 `json:"lastExcludedErr,omitempty"`
}

// ErrPIDMapRefusalsUnsupported reports that an agent cannot say whether the
// kernel refused it any process: it predates GET /agent/pidmaps, or its hooks
// do not count. That is "unknown", never "none refused".
var ErrPIDMapRefusalsUnsupported = errors.New("agent cannot report refused inserts into its PID maps")

// Any reports whether any insert was refused.
func (r PIDMapRefusals) Any() bool {
	return r.Target > 0 || r.Excluded > 0
}

// Since is what r grew by after base, an earlier read of the same agent. A
// count lower than base's, which only an agent that started again since base
// can give, is taken whole: that agent's every refusal is the session's.
func (r PIDMapRefusals) Since(base PIDMapRefusals) PIDMapRefusals {
	d := r
	if r.Target >= base.Target {
		d.Target = r.Target - base.Target
	}
	if r.Excluded >= base.Excluded {
		d.Excluded = r.Excluded - base.Excluded
	}
	if d.Target == 0 {
		d.LastTargetTGID = 0
		d.LastTargetErr = 0
	}
	if d.Excluded == 0 {
		d.LastExcludedErr = 0
	}
	return d
}

// CaptureIncomplete says, for a session whose refusals r are, what its capture
// is missing; "" when nothing was refused. It is the verdict's reason, so it
// names what the user lost first and what to do about it last. It gives the
// counts as what they are, refused inserts: "at least one process", never a
// number of processes.
func (r PIDMapRefusals) CaptureIncomplete() string {
	var lost []string
	if r.Target > 0 {
		lost = append(lost, fmt.Sprintf("the agent's PID map target_namespace_pids%s refused %d insert(s)%s, so at least one process was not intercepted: its calls were neither recorded nor mocked and went to the real destination", pidMapHolds(r.TargetCapacity), r.Target, pidMapBecause(r.LastTargetErr)))
	}
	if r.Excluded > 0 {
		lost = append(lost, fmt.Sprintf("the agent's PID map excluded_pids%s refused %d insert(s)%s, so at least one process that predates the run, or a child of one, was not excluded: calls of its children may have been captured as the app's", pidMapHolds(r.ExcludedCapacity), r.Excluded, pidMapBecause(r.LastExcludedErr)))
	}
	if len(lost) == 0 {
		return ""
	}
	remedy := pidMapRemedy
	if !pidMapMaybeFull(r) {
		remedy = pidMapLockRemedy
	}
	return "capture incomplete: " + strings.Join(lost, "; and ") + ". " + remedy
}

// Linux errno values: the agent's kernel reports these, whatever the CLI runs
// on, so they are spelled out rather than taken from this platform's syscall
// package.
const (
	linuxE2BIG     = 7
	linuxENOMEM    = 12
	linuxEBUSY     = 16
	linuxEDEADLK   = 35
	linuxETIMEDOUT = 110
)

// PIDMapRefusalCause says why the kernel refused an insert, from its
// (negative, Linux) errno; "" when that is not known. It does not claim a full
// map for an error that is not -E2BIG, and even -E2BIG is only "usually" full:
// from 6.15 a contended free list returns it too.
func PIDMapRefusalCause(err int32) string {
	switch -err {
	case 0:
		return ""
	case linuxE2BIG:
		return "the map had no room for it (E2BIG; usually it is full)"
	case linuxENOMEM:
		return "an allocation failed (ENOMEM)"
	case linuxETIMEDOUT:
		return "its bucket lock could not be taken (ETIMEDOUT)"
	case linuxEDEADLK:
		return "its bucket lock could not be taken (EDEADLK)"
	case linuxEBUSY:
		return "its bucket lock could not be taken (EBUSY)"
	default:
		return fmt.Sprintf("the kernel returned error %d", -err)
	}
}

// pidMapBecause is PIDMapRefusalCause as a reason clause, or "".
func pidMapBecause(err int32) string {
	if c := PIDMapRefusalCause(err); c != "" {
		return " (" + c + ")"
	}
	return ""
}

// pidMapMaybeFull reports whether any refusal in r may be a full map: its error
// is -E2BIG, or not known (an older agent), so the full-map remedy applies.
func pidMapMaybeFull(r PIDMapRefusals) bool {
	full := func(n uint64, err int32) bool { return n > 0 && (err == 0 || err == -linuxE2BIG) }
	return full(r.Target, r.LastTargetErr) || full(r.Excluded, r.LastExcludedErr)
}

// CaptureUnverified says, for a session whose refusals could not be read
// although the agent counts them (err, which is never
// ErrPIDMapRefusalsUnsupported), what that leaves unproven. Not a refusal: a
// read that failed proves nothing either way, so it is "unverified", which
// --strict fails and nothing else may take for "complete".
func CaptureUnverified(err error) string {
	return fmt.Sprintf("capture unverified: keploy could not read whether the agent tracked every process (%v), so it cannot prove that no call went to the real destination unrecorded or unmocked. %s", err, pidMapUnreadableRemedy)
}

// pidMapUnreadableRemedy is what a user can do when the count is unreadable.
const pidMapUnreadableRemedy = "Check the agent's log for why its count could not be read, and for a WARN that its PID map was full"

// pidMapHolds names a map's size where the agent gave it.
func pidMapHolds(capacity uint32) string {
	if capacity == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d entries)", capacity)
}

// pidMapRemedy is what a user can do when a PID map is full.
const pidMapRemedy = "A map keeps one entry for each live process that called socket() in the agent's PID namespace (for a native agent, the whole host): run with fewer live processes (stop what else runs on the machine, or run the app in Docker so the agent sees only its container)"

// pidMapLockRemedy is what a user can do when an insert failed on the map's
// bucket lock rather than for want of room: that is CPU contention on the
// agent's host (Linux 6.15+), not too many processes.
const pidMapLockRemedy = "The kernel could not take the map's lock, which happens under heavy CPU contention on the agent's host (Linux 6.15 and later): run again on a less loaded machine"
