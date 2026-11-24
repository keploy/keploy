package models

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// SessionCallsAgentFlag is the hidden `keploy agent` flag that hands an agent
// the calls the keploy that starts it declares session calls
// (SetSessionCalls, keploy.yaml sessionCalls). Under the proven switch the
// agent reads them itself: which recording made outside every test a test
// is served (Mock.DeclaredSessionCall), and which plain startup: recording a
// test's call is its own call of (agent.backgroundRecordingsOf). An agent
// that reads no keploy.yaml (an agent container, which mounts no project;
// one started on another machine) would else declare none, and a call its
// user declared would fail the test all the same, its miss telling the user
// to declare the call already declared.
//
// It is named apart from keploy.yaml's sessionCalls key, which the flags
// bound to the configuration would else overwrite with this one's text.
const SessionCallsAgentFlag = "declared-session-calls"

// SessionCallsAgentArgs is the argument that hands the declared session
// calls to an agent (SessionCallsAgentFlag), under the proven switch alone,
// where the agent reads them: --declared-session-calls=<their JSON,
// base64url>, one word with no character a shell or cmd.exe reads. None
// with the switch off or none declared, so an agent's command is as before
// then.
func SessionCallsAgentArgs() []string {
	if !provenswitch.On() {
		return nil
	}
	rules := sessionCalls.Load()
	if rules == nil || len(*rules) == 0 {
		return nil
	}
	b, err := json.Marshal(*rules)
	if err != nil {
		return nil
	}
	return []string{"--" + SessionCallsAgentFlag + "=" + base64.RawURLEncoding.EncodeToString(b)}
}

// ParseSessionCallsAgentArg reads SessionCallsAgentArgs' value back.
func ParseSessionCallsAgentArg(v string) ([]SessionCallRule, error) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("the --%s value is not base64url: %w", SessionCallsAgentFlag, err)
	}
	var rules []SessionCallRule
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, fmt.Errorf("the --%s value holds no session calls: %w", SessionCallsAgentFlag, err)
	}
	return rules, nil
}
