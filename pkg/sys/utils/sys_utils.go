package utils

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// FindPidFromPort invokes the command "lsof -i:port" on the underlying OS, parses the output
// and returns the PID of the only entry in the list.
// It returns an error if there are more than one entry in the list, or if the OS doesn't support
// the command, or if the command hangs for more than 30 seconds.
func FindPidFromPort(port uint16) (uint32, error) {
	// We allow at most 30 seconds for the command to run. If it takes more than that, we should kill it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", fmt.Sprintf("-i:%d", port))

	cmdStdOut := new(bytes.Buffer)
	cmdStdErr := new(bytes.Buffer)
	cmd.Stdout = cmdStdOut
	cmd.Stderr = cmdStdErr

	// Ensure that the command doesn't run for more than 30 seconds,
	// and the OS doesn't reject the command.
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("lsof -i:%d failed due to error %v", port, err)
	}

	return ParseOutputOfLSOF(cmdStdOut, cmdStdErr)
}

// ParseOutputOfLSOF parses the output of LSOF and extracts the unique PID.
func ParseOutputOfLSOF(cmdStdOut, cmdStdErr *bytes.Buffer) (uint32, error) {
	// Read the error stream and ensure that it's empty.
	if errorInfo := cmdStdErr.String(); errorInfo != "" {
		return 0, fmt.Errorf(errorInfo)
	}

	// Parse the output
	// A sample output would look like:
	//
	// COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
	// main    5044 aerowisca    3u  IPv6  73427      0t0  TCP *:tproxy (LISTEN)
	//
	// Hence, we need to ensure that there are 2 lines, and pick the first integer in second line.
	output := strings.TrimSpace(cmdStdOut.String())
	lines := strings.Split(output, "\n")
	if len(lines) != 2 {
		return 0, fmt.Errorf("lsof does not return exactly one PID. Output \n%s", output)
	}

	// Split the second line on spaces, after removing the redundant whitespaces.
	secondLine := strings.Fields(lines[1])
	if len(secondLine) < 2 {
		return 0, fmt.Errorf("could not get enough info to extract pid. Output of lsof \n%s", output)
	}

	pid, err := strconv.ParseUint(secondLine[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("could not parse pid due to error [%v]. Output of lsof \n%s", err, output)
	}

	return uint32(pid), nil
}
