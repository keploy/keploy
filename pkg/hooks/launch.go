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

	"github.com/cilium/ebpf"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
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
		h.logger.Debug(Emoji + "Running user application on Docker")

		// to notify the kernel hooks that the user application command is related to Docker.
		key := 0
		value := true
		h.objects.DockerCmdMap.Update(uint32(key), &value, ebpf.UpdateAny)

		if cmd == "docker-compose" {
			if len(appContainer) == 0 {
				h.logger.Error(Emoji+"please provide container name in case of docker-compose file", zap.Any("AppCmd", appCmd))
				return fmt.Errorf(Emoji + "container name not found")
			}

			if len(appNetwork) == 0 {
				h.logger.Error(Emoji+"please provide docker network name in case of docker-compose file", zap.Any("AppCmd", appCmd))
				return fmt.Errorf(Emoji + "docker network name not found")
			}
		}

		var err error
		appContainerName, appDockerNetwork, err = parseDockerCommand(appCmd)
		h.logger.Debug(Emoji, zap.String("Parsed container name", appContainerName))
		h.logger.Debug(Emoji, zap.String("Parsed docker network", appDockerNetwork))

		if err != nil {
			h.logger.Error(Emoji+"failed to parse container or network name from given docker command", zap.Error(err), zap.Any("AppCmd", appCmd))
			return err
		}

		if len(appContainer) == 0 {

			appContainer = appContainerName
		}

		dockerClient = makeDockerClient()
		errCh := make(chan error, 1)
		go func() {
			// listen for the "create container" event in order to send the inode of the container to the kernel
			go func() {
				// listen for the docker daemon events
				messages, errs := dockerClient.Events(context.Background(), types.EventsOptions{})

				for {
					select {
					case err := <-errs:
						if err != nil && err != context.Canceled {
							h.logger.Error("failed to listen for the docker daemon events", zap.Error(err))
						}
						return
					case e := <-messages:
						// extract the inode number of the user application container after the container is created
						if e.Type == events.ContainerEventType && e.Action == "create" {
							containerPid := 0
							for {
								inspect, err := dockerClient.ContainerInspect(context.Background(), appContainer)
								if err != nil {
									h.logger.Error("failed to inspect the target application container after it is created", zap.Error(err))
									continue
								}
								if inspect.State.Pid != 0 {
									h.ipAddress = inspect.NetworkSettings.Networks[appDockerNetwork].IPAddress
									containerPid = inspect.State.Pid
									break
								}
							}
							inode := getInodeNumber([15]int32{int32(containerPid)})
							// send the inode of the container to ebpf hooks to filter the network traffic
							h.SendNameSpaceId(0, inode)
						}
					}
				}
			}()
			err := h.runApp(appCmd, ok)
			errCh <- err
		}()

	
		// Check if there is an error in the channel without blocking
		select {
		// channel for only checking for error during this instant looks
		// like an overkill. TODO refactor
		case err := <-errCh:
			if err != nil {
				h.logger.Error(Emoji+"failed to launch the user application", zap.Any("err", err.Error()))
				return err
			}
		default:
			// No error received yet, continue with further flow
		}

	} else { //Supports only linux
		h.logger.Debug(Emoji + "Running user application on Linux", zap.Any("pid of keploy", os.Getpid()))

		// to notify the kernel hooks that the user application command is running in native linux.
		key := 0
		value := false
		h.objects.DockerCmdMap.Update(uint32(key), &value, ebpf.UpdateAny)

		errCh := make(chan error, 1)
		go func() {
			err := h.runApp(appCmd, false)
			errCh <- err
		}()

		h.logger.Debug(Emoji + "Waiting for any error from user application")
		time.Sleep(time.Duration(Delay) * time.Second)
		h.logger.Debug(Emoji + "After running user application")

		// Check if there is an error in the channel without blocking
		select {
		case err := <-errCh:
			if err != nil {
				h.logger.Error(Emoji+"failed to launch the user application", zap.Any("err", err.Error()))
				return err
			}
		default:
			// No error received yet, continue with further flow
		}

		h.logger.Debug(Emoji + "Now setting app pids")

		appPids, err := getAppPIDs(appCmd)
		if err != nil {
			h.logger.Error(Emoji+"failed to get the pid of user application", zap.Any("err", err.Error()))
			return err
		}

		pids = appPids
		println("Pid of application:", pids[0])
		h.logger.Debug(Emoji+"PID of application:", zap.Any("App pid", pids[0]))
	}

	err := h.SendApplicationPIDs(pids)
	if err != nil {
		h.logger.Error(Emoji+"failed to send the application pids to the ebpf program", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}

	// h.EnablePidFilter() // can enable here also

	h.logger.Info(Emoji + "User application started successfully")
	return nil
}

// It runs the application using the given command
func (h *Hook) runApp(appCmd string, isDocker bool) error {
	// Create a new command with your appCmd
	// var cmd *exec.Cmd
	// if isDocker {
	// 	parts := strings.Fields(appCmd)
	// 	cmd = exec.Command(parts[0], parts[1:]...)
	// } else {
	// 	cmd = exec.Command(appCmd)
	// }

	parts := strings.Fields(appCmd)
	cmd := exec.Command(parts[0], parts[1:]...)

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	h.userAppCmd = cmd

	// Run the command, this handles non-zero exit code get from application.
	err := cmd.Run()
	if err != nil {
		return err
	}

	//make it debug statement
	return fmt.Errorf(Emoji, "user application exited with zero error code")
}

// It checks if the cmd is related to docker or not, it also returns if its a docker compose file
func (h *Hook) IsDockerRelatedCmd(cmd string) (bool, string) {
	// Check for Docker command patterns
	dockerCommandPatterns := []string{"docker ", "docker-compose ", "sudo docker ", "sudo docker-compose "}
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
	networkNamePattern := `(--network|--net)\s+([^\s]+)`

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
	if len(networkNameMatches) < 3 {
		return "", "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}

func makeDockerClient() *client.Client {
	// Create a new Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf(Emoji, "failed to make a docker client:", err)
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
		log.Fatalf(Emoji, "failed to inspect container:%v,", containerName, err)
	}

	// Find the IP address of the container in the network
	ipAddress := inspect.NetworkSettings.Networks[appDockerNetwork].IPAddress

	h.logger.Debug(Emoji, zap.Any("Container Ip Address", ipAddress))

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
		log.Fatalf(Emoji, "failed to inspect container:%v,", containerName, err)
	}

	containerID := inspect.ID

	// Retrieve the PIDs of the container's processes
	processes, err := cli.ContainerTop(context.Background(), containerID, []string{})
	if err != nil {
		log.Fatalf(Emoji, "failed to retrieve processes inside the app container:", err)
	}

	if len(processes.Processes) > 15 {
		log.Fatalf(Emoji, "Error: More than 15 processes are running inside the application container.")
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
			log.Fatalln(Emoji, "failed to get the process IDs from the app container")
		}

		pid, err := parseToInt32(process[1])
		if err != nil {
			log.Fatalf(Emoji, "failed to convert the process id [%v] from string to int32", process[1])
		}
		hostPids[idx] = pid
	}

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
		log.Fatalf(Emoji, "failed to open file /proc/%v/status:", pid, err)
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
		log.Fatal(Emoji, "failed to scan the file:", err)
	}

	// Print the last NStgid value
	if lastNStgid == "" {
		log.Fatal(Emoji, "NStgid value not found in the file")
	}
	// fmt.Println("Last NStgid:", lastNStgid)
	NStgid, err := parseToInt32(strings.TrimSpace(lastNStgid))
	if err != nil {
		log.Fatalf(Emoji, "failed to convert the NStgid[%v] from string to int32", lastNStgid)
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
	parts := strings.Fields(appCmd)
	cmdPid, err := getCmdPid(parts[0])

	// cmdPid, err := getCmdPid(appCmd)
	if err != nil {
		return [15]int32{}, fmt.Errorf(Emoji, "failed to get the pid of the running command: %v\n", err)
	}

	if cmdPid == 0 {
		return [15]int32{}, fmt.Errorf(Emoji, "The command '%s' is not running.: %v\n", appCmd, err)
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

	output, err := cmd.Output()
	fmt.Println(Emoji, "Output of pidof", output)
	if err != nil {
		fmt.Errorf(Emoji, "failed to execute the command: %v", commandName)
		return 0, err
	}

	pidStr := strings.TrimSpace(string(output))
	// pidStrings:= strings.Split(pidStr," ")
	// pidStr = pidStrings[0]
	fmt.Println(Emoji, "Output of pidof", pidStr)
	actualPidStr := strings.Split(pidStr, " ")[0]
	pid, err := strconv.Atoi(actualPidStr)
	if err != nil {
		fmt.Errorf(Emoji, "failed to convert the process id [%v] from string to int", pidStr)
		return 0, err
	}

	return pid, nil
}

func getInodeNumber(pids [15]int32) uint64 {

	for _, pid := range pids {
		filepath := filepath.Join("/proc", strconv.Itoa(int(pid)), "ns", "pid")

		f, err := os.Stat(filepath)
		if err != nil {
			fmt.Errorf("%v failed to get the inode number or namespace Id:", Emoji, err)
			continue
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
	filepath := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(filepath)
	if err != nil {
		log.Fatal(Emoji, "failed to get the self inode number or namespace Id:", err)
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	Ino := (f.Sys().(*syscall.Stat_t)).Ino
	if Ino != 0 {
		return Ino
	}
	return 0
}
