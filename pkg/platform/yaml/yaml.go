package yaml

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version models.Version `json:"version" yaml:"version"`
	Kind    models.Kind    `json:"kind" yaml:"kind"`
	Name    string         `json:"name" yaml:"name"`
	Spec    yamlLib.Node   `json:"spec" yaml:"spec"`
	Curl    string         `json:"curl" yaml:"curl,omitempty"`
}

// ContextReader wraps an io.Reader with a context for cancellation support
type ContextReader struct {
	Reader io.Reader
	Ctx    context.Context
}

// Read implements the io.Reader interface for ContextReader
func (cr *ContextReader) Read(p []byte) (n int, err error) {
	select {
	case <-cr.Ctx.Done():
		return 0, cr.Ctx.Err()
	default:
		return cr.Reader.Read(p)
	}
}

func (nd *NetworkTrafficDoc) GetKind() string {
	return string(nd.Kind)
}

func WriteFile(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte) error {
	isFileEmpty, err := CreateYamlFile(ctx, logger, path, fileName)
	if err != nil {
		return err
	}
	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	data = append(data, docData...)
	yamlPath := filepath.Join(path, fileName+".yaml")
	err = os.WriteFile(yamlPath, data, os.ModePerm)
	if err != nil {
		logger.Error("failed to write the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	return nil
}

func ReadFile(path, name string) ([]byte, error) {
	filePath := filepath.Join(path, name+".yaml")
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}
	return data, nil
}

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	yamlPath, err := ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(yamlPath); err != nil {
		err = os.MkdirAll(filepath.Join(path), fs.ModePerm)
		if err != nil {
			Logger.Error("failed to create a directory for the yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}
		file, err := os.OpenFile(yamlPath, os.O_CREATE, 0777) // Set file permissions to 777
		if err != nil {
			Logger.Error("failed to create a yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}
		file.Close()
		return true, nil
	}
	return false, nil
}

func ReadSessionIndices(path string, Logger *zap.Logger) ([]string, error) {
	indices := []string{}
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	for _, v := range files {
		if v.Name() != "testReports" {
			indices = append(indices, v.Name())
		}
	}
	return indices, nil
}
