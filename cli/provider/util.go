package provider

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"go.keploy.io/server/v3/utils"
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
		_, err := fmt.Fprintf(wr, "%s: %v\n\n", utils.VersionIdentifier, utils.Version)
		if err != nil {
			log.Fatalf("Error printing version: %v", err)
		}
	}
}

func printKeployLogo(wr io.Writer, disableANSI bool, logo string) {
	if !disableANSI {
		const orange = "\033[38;5;208m"
		const reset = "\033[0m"
		FprintWrapper(false, wr, orange, logo, reset)
		FprintWrapper(true, wr)
	} else {
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
