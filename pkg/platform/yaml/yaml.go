package yaml

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type yaml struct {
	tcsPath string
	mockPath string
	logger *zap.Logger
}

func NewYamlStore(tcsPath, mockPath string, logger *zap.Logger) platform.TestCaseDB {
	return &yaml{
		tcsPath: tcsPath,
		mockPath: mockPath,
		logger: logger,
	}
}

// createYamlFile is used to create the yaml file along with the path directory (if does not exists)
func  createYamlFile(path string, fileName string, logger *zap.Logger) (bool, error) {
	// checks id the yaml exists
	if _, err := os.Stat(filepath.Join(path, fileName+".yaml")); err != nil {
		// creates the path director if does not exists
		err = os.MkdirAll(filepath.Join(path), os.ModePerm)
		if err != nil {
			logger.Error("failed to create a directory for the yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}
		// create the yaml file
		_, err = os.Create(filepath.Join(path, fileName+".yaml"))
		if err != nil {
			logger.Error("failed to create a yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}

		return true, nil
	}
	return false, nil
}

// findLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func findLastIndex (path string, logger *zap.Logger) (int, error) {

	dir, err := os.OpenFile(path, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		return 1, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return 1, nil
	}

	lastIndex := 0
	for _, v := range files {
		fileName := filepath.Base(v.Name())
		fileNameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		if len(strings.Split(fileNameWithoutExt, "-")) < 1 {
			logger.Error("failed to decode the last sequence number from yaml test", zap.Any("for the file", fileName), zap.Any("at path", path))
			return 0, errors.New("failed to decode the last sequence number from yaml test")
		}
		indxStr := strings.Split(fileNameWithoutExt, "-")[1]
		indx, err := strconv.Atoi(indxStr)
		if err != nil {
			logger.Error("failed to read the sequence number from the yaml file name", zap.Error(err), zap.Any("for the file", fileName))
			return 0, err
		}
		if indx > lastIndex {
			lastIndex = indx
		}
	}
	lastIndex += 1

	return lastIndex, nil
}

// write is used to generate the yaml file for the recorded calls and writes the yaml document.
func (ys *yaml) write(path, fileName string, doc models.Mock) error {
	// 
	isFileEmpty, err := createYamlFile(path, fileName, ys.logger)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(path, fileName+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		ys.logger.Error("failed to open the created yaml file", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		ys.logger.Error("failed to marshal the recorded calls into yaml", zap.Error(err), zap.Any("yaml file name", fileName))
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

func (ys *yaml) Insert(tc *models.Mock, mocks []*models.Mock) error {
	// finds the recently generated testcase to derive the sequence number for the current testcase
	lastIndx, err := findLastIndex(ys.tcsPath, ys.logger)
	if err != nil {
		return err
	}

	// write testcase yaml
	tcName := fmt.Sprintf("test-%v", lastIndx)
	tc.Name = tcName
	err = ys.write(ys.tcsPath, tcName, *tc) 
	if err!= nil {
		ys.logger.Error("failed to write testcase yaml file", zap.Error(err))
		return err
	}

	// write the mock yamls
	for i, v := range mocks {
		mockName := fmt.Sprintf("mock-%v", lastIndx)
		v.Name = mockName+fmt.Sprintf("-%v", i)
		err = ys.write(ys.mockPath, mockName, *v)
		if err != nil {
			ys.logger.Error("failed to write the yaml for mock", zap.Any("mockId", v.Name), zap.Error(err))
			return err
		}
	}

	return nil
}

func (ys *yaml) Read (options interface{}) ([]models.Mock,  map[string][]models.Mock, error) {
	tcs := []models.Mock{}
	mocks := map[string][]models.Mock{}

	dir, err := os.OpenFile(ys.tcsPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", ys.tcsPath))
		return nil, nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", ys.tcsPath))
		return nil, nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		tc, err := read(ys.tcsPath, name)
		if err != nil {
			return nil, nil, err
		}

		m, err := read(ys.mockPath, "mock-"+strings.Split(name, "-")[1]) 
		if err != nil {
			return nil, nil, err
		}
		mocks[name] = m
		if len(tc) == 1 {
			tcs = append(tcs, tc[0])
		}

		// tests, err := toTestCase(tcs, name, mockPath)
		// if err != nil {
		// 	return nil, err
		// }
		// res = append(res, tests...)
	}
	// sort.Slice(res, func(i, j int) bool {
	// 	return res[i].Captured < res[j].Captured
	// })

	// if tcsType != "" {
	// 	filteredTcs := reqTypeFilter(res, tcsType)
	// 	res = filteredTcs
	// }


	return tcs, mocks, nil
}

func read(path, name string) ([]models.Mock, error) {
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	yamlDocs := []models.Mock{}
	for {
		var doc models.Mock
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		// if !libMode || doc.Name == name {
			yamlDocs = append(yamlDocs, doc)
		// }
	}
	return yamlDocs, nil
}
