package agent

import (
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// steeringHooks are hooks that steer the application's ports, or not.
type steeringHooks struct {
	coreAgent.Hooks
	steers bool
}

func (h steeringHooks) IngressSteers() bool { return h.steers }

// The agent steers where its hooks do; hooks that cannot say do not.
func TestTheAgentSteersWhereItsHooksDo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks coreAgent.Hooks
		want  bool
	}{
		{"steering hooks", steeringHooks{steers: true}, true},
		{"hooks that move the binds", steeringHooks{steers: false}, false},
		{"hooks that cannot say", struct{ coreAgent.Hooks }{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Agent{Hooks: tc.hooks}).IngressSteers(); got != tc.want {
				t.Fatalf("IngressSteers() = %v, want %v", got, tc.want)
			}
		})
	}
}
