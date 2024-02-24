package yaml

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform"
	"go.keploy.io/server/v2/pkg/platform/yaml"
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

func (nd *NetworkTrafficDoc) GetKind() string {
	return string(nd.Kind)
}

// write is used to generate the yaml file for the recorded calls and writes the yaml document.
func Write(ctx context.Context, logger *zap.Logger, path, fileName string, docRead platform.KindSpecifier) error {
	//
	doc, _ := docRead.(*yaml.NetworkTrafficDoc)
	isFileEmpty, err := CreateYamlFile(path, fileName, logger)
	if err != nil {
		return err
	}

	yamlPath, err := ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		return err
	}

	file, err := os.OpenFile(yamlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		logger.Error("failed to open the created yaml file", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		logger.Error("failed to marshal the recorded calls into yaml", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		logger.Error("failed to write the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	defer file.Close()

	return nil
}

func Read(path, name string) ([]*NetworkTrafficDoc, error) {
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	yamlDocs := []*yaml.NetworkTrafficDoc{}
	for {
		var doc yaml.NetworkTrafficDoc
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		yamlDocs = append(yamlDocs, &doc)
	}
	return yamlDocs, nil
}

var idCounter int64 = -1

func GetNextID() int64 {
	return atomic.AddInt64(&idCounter, 1)
}

// createYamlFile is used to create the yaml file along with the path directory (if does not exists)
func CreateYamlFile(path string, fileName string, Logger *zap.Logger) (bool, error) {
	// checks id the yaml exists
	yamlPath, err := ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(yamlPath); err != nil {
		// creates the path director if does not exists
		err = os.MkdirAll(filepath.Join(path), fs.ModePerm)
		if err != nil {
			Logger.Error("failed to create a directory for the yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}

		// create the yaml file
		_, err := os.Create(yamlPath)
		if err != nil {
			Logger.Error("failed to create a yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}

		// since, keploy requires root access. The permissions for generated files
		// should be updated to share it with all users.
		keployPath := path
		if strings.Contains(path, "keploy/"+models.TestSetPattern) {
			keployPath = filepath.Join(strings.TrimSuffix(path, filepath.Base(path)))
		}
		Logger.Debug("the path to the generated keploy directory", zap.Any("path", keployPath))
		cmd := exec.Command("sudo", "chmod", "-R", "777", keployPath)
		err = cmd.Run()
		if err != nil {
			Logger.Error("failed to set the permission of keploy directory", zap.Error(err))
			return false, err
		}

		return true, nil
	}
	return false, nil
}
