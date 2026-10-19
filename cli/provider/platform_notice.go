package provider

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// platformNotice is what a run on a platform that stays out of the proven
// model's flip says (provenswitch.PlatformNotice). A variable only so a test
// on Linux can see where keploy says it.
var platformNotice = provenswitch.PlatformNotice

// recordsOrJudges reports whether cmd records or judges a run, the commands
// whose results the proven model decides: keploy record and keploy test, and
// keploy mock record and replay. keploy contract test judges no recording.
func recordsOrJudges(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "record", "replay":
		return true
	case "test":
		return cmd.Parent() == nil || cmd.Parent().Name() != "contract"
	}
	return false
}
