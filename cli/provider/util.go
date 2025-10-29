package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func (c *CmdConfigurator) noCommandError() error {
	return errors.New("missing required -c flag or appCmd in config file")
}

// alreadyRunning checks that during test mode, if user provides the basePath, then it implies that the application is already running somewhere.
func alreadyRunning(cmd, basePath string) bool {
	return (cmd == "test" && basePath != "")
}

// parseAndMountProtoPaths parses proto flags and mounts external directories for Docker
func parseAndMountProtoPaths(_ context.Context, logger *zap.Logger, cfg *config.Config, cmd *cobra.Command) error {
	// Parse proto-file flag
	protoFile, err := cmd.Flags().GetString("proto-file")
	if err != nil {
		utils.LogError(logger, err, "failed to get the proto-file flag")
		return errors.New("failed to get the proto-file flag")
	}
	if protoFile != "" {
		cfg.Test.ProtoFile, err = utils.GetAbsPath(protoFile)
		if err != nil {
			utils.LogError(logger, err, "failed to get the absolute path of proto-file")
			return errors.New("failed to get the absolute path of proto-file")
		}
	}

	// Parse proto-dir flag
	protoDir, err := cmd.Flags().GetString("proto-dir")
	if err != nil {
		utils.LogError(logger, err, "failed to get the proto-dir flag")
		return errors.New("failed to get the proto-dir flag")
	}
	if protoDir != "" {
		cfg.Test.ProtoDir, err = utils.GetAbsPath(protoDir)
		if err != nil {
			utils.LogError(logger, err, "failed to get the absolute path of proto-dir")
			return errors.New("failed to get the absolute path of proto-dir")
		}
	}

	// Parse proto-include flag
	protoInclude, err := cmd.Flags().GetStringArray("proto-include")
	if err != nil {
		utils.LogError(logger, err, "failed to get the proto-include flag")
		return errors.New("failed to get the proto-include flag")
	}
	if len(protoInclude) > 0 {
		cfg.Test.ProtoInclude = []string{} // Reset to avoid duplicates
		for _, dir := range protoInclude {
			absDir, err := utils.GetAbsPath(dir)
			if err != nil {
				utils.LogError(logger, err, "failed to get the absolute path of proto-include")
				return errors.New("failed to get the absolute path of proto-include")
			}
			cfg.Test.ProtoInclude = append(cfg.Test.ProtoInclude, absDir)
		}
	}

	// Mount external proto paths for Docker
	return mountExternalProtoPaths(logger, cfg)
}

// mountExternalProtoPaths checks if proto paths are outside the current working directory
// and adds them to DockerConfig.VolumeMounts so they can be accessed in Docker containers
func mountExternalProtoPaths(logger *zap.Logger, cfg *config.Config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	// Collect all proto paths
	protoPaths := []struct {
		path   string
		isFile bool
	}{
		{cfg.Test.ProtoFile, true},
		{cfg.Test.ProtoDir, false},
	}
	for _, p := range cfg.Test.ProtoInclude {
		protoPaths = append(protoPaths, struct {
			path   string
			isFile bool
		}{p, false})
	}

	// Track unique directories to mount
	pathsToMount := make(map[string]bool)

	for _, proto := range protoPaths {
		if proto.path == "" {
			continue
		}

		// For files, mount the directory containing the file
		dirToMount := proto.path
		if proto.isFile {
			dirToMount = filepath.Dir(proto.path)
		}

		// Check if outside current working directory
		if isPathOutsideCwd(cwd, dirToMount) {
			pathsToMount[dirToMount] = true
		}
	}

	// Add unique paths to DockerConfig.VolumeMounts
	addVolumeMounts(logger, pathsToMount)
	return nil
}

// isPathOutsideCwd checks if a path is outside the current working directory
func isPathOutsideCwd(cwd, path string) bool {
	relPath, err := filepath.Rel(cwd, path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(relPath, "..")
}

// addVolumeMounts adds paths to DockerConfig.VolumeMounts if not already present
func addVolumeMounts(logger *zap.Logger, paths map[string]bool) {
	for path := range paths {
		volumeMount := path + ":" + path
		if !volumeMountExists(volumeMount) {
			DockerConfig.VolumeMounts = append(DockerConfig.VolumeMounts, volumeMount)
			logger.Info("Mounting external proto path", zap.String("path", path))
		}
	}
}

// volumeMountExists checks if a volume mount already exists in DockerConfig
func volumeMountExists(volumeMount string) bool {
	for _, existingMount := range DockerConfig.VolumeMounts {
		if existingMount == volumeMount {
			return true
		}
	}
	return false
}

var Logo = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func PrintLogo(wr io.Writer, disableANSI bool) {
	if os.Getenv("BINARY_TO_DOCKER") != "true" {
		printKeployLogo(wr, disableANSI, Logo)
		// print version to the same writer
		_, err := fmt.Fprintf(wr, "%s: %v\n\n", utils.VersionIdenitfier, utils.Version)
		if err != nil {
			log.Fatalf("Error printing version: %v", err)
		}
	}
}

func printKeployLogo(wr io.Writer, disableANSI bool, logo string) {
	const reset = "\033[0m"
	lines := strings.Split(logo, "\n")

	if !disableANSI {
		for i, line := range lines {
			for j, ch := range line {
				color := getLogoColor(i, j)
				// wrapper now uses fmt.Fprint, so this will correctly print color + char + reset
				FprintWrapper(false, wr, color, string(ch), reset)
			}
			FprintWrapper(true, wr) // newline after each line
		}
	} else {
		// plain logo (no per-char coloring)
		FprintWrapper(false, wr, logo)
		FprintWrapper(true, wr)
	}
}

// FprintWrapper prints all its args (like fmt.Fprint) and optionally a leading newline.
func FprintWrapper(newLine bool, wr io.Writer, a ...interface{}) {
	if newLine {
		if _, err := fmt.Fprintln(wr); err != nil {
			log.Fatalf("Error printing newline: %v", err)
		}
	}
	if len(a) > 0 {
		if _, err := fmt.Fprint(wr, a...); err != nil {
			log.Fatalf("Error printing output: %v", err)
		}
	}
}

// Get the color for the logo at position (i, j)
func getLogoColor(i, j int) string {
	gradientColors := []string{
		"\033[38;5;202m", // Dark Orange
		"\033[38;5;208m",
		"\033[38;5;214m", // Light Orange
		"\033[38;5;226m", // Light Yellow
	}

	switch {
	case i <= 5:
		return gradientColors[0]
	case i == 6 && j <= 42:
		return gradientColors[1]
	case i == 7 && j <= 49:
		return gradientColors[2]
	case j <= 38:
		return gradientColors[3]
	default:
		return gradientColors[0]
	}
}
