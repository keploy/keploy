package provider

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// HonorTelemetryOptOut turns telemetry off in c's config when cmd was given
// --disable-tele, or KEPLOY_DISABLETELE is true, for a command that runs no
// PreProcessFlags (cli's honorTelemetryOptOut). It only ever turns telemetry
// off, and writes nothing: keploy.yml is not read here, nor created.
func (c *CmdConfigurator) HonorTelemetryOptOut(cmd *cobra.Command) {
	if c == nil || c.cfg == nil || c.cfg.DisableTele {
		return
	}
	if f := cmd.Flags().Lookup("disable-tele"); f != nil && f.Changed {
		if v, err := strconv.ParseBool(f.Value.String()); err == nil && v {
			c.cfg.DisableTele = true
		}
		return
	}
	if v, err := strconv.ParseBool(os.Getenv("KEPLOY_DISABLETELE")); err == nil && v {
		c.cfg.DisableTele = true
	}
}
