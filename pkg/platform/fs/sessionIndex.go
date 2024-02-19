package fs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewSessionIndex(path string, Logger *zap.Logger) (string, error) {
	indx := 0
	dir, err := os.OpenFile(path, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return fmt.Sprintf("%s%v", models.TestSetPattern, indx), nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return "", err
	}

	for _, v := range files {
		// fmt.Println("name for the file", v.Name())
		fileName := filepath.Base(v.Name())
		fileNamePackets := strings.Split(fileName, "-")
		if len(fileNamePackets) == 3 {
			fileIndx, err := strconv.Atoi(fileNamePackets[2])
			if err != nil {
				Logger.Debug("failed to convert the index string to integer", zap.Error(err))
				continue
			}
			if indx < fileIndx+1 {
				indx = fileIndx + 1
			}
		}
	}
	return fmt.Sprintf("%s%v", models.TestSetPattern, indx), nil
}
