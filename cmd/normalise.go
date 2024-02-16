package cmd

import (
	"github.com/spf13/cobra"
	normalise "go.keploy.io/server/pkg/service/normalise"
	"go.uber.org/zap"
)

// NewCmdNormalise initializes a new command to update the Keploy binary file.
func NewCmdNormalise(logger *zap.Logger) *Normalise {
	normaliser := normalise.NewNormaliser(logger)
	return &Normalise{
		normaliser: normaliser,
		logger:     logger,
	}
}

// Normalise holds the normaliser instance for managing binary updates.
type Normalise struct {
	normaliser normalise.Normaliser
	logger     *zap.Logger
}

// GetCmd retrieves the command to normalise Keploy
func (n *Normalise) GetCmd() *cobra.Command {
	var normaliseCmd = &cobra.Command{
		Use:     "normalise",
		Short:   "Normalise Keploy",
		Example: "keploy normalise",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logic to normalise testcases
			n.normaliser.Normalise()
			return nil
		},
	}
	return normaliseCmd
}
