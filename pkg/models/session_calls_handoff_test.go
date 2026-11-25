package models

import (
	"encoding/json"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// manySessionCalls is n declared calls, each its own host and path.
func manySessionCalls(n int) []SessionCallRule {
	out := make([]SessionCallRule, n)
	for i := range out {
		out[i] = SessionCallRule{Host: fmt.Sprintf("svc-%05d.internal.example.com:8443", i), Method: "GET", PathPrefix: fmt.Sprintf("/v1/config/%05d/settings", i)}
	}
	return out
}

// A keploy client hands its agent its declared session calls under the
// proven switch alone, none an empty declaration; the agent's Install
// replaces its own with them, so a declaration of none clears an earlier
// one, and one no request carried changes nothing. Any size round-trips the
// control plane's JSON whole: well past a command line's 8191 characters and
// one argument's 128 KiB.
// Mutation: hand them with the switch off, keep the earlier declaration, or
// let a missing one clear it.
func TestTheDeclaredSessionCallsGoToTheAgentWhole(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	big := manySessionCalls(2500)
	SetSessionCalls(big)
	t.Setenv(provenswitch.Env, "")
	if d := DeclaredSessionCallsToHand(); d != nil {
		t.Fatalf("switch off: the agent is handed %d session calls", len(d.Rules))
	}
	t.Setenv(provenswitch.Env, "1")
	b, err := json.Marshal(DeclaredSessionCallsToHand())
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= 128<<10 {
		t.Fatalf("the declaration is %d bytes; the row needs one over 128 KiB", len(b))
	}
	SetSessionCalls(nil)
	var got *DeclaredSessionCalls
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	got.Install()
	last := big[len(big)-1]
	if covered, named := SessionCallDeclared("GET", last.Host, last.PathPrefix); !covered || !named {
		t.Fatalf("the agent does not declare the last of %d session calls it was handed", len(big))
	}
	(*DeclaredSessionCalls)(nil).Install()
	if !SessionCallsDeclared() {
		t.Fatal("a request that carried no declaration cleared the agent's")
	}
	(&DeclaredSessionCalls{}).Install()
	if SessionCallsDeclared() {
		t.Fatal("a declaration of none left the earlier one in force")
	}
	SetSessionCalls(nil)
	if d := DeclaredSessionCallsToHand(); d == nil || len(d.Rules) != 0 {
		t.Fatalf("none declared: the agent is handed %+v; want an empty declaration", d)
	}
}
