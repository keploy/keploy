package cmd

import (
	"compress/gzip"
	"fmt"
	"github.com/cloudflare/cfssl/log"
	"github.com/spf13/cobra"
	// "go.uber.org/zap"
	"io"
	"os"
	"regexp"
)

const (
	maxSizeBytes = 5 * 1024 * 1024 // 5MB
)

// func NewCmdCompress(logger *zap.Logger) *Example {
// 	return &Example{
// 		logger: logger,
// 	}
// }

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

func compress(cmd *cobra.Command, args []string) error {
	if len(os.Args) != 2 {
		fmt.Println("Usage: go run compress.go <test-set-number>")
		os.Exit(1)
	}

	logFilePath := os.Args[1]
	log.Info("log file path : ", logFilePath)

	path, err := cmd.Flags().GetString("path")
	if err != nil {
		log.Error("failed to read the testcase path input")
		return err
	}

	pattern := `test-set-\d+`
	re := regexp.MustCompile(pattern)
	if !re.MatchString(path) {
		log.Error("Invalid path. PLease provide the path to the test-set folder")
		return nil
	}

	currentDir, err := os.Getwd()
	if err != nil {
		fmt.Println("Error:", err)
		return err
	}

	testSetPath := currentDir + "/keploy" + "/" + path
	log.Debug("test-set path : ", testSetPath)

	checkAndCompress(testSetPath)
	return nil
}

func GetCmd() *cobra.Command {
	// TODO: Have to make this owrk like a command "keploy compress <test-set-i>"

	var compressCmd = &cobra.Command{
		Use:     "compress mocks.yaml in deesired test-set folder",
		Short:   "Compress dseired mocks.yaml file",
		Example: `keploy compress <test-set-number>`,
		Args:    cobra.ExactArgs(1),
		RunE:    compress,
	}

	compressCmd.Flags().StringP("path", "p", "", "Path to the mocks folder")
	return compressCmd
}
