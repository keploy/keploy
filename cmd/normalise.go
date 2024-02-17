package cmd

import (
	"os"
	"path/filepath"

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
			path, err := cmd.Flags().GetString("path")
			if err != nil {
				n.logger.Error("Error in getting path", zap.Error(err))
				return err
			}
			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					n.logger.Error("failed to get the absolute path from relative path", zap.Error(err))
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				cdirPath, err := os.Getwd()
				if err != nil {
					n.logger.Error("failed to get the path of current directory", zap.Error(err))
				}
				path = cdirPath
			} else {
				// user provided the absolute path
			}
			path += "/keploy"
			n.normaliser.Normalise(path)
			return nil
		},
	}

	normaliseCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")
	return normaliseCmd
}
