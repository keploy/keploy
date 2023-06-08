package hooks

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/docker/docker/client"
)

func (h *Hook) LaunchUserApplication(appCmd, appContainer string) error {

	// Supports Linux, Windows
	if dCmd := isDockerCmd(appCmd); dCmd {
		// check if appContainer name if provided by the user
		if len(appContainer) == 0 {
			appContainer = parseContainerName(appCmd)
		}

		pids := getNameSpacePIDs(appContainer)

	} else { //Supports only linux

	}

}

func runApp(appCmd string) error {
	cmd := exec.Command("sh", "-c", appCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func isDockerCmd(cmd string) bool {
	// Parse the command and check if its docker cmd
	return false
}

func parseContainerName(cmd string) string {
	containerName := ""

	//parse the container Name from cmd, if failed then panic.

	return containerName
}

func getNameSpacePIDs(containerName string) *[15]int32 {
	// Create a new Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("failed to make a docker client", err)
	}

	// Create a new context
	ctx := context.Background()

	// Inspect the specified container
	inspect, err := cli.ContainerInspect(ctx, containerName)
	if err != nil {
		log.Fatalf("failed to inspect container:%v", containerName, err)
	}

	containerID := inspect.ID

	// Retrieve the PIDs of the container's processes
	processes, err := cli.ContainerTop(context.Background(), containerID, []string{})
	if err != nil {
		log.Fatalf("failed to retrieve processes inside the app container", err)
	}

	if len(processes.Processes) > 12 {
		log.Fatalf("Error: More than 12 processes are running inside the application container.")
	}

	var pids [15]int32

	for i := 0; i < len(pids); i++ {
		pids[i] = -1
	}

	// Extract the PIDs from the processes
	for idx, process := range processes.Processes {

		pid, err := parseToInt32(process[1])
		if err != nil {
			log.Fatalf("failed to convert the process id [%v] from string to int32", process[1])
		}
		pids[idx] = pid
	}

	// Print the PIDs
	for _, pid := range pids {
		fmt.Println(pid)
	}
	return &pids
}

func parseToInt32(str string) (int32, error) {
	num, err := strconv.ParseInt(str, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(num), nil
}

func getChildPids(pid int) ([]int, error) {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}

	s := strings.TrimSuffix(string(out), "\n")
	if s == "" {
		return nil, nil
	}

	var pids []int
	for _, pidStr := range strings.Split(s, "\n") {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, nil
}
