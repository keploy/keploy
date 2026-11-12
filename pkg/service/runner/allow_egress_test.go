package integration

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// egressSetupInstr keeps the options the runner set its agent up with.
type egressSetupInstr struct {
	setupInstr
	setups int
	opts   models.SetupOptions
}

func (s *egressSetupInstr) Setup(_ context.Context, _ string, opts models.SetupOptions) error {
	s.setups++
	s.opts = opts
	return nil
}

// The runner's agent (enterprise's test sandbox sets one up through it) is a
// replay agent, so it gets the user's real-egress opt-ins exactly as keploy
// test's does: test.allowEgress and --global-passthrough. Without them it
// would forward no DNS miss even where the user named "dns".
func TestTheRunnersReplayAgentGetsTheEgressOptIns(t *testing.T) {
	cases := []struct {
		name        string
		allow       []string
		passthrough bool
	}{
		{name: "none named: nothing opened"},
		{name: "test.allowEgress", allow: []string{models.AllowEgressDNS, "imds"}},
		{name: "--global-passthrough", passthrough: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.AllowEgress = tc.allow
			cfg.Record.GlobalPassthrough = tc.passthrough
			instr := &egressSetupInstr{}
			r := &Runner{
				logger:          zap.NewNop(),
				instrumentation: instr,
				config:          cfg,
				mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
				mockDB:          &skippingMockDB{},
			}
			setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
			if err != nil {
				t.Fatalf("setupTestSet: %v", err)
			}
			defer setup.cleanup()

			if instr.setups != 1 {
				t.Fatalf("the agent was set up %d time(s); want 1", instr.setups)
			}
			if instr.opts.Mode != models.MODE_TEST {
				t.Fatalf("the agent was set up in mode %q; want %q", instr.opts.Mode, models.MODE_TEST)
			}
			if !slices.Equal(instr.opts.AllowEgress, tc.allow) {
				t.Fatalf("the agent got AllowEgress %v; want %v", instr.opts.AllowEgress, tc.allow)
			}
			if instr.opts.GlobalPassthrough != tc.passthrough {
				t.Fatalf("the agent got GlobalPassthrough %v; want %v", instr.opts.GlobalPassthrough, tc.passthrough)
			}
		})
	}
}
