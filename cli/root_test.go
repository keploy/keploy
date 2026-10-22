package cli

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// A registered command that exists only in some processes (keploy
// enterprise's `keploy quarantine` exists only under the proven model) says
// so when Root builds the root, after every init has run, by building
// nothing: Root adds no command for it, and every other registered command
// is still there. Deciding at init read the switch before keploy enterprise
// had said it runs release 1's flow, so the verb's existence hung on the
// order of file names. Mutation: Root adds what a hook returns, nil included
// (cobra panics on it).
func TestRootAddsNoCommandForAHookThatBuildsNone(t *testing.T) {
	saved := Registered
	t.Cleanup(func() { Registered = saved })
	Registered = nil
	Register("only-sometimes", func(context.Context, *zap.Logger, *config.Config, ServiceFactory, CmdConfigurator) *cobra.Command {
		return nil
	})
	Register("always", func(context.Context, *zap.Logger, *config.Config, ServiceFactory, CmdConfigurator) *cobra.Command {
		return &cobra.Command{Use: "always", RunE: func(*cobra.Command, []string) error { return nil }}
	})
	var root *cobra.Command
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Root panicked on a hook that built no command: %v", p)
			}
		}()
		root = Root(context.Background(), zap.NewNop(), agentSvcFactory{}, noFlags{})
	}()
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
	}
	if len(names) != 1 || names[0] != "always" {
		t.Fatalf("root's commands = %q, want only %q", names, "always")
	}
}
