package models

import (
	"errors"
	"strings"
	"testing"
)

// A session's refusals are what the agent's counts grew by while it ran; an
// agent that started again since the first read gives lower counts, all of
// which are the session's.
func TestPIDMapRefusalsSince(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base, now PIDMapRefusals
		want      PIDMapRefusals
	}{
		{"no growth", PIDMapRefusals{Target: 4, Excluded: 1, LastTargetTGID: 9}, PIDMapRefusals{Target: 4, Excluded: 1, LastTargetTGID: 9}, PIDMapRefusals{}},
		{"growth", PIDMapRefusals{Target: 4}, PIDMapRefusals{Target: 6, Excluded: 2, LastTargetTGID: 11}, PIDMapRefusals{Target: 2, Excluded: 2, LastTargetTGID: 11}},
		{"a restarted agent", PIDMapRefusals{Target: 4, Excluded: 3}, PIDMapRefusals{Target: 1, LastTargetTGID: 5}, PIDMapRefusals{Target: 1, LastTargetTGID: 5}},
		{"the maps' sizes are the later read's", PIDMapRefusals{Target: 4}, PIDMapRefusals{Target: 5, LastTargetTGID: 7, TargetCapacity: 10240, ExcludedCapacity: 10240}, PIDMapRefusals{Target: 1, LastTargetTGID: 7, TargetCapacity: 10240, ExcludedCapacity: 10240}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.now.Since(tc.base)
			if got != tc.want {
				t.Fatalf("%+v.Since(%+v) = %+v, want %+v", tc.now, tc.base, got, tc.want)
			}
			if got.Any() != (tc.want.Target > 0 || tc.want.Excluded > 0) {
				t.Fatalf("Any() = %v for %+v", got.Any(), got)
			}
		})
	}
}

// A count that could not be read is not a refusal and not "none": the reason
// says what is unproven, why, and where to look, and never claims a loss.
func TestPIDMapCaptureUnverified(t *testing.T) {
	got := CaptureUnverified(errors.New("agent returned 500: bad file descriptor"))
	for _, w := range []string{"capture unverified", "could not read whether the agent tracked every process", "agent returned 500: bad file descriptor", "cannot prove", "agent's log"} {
		if !strings.Contains(got, w) {
			t.Errorf("reason %q does not say %q", got, w)
		}
	}
	if strings.Contains(got, "capture incomplete") || strings.Contains(got, "refused") {
		t.Errorf("reason %q claims a refusal it never read", got)
	}
}

// The reason names the map that refused, what that cost, and what to do; no
// refusal has no reason. The counts are refused inserts, one for every
// socket() of a process the map had no room for, so the reason gives them as
// inserts and never as a number of processes.
func TestPIDMapRefusalsCaptureIncomplete(t *testing.T) {
	if r := (PIDMapRefusals{}).CaptureIncomplete(); r != "" {
		t.Fatalf("no refusal gave a reason: %q", r)
	}
	for _, tc := range []struct {
		r    PIDMapRefusals
		want []string
		not  []string
	}{
		{PIDMapRefusals{Target: 3}, []string{"capture incomplete", "refused 3 insert(s)", "at least one process was not intercepted", "target_namespace_pids", "real destination", "live processes"}, []string{"excluded_pids", "entries)"}},
		{PIDMapRefusals{Excluded: 2}, []string{"capture incomplete", "refused 2 insert(s)", "excluded_pids", "predates the run"}, []string{"target_namespace_pids"}},
		{PIDMapRefusals{Target: 1, Excluded: 1}, []string{"target_namespace_pids", "excluded_pids"}, nil},
		// The map's size is the agent's to give, not a number written here.
		{PIDMapRefusals{Target: 1, TargetCapacity: 10240, ExcludedCapacity: 64}, []string{"target_namespace_pids (10240 entries)"}, []string{"64"}},
		{PIDMapRefusals{Excluded: 1, TargetCapacity: 10240, ExcludedCapacity: 64}, []string{"excluded_pids (64 entries)"}, []string{"10240"}},
		// The cause is named from the kernel's error, not assumed: a full map
		// (-E2BIG) gets the "fewer live processes" remedy; a lock failure
		// (-ETIMEDOUT, Linux 6.15+) is not called full and gets the contention
		// remedy instead.
		{PIDMapRefusals{Target: 1, LastTargetErr: -7}, []string{"(E2BIG: the map had no room for it, usually because it is full)", "live processes"}, []string{"bucket lock", "CPU contention", "memory"}},
		{PIDMapRefusals{Target: 1, LastTargetErr: -110}, []string{"(ETIMEDOUT: its bucket lock could not be taken)", "CPU contention"}, []string{"is full", "live processes", "E2BIG", "memory"}},
		{PIDMapRefusals{Excluded: 1, LastExcludedErr: -35}, []string{"excluded_pids", "EDEADLK", "CPU contention"}, []string{"is full", "live processes"}},
		{PIDMapRefusals{Target: 1, LastTargetErr: -16}, []string{"(EBUSY: its bucket lock could not be taken)", "CPU contention"}, []string{"is full", "live processes"}},
		// Each cause gets its own remedy: memory is not a lock, and an error
		// keploy does not expect is not called either.
		{PIDMapRefusals{Target: 1, LastTargetErr: -12}, []string{"(ENOMEM: the kernel could not allocate the entry)", "memory is short"}, []string{"CPU contention", "live processes", "is full"}},
		{PIDMapRefusals{Target: 1, LastTargetErr: -22}, []string{"the kernel returned error 22", "does not expect this error"}, []string{"CPU contention", "live processes", "memory"}},
		// An older agent that does not give the error: the full-map remedy.
		{PIDMapRefusals{Target: 1}, []string{"live processes"}, []string{"CPU contention", "memory"}},
		// Two maps, two causes: both remedies, each once.
		{PIDMapRefusals{Target: 1, LastTargetErr: -7, Excluded: 1, LastExcludedErr: -12}, []string{"live processes", "memory is short"}, nil},
	} {
		got := tc.r.CaptureIncomplete()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: reason %q does not say %q", tc.r, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%+v: reason %q says %q, which is not true of it", tc.r, got, n)
			}
		}
		// A count of refused inserts is not a count of processes.
		if strings.Contains(got, "process(es)") || strings.Contains(got, "processes a place") {
			t.Errorf("%+v: reason %q gives the refused inserts as a number of processes", tc.r, got)
		}
	}
}

// The WARN's next step and the reason's remedy are one table: each errno has
// its own.
func TestPIDMapRefusalRemedyNamesEachCause(t *testing.T) {
	for err, want := range map[int32]string{
		0:    "live processes",
		-7:   "live processes",
		-110: "CPU contention",
		-35:  "CPU contention",
		-16:  "CPU contention",
		-12:  "memory is short",
		-22:  "does not expect this error",
	} {
		if got := PIDMapRefusalRemedy(err); !strings.Contains(got, want) {
			t.Errorf("PIDMapRefusalRemedy(%d) = %q, want it to say %q", err, got, want)
		}
	}
}
