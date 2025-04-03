package provider

import (
	"errors"
	"fmt"
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

func PrintLogo(disableANSI bool) {
	fmt.Printf("Enterprise binary sync testing")
	if binaryToDocker := os.Getenv("BINARY_TO_DOCKER"); binaryToDocker != "true" {
		printKeployLogo(disableANSI, Logo)
		fmt.Printf("%s: %v\n\n", utils.VersionIdenitfier, utils.Version)
	}
}

// PrintKeployLogo prints the Keploy logo to the console, optionally with a gradient color effect.
func printKeployLogo(useColor bool, logo string) {
	// ANSI escape code to reset color
	const reset = "\033[0m"
	if !useColor {
		// Print each line of the logo
		lines := strings.Split(logo, "\n")
		for i, line := range lines {
			for j, char := range line {
				var color = getLogoColor(i, j)

				// Print each character
				fmt.Print(color, string(char), reset)
			}
			fmt.Println()
		}
	} else {
		fmt.Print(logo)
		fmt.Println()
	}

}

// Get the color for the logo at position (i, j)
func getLogoColor(i, j int) string {
	// Orange to Yellow gradient colors (reversed order)
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
