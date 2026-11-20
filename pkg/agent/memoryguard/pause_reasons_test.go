package memoryguard

import (
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// clearPauseReasons ends every pause a test left on, and forgets its causes.
func clearPauseReasons(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetExternalPause(false, "")
		applyPausedState(false)
		syncMock.ResetPauseCausesForTest()
	})
}

// Recording is paused while any reason holds it paused, and each owner ends
// only its own reason: memory recovering does not end a pause the control
// plane asked for, and the control plane's resume does not end a pause the
// memory guard holds, whichever of the two began first. The mock managers and
// the pressure hooks are never told that recording goes on while a reason
// still holds it paused, nor told "paused" for a reason that ended.
func TestAPauseLastsWhileAnyReasonHoldsIt(t *testing.T) {
	type step struct {
		name       string
		do         func()
		paused     bool
		external   bool
		forMemory  bool
		cause      string
		hookTold   []bool // what the hook is told at this step
		spanIsOpen bool
	}
	memory := func(on bool) func() { return func() { applyPausedState(on) } }
	external := func(on bool) func() { return func() { SetExternalPause(on, "rollout") } }
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"the external pause begins first and ends last", []step{
			{"external on", external(true), true, true, false, "control_plane_pause", []bool{true}, true},
			{"memory on", memory(true), true, true, true, "memory_pressure+control_plane_pause", []bool{true}, true},
			{"memory recovers", memory(false), true, true, false, "control_plane_pause", nil, true},
			{"external off", external(false), false, false, false, "", []bool{false}, false},
		}},
		{"the memory pause begins first and ends last", []step{
			{"memory on", memory(true), true, false, true, "memory_pressure", []bool{true}, true},
			{"external on", external(true), true, true, true, "memory_pressure+control_plane_pause", []bool{true}, true},
			{"external off", external(false), true, false, true, "memory_pressure", nil, true},
			{"memory recovers", memory(false), false, false, false, "", []bool{false}, false},
		}},
		{"the memory pause begins first and ends first", []step{
			{"memory on", memory(true), true, false, true, "memory_pressure", []bool{true}, true},
			{"external on", external(true), true, true, true, "memory_pressure+control_plane_pause", []bool{true}, true},
			{"memory recovers", memory(false), true, true, false, "control_plane_pause", nil, true},
			{"external off", external(false), false, false, false, "", []bool{false}, false},
		}},
		{"the external pause begins first and ends first", []step{
			{"external on", external(true), true, true, false, "control_plane_pause", []bool{true}, true},
			{"memory on", memory(true), true, true, true, "memory_pressure+control_plane_pause", []bool{true}, true},
			{"external off", external(false), true, false, true, "memory_pressure", nil, true},
			{"memory recovers", memory(false), false, false, false, "", []bool{false}, false},
		}},
		{"each is idempotent", []step{
			{"external on", external(true), true, true, false, "control_plane_pause", []bool{true}, true},
			{"external on again", external(true), true, true, false, "control_plane_pause", []bool{true}, true},
			{"external off", external(false), false, false, false, "", []bool{false}, false},
			{"external off again", external(false), false, false, false, "", []bool{false}, false},
			{"memory recovers with no pause on", memory(false), false, false, false, "", []bool{false}, false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearPauseReasons(t)
			var told []bool
			t.Cleanup(RegisterPressureHook(func(paused bool) { told = append(told, paused) }))
			mgr := syncMock.Get()
			for _, s := range tc.steps {
				told = nil
				s.do()
				if got := IsRecordingPaused(); got != s.paused {
					t.Fatalf("%s: IsRecordingPaused() = %v, want %v", s.name, got, s.paused)
				}
				if got, reason := ExternalPaused(); got != s.external || (got && reason != "rollout") || (!got && reason != "") {
					t.Fatalf("%s: ExternalPaused() = %v, %q, want %v", s.name, got, reason, s.external)
				}
				if got := ExternalPausedNow(); got != s.external {
					t.Fatalf("%s: ExternalPausedNow() = %v, want %v", s.name, got, s.external)
				}
				if got := PausedForMemory(); got != s.forMemory {
					t.Fatalf("%s: PausedForMemory() = %v, want %v", s.name, got, s.forMemory)
				}
				if got := PauseCause(); got != s.cause {
					t.Fatalf("%s: PauseCause() = %q, want %q", s.name, got, s.cause)
				}
				if len(told) != len(s.hookTold) || (len(told) == 1 && told[0] != s.hookTold[0]) {
					t.Fatalf("%s: the pressure hook was told %v, want %v", s.name, told, s.hookTold)
				}
				// The manager's span is open exactly while recording is
				// paused: a window that is all in the future of the step
				// overlaps it only then.
				now := time.Now()
				if open, _ := mgr.WasPressureActiveInWindow(now, now); open != s.spanIsOpen {
					t.Fatalf("%s: the mock manager's pause span is open: %v, want %v", s.name, open, s.spanIsOpen)
				}
			}
		})
	}
}

// A repeated external pause replaces its reason, and the resume forgets it.
func TestAnExternalPauseKeepsItsLatestReason(t *testing.T) {
	clearPauseReasons(t)
	SetExternalPause(true, "rollout")
	SetExternalPause(true, "rollout of v2")
	if on, reason := ExternalPaused(); !on || reason != "rollout of v2" {
		t.Fatalf("ExternalPaused() = %v, %q, want the latest reason", on, reason)
	}
	SetExternalPause(false, "ignored")
	if on, reason := ExternalPaused(); on || reason != "" {
		t.Fatalf("ExternalPaused() = %v, %q after the resume, want false and no reason", on, reason)
	}
}

// A PauseTally counts the requests of the OR's pause, not of each reason's: a
// reason that begins or ends while another holds the pause is no edge.
func TestAPauseTallyCountsThePauseNotItsReasons(t *testing.T) {
	clearPauseReasons(t)
	tally := OpenPauseTally()
	defer tally.Close()
	IngressRequest() // before the pause
	SetExternalPause(true, "rollout")
	IngressRequest()
	applyPausedState(true)
	IngressRequest()
	applyPausedState(false) // memory recovers; still paused
	IngressRequest()
	SetExternalPause(false, "")
	IngressRequest() // after it
	got := tally.Read()
	if got.Requests != 5 || got.DuringPauses != 3 || got.BeforeFirstPause != 1 {
		t.Fatalf("the tally read %+v, want 5 requests, 3 during the one pause, 1 before it", got)
	}
}

// What the external pause costs is the same as what a memory pause costs, and
// is told as the control plane's: a per-test mock requested during it is
// dropped and counted apart from memory pressure's drops, a window in flight
// across either of its edges overlaps its span, and a mock or a window wholly
// before or after it is untouched.
func TestAnExternalPauseDropsItsMocksAndCoversItsEdges(t *testing.T) {
	clearPauseReasons(t)
	mgr := syncMock.Get()
	mark := mgr.StartDropSession()
	skipsBefore, pausedBefore := mgr.NotRecordedUnderPressure(), mgr.NotRecordedWhilePaused()

	perTest := func(at time.Time) *models.Mock {
		m := &models.Mock{Kind: models.Kind("Kafka"), ConnectionID: "external-pause-test"}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		m.Spec.ReqTimestampMock = at
		m.Spec.ResTimestampMock = at.Add(time.Millisecond)
		return m
	}

	before := time.Now()
	time.Sleep(5 * time.Millisecond)
	SetExternalPause(true, "rollout")
	time.Sleep(5 * time.Millisecond)
	during := time.Now()
	if drop := mgr.AddMock(perTest(during)); drop != syncMock.MockDroppedUnderPressure {
		t.Fatalf("a per-test mock requested during the pause was not dropped: %q", drop)
	}
	mgr.NoteNotRecordedUnderPressure("10.0.0.1:5000 -> 127.0.0.1:8080")
	time.Sleep(5 * time.Millisecond)
	SetExternalPause(false, "")
	time.Sleep(5 * time.Millisecond)
	after := time.Now()

	// A mock decoded after the pause for a request made during it is dropped
	// too: no test case of the recording owns it.
	if drop := mgr.AddMock(perTest(during)); drop != syncMock.MockDroppedUnderPressure {
		t.Fatalf("a per-test mock requested during the pause and handed over after it was not dropped: %q", drop)
	}

	drops := mgr.DropsSince(mark)
	if drops.ControlPlanePause != 2 || drops.Pressure != 0 {
		t.Fatalf("the drops were counted %+v, want 2 for the control plane's pause and none for memory pressure", drops)
	}
	if got := mgr.NotRecordedWhilePaused().Since(pausedBefore); got.Total != 1 {
		t.Fatalf("the request the pause cost was counted %+v, want 1 not recorded while paused", got)
	}
	if got := mgr.NotRecordedUnderPressure().Since(skipsBefore); got.Total != 0 {
		t.Fatalf("the request the control plane's pause cost was reported as memory pressure's: %+v", got)
	}

	for _, tc := range []struct {
		name     string
		from, to time.Time
		overlaps bool
	}{
		{"a test case that ended before the pause", before, before, false},
		{"a test case in flight as the pause began", before, during, true},
		{"a test case within the pause", during, during, true},
		{"a test case in flight as the pause ended", during, after, true},
		{"a test case in flight across the whole pause", before, after, true},
		{"a test case that began after the resume", after, after, false},
	} {
		got, _ := mgr.WasPressureActiveInWindow(tc.from, tc.to)
		if got != tc.overlaps {
			t.Errorf("%s: the pause's span overlaps its window: %v, want %v", tc.name, got, tc.overlaps)
		}
		if tc.overlaps {
			if cause := syncMock.PauseCauseIn(tc.from, tc.to); cause != syncMock.PauseCauseExternal {
				t.Errorf("%s: its pause is reported as %v, want the control plane's", tc.name, cause)
			}
		}
	}
}

// A memory pause that overlaps the control plane's is reported as memory
// pressure: the older reports keep their meaning.
func TestAMemoryPauseKeepsItsOwnReport(t *testing.T) {
	clearPauseReasons(t)
	mgr := syncMock.Get()
	mark := mgr.StartDropSession()
	applyPausedState(true)
	time.Sleep(2 * time.Millisecond)
	at := time.Now()
	m := &models.Mock{Kind: models.Kind("Kafka"), ConnectionID: "memory-pause-test"}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	m.Spec.ReqTimestampMock = at
	if drop := mgr.AddMock(m); drop != syncMock.MockDroppedUnderPressure {
		t.Fatalf("a per-test mock requested during a memory pause was not dropped: %q", drop)
	}
	mgr.NoteNotRecordedUnderPressure("c")
	if drops := mgr.DropsSince(mark); drops.Pressure != 1 || drops.ControlPlanePause != 0 {
		t.Fatalf("a memory pause's drop was counted %+v, want 1 for memory pressure", drops)
	}
	if cause := syncMock.PauseCauseIn(at, at); cause != syncMock.PauseCauseMemory {
		t.Fatalf("a memory pause is reported as %v", cause)
	}
}
