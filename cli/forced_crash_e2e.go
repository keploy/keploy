//go:build keploy_e2e_crash

package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ForcedCrashEnv names the command a keploy_e2e_crash build panics in.
const ForcedCrashEnv = "KEPLOY_E2E_CRASH"

// armForcedCrash makes the command $KEPLOY_E2E_CRASH names by its path below
// the root (record, agent, mock record, ...) panic as its run starts, after its PreRunE has read the flags and
// keploy.yml: an e2e's way to crash keploy on purpose and watch what the crash
// sends (disable_tele's crash legs). Only a build with the keploy_e2e_crash
// tag has it; a release build cannot be made to crash so.
func armForcedCrash(root *cobra.Command) {
	name := os.Getenv(ForcedCrashEnv)
	if name == "" {
		return
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if strings.TrimPrefix(c.CommandPath(), root.Name()+" ") == name && (c.RunE != nil || c.Run != nil) {
			c.Run = nil
			c.RunE = func(*cobra.Command, []string) error {
				panic("keploy e2e: forced crash in " + name + " (" + ForcedCrashEnv + ")")
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
