package provenswitch

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// unpin undoes Pin for the rest of the test. Pin is one-way by design (an
// agent never unpins), so only this package's own tests may reach the state.
func unpin(t *testing.T) {
	t.Helper()
	pinned.Store(unpinned)
	t.Cleanup(func() { pinned.Store(unpinned) })
}

// onPlatform runs the rest of the test as a process of a build that runs
// release 1's flow (RunsTheFlow, as keploy enterprise does) whose platform
// default (PlatformDefault) is def: on, as on Linux, or off, as on Windows and
// macOS.
func onPlatform(t *testing.T, def bool) {
	t.Helper()
	savedPlatform, savedFlow := thisPlatform, flowBuild.Load()
	thisPlatform = def
	RunsTheFlow()
	t.Cleanup(func() { thisPlatform = savedPlatform; flowBuild.Store(savedFlow) })
}

// inOSSBuild runs the rest of the test as a process of a build that does not
// run release 1's flow (keploy's open-source build) on a platform whose
// default in a build that does would be def.
func inOSSBuild(t *testing.T, def bool) {
	t.Helper()
	savedPlatform, savedFlow := thisPlatform, flowBuild.Load()
	thisPlatform = def
	flowBuild.Store(false)
	t.Cleanup(func() { thisPlatform = savedPlatform; flowBuild.Store(savedFlow) })
}

// Release 1 flips the proven model on by default on Linux only: Windows and
// macOS keep today's default, off (P221), and WSL is Linux.
func TestPlatformDefaultIsOnOnlyOnLinux(t *testing.T) {
	for goos, want := range map[string]string{"linux": "Linux", "darwin": "macOS", "windows": "Windows", "freebsd": "freebsd"} {
		if got := platformName(goos); got != want {
			t.Fatalf("platformName(%q) = %q, want %q", goos, got, want)
		}
	}
	for goos, want := range map[string]bool{"linux": true, "windows": false, "darwin": false, "freebsd": false} {
		if got := PlatformDefault(goos); got != want {
			t.Fatalf("PlatformDefault(%q) = %v, want %v", goos, got, want)
		}
	}
	if thisPlatform != PlatformDefault(runtime.GOOS) {
		t.Fatalf("this process's platform default is %v; %s's is %v", thisPlatform, runtime.GOOS, PlatformDefault(runtime.GOOS))
	}
}

// Only a build that runs release 1's flow (keploy enterprise: record, an
// auto-replay that judges each test and learns what the app changes at each
// run, replay) turns the proven model on by default. keploy's open-source
// build, which this package's test binary is (nothing in it calls
// RunsTheFlow), has no auto-replay to learn the app's clock reads, so on
// every platform an unset KEPLOY_PROVEN is off there, as in earlier releases,
// and says nothing new; KEPLOY_PROVEN=1 still turns it on. Mutation: Default
// ignores the build; PlatformNotice speaks in a build without the flow.
func TestOnlyABuildThatRunsTheFlowTurnsTheProvenModelOnByDefault(t *testing.T) {
	unpin(t)
	if flowBuild.Load() || Default() {
		t.Fatal("keploy's open-source build (this test binary) runs release 1's flow: nothing in it may call RunsTheFlow")
	}
	for _, linux := range []bool{true, false} {
		inOSSBuild(t, linux)
		for _, v := range []string{"", "  ", "yes"} {
			t.Setenv(Env, v)
			if On() || OnFor(v) || State() != "off" || AgentArgs() != nil {
				t.Fatalf("open-source build, platform default %v, %s=%q: On() = %v, OnFor = %v, State() = %q, AgentArgs() = %q, want off and the argv of before", linux, Env, v, On(), OnFor(v), State(), AgentArgs())
			}
			if got := PlatformNotice(); got != "" {
				t.Fatalf("open-source build, platform default %v, %s=%q: PlatformNotice() = %q, want nothing, as before the flip", linux, Env, v, got)
			}
		}
		t.Setenv(Env, "1")
		if !On() {
			t.Fatalf("open-source build, platform default %v, %s=1: On() = false, want on", linux, Env)
		}
		t.Setenv(Env, "yes")
		if err := Validate(); err == nil || !strings.Contains(err.Error(), "set it to 1") || !strings.Contains(err.Error(), "this keploy build runs without it by default") {
			t.Fatalf("open-source build, platform default %v: Validate() = %v, want a refusal that says to set it to 1, as this build runs without it by default", linux, err)
		}
		if got, want := HowToTurnOn(), Env+"=1"; got != want {
			t.Fatalf("open-source build: HowToTurnOn() = %q, want %q", got, want)
		}
		onPlatform(t, linux)
		t.Setenv(Env, "")
		if On() != linux || Default() != linux {
			t.Fatalf("a build that runs the flow, platform default %v: On() = %v, Default() = %v", linux, On(), Default())
		}
		if got, want := HowToTurnOn(), "its default on Linux, "+Env+"=1 elsewhere"; got != want {
			t.Fatalf("a build that runs the flow: HowToTurnOn() = %q, want %q", got, want)
		}
	}
}

// A read of the switch before the build says it runs release 1's flow took
// the default of a build that does not, and ReadBeforeTheFlow says so, for
// each way the switch is read; a read after it does not count. keploy
// enterprise's tests refuse such a read: its `keploy quarantine` verb was
// decided by one, at init, and existed on Linux only because of the order of
// its file names. Mutation: a read does not go through flow; RunsTheFlow does
// not look.
func TestAReadBeforeTheFlowIsSeen(t *testing.T) {
	unpin(t)
	saved := [3]bool{flowBuild.Load(), readFirst.Load(), readBeforeFlow.Load()}
	t.Cleanup(func() { flowBuild.Store(saved[0]); readFirst.Store(saved[1]); readBeforeFlow.Store(saved[2]) })
	reset := func() { flowBuild.Store(false); readFirst.Store(false); readBeforeFlow.Store(false) }
	for _, r := range []struct {
		name string
		read func()
	}{
		{"On, the variable unset", func() { t.Setenv(Env, ""); On() }},
		{"On, the variable set", func() { t.Setenv(Env, "1"); On() }},
		{"OnFor", func() { OnFor("0") }},
		{"Default", func() { Default() }},
		{"State", func() { t.Setenv(Env, ""); State() }},
		{"AgentArgs", func() { t.Setenv(Env, ""); AgentArgs() }},
		{"HowToTurnOn", func() { HowToTurnOn() }},
		{"Validate", func() { t.Setenv(Env, "yes"); _ = Validate() }},
		{"PlatformNotice", func() { t.Setenv(Env, ""); PlatformNotice() }},
	} {
		reset()
		r.read()
		RunsTheFlow()
		if !ReadBeforeTheFlow() {
			t.Fatalf("%s before RunsTheFlow: ReadBeforeTheFlow() = false, want true", r.name)
		}
		reset()
		RunsTheFlow()
		r.read()
		if ReadBeforeTheFlow() {
			t.Fatalf("%s after RunsTheFlow: ReadBeforeTheFlow() = true, want false", r.name)
		}
	}
}

// An unset KEPLOY_PROVEN is the platform's default, here as in a keploy that
// nothing configures; 0 and 1 decide on every platform, so a Linux user can
// still run the old paths and a Windows or macOS user can still opt in, as
// before.
func TestOnReadsTheVariable(t *testing.T) {
	unpin(t)
	for _, linux := range []bool{true, false} {
		onPlatform(t, linux)
		for _, tc := range []struct {
			value string
			want  bool
		}{
			{"", linux},
			{"  ", linux},
			{"0", false},
			{"false", false},
			{"1", true},
			{" 1 ", true},
			{"true", true},
			{"TRUE", true},
			// Not read at all: the default here, and refused at startup by
			// Validate.
			{"yes", linux},
			{"on", linux},
			{"ture", linux},
		} {
			t.Run(tc.value, func(t *testing.T) {
				t.Setenv(Env, tc.value)
				if got := On(); got != tc.want {
					t.Fatalf("platform default %v, %s=%q: On() = %v, want %v", linux, Env, tc.value, got, tc.want)
				}
				// OnFor reads a value it is handed by the same rule, whatever
				// this process's own environment says.
				t.Setenv(Env, "")
				if got := OnFor(tc.value); got != tc.want {
					t.Fatalf("platform default %v: OnFor(%q) = %v, want %v, as On reads it", linux, tc.value, got, tc.want)
				}
			})
		}
	}
}

// OnFor is an unpinned process's switch: a pin of this one does not decide it.
func TestOnForIgnoresThisProcesssPin(t *testing.T) {
	unpin(t)
	onPlatform(t, true)
	Pin(false)
	if !OnFor("1") || !OnFor("") || OnFor("0") {
		t.Fatalf("pinned off: OnFor(1), OnFor(\"\"), OnFor(0) = %v, %v, %v, want true, true (Linux's default), false", OnFor("1"), OnFor(""), OnFor("0"))
	}
}

// With the switch at its default, the agent a Linux keploy starts is told to
// run the proven paths, and the one a macOS or Windows keploy starts (in its
// Linux container) is not: the agent's own platform never decides.
func TestAgentArgsFollowTheCLIsPlatform(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "")
	onPlatform(t, true)
	if got, want := AgentArgs(), []string{"--proven"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Linux, KEPLOY_PROVEN unset: AgentArgs() = %q, want %q", got, want)
	}
	onPlatform(t, false)
	if got := AgentArgs(); got != nil {
		t.Fatalf("macOS or Windows, KEPLOY_PROVEN unset: AgentArgs() = %q, want nil, the argv of before", got)
	}
}

// Windows and macOS say once that they stay out of the flip, and Linux once
// that the proven model is on and KEPLOY_PROVEN=0 runs without it, unless the
// user set the switch either way: a user whose KEPLOY_PROVEN=0 sudo dropped
// is told which model ran. Mutation: say nothing on Linux; say it with the
// switch set.
func TestPlatformNotice(t *testing.T) {
	unpin(t)
	for _, tc := range []struct {
		linux  bool
		value  string
		notice bool
	}{
		{true, "", true},
		{true, " ", true},
		{true, "0", false},
		{true, "1", false},
		{true, "yes", false}, // refused by Validate, which says how to set it
		{false, "", true},
		{false, " ", true},
		{false, "0", false},
		{false, "1", false},
		{false, "yes", false},
	} {
		onPlatform(t, tc.linux)
		t.Setenv(Env, tc.value)
		got := PlatformNotice()
		if (got != "") != tc.notice {
			t.Fatalf("platform default %v, %s=%q: PlatformNotice() = %q, want a notice: %v", tc.linux, Env, tc.value, got, tc.notice)
		}
		if got == "" {
			continue
		}
		if tc.linux {
			if !strings.Contains(got, "the proven model is on") || !strings.Contains(got, Env+"=0 runs without it") {
				t.Fatalf("on Linux the notice must say the proven model is on, and how to run without it: %q", got)
			}
			continue
		}
		if !strings.Contains(got, "Linux") || !strings.Contains(got, platformName(runtime.GOOS)) {
			t.Fatalf("the notice must name Linux, where the proven model is the default, and this platform (%s): %q", platformName(runtime.GOOS), got)
		}
	}
	// An agent's switch is its flag's: it says nothing.
	onPlatform(t, false)
	t.Setenv(Env, "")
	Pin(false)
	if got := PlatformNotice(); got != "" {
		t.Fatalf("a pinned agent: PlatformNotice() = %q, want none", got)
	}
}

// A pinned process (an agent) answers with what was pinned, on or off, and
// never with its own environment: the CLI's flag is the only thing that
// decides an agent's switch.
func TestPinIgnoresTheVariable(t *testing.T) {
	unpin(t)
	onPlatform(t, false)
	t.Setenv(Env, "")
	Pin(true)
	if !On() || State() != "on" {
		t.Fatal("Pin(true) with KEPLOY_PROVEN unset, off by default: the agent must run the new paths the CLI asked for")
	}
	unpin(t)
	t.Setenv(Env, "1")
	Pin(false)
	if On() || State() != "off" {
		t.Fatal("Pin(false) with KEPLOY_PROVEN=1 in the agent's environment: the agent must not run paths its CLI did not ask for")
	}
	// Nor with the variable unset on Linux, where unset is on: an agent a
	// macOS or Windows keploy started in its Linux container runs that
	// keploy's switch.
	unpin(t)
	onPlatform(t, true)
	t.Setenv(Env, "")
	Pin(false)
	if On() || State() != "off" {
		t.Fatal("Pin(false) on Linux with KEPLOY_PROVEN unset: the agent must not run paths its CLI did not ask for")
	}
}

func TestAgentArgs(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "0")
	if got := AgentArgs(); got != nil {
		t.Fatalf("switch off: AgentArgs() = %q, want nil so the agent argv is byte-identical to before", got)
	}
	t.Setenv(Env, "1")
	if got, want := AgentArgs(), []string{"--proven"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch on: AgentArgs() = %q, want %q", got, want)
	}
}

// A mistyped value must not quietly mean the default, on or off. The refusal
// says how to leave the default on this platform.
func TestValidateRefusesAValueItCannotRead(t *testing.T) {
	unpin(t)
	for _, linux := range []bool{true, false} {
		onPlatform(t, linux)
		for _, ok := range []string{"", " ", "1", "0", "true", "false"} {
			t.Setenv(Env, ok)
			if err := Validate(); err != nil {
				t.Fatalf("%s=%q refused: %v", Env, ok, err)
			}
		}
		leave := map[bool]string{true: "set it to 0", false: "set it to 1"}[linux]
		for _, bad := range []string{"yes", "on", "ture", "2"} {
			t.Setenv(Env, bad)
			err := Validate()
			if err == nil || !strings.Contains(err.Error(), Env) || !strings.Contains(err.Error(), leave) {
				t.Fatalf("platform default %v, %s=%q: Validate() = %v, want a refusal naming the variable that says to %s", linux, Env, bad, err, leave)
			}
		}
	}
}

func TestCheckAgent(t *testing.T) {
	unpin(t)
	for _, tc := range []struct {
		local, reported string
		ok              bool
	}{
		{"0", "off", true},
		{"1", "on", true},
		{"0", "", true},     // switch off, an agent that predates the switch: today's pairing
		{"1", "", false},    // switch on, an agent that cannot have it
		{"1", "off", false}, // an agent started without --proven (k8s-proxy today)
		{"0", "on", false},  // an agent running paths this keploy does not
	} {
		t.Setenv(Env, tc.local)
		err := CheckAgent(tc.reported)
		if (err == nil) != tc.ok {
			t.Fatalf("local %q, agent %q: CheckAgent = %v, want ok=%v", tc.local, tc.reported, err, tc.ok)
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrAgentSwitch) {
			t.Fatalf("the refusal must wrap ErrAgentSwitch, so every caller can make it fatal: %v", err)
		}
		local := State()
		if !strings.Contains(err.Error(), local+" in this keploy") {
			t.Fatalf("the refusal must name this keploy's side (%s): %v", local, err)
		}
		if tc.reported != "" && !strings.Contains(err.Error(), tc.reported+" in its agent") {
			t.Fatalf("the refusal must name the agent's side (%s): %v", tc.reported, err)
		}
		// It names the proven model, not a variable the user may never
		// have set (P222).
		if strings.Contains(err.Error(), "("+Env+")") {
			t.Fatalf("the refusal tags itself with %s: %v", Env, err)
		}
		// An agent that predates the switch is a version mismatch: the
		// remedy is this version's agent, or the run without the model.
		if tc.reported == "" && (!strings.Contains(err.Error(), "run the agent of this keploy version") || !strings.Contains(err.Error(), Env+"=0")) {
			t.Fatalf("the refusal of an agent that predates the switch gives no way on: %v", err)
		}
		// An agent whose switch is the other one: this keploy starting its
		// agent, or this keploy run with the agent's switch, goes on.
		if tc.reported != "" {
			match := Env + "=0"
			if tc.reported == "on" {
				match = Env + "=1"
			}
			if !strings.Contains(err.Error(), "Let this keploy start its agent") || !strings.Contains(err.Error(), match) {
				t.Fatalf("the refusal of an agent with the other switch gives no way on (want %s): %v", match, err)
			}
		}
	}
}

// A pinned agent warns about a KEPLOY_PROVEN in its own environment only when
// it says something other than what was pinned.
func TestIgnoredEnv(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "1")
	if _, ignored := IgnoredEnv(); ignored {
		t.Fatal("an unpinned process reads its variable; nothing is ignored")
	}
	for _, tc := range []struct {
		pin     bool
		value   string
		ignored bool
	}{
		{true, "1", false},  // the CLI ran as root and passed both: they agree
		{true, "", false},   // nothing set
		{false, "", false},  // nothing set, though unset is on on Linux
		{false, "1", true},  // an agent started without --proven, the variable on
		{true, "0", true},   // the reverse
		{true, "yes", true}, // unreadable, and ignored
		{false, "false", false},
	} {
		unpin(t)
		Pin(tc.pin)
		t.Setenv(Env, tc.value)
		if v, ignored := IgnoredEnv(); ignored != tc.ignored || (ignored && v != tc.value) {
			t.Fatalf("pin %v, %s=%q: IgnoredEnv = (%q, %v), want ignored=%v", tc.pin, Env, tc.value, v, ignored, tc.ignored)
		}
	}
}

// Unpin hands the switch back to KEPLOY_PROVEN.
func TestUnpin(t *testing.T) {
	t.Cleanup(Unpin)
	onPlatform(t, false)
	t.Setenv(Env, "1")
	Pin(false)
	if On() {
		t.Fatal("pinned off, the switch read the environment")
	}
	Unpin()
	if !On() {
		t.Fatal("unpinned, the switch did not read KEPLOY_PROVEN=1")
	}
}
