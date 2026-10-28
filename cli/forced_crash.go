//go:build !keploy_e2e_crash

package cli

import "github.com/spf13/cobra"

// armForcedCrash does nothing in a keploy build: only a build with the
// keploy_e2e_crash tag can be made to crash on purpose (forced_crash_e2e.go).
func armForcedCrash(*cobra.Command) {}
