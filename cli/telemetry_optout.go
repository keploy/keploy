package cli

import "github.com/spf13/cobra"

// telemetryOptOutHonorer is a configurator that can give the run's config the
// user's telemetry opt-out from cmd's flags and environment.
type telemetryOptOutHonorer interface {
	HonorTelemetryOptOut(cmd *cobra.Command)
}

// honorTelemetryOptOut gives the run's config (the configurator's, which the
// service factory shares) the user's opt-out, for a command that runs no
// Validate: config, update, export and import. PreProcessFlags is what reads
// --disable-tele and KEPLOY_DISABLETELE into it, and those commands never run
// it, so they pinged telemetry, and resolved and persisted an installation
// id, for a user who had opted out. A configurator that cannot (one another
// binary built) is left to decide it itself.
func honorTelemetryOptOut(cmd *cobra.Command, c CmdConfigurator) {
	if h, ok := c.(telemetryOptOutHonorer); ok {
		h.HonorTelemetryOptOut(cmd)
	}
}
