// Package provenswitch is the staging gate of the proven model: KEPLOY_PROVEN=1.
//
// Every row of the proven model (strict verdicts on misses and unconsumed test
// mocks, unread misses as an incomplete round, skip reasons, strict response
// comparison, and the rest of the design's build plan) lands behind this one
// switch, so it can merge piece by piece while today's behaviour stays exactly
// as it is for everyone who does not set it. The e2e lanes that exercise those
// rows set it. It is a staging gate, not a setting: the flip deletes this
// package together with every old path, and no user is meant to choose between
// the two behaviours.
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
// runs the old ones, or the reverse. Parsers run inside the agent, so they see
// the pinned switch too.
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
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	// Env is the variable that turns the switch on for a keploy process:
	// KEPLOY_PROVEN=1.
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

// On reports whether the proven model's rows are on in this process. A process
// whose switch was pinned (an agent, from its --proven flag) answers with what
// was pinned. Any other process reads KEPLOY_PROVEN, on every call, so the
// answer is its environment as it is now (and a test can set the switch with
// t.Setenv): on for a value strconv.ParseBool reads as true ("1", "true", …),
// off otherwise. A value it does not read at all is off here; keploy's own
// commands refuse it before they start (Validate).
func On() bool {
	switch pinned.Load() {
	case pinnedOn:
		return true
	case pinnedOff:
		return false
	}
	on, err := parse(os.Getenv(Env))
	return err == nil && on
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
	if p == unpinned || strings.TrimSpace(v) == "" {
		return "", false
	}
	on, err := parse(v)
	if err == nil && on == (p == pinnedOn) {
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
// does not read ("yes", "on", a typo): such a value would otherwise mean "off"
// without a word, and run a staging lane on the old paths. Unset or empty is
// fine. keploy's CmdConfigurator.ValidateFlags calls it, so every command
// whose flags it validates (record, test, mock, report and the others whose
// PreRunE runs it) refuses such a value before it runs; the agent does not,
// because its switch comes from its --proven flag alone. Commands that do not
// validate their flags there, and k8s-proxy, do not refuse it yet.
func Validate() error {
	if _, err := parse(os.Getenv(Env)); err != nil {
		return fmt.Errorf("%s=%q is not a value keploy reads: set it to 1 to turn the proven model's staging switch on, or unset it", Env, os.Getenv(Env))
	}
	return nil
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
		return fmt.Errorf("%w: the switch is on in this keploy (%s) and its agent reports none, so the agent predates it; run the agent of this keploy version", ErrAgentSwitch, Env)
	case reported == local:
		return nil
	}
	return fmt.Errorf("%w: the switch is %s in this keploy (%s) and %s in its agent. An agent takes it only from the --%s flag of the keploy that starts it; an agent started by anything else (k8s-proxy does not pass the flag yet) or by another keploy version has its own",
		ErrAgentSwitch, local, Env, reported, AgentFlag)
}

// parse reads a value of KEPLOY_PROVEN. The empty string is off and no error.
func parse(v string) (bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return false, nil
	}
	return strconv.ParseBool(v)
}
