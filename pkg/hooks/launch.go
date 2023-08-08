package hooks

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
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
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

var (
	dockerClient     *client.Client
	appDockerNetwork string
	appContainerName string
)

func (h *Hook) LaunchUserApplication(appCmd, appContainer, appNetwork string, Delay uint64) error {
	// Supports Linux, Windows

	if appCmd == "" && len(appContainer) == 0 {
		return fmt.Errorf(Emoji + "please provide containerName when trying to run using IDE")
	}

	if appCmd == "" && len(appContainer) != 0 {

		if len(appNetwork) == 0 {
			h.logger.Error(Emoji + "please provide docker network name when running with IDE")
			return fmt.Errorf(Emoji + "docker network name not found")
		}

		h.logger.Debug(Emoji + "User Application is running inside docker using IDE")
		//search for the container and process it
		err := h.processDockerEnv("", appContainer, appNetwork)
		if err != nil {
			return err
		}
		h.logger.Info(Emoji + "User application container fetced successfully")
		return nil
	}

	ok, cmd := h.IsDockerRelatedCmd(appCmd)
	if ok {

		h.logger.Debug(Emoji + "Running user application on Docker")

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

		if len(appNetwork) == 0 {

			appNetwork = appDockerNetwork
		}

		err = h.processDockerEnv(appCmd, appContainer, appNetwork)
		if err != nil {
			return err
		}
	} else { //Supports only linux
		h.logger.Debug(Emoji+"Running user application on Linux", zap.Any("pid of keploy", os.Getpid()))

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

		// Check if there is an error in the channel without blocking
		select {
		case err := <-errCh:
			if err != nil {
				h.logger.Error(Emoji+"failed to launch the user application", zap.Any("err", err.Error()))
				return err
			}
		default:
			h.logger.Debug(Emoji + "no error found while running user application")
			// No error received yet, continue with further flow
		}
	}

	h.logger.Info(Emoji + "User application started successfully")
	return nil
}

func (h *Hook) processDockerEnv(appCmd, appContainer, appNetwork string) error {

	// to notify the kernel hooks that the user application is related to Docker.
	key := 0
	value := true
	h.objects.DockerCmdMap.Update(uint32(key), &value, ebpf.UpdateAny)

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	dockerClient := makeDockerClient()
	appErrCh := make(chan error, 1)

	//User is using keploy with IDE when appCmd is empty
	if len(appCmd) != 0 {
		go func() {
			err := h.runApp(appCmd, true)
			appErrCh <- err
		}()

		select {
		case err := <-appErrCh:
			if err != nil {
				h.logger.Error(Emoji+"failed to launch the user application container", zap.Any("err", err.Error()))
				return err
			}
		default:
			h.logger.Debug(Emoji + "no error found while running user application container")
			// No error received yet, continue with further flow
		}
	}

	dockerErrCh := make(chan error, 1)
	done := make(chan bool, 1)

	// listen for the "create container" event in order to send the inode of the container to the kernel
	go func() {
		// listen for the docker daemon events
		defer func() {
			h.logger.Debug(Emoji + "exiting from goroutine of docker daemon event listener")
		}()

		endTime := time.Now().Add(30 * time.Second)
		logTicker := time.NewTicker(1 * time.Second)
		defer logTicker.Stop()

		messages, errs := dockerClient.Events(context.Background(), types.EventsOptions{})
		for {
			if time.Now().After(endTime) {
				dockerErrCh <- fmt.Errorf("no container found for :%v", appContainer)
				return
			}

			select {
			case err := <-errs:
				if err != nil && err != context.Canceled {
					h.logger.Error("failed to listen for the docker events", zap.Error(err))
				}
				return
			case <-stopper:
				dockerErrCh <- fmt.Errorf("found sudden interrupt")
				return
			case <-logTicker.C:
				h.logger.Info(Emoji+"Still waiting for the container to start.", zap.String("containerName", appContainer))
			case e := <-messages:
				if e.Type == events.ContainerEventType && e.Action == "create" {
					fmt.Println(Emoji+"Container created: ", e.ID)
					containerPid := 0
					containerIp := ""
					containerFound := false
					for {
						if time.Now().After(endTime) {
							h.logger.Error(Emoji+"failed to find the user application container", zap.Any("appContainer", appContainer))
							break
						}
						inspect, err := dockerClient.ContainerInspect(context.Background(), appContainer)
						if err != nil {
							// h.logger.Debug(Emoji+fmt.Sprintf("failed to get inspect:%v", inspect), zap.Error(err))
							continue
						}

						h.logger.Debug(Emoji+"checking for container pid", zap.Any("inspect.State.Pid", inspect.State.Pid))
						if inspect.State.Pid != 0 {
							h.logger.Debug(Emoji, zap.Any("inspect.State.Pid", inspect.State.Pid))

							if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks != nil {
								networkDetails, ok := inspect.NetworkSettings.Networks[appNetwork]
								if ok && networkDetails != nil {
									h.logger.Debug(Emoji + fmt.Sprintf("the ip of the docker container: %v", networkDetails.IPAddress))
									if models.GetMode() == models.MODE_TEST {
										h.logger.Debug(Emoji + "setting container ip address")
										containerIp = networkDetails.IPAddress
										h.logger.Debug(Emoji + "receiver channel received the ip address")
									}
								} else {
									h.logger.Debug(Emoji+"Network details for given network not found", zap.Any("network", appNetwork))
								}
							} else {
								h.logger.Debug(Emoji + "Network settings or networks not available in inspect data")
							}
							containerPid = inspect.State.Pid
							containerFound = true
							h.logger.Debug(Emoji + "container and ip found...")
							break
						}
					}
					if containerFound {
						h.logger.Debug(Emoji + fmt.Sprintf("the user application container pid: %v", containerPid))
						inode := getInodeNumber([15]int32{int32(containerPid)})
						h.logger.Debug(Emoji, zap.Any("user inode", inode))
						// send the inode of the container to ebpf hooks to filter the network traffic
						err := h.SendNameSpaceId(0, inode)
						if err == nil {
							h.logger.Debug(Emoji+"application inode sent to kernel successfully", zap.Any("user inode", inode), zap.Any("time", time.Now().UnixNano()))
						}
						done <- true
						if models.GetMode() == models.MODE_TEST {
							h.userIpAddress <- containerIp
						}
						return
					}
				}
			}
		}
	}()

	select {
	case err := <-dockerErrCh:
		if err != nil {
			h.logger.Error(Emoji+"failed to find the user application container", zap.Any("err", err.Error()))
			return err
		}
	case <-done:
		h.logger.Debug(Emoji+"container found and processed successfully", zap.Any("time", time.Now().UnixNano()))
		// No error received yet, continue with further flow
	}

	h.logger.Debug(Emoji + "processDockerEnv executed successfully")
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

	// out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", "0b6c6856d706").Output()
	// if err != nil {
	// 	fmt.Printf("Failed to execute command: %s", err)
	// 	// return .
	// }

	// fmt.Printf("time before starting the user application: %v", time.Now())
	// pid := strings.TrimSpace(string(out))
	// fmt.Printf("PID of the docker : %s\n", pid)
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

func getCmdPid(commandName string) (int, error) {
	cmd := exec.Command("pidof", commandName)

	output, err := cmd.Output()
	// fmt.Println(Emoji, "Output of pidof", output)
	if err != nil {
		fmt.Errorf(Emoji, "failed to execute the command: %v", commandName)
		return 0, err
	}

	pidStr := strings.TrimSpace(string(output))
	// pidStrings:= strings.Split(pidStr," ")
	// pidStr = pidStrings[0]
	// fmt.Println(Emoji, "Output of pidof", pidStr)
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
