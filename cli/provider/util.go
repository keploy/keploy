package provider

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

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

// mountPathIfExternal mounts a path if it's outside the current working directory
// isFile indicates whether the path points to a file (if true, mount its parent directory)
// path is expected to be an absolute path
// pathType is used for logging (e.g., "proto", "config")
func mountPathIfExternal(logger *zap.Logger, path string, isFile bool, pathType string) error {
	if path == "" {
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	// For files, mount the directory containing the file
	dirToMount := path
	if isFile {
		dirToMount = filepath.Dir(path)
	}

	// Ensure dirToMount is absolute
	if !filepath.IsAbs(dirToMount) {
		dirToMount, err = filepath.Abs(dirToMount)
		if err != nil {
			return fmt.Errorf("failed to get absolute path for %s: %w", dirToMount, err)
		}
	}

	// Check if outside current working directory
	if isPathOutsideCwd(cwd, dirToMount) {
		volumeMount := dirToMount + ":" + dirToMount
		if !volumeMountExists(volumeMount) {
			DockerConfig.VolumeMounts = append(DockerConfig.VolumeMounts, volumeMount)
			logger.Debug(fmt.Sprintf("Mounting external %s path", pathType), zap.String("path", dirToMount))
		}
	}

	return nil
}

// isPathOutsideCwd checks if a path is outside the current working directory
// by comparing absolute paths - if the path doesn't have cwd as a prefix, it's outside
func isPathOutsideCwd(cwd, path string) bool {
	// Ensure both paths are absolute and clean
	absCwd := filepath.Clean(cwd)
	absPath := filepath.Clean(path)

	if !strings.HasSuffix(absCwd, string(filepath.Separator)) {
		absCwd += string(filepath.Separator)
	}

	// If path starts with cwd, it's inside; otherwise, it's outside
	return !strings.HasPrefix(absPath+string(filepath.Separator), absCwd)
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
