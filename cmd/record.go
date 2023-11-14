package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/record"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewCmdRecord(logger *zap.Logger) *Record {
	recorder := record.NewRecorder(logger)
	return &Record{
		recorder: recorder,
		logger:   logger,
	}
}

func readRecordConfig(configPath string) (*models.Record, error) {
	file, err := os.OpenFile(configPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.Config
	err = decoder.Decode(&doc)
	if err != nil {
		return nil, err
	}
	return &doc.Record, nil
}

func (t *Record) GetRecordConfig(path *string, proxyPort *uint32, appCmd *string, appContainer, networkName *string, Delay *uint64, passThorughPorts *[]uint, configPath string) error {
	configFilePath := filepath.Join(configPath, "keploy-config.yaml")
	if isExist := utils.CheckFileExists(configFilePath); !isExist {
		return errFileNotFound
	}
	confRecord, err := readRecordConfig(configFilePath)
	if err != nil {
		return fmt.Errorf("failed to get the record config from config file due to error: %s", err)
	}
	if len(*path) == 0 {
		*path = confRecord.Path
	}
	if *proxyPort == 0 {
		*proxyPort = confRecord.ProxyPort
	}
	if *appCmd == "" {
		*appCmd = confRecord.Command
	}
	if *appContainer == "" {
		*appContainer = confRecord.ContainerName
	}
	if *networkName == "" {
		*networkName = confRecord.NetworkName
	}
	if *Delay == 5 {
		*Delay = confRecord.Delay
	}
	if len(*passThorughPorts) == 0 {
		*passThorughPorts = confRecord.PassThroughPorts
	}
	return nil
}

type Record struct {
	recorder record.Recorder
	logger   *zap.Logger
}

func (r *Record) GetCmd() *cobra.Command {
	// record the keploy testcases/mocks for the user application
	var recordCmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `sudo -E env PATH=$PATH keploy record -c "/path/to/user/app"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				r.logger.Error("failed to read the testcase path input")
				return err
			}

			appCmd, err := cmd.Flags().GetString("command")
			if err != nil {
				r.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
				return err
			}

			appContainer, err := cmd.Flags().GetString("containerName")
			if err != nil {
				r.logger.Error("Failed to get the application's docker container name", zap.Error((err)))
				return err
			}

			networkName, err := cmd.Flags().GetString("networkName")
			if err != nil {
				r.logger.Error("Failed to get the application's docker network name", zap.Error((err)))
				return err
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if err != nil {
				r.logger.Error("Failed to get the delay flag", zap.Error((err)))
				return err
			}

			ports, err := cmd.Flags().GetUintSlice("passThroughPorts")
			if err != nil {
				r.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				r.logger.Error("failed to read the proxy port")
				return err
			}

			configPath, err := cmd.Flags().GetString("config-path")
			if err != nil {
				r.logger.Error("failed to read the config path")
				return err
			}

			err = r.GetRecordConfig(&path, &proxyPort, &appCmd, &appContainer, &networkName, &delay, &ports, configPath)
			if err != nil {
				if err == errFileNotFound {
					r.logger.Info("continuing without configuration file because file not found")
				} else {
					r.logger.Error("", zap.Error(err))
					return err
				}
			}

			if appCmd == "" {
				fmt.Println("Error: missing required -c flag or appCmd in config file")
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy record -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6\n`)
				}else {
					fmt.Println("Example usage:\n", cmd.Example)
				}
				return errors.New("missing required -c flag or appCmd in config file")
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					r.logger.Error("failed to get the absolute path from relative path", zap.Error(err))
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				cdirPath, err := os.Getwd()
				if err != nil {
					r.logger.Error("failed to get the path of current directory", zap.Error(err))
				}
				path = cdirPath
			} else {
				// user provided the absolute path
			}

			path += "/keploy"

			r.logger.Info("", zap.Any("keploy test and mock path", path))

			var hasContainerName bool
			if isDockerCmd {
				for _, arg := range os.Args {
					if strings.Contains(arg, "--name") {
						hasContainerName = true
						break
					}
				}
				if !hasContainerName && appContainer == "" {
					fmt.Println("Error: missing required --containerName flag or containerName in config file")
					fmt.Println("\nExample usage:\n", `keploy record -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}

			r.logger.Debug("the ports are", zap.Any("ports", ports))
			r.recorder.CaptureTraffic(path, proxyPort,  appCmd, appContainer, networkName, delay, ports)
			return nil
		},
	}

	recordCmd.Flags().StringP("path", "p", "", "Path to the local directory where generated testcases/mocks should be stored")

	recordCmd.Flags().StringP("command", "c", "", "Command to start the user application")

	recordCmd.Flags().String("containerName", "", "Name of the application's docker container")

	recordCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")

	recordCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")

	recordCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")

	recordCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	recordCmd.Flags().String("config-path", ".", "Path to the local directory where keploy configuration file is stored")

	recordCmd.SilenceUsage = true
	recordCmd.SilenceErrors = true

	return recordCmd
}