// Package provenswitch is the proven model's switch: KEPLOY_PROVEN.
//
// Every row of the proven model (strict verdicts on misses and unconsumed test
// mocks, unread misses as an incomplete round, skip reasons, strict response
// comparison, and the rest of the design's build plan) landed behind this one
// switch, so it could merge piece by piece while the old behaviour stayed
// exactly as it was. Release 1 turns it on by default on Linux, the platform
// it was built and proven on: there KEPLOY_PROVEN unset or empty means on, and
// KEPLOY_PROVEN=0 runs the old paths. Every other platform keeps the old
// default, off, until it is ruled on (Windows and macOS, P221): there an unset
// KEPLOY_PROVEN means off, and KEPLOY_PROVEN=1 turns it on, as before. The old
// paths stay for as long as a platform runs them.
//
// On is the ONE place the switch is read. keploy, enterprise and the
// integrations all call it, so a row cannot be on in one of them and off in
// another within one process.
//
// The agent is a separate process, and it does not inherit the variable: the
// native agent is started through sudo, which drops the caller's environment,
// and the agent container gets only the variables keploy passes it. So every
// launcher appends AgentArgs to the agent's argv, and the agent pins its switch
// from its --proven flag, on or off (Pin): an agent's own environment never
// decides, so an agent cannot run the new paths while the CLI that started it
// runs the old ones, or the reverse, and an agent on Linux that a macOS or
// Windows keploy started (in a container) runs that keploy's switch, not
// Linux's default. Parsers run inside the agent, so they see the pinned switch
// too.
//
// The agent reports what it pinned on its health check (HealthHeader, State),
// and the CLI refuses an agent whose state is not its own (CheckAgent). A
// native or docker-run agent is checked at its bring-up, with the switch on or
// off, from the health answer that found it ready. A docker compose agent is
// checked before it is told to release the app, with the switch on only: with
// it off its switch is not read, so no agent is asked anything it was not asked
// before the switch existed. A compose agent is the one keploy wrote into the
// compose file, whose command carries the flag only when the switch is on.
package provenswitch

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	// Env is the variable that sets the switch for a keploy process:
	// KEPLOY_PROVEN=0 turns it off, KEPLOY_PROVEN=1 on, and unset or empty
	// leaves it at the platform's default (PlatformDefault).
	Env = "KEPLOY_PROVEN"
	// AgentFlag is the hidden `keploy agent` flag that carries the switch from
	// the CLI to the agent it starts (see AgentArgs).
	AgentFlag = "proven"
	// HealthHeader is the /agent/health response header in which an agent says
	// whether its switch is on (State). An agent that does not send it predates
	// the switch.
	HealthHeader = "X-Keploy-Proven"
)

// ErrAgentSwitch marks an agent this keploy must not run against: the proven
// switch it reports is not this keploy's (CheckAgent). The bring-up of a native
// or docker-run agent fails with it. The docker compose readiness step returns
// it with the switch on (with it off it reads nothing), and record, replay and
// the runner stop on it (errors.Is). A compose agent that compose starts again
// mid-run is started from the same generated file, with the same flag.
var ErrAgentSwitch = errors.New("keploy's agent does not have this keploy's proven switch")

// The pinned states. unpinned (the zero value) means On reads the variable.
const (
	unpinned int32 = iota
	pinnedOff
	pinnedOn
)

var pinned atomic.Int32

// PlatformDefault is the switch on a platform (a runtime.GOOS value) when
// KEPLOY_PROVEN does not set it: on on Linux, which release 1 flips, and off
// everywhere else. Windows and macOS stay out of the flip (P221): their
// keploy judges as it did before, so a platform turns the proven model on by
// default only once it is ruled on. WSL, and Lima on an Intel Mac, run the
// Linux binary, so they are Linux here.
func PlatformDefault(goos string) bool {
	return goos == "linux"
}

// defaultOn is this process's PlatformDefault. A variable only so this
// package's tests can check another platform's default on any machine.
var defaultOn = PlatformDefault(runtime.GOOS)

// On reports whether the proven model's rows are on in this process. A process
// whose switch was pinned (an agent, from its --proven flag) answers with what
// was pinned. Any other process reads KEPLOY_PROVEN, on every call, so the
// answer is its environment as it is now (and a test can set the switch with
// t.Setenv): on for a value strconv.ParseBool reads as true ("1", "true", …),
// off for one it reads as false ("0", "false", …), and the platform's default
// (PlatformDefault: on on Linux) when it is unset or empty. A value it does not
// read at all is the default here too; keploy's own commands refuse it before
// they start (Validate).
func On() bool {
	switch pinned.Load() {
	case pinnedOn:
		return true
	case pinnedOff:
		return false
	}
	return OnFor(os.Getenv(Env))
}

// OnFor is the switch of an unpinned keploy process whose KEPLOY_PROVEN is v:
// what On answers in it. It is for a keploy program that reads the switch
// from an environment it is handed (keploy-go-exec's tests stand in for the
// test command's), so it reads it as keploy does, the platform's default
// included, and not by a rule of its own.
func OnFor(v string) bool {
	on, set, err := parse(v)
	if err != nil || !set {
		return defaultOn
	}
	return on
}

// Pin fixes this process's switch for the rest of its life, on or off, and
// stops it reading KEPLOY_PROVEN. The agent pins from its --proven flag, so the
// CLI that started it is the only thing that decides.
func Pin(on bool) {
	if on {
		pinned.Store(pinnedOn)
		return
	}
	pinned.Store(pinnedOff)
}

// Unpin undoes Pin, so the process reads KEPLOY_PROVEN again. keploy itself
// never calls it: it is for a test that runs the agent's flag handling (which
// pins) in a process whose later tests set the switch with t.Setenv.
func Unpin() {
	pinned.Store(unpinned)
}

// IgnoredEnv is, for a pinned process, a KEPLOY_PROVEN in its environment
// that says something other than what was pinned (a value that is set and does
// not read as the pinned state, unreadable values included), and whether there
// is one. The agent warns about it: its environment never decides its switch.
func IgnoredEnv() (string, bool) {
	p := pinned.Load()
	v := os.Getenv(Env)
	if p == unpinned {
		return "", false
	}
	on, set, err := parse(v)
	if !set || err == nil && on == (p == pinnedOn) {
		return "", false
	}
	return v, true
}

// State is On as a health-check value: "on" or "off".
func State() string {
	if On() {
		return "on"
	}
	return "off"
}

// AgentArgs is what a launcher appends to the argv of the agent it starts: the
// --proven flag when the switch is on in this process, nothing otherwise. With
// the switch off the agent's argv is exactly what it was before the switch
// existed.
func AgentArgs() []string {
	if !On() {
		return nil
	}
	return []string{"--" + AgentFlag}
}

// Validate refuses a KEPLOY_PROVEN that is set to something strconv.ParseBool
// does not read ("yes", "on", a typo): such a value would otherwise mean the
// platform's default without a word, whichever side the user meant. Unset or
// empty is fine. keploy's CmdConfigurator.ValidateFlags calls it, so every
// command whose flags it validates (record, test, mock, report and the others
// whose PreRunE runs it) refuses such a value before it runs; the agent does
// not, because its switch comes from its --proven flag alone. Commands that do
// not validate their flags there, and k8s-proxy, do not refuse it yet.
func Validate() error {
	if _, _, err := parse(os.Getenv(Env)); err != nil {
		return fmt.Errorf("%s=%q is not a value keploy reads: %s", Env, os.Getenv(Env), settingHint())
	}
	return nil
}

// settingHint says how to set the switch on this platform.
func settingHint() string {
	if defaultOn {
		return "set it to 0 to run without the proven model, or unset it: the proven model is on by default on " + platformName(runtime.GOOS)
	}
	return "set it to 1 to run the proven model, or unset it: the proven model is off by default on " + platformName(runtime.GOOS)
}

// platformName is a runtime.GOOS value as a user names the platform.
func platformName(goos string) string {
	switch goos {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return goos
}

// PlatformNotice is what keploy says once, as a judged or recorded run starts,
// on a platform whose default is off (P221) while KEPLOY_PROVEN leaves the
// switch at that default: that this platform's keploy still judges as it did
// before, while Linux runs the proven model. "" otherwise: on Linux, or when
// the user set the switch either way.
func PlatformNotice() string {
	if defaultOn || pinned.Load() != unpinned {
		return ""
	}
	if _, set, err := parse(os.Getenv(Env)); set || err != nil {
		return ""
	}
	return fmt.Sprintf("the proven model, keploy's default on Linux, is not on by default on %s in this release: tests here are recorded and judged as in earlier releases", platformName(runtime.GOOS))
}

// CheckAgent compares an agent's health-check report of its switch (the
// HealthHeader value; "" when the agent sent none) with this process's, and
// says why they cannot run together when they differ: the agent's parsers and
// this keploy's verdicts would be on different paths. An agent that sends no
// report predates the switch, which is only a problem when the switch is on.
// The error wraps ErrAgentSwitch and names both sides.
func CheckAgent(reported string) error {
	local := State()
	switch {
	case reported == "" && local == "off":
		return nil
	case reported == "":
		return fmt.Errorf("%w: the proven model is on in this keploy and its agent reports no switch, so the agent predates it: run the agent of this keploy version, or set %s=0 to run without the proven model", ErrAgentSwitch, Env)
	case reported == local:
		return nil
	}
	// The way on: this keploy's own agent, or this keploy run with the
	// switch its agent has.
	match := "0"
	if reported == "on" {
		match = "1"
	}
	return fmt.Errorf("%w: the proven model is %s in this keploy and %s in its agent. An agent takes its switch only from the --%s flag of the keploy that starts it; an agent started by anything else (k8s-proxy does not pass the flag yet) or by another keploy version has its own. Let this keploy start its agent, or set %s=%s to run this keploy as its agent runs",
		ErrAgentSwitch, local, reported, AgentFlag, Env, match)
}

// parse reads a value of KEPLOY_PROVEN: whether it says on, and whether it
// says anything (set). The empty string, or blanks, says nothing, and is no
// error.
func parse(v string) (on, set bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return false, false, nil
	}
	on, err = strconv.ParseBool(v)
	return on, true, err
}
