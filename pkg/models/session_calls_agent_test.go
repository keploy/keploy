package models

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The declared session calls reach an agent as one word a shell or cmd.exe
// reads as is, under the proven switch alone, and read back whole; none
// declared, or the switch off, adds no argument.
// Mutation: hand them with the switch off, or lose a field on the way.
func TestTheSessionCallsReachTheAgentAsOneWord(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	rules := []SessionCallRule{{Host: "rates.example.com:8443", Method: "GET", PathPrefix: "/v1/rates"}, {Host: "auth.example.com"}}
	SetSessionCalls(rules)

	t.Setenv(provenswitch.Env, "")
	if args := SessionCallsAgentArgs(); args != nil {
		t.Fatalf("switch off: the agent is handed %v", args)
	}
	t.Setenv(provenswitch.Env, "1")
	args := SessionCallsAgentArgs()
	if len(args) != 1 || !strings.HasPrefix(args[0], "--"+SessionCallsAgentFlag+"=") {
		t.Fatalf("the agent is handed %v; want one --%s=VALUE word", args, SessionCallsAgentFlag)
	}
	v := strings.TrimPrefix(args[0], "--"+SessionCallsAgentFlag+"=")
	if strings.ContainsAny(v, " \"'`$&|<>^%!;(){}[]*?\\/+=") {
		t.Fatalf("the value %q holds a character a shell or cmd.exe reads", v)
	}
	got, err := ParseSessionCallsAgentArg(v)
	if err != nil || !reflect.DeepEqual(got, rules) {
		t.Fatalf("read back %+v, %v; want %+v", got, err, rules)
	}
	if _, err := ParseSessionCallsAgentArg("not base64!"); err == nil {
		t.Fatal("an unreadable value was read")
	}
	SetSessionCalls(nil)
	if args := SessionCallsAgentArgs(); args != nil {
		t.Fatalf("none declared: the agent is handed %v", args)
	}
}
