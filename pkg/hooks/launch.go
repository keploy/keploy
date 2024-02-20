package hooks

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/docker/docker/api/types/filters"
	"go.uber.org/zap"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
)

const (
	// TODO : Remove hard-coded container & network name.
	KeployContainerName = "keploy-v2"
	KeployNetworkName   = "keploy-network"
)

// Define custom error variables
var (
	ErrInterrupted    = errors.New("exited with interrupt")
	ErrCommandError   = errors.New("exited due to command error")
	ErrUnExpected     = errors.New("an unexpected error occurred")
	ErrDockerError    = errors.New("an error occurred while using docker client")
	ErrFailedUnitTest = errors.New("test failure occured when running keploy tests along with unit tests")
)

func (h *Hook) LaunchUserApplication(appCmd, appContainer, appNetwork string, Delay uint64, buildDelay time.Duration, isUnitTestIntegration bool) error {
	// Supports Native-Linux, Windows (WSL), Lima, Colima

	if appCmd == "" {
		if len(appContainer) == 0 {
			return fmt.Errorf(Emoji + "please provide container name when running application container in isolation")
		}
		if len(appNetwork) == 0 {
			return fmt.Errorf(Emoji + "please provide network name when running application container in isolation")
		}
	}

	if appCmd == "" && len(appContainer) != 0 && len(appNetwork) != 0 {

		h.logger.Debug("User Application is running inside docker in isolation", zap.Any("Container", appContainer), zap.Any("Network", appNetwork))
		//search for the container and process it
		err := h.processDockerEnv("", appContainer, appNetwork, buildDelay)
		if err != nil {
			return err
		}
		return nil
	} else {
		ok, cmd := utils.IsDockerRelatedCmd(appCmd)
		if ok {

			h.logger.Debug("Running user application on Docker", zap.Any("Docker env", cmd))

			if cmd == "docker-compose" {
				if len(appContainer) == 0 {
					h.logger.Error("please provide container name in case of docker-compose file", zap.Any("AppCmd", appCmd))
					return fmt.Errorf(Emoji + "container name not found")
				}

				//finding the user docker-compose file in the current directory.
				dockerComposeFile := findDockerComposeFile()

				if dockerComposeFile == "" {
					return fmt.Errorf("can't find the docker compose file of user. Are you in the right directory? ")
				}

				// kdocker-compose.yaml file will be run instead of the user docker-compose.yaml file acc to below cases
				newComposeFile := "kdocker-compose.yaml"

				// Check if docker compose file uses relative file names for bind mounts
				hasRelativeBindMounts := h.idc.CheckBindMounts(dockerComposeFile)

				if hasRelativeBindMounts {
					err := h.idc.ReplaceRelativePaths(dockerComposeFile, newComposeFile)
					if err != nil {
						h.logger.Error("failed to convert relative paths to absolute paths in volume mounts in docker compose file")
						return err
					}
					h.logger.Info("Created kdocker-compose.yml file and Replaced relative file paths in docker compose file.")
					//Now replace the running command to run the kdocker-compose.yaml file instead of user docker compose file.
					appCmd = modifyDockerComposeCommand(appCmd, newComposeFile)
					dockerComposeFile = newComposeFile
				}

				// Checking info about the network and whether its external:true
				hasNetwork, isExternal, network := h.idc.CheckNetworkInfo(dockerComposeFile)

				if hasNetwork {
					appNetwork = network
					// if external is true that means the network is present locally.
					if isExternal {
						//injecting application network to keploy.
						err := h.injectNetworkToKeploy(appNetwork)
						if err != nil {
							h.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", appNetwork))
							return err
						}
					} else {
						//make external = true and injecting that network to keploy
						if len(appNetwork) == 0 {
							h.logger.Error("couldn't find any network")
							return fmt.Errorf("unable to find user docker network")
						}

						h.logger.Info("trying to make your docker network external")
						//make a new compose file (kdocker-compose.yaml file) having external:true
						err := h.idc.MakeNetworkExternal(dockerComposeFile, newComposeFile)
						if err != nil {
							h.logger.Error("couldn't make your docker network external")
							return fmt.Errorf("error while updating network to external: %v", err)
						}

						h.logger.Info("successfully made your docker network external")

						// check if this network exists locally
						ok, err := h.idc.NetworkExists(appNetwork)
						if err != nil {
							h.logger.Error("failed to find user docker network locally", zap.Any("appNetwork", appNetwork))
							return err
						}

						// if this network doesn't exist locally then create it
						if !ok {
							err := h.idc.CreateCustomNetwork(appNetwork)
							if err != nil {
								h.logger.Error("failed to create custom network", zap.Any("appNetwork", appNetwork))
								return err
							}
						}

						//injecting application network to keploy
						err = h.injectNetworkToKeploy(appNetwork)
						if err != nil {
							h.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", appNetwork))
							return err
						}

						oldCmd := appCmd
						//Now replace the running command to run the kdocker-compose.yaml file instead of user docker compose file.
						appCmd = modifyDockerComposeCommand(appCmd, newComposeFile)
						h.logger.Debug(fmt.Sprintf("docker compose run command changed from %v to %v", oldCmd, appCmd))
					}
				} else {
					h.logger.Debug("no network found hence adding keploy-network to the user docker compose file")
					//no network hence injecting keploy-network.
					ok, err := h.idc.NetworkExists(KeployNetworkName)
					if err != nil {
						h.logger.Error("failed to find keploy-network")
						return err
					}

					//if keploy-network doesn't exist locally then create it
					if !ok {
						err := h.idc.CreateCustomNetwork(KeployNetworkName)
						if err != nil {
							h.logger.Error("failed to create keploy-network")
							return err
						}
					}

					// make new a docker-compose file (kdocker-compose.yaml)
					// to run user docker compose file with this custom keploy-network
					err = h.idc.AddNetworkToCompose(dockerComposeFile, newComposeFile)
					if err != nil {
						h.logger.Error("failed to add external keploy network to the user docker compose file")
						return err
					}

					//injecting application network to keploy
					err = h.injectNetworkToKeploy(KeployNetworkName)
					if err != nil {
						h.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", appNetwork))
						return err
					}

					//set current network as keploy-network
					appNetwork = KeployNetworkName

					// time.Sleep(5 * time.Second)
					oldCmd := appCmd
					path := "./" + newComposeFile
					//Now replace the running command to run the kdocker-compose.yaml file instead of user docker compose file.
					appCmd = modifyDockerComposeCommand(appCmd, path)
					h.logger.Debug(fmt.Sprintf("docker compose run command changed from %v to %v", oldCmd, appCmd))
				}

				h.logger.Debug("", zap.Any("appContainer", appContainer), zap.Any("appNetwork", appNetwork), zap.Any("appCmd", appCmd))
			} else if cmd == "docker" || cmd == "docker-start"{
				var err error

				if cmd == "docker" {
					cont, net, err := parseDockerCommand(appCmd)
					h.logger.Debug("", zap.String("Parsed container name", cont))
					h.logger.Debug("", zap.String("Parsed docker network", net))

					if err != nil {
						h.logger.Error("failed to parse container name from given docker command", zap.Error(err), zap.Any("AppCmd", appCmd))
						return err
					}

					if err != nil {
						h.logger.Error("failed to parse network name from given docker command", zap.Error(err), zap.Any("AppCmd", appCmd))
						return err
					}

					if len(appContainer) != 0 && appContainer != cont {
						h.logger.Warn(fmt.Sprintf("given app container:(%v) is different from parsed app container:(%v)", appContainer, cont))
					}

					if len(appNetwork) != 0 && appNetwork != net {
						h.logger.Warn(fmt.Sprintf("given docker network:(%v) is different from parsed docker network:(%v)", appNetwork, net))
					}

					appContainer, appNetwork = cont, net
				}
				
				//injecting appNetwork to keploy.
				err = h.injectNetworkToKeploy(appNetwork)
				if err != nil {
					h.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", appNetwork))
					return err
				}
			}

			err := h.processDockerEnv(appCmd, appContainer, appNetwork, buildDelay)
			if err != nil {
				return err
			}
		} else { //Supports only linux
			h.logger.Debug("Running user application on Linux", zap.Any("pid of keploy", os.Getpid()))

			// to notify the kernel hooks that the user application command is running in native linux.
			key := 0
			value := false
			h.objects.DockerCmdMap.Update(uint32(key), &value, ebpf.UpdateAny)

			// Recover from panic and gracefully shutdown
			defer h.Recover(pkg.GenerateRandomID())
			err := h.runApp(appCmd, isUnitTestIntegration)
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func (h *Hook) processDockerEnv(appCmd, appContainer, appNetwork string, buildDelay time.Duration) error {
	// to notify the kernel hooks that the user application is related to Docker. 
	key := 0
	value := true
	h.objects.DockerCmdMap.Update(uint32(key), &value, ebpf.UpdateAny)

	stopListenContainer := make(chan bool)
	stopApplicationErrors := false
	abortStopListenContainerChan := false

	dockerClient := h.idc
	appErrCh := make(chan error, 1)

	//User is running its application in isolation when appCmd is empty
	if len(appCmd) != 0 {
		go func() {
			// Recover from panic and gracefully shutdown
			defer h.Recover(pkg.GenerateRandomID())

			err := h.runApp(appCmd, true)
			if err != nil {
				h.logger.Debug("Application stopped with the error", zap.Error(err))
				if !stopApplicationErrors {
					appErrCh <- err
				}
			}
		}()
	}

	dockerErrCh := make(chan error, 1)

	// listen for the "create container" event in order to send the inode of the container to the kernel
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())

		// listen for the docker daemon events
		defer func() {
			h.logger.Debug("exiting from goroutine of docker daemon event listener")
		}()

		endTime := time.Now().Add(buildDelay)
		logTicker := time.NewTicker(1 * time.Second)
		defer logTicker.Stop()

		eventFilter := filters.NewArgs()
		eventFilter.Add("type", "container")
		eventFilter.Add("event", "create")
		eventFilter.Add("event", "start")

		messages, errs := dockerClient.Events(context.Background(), types.EventsOptions{
			Filters: eventFilter,
		})

		for {
			if time.Now().After(endTime) {
				select {
				case <-stopListenContainer:
					return
				default:
					dockerErrCh <- fmt.Errorf("no container found for :%v", appContainer)
					return
				}
			}

			select {
			case <-stopListenContainer:
				return
			case err := <-errs:
				if err != nil && err != context.Canceled {
					if err != nil && err != context.Canceled {
						select {
						case <-stopListenContainer:
						default:
							dockerErrCh <- fmt.Errorf("failed to listen for the docker events: %v", err)
						}
					}
				}
				return
			case <-logTicker.C:
				h.logger.Info("still waiting for the container to start.", zap.String("containerName", appContainer))
			case e := <-messages:
				if e.Type == events.ContainerEventType && (e.Action == "create" || e.Action == "start") {
					// Set Docker Container ID
					h.idc.SetContainerID(e.ID)

					// Fetch container details by inspecting using container ID to check if container is created
					containerDetails, err := dockerClient.ContainerInspect(context.Background(), e.ID)
					if err != nil {
						h.logger.Debug("failed to inspect container by container Id", zap.Error(err))
						continue
					}

					// Check if the container's name matches the desired name
					if containerDetails.Name != "/"+appContainer {
						h.logger.Debug("ignoring container creation for unrelated container", zap.String("containerName", containerDetails.Name))
						continue
					}

					h.logger.Debug("container created for desired app", zap.Any("ID", e.ID))

					containerPid := 0
					containerIp := ""
					containerFound := false
					for {
						if time.Now().After(endTime) {
							h.logger.Error("failed to find the user application container", zap.Any("appContainer", appContainer))
							break
						}

						//Inspecting the application container again since the ip and pid takes some time to be linked to the container.
						containerDetails, err := dockerClient.ContainerInspect(context.Background(), appContainer)
						if err != nil {
							// h.logger.Debug(fmt.Sprintf("failed to get inspect:%v by containerName", containerDetails), zap.Error(err))
							continue
						}

						h.logger.Debug("checking for container pid", zap.Any("containerDetails.State.Pid", containerDetails.State.Pid))
						if containerDetails.State.Pid != 0 {
							h.logger.Debug("", zap.Any("containerDetails.State.Pid", containerDetails.State.Pid))
							containerPid = containerDetails.State.Pid
							containerFound = true
							h.logger.Debug(fmt.Sprintf("user container:(%v) found", appContainer))
							break
						}
					}

					if containerFound {
						h.logger.Debug(fmt.Sprintf("the user application container pid: %v", containerPid))
						inode := getInodeNumber(containerPid)
						h.logger.Debug("", zap.Any("user inode", inode))

						// send the inode of the container to ebpf hooks to filter the network traffic
						err := h.SendNameSpaceId(0, inode)
						if err == nil {
							h.logger.Debug("application inode sent to kernel successfully", zap.Any("user inode", inode), zap.Any("time", time.Now().UnixNano()))
						}

						//inspecting it again to get the ip of the container used in test mode.
						containerDetails, err := dockerClient.ContainerInspect(context.Background(), appContainer)
						if err != nil {
							h.logger.Error(fmt.Sprintf("failed to get inspect app container:%v to retrive the ip", containerDetails))
							select {
							case <-stopListenContainer:
								return
							default:
								dockerErrCh <- err
								return
							}
						}

						// find the application container ip in case of test mode
						if containerDetails.NetworkSettings != nil && containerDetails.NetworkSettings.Networks != nil {
							networkDetails, ok := containerDetails.NetworkSettings.Networks[appNetwork]
							if ok && networkDetails != nil {
								h.logger.Debug(fmt.Sprintf("the ip of the docker container: %v", networkDetails.IPAddress))
								if models.GetMode() == models.MODE_TEST {
									h.logger.Debug("setting container ip address")
									containerIp = networkDetails.IPAddress
									h.logger.Debug("receiver channel received the ip address", zap.Any("containerIp found", containerIp))
								}
							} else {
								select {
								case <-stopListenContainer:
									return
								default:
									dockerErrCh <- fmt.Errorf("network details for %v network not found", appNetwork)
									return
								}
							}
						} else {
							select {
							case <-stopListenContainer:
								return
							default:
								dockerErrCh <- fmt.Errorf("network settings or networks not available in inspect data")
								return
							}
						}

						h.logger.Info("container & network found and processed successfully", zap.Any("time", time.Now().UnixNano()))
						abortStopListenContainerChan = true
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
		stopApplicationErrors = true
		if err != nil {
			h.logger.Error("failed to process the user application container", zap.Any("err", err.Error()))
			return ErrDockerError
		}
	case err := <-appErrCh:
		if !abortStopListenContainerChan {
			stopListenContainer <- true
		}
		if err != nil {
			return err
		}
		// No error received yet, continue with further flow
	}

	h.logger.Debug("processDockerEnv executed successfully")
	return nil
}

// It runs the application using the given command
func (h *Hook) runApp(appCmd string, isUnitTestIntegration bool) error {
	// Create a new command with your appCmd
	cmd := exec.Command("sh", "-c", appCmd)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	h.userAppCmd = cmd

	// Run the app as the user who invoked sudo
	username := os.Getenv("SUDO_USER")
	if username != "" {
		uidCmd := exec.Command("id", "-u", username)
		gidCmd := exec.Command("id", "-g", username)

		var uidOut, gidOut bytes.Buffer
		uidCmd.Stdout = &uidOut
		gidCmd.Stdout = &gidOut

		err := uidCmd.Run()
		if err != nil {
			return err
		}

		err = gidCmd.Run()
		if err != nil {
			return err
		}

		uidStr := strings.TrimSpace(uidOut.String())
		gidStr := strings.TrimSpace(gidOut.String())

		uid, err := strconv.ParseUint(uidStr, 10, 32)
		if err != nil {
			return err
		}

		gid, err := strconv.ParseUint(gidStr, 10, 32)
		if err != nil {
			return err
		}

		// Switch the user
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}

	h.logger.Debug("", zap.Any("executing cmd", cmd.String()))

	err := cmd.Run()
	if err != nil {
		if h.IsUserAppTerminateInitiated() {
			if exitError, ok := err.(*exec.ExitError); ok {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						return ErrInterrupted
					}
					if status.Exited() {
						h.logger.Warn(fmt.Sprintf("userApplication has exited with exit code: %v", status.ExitStatus()))
						return ErrInterrupted
					}
				}
			}
			h.logger.Warn("userApplication might not have shut down correctly. Please verify if it has been closed", zap.Error(err))
			return ErrInterrupted
		}

		// This is done for non-server running commands like "mvn test", "npm test", "go test" etc
		if isUnitTestIntegration {
			return ErrFailedUnitTest
		}
		h.logger.Error("userApplication failed to run with the following error. Please check application logs", zap.Error(err))
		return ErrCommandError
	} 

	return nil
}

// injectNetworkToKeploy attaches the given network to the keploy container and also sends the keploy container ip of the new network interface to the kernel space
func (h *Hook) injectNetworkToKeploy(appNetwork string) error {
	// inject the network to the keploy container
	h.logger.Info(fmt.Sprintf("trying to inject network:%v to the keploy container", appNetwork))
	err := h.idc.ConnectContainerToNetworksByNames(KeployContainerName, []string{appNetwork})
	if err != nil {
		h.logger.Error("could not inject application network to the keploy container")
		return err
	}

	//sending new proxy ip to kernel, since dynamically injected new network has different ip for keploy.
	kInspect, err := h.idc.ContainerInspect(context.Background(), KeployContainerName)
	if err != nil {
		h.logger.Error(fmt.Sprintf("failed to get inspect keploy container:%v", kInspect))
		return err
	}

	var newProxyIpString string
	keployNetworks := kInspect.NetworkSettings.Networks
	//Here we considering that the application would use only one custom network.
	//TODO: handle for application having multiple custom networks
	for networkName, networkSettings := range keployNetworks {
		if networkName != "bridge" {
			appNetwork = networkName
			newProxyIpString = networkSettings.IPAddress
			h.logger.Debug(fmt.Sprintf("Network Name: %s, New Proxy IP: %s\n", networkName, networkSettings.IPAddress))
		}
	}
	proxyIp, err := ConvertIPToUint32(newProxyIpString)
	if err != nil {
		return fmt.Errorf("failed to convert ip string:[%v] to 32-bit integer", newProxyIpString)
	}

	proxyPort := h.GetProxyPort()
	err = h.SendProxyInfo(proxyIp, proxyPort, [4]uint32{0000, 0000, 0000, 0001})
	if err != nil {
		h.logger.Error("failed to send new proxy ip to kernel", zap.Any("NewProxyIp", proxyIp))
		return err
	}

	h.logger.Debug(fmt.Sprintf("New proxy ip:%v & proxy port:%v sent to kernel", proxyIp, proxyPort))
	h.logger.Info("Successfully injected network to the keploy container", zap.Any("Keploy container", KeployContainerName), zap.Any("appNetwork", appNetwork))
	return nil
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
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}

func getInodeNumber(pid int) uint64 {

	filepath := filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid")

	f, err := os.Stat(filepath)
	if err != nil {
		return 0
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	Ino := (f.Sys().(*syscall.Stat_t)).Ino
	if Ino != 0 {
		return Ino
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

func findDockerComposeFile() string {
	filenames := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

	for _, filename := range filenames {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			return filename
		}
	}

	return ""
}

func modifyDockerComposeCommand(appCmd, newComposeFile string) string {
	// Ensure newComposeFile starts with ./
	if !strings.HasPrefix(newComposeFile, "./") {
		newComposeFile = "./" + newComposeFile
	}

	// Define a regular expression pattern to match "-f <file>"
	pattern := `(-f\s+("[^"]+"|'[^']+'|\S+))`
	re := regexp.MustCompile(pattern)

	// Check if the "-f <file>" pattern exists in the appCmd
	if re.MatchString(appCmd) {
		// Replace it with the new Compose file
		return re.ReplaceAllString(appCmd, fmt.Sprintf("-f %s", newComposeFile))
	}

	// If the pattern doesn't exist, inject the new Compose file right after "docker-compose" or "docker compose"
	upIdx := strings.Index(appCmd, " up")
	if upIdx != -1 {
		return fmt.Sprintf("%s -f %s%s", appCmd[:upIdx], newComposeFile, appCmd[upIdx:])
	}

	return fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
}
