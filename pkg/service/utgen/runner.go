package utgen

import (
	"bytes"
	"os/exec"
	"time"
)

// RunCommand executes a shell command in a specified working directory and returns its output, error, exit code, and the time of the executed command.
func RunCommand(command string, cwd string) (stdout string, stderr string, exitCode int, commandStartTime int64, err error) {
	// Get the current time before running the test command, in milliseconds
	commandStartTime = time.Now().UnixNano() / int64(time.Millisecond)

	// Create the command with the specified working directory
	cmd := exec.Command("sh", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Capture the stdout and stderr
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// Run the command
	err = cmd.Run()

	// Get the exit code
	exitCode = cmd.ProcessState.ExitCode()

	// Return the captured output and other information
	stdout = outBuf.String()
	stderr = errBuf.String()
	return stdout, stderr, exitCode, commandStartTime, err
}
