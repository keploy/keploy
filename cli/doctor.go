package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func init() {
	Register("doctor", DoctorCommand)
}

func DoctorCommand(ctx context.Context, logger *zap.Logger, conf *config.Config, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate Keploy environment setup",
		Run: func(cmd *cobra.Command, args []string) {
			runDoctorChecks(logger)
		},
	}

	return cmd
}

ffunc runDoctorChecks(logger *zap.Logger) {

	fmt.Println("🔎 Running Keploy Environment Checks...\n")

	total := 0
	passed := 0

	// OS
	total++
	fmt.Printf("✔ OS: %s\n", runtime.GOOS)
	passed++
	if runtime.GOOS == "windows" {
	fmt.Println("⚠ Windows detected. Ensure WSL or Docker Desktop is properly configured.")
}

	// Go
	total++
    if checkCommand("docker", "--version") {
	if checkDockerDaemon() {
		passed++
	}
}

	// Docker
	total++
	if checkCommand("docker", "--version") {
		passed++
	}

	// Git
	total++
	if checkCommand("git", "--version") {
		passed++
	}
	// Keploy Binary Check
    total++
    if checkCommand("keploy", "--version") {
	passed++
}
   // Port Check
   total++
   if checkPortFree("8080") {
	passed++
}

	fmt.Println("\n-----------------------------------")
	fmt.Printf("Checks Passed: %d/%d\n", passed, total)

	if passed == total {
		fmt.Println("✅ Your environment is fully ready for Keploy!")
	} else {
		fmt.Println("⚠ Some checks failed. Please fix the above issues.")
	}
}

func checkCommand(name string, arg string) bool {
	cmd := exec.Command(name, arg)
	output, err := cmd.CombinedOutput()

	if err != nil {
		fmt.Printf("❌ %s not found or not working\n", name)
		return false
	}

	fmt.Printf("✔ %s detected\n", name)
	return true
}