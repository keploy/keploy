package cmd

import (
	"compress/gzip"
	"fmt"
	"github.com/cloudflare/cfssl/log"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"io"
	"os"
	"regexp"
)

const (
	maxSizeBytes = 5 * 1024 * 1024 // Max file size defined is 5MB. Can be changed according to convenience
)

type Compress struct {
	logger *zap.Logger
}

func NewCmdCompress(logger *zap.Logger) *Compress {
	return &Compress{
		logger: logger,
	}
}

func compressLogFile(logFilePath string) error {
	logFile, err := os.Open(logFilePath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	compressedLogFilePath := logFilePath + ".gz"
	compressedLogFile, err := os.Create(compressedLogFilePath)
	if err != nil {
		return err
	}
	defer compressedLogFile.Close()

	gzipWriter := gzip.NewWriter(compressedLogFile)
	defer gzipWriter.Close()

	_, err = io.Copy(gzipWriter, logFile)
	if err != nil {
		return err
	}

	return nil
}

func checkAndCompress(logFilePath string) {
	fileInfo, err := os.Stat(logFilePath)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fileSizeBytes := fileInfo.Size()

	if fileSizeBytes > maxSizeBytes {
		err := compressLogFile(logFilePath)
		if err != nil {
			fmt.Println("Error compressing log file:", err)
			return
		}

		err = os.Remove(logFilePath)
		if err != nil {
			fmt.Println("Error removing original log file:", err)
			return
		}

		fmt.Println("Log file compressed successfully.")
	} else {
		fmt.Println("Log file size is below the threshold. No compression needed.")
	}
}

func fileExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

func compress(cmd *cobra.Command, args []string) error {
	path, err := cmd.Flags().GetString("path")
	if err != nil {
		log.Error("failed to read the testcase path input")
		return err
	}

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run compress.go <test-set-number>")
		os.Exit(1)
	}

	pattern := `test-set-\d+` // Regex to identify the test-set folder correctly (test-set-<number>)
	re := regexp.MustCompile(pattern)
	if re.MatchString(path) == false {
		log.Error("Invalid path. PLease provide the path to the test-set folder")
		return nil
	}

	currentDir, err := os.Getwd()
	if err != nil {
		log.Error("failed to get the current working directory", zap.Any("error", err))
		return err
	}

	testSetPath := currentDir + "/keploy" + "/" + path + "/mocks.yaml"
	compressedTestSetPath := currentDir + "/keploy" + "/" + path + "/mocks.yaml.gz"

	if fileExists(compressedTestSetPath) == true {
		log.Error("File already compressed")
	} else if fileExists(testSetPath) == false {
		log.Error("File doesn't exists. PLease check the path to the test-set folder")
		return nil
	} else {
		checkAndCompress(testSetPath)
	}
	return nil
}

func (c *Compress) GetCmd() *cobra.Command {
	// Currently it can zip in gzip format but after unzipping it is not in yaml format. You need to mannually change the name to mocks.yaml

	var compressCmd = &cobra.Command{
		Use:     "compress mocks.yaml in desired test-set folder",
		Short:   "Compress desired mocks.yaml file",
		Example: `keploy compress --path test-set-<number>`,
		RunE:    compress,
	}

	compressCmd.Flags().StringP("path", "p", "", "Path to the test-set folder")

	return compressCmd
}
