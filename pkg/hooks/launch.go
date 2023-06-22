package hooks

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

var (
	dockerClient     *client.Client
	appDockerNetwork string
	appContainerName string
)

func (h *Hook) LaunchUserApplication(appCmd, appContainer, appNetwork string, Delay uint64) error {

	var pids [15]int32
	// Supports Linux, Windows
	ok, cmd := h.IsDockerRelatedCmd(appCmd)
	if ok {
		println("Running user application on Docker")
		println("given containerName length", len(appContainer))
		println("given network name length", len(appNetwork))

		if cmd == "docker-compose" {
			if len(appContainer) == 0 {
				h.logger.Error("please provide container name in case of docker-compose file", zap.Any("AppCmd", appCmd))
				return fmt.Errorf("container name not found")
			}

			if len(appNetwork) == 0 {
				h.logger.Error("please provide docker network name in case of docker-compose file", zap.Any("AppCmd", appCmd))
				return fmt.Errorf("docker network name not found")
			}
		}

		var err error
		appContainerName, appDockerNetwork, err = parseDockerCommand(appCmd)
		println("parsed Containername", appContainerName)
		println("parsed DockerNetwork", appDockerNetwork)
		if err != nil {
			h.logger.Error("failed to parse container or network name from given docker command", zap.Error(err), zap.Any("AppCmd", appCmd))
			return err
		}

		if len(appContainer) == 0 {

			appContainer = appContainerName
		}

		errCh := make(chan error, 1)
		go func() {
			err := h.runApp(appCmd, ok)
			errCh <- err
		}()

		println("Waiting for any error from user application")
		time.Sleep(time.Duration(Delay) * time.Second)
		println("After running application")

		// Check if there is an error in the channel without blocking
		select {
		// channel for only checking for error during this instant looks
		// like an overkill. TODO refactor
		case err := <-errCh:
			if err != nil {
				h.logger.Error("failed to launch the user application", zap.Any("err", err.Error()))
				return err
			}
		default:
			// No error received yet, continue with further flow
		}

		println("Now setting app pids")

		dockerClient = makeDockerClient()
		nsPids, hostPids := getAppNameSpacePIDs(appContainer)

		println("Namespace PIDS of application:--->")
		pids = nsPids
		for i, v := range pids {
			println("nsPid-", i, ":", v)
		}

		inode := getInodeNumber(hostPids)
		println("INODE Number:", inode)
		h.SendNameSpaceId(0, inode)

	} else { //Supports only linux
		println("Running user application on linux")

		errCh := make(chan error, 1)
		go func() {
			err := h.runApp(appCmd, false)
			errCh <- err
		}()

		println("Waiting....")
		time.Sleep(time.Duration(Delay) * time.Second)
		println("After running application")

		// Check if there is an error in the channel without blocking
		select {
		case err := <-errCh:
			if err != nil {
				h.logger.Error("failed to launch the user application", zap.Any("err", err.Error()))
				return err
			}
		default:
			// No error received yet, continue with further flow
		}

		println("Now setting app pids")

		appPids, err := getAppPIDs(appCmd)
		if err != nil {
			h.logger.Error("failed to get the pid of user application", zap.Any("err", err.Error()))
			return err
		}

		pids = appPids
		// var pids [15]int32
		// pids[0] = value

		// for i := 1; i < 15; i++ {
		// 	pids[i] = -1
		// }

		println("PIDS of application:", pids[0])
	}

	err := h.SendApplicationPIDs(pids)
	if err != nil {
		h.logger.Error("failed to send the application pids to the ebpf program", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}

	// h.EnablePidFilter() // can enable here also

	h.logger.Info("User application started successfully")
	return nil
}

// It runs the application using the given command
func (h *Hook) runApp(appCmd string, isDocker bool) error {
	// Create a new command with your appCmd
	var cmd *exec.Cmd
	if isDocker {
		parts := strings.Fields(appCmd)
		cmd = exec.Command(parts[0], parts[1:]...)
	} else {
		cmd = exec.Command(appCmd)
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	h.userAppCmd = cmd

	// Run the command, this handles non-zero exit code get from application.
	err := cmd.Run()
	if err != nil {
		return err
	}
	//make it debug statemen
	return fmt.Errorf("user application exited with zero error code")
}

// It checks if the cmd is related to docker or not, it also returns if its a docker compose file
func (h *Hook) IsDockerRelatedCmd(cmd string) (bool, string) {
	// Check for Docker command patterns
	dockerCommandPatterns := []string{"docker ", "docker-compose "}
	for _, pattern := range dockerCommandPatterns {
		if strings.HasPrefix(strings.ToLower(cmd), pattern) {
			return true, "docker"
		}
	}

	// Check for Docker Compose file extension
	dockerComposeFileExtensions := []string{".yaml", ".yml"}
	for _, extension := range dockerComposeFileExtensions {
		if strings.HasSuffix(strings.ToLower(cmd), extension) {
			return true, "docker-compose"
		}
	}

	return false, ""
}

func parseDockerCommand(dockerCmd string) (string, string, error) {
	// Regular expression patterns
	containerNamePattern := `--name\s+([^\s]+)`
	networkNamePattern := `--network\s+([^\s]+)`

	// Extract container name
	containerNameRegex := regexp.MustCompile(containerNamePattern)
	containerNameMatches := containerNameRegex.FindStringSubmatch(dockerCmd)
	if len(containerNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse container name")
	}
	containerName := containerNameMatches[1]

	// Extract network name
	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(dockerCmd)
	if len(networkNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[1]

	return containerName, networkName, nil
}

func makeDockerClient() *client.Client {
	// Create a new Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("failed to make a docker client:", err)
	}
	return cli
}

func (h *Hook) GetUserIp(containerName, networkName string) string {

	// If both containerName, networkName are not provided then it must not be a docker compose file,
	// And it is checked at the time of launching the applicaton

	if len(containerName) == 0 {
		containerName = appContainerName
	}

	if len(networkName) == 0 {
		networkName = appDockerNetwork
	}

	cli := dockerClient
	// Create a new context
	ctx := context.Background()

	// Inspect the specified container
	inspect, err := cli.ContainerInspect(ctx, containerName)
	if err != nil {
		log.Fatalf("failed to inspect container:%v,", containerName, err)
	}

	// Find the IP address of the container in the network
	ipAddress := inspect.NetworkSettings.Networks[appDockerNetwork].IPAddress

	fmt.Printf("Container IP Address: %s\n", ipAddress)

	return ipAddress
}

func getAppNameSpacePIDs(containerName string) ([15]int32, [15]int32) {

	// Get the docker client
	cli := dockerClient

	// Create a new context
	ctx := context.Background()

	// Inspect the specified container
	inspect, err := cli.ContainerInspect(ctx, containerName)
	if err != nil {
		log.Fatalf("failed to inspect container:%v,", containerName, err)
	}

	containerID := inspect.ID

	// Retrieve the PIDs of the container's processes
	processes, err := cli.ContainerTop(context.Background(), containerID, []string{})
	if err != nil {
		log.Fatalf("failed to retrieve processes inside the app container:", err)
	}

	if len(processes.Processes) > 15 {
		log.Fatalf("Error: More than 15 processes are running inside the application container.")
	}

	var hostPids [15]int32
	var nsPids [15]int32

	for i := 0; i < len(hostPids); i++ {
		hostPids[i] = -1
		nsPids[i] = -1
	}

	// Extract the PIDs from the processes
	for idx, process := range processes.Processes {
		if len(process) < 2 {
			log.Fatalln("failed to get the process IDs from the app container")
		}

		pid, err := parseToInt32(process[1])
		if err != nil {
			log.Fatalf("failed to convert the process id [%v] from string to int32", process[1])
		}
		hostPids[idx] = pid
	}

	// for i, v := range pids {
	// 	println("[docker]:Pid-", i, ":", v)
	// }

	// time.Sleep(time.Duration(10) * time.Second)

	for i, v := range hostPids {
		if v == -1 {
			break
		}
		nsPids[i] = getNStgids(int(v))
	}

	return nsPids, hostPids
}

// This function fetches the actual pids from container point of view.
func getNStgids(pid int) int32 {
	fName := "/proc/" + strconv.Itoa(pid) + "/status"

	file, err := os.Open(fName)
	if err != nil {
		log.Fatalf("failed to open file /proc/%v/status:", pid, err)
	}
	defer file.Close()

	var lastNStgid string

	// Read the file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "NStgid:") {
			// Extract the values from the line
			// println(line)
			fields := strings.Fields(line)
			if len(fields) > 0 {
				lastNStgid = fields[len(fields)-1]
			}
		}
	}

	// Check if any error occurred during scanning the file
	if err := scanner.Err(); err != nil {
		log.Fatal("failed to scan the file:", err)
	}

	// Print the last NStgid value
	if lastNStgid == "" {
		log.Fatal("NStgid value not found in the file")
	}
	// fmt.Println("Last NStgid:", lastNStgid)
	NStgid, err := parseToInt32(strings.TrimSpace(lastNStgid))
	if err != nil {
		log.Fatalf("failed to convert the NStgid[%v] from string to int32", lastNStgid)
	}

	return NStgid
}

func parseToInt32(str string) (int32, error) {
	num, err := strconv.ParseInt(str, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(num), nil
}

// An application can launch as many child processes as it wants but here we are only handling for 15 child process.
func getAppPIDs(appCmd string) ([15]int32, error) {
	// Getting pid of the command
	cmdPid, err := getCmdPid(appCmd)
	if err != nil {
		return [15]int32{}, fmt.Errorf("failed to get the pid of the running command: %v\n", err)
	}

	if cmdPid == 0 {
		return [15]int32{}, fmt.Errorf("The command '%s' is not running.: %v\n", appCmd, err)
	} else {
		// log.Printf("The command '%s' is running with PID: %d\n", appCmd, pid)
	}

	// for i, pid := range cmdPids {

	// 	println("PID-", i, ":", pid)
	// }

	const maxChildProcesses = 15
	var pids [maxChildProcesses]int32

	for i := 0; i < len(pids); i++ {
		pids[i] = -1
	}
	pids[0] = int32(cmdPid)
	// for i, pid := range cmdPids {

	// 	if i >= maxChildProcesses {
	// 		log.Fatalf("Error: More than %d child processes of cmd:[%v] are running", maxChildProcesses, appCmd)
	// 	}
	// 	pids[i] = pid
	// 	println("PID-", i, ":", pids[i])
	// }

	return pids, nil

	//---------------------------------------------------------------//
	// Getting child pids
	// cmdPid := "/usr/bin/sudo /usr/bin/pgrep -P " + strconv.Itoa(pid)
	// out, err := exec.Command("sh", "-c", cmdPid).Output()
	// if err != nil {
	// 	log.Fatal("failed to get the application pid:", err)
	// }

	// s := strings.TrimSuffix(string(out), "\n")
	// if s == "" {
	// 	log.Fatal("unable to retrieve the application pid")
	// }

	// const maxChildProcesses = 15
	// var pids [maxChildProcesses]int32

	// for i := 0; i < len(pids); i++ {
	// 	pids[i] = -1
	// }

	// for i, pidStr := range strings.Split(s, "\n") {
	// 	pid, err := strconv.Atoi(pidStr)
	// 	if err != nil {
	// 		log.Fatalf("failed to convert the process id [%v] from string to int", pidStr)
	// 	}
	// 	if i >= maxChildProcesses {
	// 		log.Fatalf("Error: More than %d child processes of cmd:[%v] are running", maxChildProcesses, appCmd)
	// 	}
	// 	pids[i] = int32(pid)
	// 	println("PID-", i, ":", pids[i])
	// }
	// return pids
}

func getCmdPid(commandName string) (int, error) {
	cmd := exec.Command("pidof", commandName)

	println("Executing the command....")
	output, err := cmd.Output()
	if err != nil {
		fmt.Errorf("failed to execute the command: %v", commandName)
		return 0, err
	}

	println("pidof the cmd is:", string(output))
	pidStr := strings.TrimSpace(string(output))

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Errorf("failed to convert the process id [%v] from string to int", pidStr)
		return 0, err
	}

	return pid, nil
}

func getInodeNumber(pids [15]int32) uint64 {

	for _, pid := range pids {
		println("Checking Inode for pid:", pid)

		filepath := filepath.Join("/proc", strconv.Itoa(int(pid)), "ns", "pid")

		f, err := os.Stat(filepath)
		if err != nil {
			log.Fatal("failed to get the inode number or namespace Id:", err)
		}
		// Dev := (f.Sys().(*syscall.Stat_t)).Dev
		Ino := (f.Sys().(*syscall.Stat_t)).Ino
		if Ino != 0 {
			return Ino
		}
	}
	return 0
}

func getSelfInodeNumber() uint64 {
	println("Checking self Inode for keploy")

	filepath := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(filepath)
	if err != nil {
		log.Fatal("failed to get the self inode number or namespace Id:", err)
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	Ino := (f.Sys().(*syscall.Stat_t)).Ino
	if Ino != 0 {
		return Ino
	}
	return 0
}
