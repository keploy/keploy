package yaml

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version      models.Version `json:"version" yaml:"version"`
	Kind         models.Kind    `json:"kind" yaml:"kind"`
	Name         string         `json:"name" yaml:"name"`
	Spec         yamlLib.Node   `json:"spec" yaml:"spec"`
	Curl         string         `json:"curl" yaml:"curl,omitempty"`
	ConnectionID string         `json:"connectionId" yaml:"connectionId,omitempty"`
}

// ctxReader wraps an io.Reader with a context for cancellation support
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (n int, err error) {
	select {
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	default:
		return cr.r.Read(p)
	}
}

// ctxWriter wraps an io.Writer with a context for cancellation support
type ctxWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (cw *ctxWriter) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		var written int
		written, err = cw.writer.Write(p)
		n += written
		if err != nil {
			return n, err
		}
		p = p[written:]
	}
	return n, nil
}

func WriteFile(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool) error {
	isFileEmpty, err := CreateYamlFile(ctx, logger, path, fileName)
	if err != nil {
		return err
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if isAppend {
		data := []byte("---\n")
		if isFileEmpty {
			data = []byte{}
		}
		docData = append(data, docData...)
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	yamlPath := filepath.Join(path, fileName+".yaml")
	file, err := os.OpenFile(yamlPath, flag, fs.ModePerm)
	if err != nil {
		utils.LogError(logger, err, "failed to open file for writing", zap.String("file", yamlPath))
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", yamlPath))
		}
	}()

	cw := &ctxWriter{
		ctx:    ctx,
		writer: file,
	}

	_, err = cw.Write(docData)
	if err != nil {
		if err == ctx.Err() {
			return nil // Ignore context cancellation error
		}
		utils.LogError(logger, err, "failed to write the yaml document", zap.String("yaml file name", fileName))
		return err
	}
	return nil
}

func ReadFile(ctx context.Context, logger *zap.Logger, path, name string) ([]byte, error) {
	filePath := filepath.Join(path, name+".yaml")
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	cr := &ctxReader{
		ctx: ctx,
		r:   file,
	}

	data, err := io.ReadAll(cr)
	if err != nil {
		if err == ctx.Err() {
			return nil, err // Ignore context cancellation error
		}
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}
	return data, nil
}

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	yamlPath, err := ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		utils.LogError(Logger, err, "failed to validate the yaml file path", zap.String("path directory", path), zap.String("yaml", fileName))
		return false, err
	}
	if _, err := os.Stat(yamlPath); err != nil {
		if ctx.Err() == nil {
			err = os.MkdirAll(filepath.Join(path), 0777)
			if err != nil {
				utils.LogError(Logger, err, "failed to create a directory for the yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			err = os.Chmod(path, 0777)
			if err != nil {
				utils.LogError(Logger, err, "failed to set permissions for the directory", zap.String("path directory", path))
				return false, err
			}
			file, err := os.OpenFile(yamlPath, os.O_CREATE, 0777) // Set file permissions to 777
			if err != nil {
				utils.LogError(Logger, err, "failed to create a yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			err = file.Close()
			if err != nil {
				utils.LogError(Logger, err, "failed to close the yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			err = os.Chmod(yamlPath, 0777)
			if err != nil {
				utils.LogError(Logger, err, "failed to set permissions for the directory", zap.String("path directory", path))
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func ReadSessionIndices(_ context.Context, path string, Logger *zap.Logger) ([]string, error) {
	var indices []string
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
		if v.Name() != "reports" && v.Name() != "testReports" {
			indices = append(indices, v.Name())
		}
	}
	return indices, nil
}
