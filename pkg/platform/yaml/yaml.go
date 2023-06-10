package yaml

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/persistence"
	"go.keploy.io/server/pkg/platform"
)

type yamlStore struct {
	tcsPath    string
	mockPath   string
	fileSystem persistence.FileSystem
	logger     *zap.Logger
}

func NewYamlStore(tcsPath, mockPath string, fileSystem persistence.FileSystem, logger *zap.Logger) platform.TestCaseDB {
	return &yamlStore{
		tcsPath:    tcsPath,
		mockPath:   mockPath,
		fileSystem: fileSystem,
		logger:     logger,
	}
}

// write is used to generate the yaml file for the recorded calls and writes the yaml document.
func (ys *yamlStore) write(path, fileName string, doc models.Mock) error {
	isFileEmpty, err := ys.fileSystem.CreateFile(path, fileName, "yaml")
	if err != nil {
		return err
	}

	file, err := ys.fileSystem.OpenFile(filepath.Join(path, fileName+".yaml"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		ys.logger.Error("failed to open the created yaml file", zap.Error(err),
			zap.Any("yaml file name", fileName))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		ys.logger.Error("failed to marshal the recorded calls into yaml", zap.Error(err),
			zap.Any("yaml file name", fileName))
		return err
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		ys.logger.Error("failed to write the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	defer file.Close()

	return nil
}

func (ys *yamlStore) Insert(tc *models.Mock, mocks []*models.Mock) error {
	// finds the recently generated testcase to derive the sequence number for the current testcase
	lastIndx, err := ys.fileSystem.FindNextUsableIndexForYaml(ys.tcsPath)
	if err != nil {
		return err
	}

	// write testcase yaml
	tcName := fmt.Sprintf("test-%v", lastIndx)
	tc.Name = tcName
	err = ys.write(ys.tcsPath, tcName, *tc)
	if err != nil {
		ys.logger.Error("failed to write testcase yaml file", zap.Error(err))
		return err
	}

	// write the mock yamls
	for i, v := range mocks {
		mockName := fmt.Sprintf("mock-%v", lastIndx)
		v.Name = mockName + fmt.Sprintf("-%v", i)
		err = ys.write(ys.mockPath, mockName, *v)
		if err != nil {
			ys.logger.Error("failed to write the yaml for mock", zap.Any("mockId", v.Name), zap.Error(err))
			return err
		}
	}

	return nil
}

func (ys *yamlStore) Read(options interface{}) ([]models.Mock, map[string][]models.Mock, error) {
	var tcs []models.Mock
	mocks := map[string][]models.Mock{}

	names, err := ys.fileSystem.GetAllYamlFileNamesInDirectory(ys.tcsPath)
	if err != nil {
		return nil, nil, err
	}

	for _, name := range names {
		tc, err := ys.read(ys.tcsPath, name)
		if err != nil {
			return nil, nil, err
		}

		m, err := ys.read(ys.mockPath, "mock-"+strings.Split(name, "-")[1])
		if err != nil {
			return nil, nil, err
		}
		mocks[name] = m
		if len(tc) == 1 {
			tcs = append(tcs, tc[0])
		}
	}

	return tcs, mocks, nil
}

func (ys *yamlStore) read(path, name string) ([]models.Mock, error) {
	file, err := ys.fileSystem.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := yamlLib.NewDecoder(file)
	var yamlDocs []models.Mock
	for {
		var doc models.Mock
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		yamlDocs = append(yamlDocs, doc)
	}
	return yamlDocs, nil
}
