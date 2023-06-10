package persistence

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

type FileSystem interface {
	OpenFile(name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	CreateFile(folderLocation, fileNameWithoutExtension, fileExtensionWithoutDot string) (bool, error)
	FindNextUsableIndexForYaml(directoryPath string) (int, error)
	GetAllYamlFileNamesInDirectory(directoryPath string) ([]string, error)
}

// Native fileSystem is a wrapper over the os functions for local storage.
type Native struct {
	logger *zap.Logger
}

func NewNativeFilesystem(logger *zap.Logger) FileSystem {
	return &Native{logger: logger}
}

func (n *Native) OpenFile(name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	return os.OpenFile(name, flag, perm)
}

func (n *Native) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

func (n *Native) CreateFile(folderLocation, fileNameWithoutExtension, fileExtensionWithoutDot string) (bool, error) {
	// If the file does not exist, create it.
	if _, err := os.Stat(filepath.Join(folderLocation,
		fmt.Sprintf("%s.%s", fileNameWithoutExtension, fileExtensionWithoutDot))); err != nil {
		// Create the nested parent folders if they do not exist.
		err = os.MkdirAll(filepath.Join(folderLocation), os.ModePerm)
		if err != nil {
			n.logger.Error("failed to create directory for the yaml file", zap.Error(err),
				zap.Any("path directory", folderLocation), zap.Any("name", fileNameWithoutExtension))
			return false, err
		}
		// Create the file with the specified extension.
		_, err = os.Create(filepath.Join(folderLocation,
			fmt.Sprintf("%s.%s", fileNameWithoutExtension, fileExtensionWithoutDot)))
		if err != nil {
			n.logger.Error("Failed to create file", zap.Error(err),
				zap.Any("path directory", folderLocation), zap.Any("name", fileNameWithoutExtension))
			return false, err
		}

		return true, nil
	}
	return false, nil
}

func (n *Native) FindNextUsableIndexForYaml(directoryPath string) (int, error) {
	dir, err := os.OpenFile(directoryPath, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		return 0, nil
	}

	// Read all the files in that directory.
	files, err := dir.ReadDir(0)
	if err != nil {
		return 0, nil
	}

	lastUsedIndex := 0
	for _, v := range files {
		fileName := filepath.Base(v.Name())
		fileNameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		if len(strings.Split(fileNameWithoutExt, "-")) < 1 {
			n.logger.Error("failed to decode the last sequence number from yaml test",
				zap.Any("filenae", fileName), zap.Any("directory", directoryPath))
			return 0, fmt.Errorf("failed to decode the last sequence number from yaml test")
		}
		indexStr := strings.Split(fileNameWithoutExt, "-")[1]
		index, err := strconv.Atoi(indexStr)
		if err != nil {
			n.logger.Error("failed to read the sequence number from the yaml file name",
				zap.Error(err), zap.Any("filename", fileName))
			return 0, err
		}
		if index > lastUsedIndex {
			lastUsedIndex = index
		}
	}

	// The next usable index would be last used index + 1.
	return lastUsedIndex + 1, nil
}

// GetAllYamlFileNamesInDirectory returns the list of all filenames with YAML extension in the current directory.
// Note that the extension is stripped off. For example, test-1.yaml, test-2.yaml would be reported as test-1, test-2.
func (n *Native) GetAllYamlFileNamesInDirectory(directoryPath string) ([]string, error) {
	// Open the directory metadata.
	dir, err := os.OpenFile(directoryPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		n.logger.Error("Failed to open the directory containing yaml testcases",
			zap.Error(err), zap.Any("directoryPath", directoryPath))
		return nil, err
	}

	filesMetadata, err := dir.ReadDir(0)
	if err != nil {
		n.logger.Error("Failed to read the file names of yaml testcases",
			zap.Error(err), zap.Any("path", directoryPath))
		return nil, err
	}

	var names []string
	for _, file := range filesMetadata {
		// Ignore directories and non YAML files.
		if file.IsDir() || filepath.Ext(file.Name()) != ".yaml" {
			continue
		}

		name := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
		names = append(names, name)
	}

	return names, nil
}
