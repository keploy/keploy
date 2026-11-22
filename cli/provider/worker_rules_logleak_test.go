package provider

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/logleak"
)

// A worker-mock rule keploy cannot apply is named, in keploy record's
// refusal and in every other command's warning, by its number and what is
// wrong with it, never its id or pattern as written: keploy.yaml's rules can
// hold a path or a value (a tenant, a token in a path regex), and both the
// refusal and the warning are logged.
func TestAWorkerMockRuleIsRefusedWithoutItsContent(t *testing.T) {
	t.Cleanup(viper.Reset)
	const s = logleak.Canary
	yml := "record:\n  workerMocks:\n    - id: \"Bad " + s + "\"\n      kind: http\n      match:\n        pathRegex: \"^/v1/(" + s + "\"\n"
	run := func(t *testing.T, cmd *cobra.Command, cfg *config.Config) error {
		t.Helper()
		viper.Reset()
		if err := cmd.ParseFlags([]string{"--config-path", writeKeployYML(t, yml)}); err != nil {
			t.Fatal(err)
		}
		logger, logs := logleak.Observe()
		c := NewCmdConfigurator(logger, cfg)
		if err := c.PreProcessFlags(cmd); err != nil {
			t.Fatalf("PreProcessFlags: %v", err)
		}
		err := c.ValidateFlags(context.Background(), cmd)
		logleak.Check(t, logs, s, "the worker-mock rules' validation")
		if cmd.Name() != "record" && logs.FilterMessageSnippet("record.workerMocks in keploy.yaml has rules").Len() != 1 {
			t.Fatalf("precondition: the warning was not logged (%d lines)", logs.Len())
		}
		return err
	}
	cfg := config.New()
	err := run(t, newRecordCmdForTest(t, cfg), cfg)
	if err == nil {
		t.Fatal("precondition: keploy record took a rule it cannot apply")
	}
	if logleak.Holds(err.Error(), s) {
		t.Errorf("keploy record's refusal holds the rule's content (%d bytes)", len(err.Error()))
	}
	cfg = config.New()
	_ = run(t, newTestCmdForTest(t, cfg), cfg)
}
