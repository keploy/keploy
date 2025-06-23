package provider

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"go.keploy.io/server/v2/utils"
)

func (c *CmdConfigurator) noCommandError() error {
	return errors.New("missing required -c flag or appCmd in config file")
}

// alreadyRunning checks that during test mode, if user provides the basePath, then it implies that the application is already running somewhere.
func alreadyRunning(cmd, basePath string) bool {
	return (cmd == "test" && basePath != "")
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
		fmt.Fprintf(wr, "%s: %v\n\n", utils.VersionIdenitfier, utils.Version)
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
