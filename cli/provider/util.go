package provider

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// errNoCommand is ValidateFlags' error for a `keploy record` or `keploy test`
// given no application command (no -c, no appCmd in keploy.yml). One value,
// so Validate can tell it from the rest and show the user a command line:
// it used to be built anew on each call (noCommandError), so Validate's
// `err == c.noCommandError()` compared two different values, was never true,
// and the hint under it was never printed.
var errNoCommand = errors.New("missing required -c flag or appCmd in config file")

// ErrNoTestSets is ValidateFlags' error for a `keploy test` with no keploy
// folder to replay. ValidateFlags has said so already, with the command that
// records one, so a caller returns it without saying it again; the binary's
// main exits 1 (utils.ExitKeployError) for it, as for any error a command
// returns. It used to be an os.Exit(1) inside ValidateFlags, which skipped
// every deferred call of the run: main's removal of the run's temp log among
// them, so each such run left one more keploy-logs-*.txt in the temp
// directory.
var ErrNoTestSets = errors.New("no test-sets found to replay")

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
        ▓                                         OPEN SOURCE
`

func PrintLogo(wr io.Writer, disableANSI bool) {
	if os.Getenv("BINARY_TO_DOCKER") != "true" {
		// Under the proven switch the run's output is read line by line,
		// by an agent as often as a person: the art says nothing, and the
		// version and the log's path below it are kept.
		if !provenswitch.On() {
			printKeployLogo(wr, disableANSI, Logo)
		}
		// print version to the same writer
		// A console that cannot be written to does not end the run: the
		// logo, the version and the `logs:` line are said to it as every
		// other console line is, whose write errors end nothing either. A
		// log.Fatalf here exited from inside a command, past every deferred
		// call of the run, the removal of its temp log among them.
		_, _ = fmt.Fprintf(wr, "%s: %v\n", utils.VersionIdentifier, utils.Version)
		// Where this run's log is, beside its version: a temp file at a name
		// the run picked (keploy log.LogFilePattern), which a user can find
		// only by being told.
		if utils.LogFile != nil {
			_, _ = fmt.Fprintf(wr, "logs: %s\n", utils.LogFile.Name())
		}
		_, _ = fmt.Fprintln(wr)
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

// FprintWrapper prints all its args (like fmt.Fprint) and optionally a leading
// newline. A write that fails ends nothing (PrintLogo).
func FprintWrapper(newLine bool, wr io.Writer, a ...interface{}) {
	if newLine {
		_, _ = fmt.Fprintln(wr)
	}
	if len(a) > 0 {
		_, _ = fmt.Fprint(wr, a...)
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
