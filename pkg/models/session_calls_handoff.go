package models

import (
	"slices"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// DeclaredSessionCalls hands an agent, over its control plane, the session
// calls the keploy client that drives it declares (SetSessionCalls,
// keploy.yaml sessionCalls): on keploy test's staging call
// (MockFilterParams.SessionCalls) and on keploy mock replay's scope table
// (ScopeTableReq.SessionCalls). Under the proven switch the agent reads them
// itself, which recording made outside every test a test is served
// (Mock.DeclaredSessionCall) and which plain startup: recording a test's call
// is its own call of (agent.backgroundRecordingsOf), and an agent that reads
// no keploy.yaml (an agent container, which mounts no project; one started
// with another config path) would else declare none. A request is no
// command line: no size bounds it, on any launcher or platform.
//
// A request that carries one replaces the agent's declaration with Rules,
// none included (Install): each set names the client's whole declaration,
// so no set's declaration outlives it on an agent that serves the next.
type DeclaredSessionCalls struct {
	Rules []SessionCallRule `json:"rules"`
}

// DeclaredSessionCallsToHand is what a keploy client hands its agent with a
// set: under the proven switch its declared session calls, none an empty
// Rules; nil with the switch off, which leaves the agent's as they are, as
// before.
func DeclaredSessionCallsToHand() *DeclaredSessionCalls {
	if !provenswitch.On() {
		return nil
	}
	d := &DeclaredSessionCalls{}
	if rules := sessionCalls.Load(); rules != nil {
		d.Rules = slices.Clone(*rules)
	}
	return d
}

// Install makes d's rules this process's declared session calls
// (SetSessionCalls), replacing any it had. A nil d, one no request carried,
// changes nothing.
func (d *DeclaredSessionCalls) Install() {
	if d == nil {
		return
	}
	SetSessionCalls(d.Rules)
}

// Clone is a copy of d sharing nothing with it; nil for nil.
func (d *DeclaredSessionCalls) Clone() *DeclaredSessionCalls {
	if d == nil {
		return nil
	}
	return &DeclaredSessionCalls{Rules: slices.Clone(d.Rules)}
}
