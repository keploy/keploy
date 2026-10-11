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
// launcher appends AgentArgs to the agent's argv, and the agent's --proven flag
// calls Enable. Parsers run inside the agent, so they see the switch too.
package provenswitch

import (
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
)

// enabled is set by Enable: the agent's --proven flag.
var enabled atomic.Bool

// On reports whether the proven model's rows are on in this process: the
// process was started with KEPLOY_PROVEN set to a true value ("1", "true", as
// strconv.ParseBool reads it), or it is an agent started with --proven.
//
// The variable is read on every call rather than once, so the answer is the
// process's environment as it is now. That costs a map lookup under a read
// lock, and it lets a test set the switch with t.Setenv.
func On() bool {
	if enabled.Load() {
		return true
	}
	return envOn(os.Getenv(Env))
}

// Enable turns the switch on for the rest of this process. The agent calls it
// when it is started with --proven; nothing turns it off again.
func Enable() {
	enabled.Store(true)
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

// envOn reads a value of KEPLOY_PROVEN. Anything strconv.ParseBool does not
// read as true, the empty string included, leaves the switch off.
func envOn(v string) bool {
	on, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && on
}
