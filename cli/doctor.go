package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func init() {
	Register("doctor", DoctorCommand)
}

func DoctorCommand(
	ctx context.Context,
	logger *zap.Logger,
	conf *config.Config,
	svcFactory ServiceFactory,
	cmdConfigurator CmdConfigurator,
) *cobra.Command {

	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate Keploy environment setup",
		Run: func(cmd *cobra.Command, args []string) {
			runDoctorChecks(logger, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results in JSON format")

	return cmd
}

type DoctorResult struct {
	OS              string `json:"os"`
	GoInstalled     bool   `json:"go_installed"`
	DockerInstalled bool   `json:"docker_installed"`
	GitInstalled    bool   `json:"git_installed"`
	Port8080Free    bool   `json:"port_8080_available"`
	Summary         struct {
		Total  int    `json:"total_checks"`
		Passed int    `json:"passed"`
		Status string `json:"status"`
	} `json:"summary"`
}

func runDoctorChecks(logger *zap.Logger, jsonOutput bool) {

	result := DoctorResult{}
	result.OS = runtime.GOOS

	total := 0
	passed := 0

	if !jsonOutput {
		fmt.Println("🔎 Running Keploy Environment Checks...\n")
	}

	// OS Check
	total++
	passed++

	if runtime.GOOS == "windows" && !jsonOutput {
		fmt.Println("⚠ Windows detected. Ensure WSL or Docker Desktop is properly configured.")
	}

	// Go Check
	total++
	if checkCommand("go", "version") {
		result.GoInstalled = true
		passed++
	}

	// Docker Check
	total++
	if checkCommand("docker", "--version") {
		result.DockerInstalled = true
		passed++
	}

	// Git Check
	total++
	if checkCommand("git", "--version") {
		result.GitInstalled = true
		passed++
	}

	// Keploy Binary Check
	total++
	if checkCommand("keploy", "--version") {
		passed++
	}

	// Port 8080 Check
	total++
	if checkPortFree("8080") {
		result.Port8080Free = true
		passed++
	}

	// Summary
	result.Summary.Total = total
	result.Summary.Passed = passed

	if passed == total {
		result.Summary.Status = "healthy"
	} else {
		result.Summary.Status = "unhealthy"
	}

	// JSON Mode
	if jsonOutput {
		output, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			logger.Error("failed to marshal doctor result", zap.Error(err))
			return
		}
		fmt.Println(string(output))
		return
	}

	// Default Human-readable Output
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
	err := cmd.Run()

	if err != nil {
		fmt.Printf("❌ %s not found or not working\n", name)
		return false
	}

	fmt.Printf("✔ %s detected\n", name)
	return true
}

func checkPortFree(port string) bool {
	conn, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Printf("❌ Port %s is not available\n", port)
		return false
	}
	defer conn.Close()

	fmt.Printf("✔ Port %s is available\n", port)
	return true
}