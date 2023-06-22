package yaml

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		err = os.MkdirAll(filepath.Join(path), fs.ModePerm)
		if err != nil {
			logger.Error("failed to create a directory for the yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}
		// Changes the permission of created directory to 777
		if strings.HasSuffix(path, "Keploy/tests") || strings.HasSuffix(path, "Keploy/mocks") {
			err = os.Chmod(filepath.Join(strings.TrimSuffix( strings.TrimSuffix(path, "/mocks"), "/tests")), fs.ModePerm)
			if err != nil {
				logger.Error("failed to change the ./Keploy directory permission", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
				return false, err
			}
		}
		err = os.Chmod(filepath.Join(path), fs.ModePerm)
		if err != nil {
			logger.Error("failed to change the created directory permission", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}

		// create the yaml file
		yamlFile, err := os.Create(filepath.Join(path, fileName+".yaml"))
		if err != nil {
			logger.Error("failed to create a yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}
		// changes user permission to allow write operation on yaml file
		err = yamlFile.Chmod(fs.ModePerm)
		if err!=nil {
			logger.Error("failed to set the permission of yaml file", zap.Error(err))
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
func (ys *yaml) write(path, fileName string, doc NetworkTrafficDoc) error {
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

// func (ys *yaml) Insert(tc *models.Mock, mocks []*models.Mock) error {
func (ys *yaml) Insert(tc *models.TestCase) error {
	// finds the recently generated testcase to derive the sequence number for the current testcase
	lastIndx, err := findLastIndex(ys.tcsPath, ys.logger)
	if err != nil {
		return err
	}

	// encode the testcase and its mocks into yaml docs
	yamlTc, yamlMocks, err := Encode(*tc, ys.logger)
	if err != nil {
		return err
	}

	// write testcase yaml
	tcName := fmt.Sprintf("test-%v", lastIndx)
	yamlTc.Name = tcName
	err = ys.write(ys.tcsPath, tcName, *yamlTc) 
	if err!= nil {
		ys.logger.Error("failed to write testcase yaml file", zap.Error(err))
		return err
	}
	ys.logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", ys.tcsPath), zap.String("testcase name", tcName))
	
	// write the mock yamls
	mockName := fmt.Sprintf("mock-%v", lastIndx)
	for i, v := range yamlMocks {
		v.Name = mockName+fmt.Sprintf("-%v", i)
		err = ys.write(ys.mockPath, mockName, v)
		if err != nil {
			ys.logger.Error("failed to write the yaml for mock", zap.Any("mockId", v.Name), zap.Error(err))
			return err
		}
	}
	if len(yamlMocks) > 0 {
		ys.logger.Info("🟠 Keploy has recorded mocks for the external calls of user's application", zap.String("path", ys.mockPath), zap.String("mock name", mockName))
	}

	return nil
}

// func (ys *yaml) Read (options interface{}) ([]models.Mock,  map[string][]models.Mock, error) {
func (ys *yaml) Read (options interface{}) ([]*models.TestCase, error) {
	tcs := []*models.TestCase{}

	dir, err := os.OpenFile(ys.tcsPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", ys.tcsPath))
		return nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", ys.tcsPath))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		yamlTestcase, err := read(ys.tcsPath, name)
		if err != nil {
			ys.logger.Error("failed to read the testcase from yaml", zap.Error(err))
			return nil, err
		}

		yamlMocks := []*NetworkTrafficDoc{}
		mockName := "mock-"+strings.Split(name, "-")[1]
		// check if mocks exists for the current testcase. read the yaml documents if mock exists.
		if _, err := os.Stat(filepath.Join(ys.mockPath, mockName + ".yaml")); err==nil {
			yamlMocks, err = read(ys.mockPath, mockName) 
			if err != nil {
				ys.logger.Error("failed to read the mocks from yaml", zap.Error(err), zap.Any("mocks for testcase", yamlTestcase[0].Name))
				return nil, err
			}
		}

		// Unmarshal the yaml doc into Testcase
		tc, err := Decode(yamlTestcase[0], yamlMocks, ys.logger)
		if err != nil {
			return nil, err
		}
		// Append the encoded testcase
		tcs = append(tcs, tc)
	}

	sort.Slice(tcs, func(i, j int) bool {
		return tcs[i].Created < tcs[j].Created
	})

	return tcs, nil
}

func read(path, name string) ([]*NetworkTrafficDoc, error) {
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	yamlDocs := []*NetworkTrafficDoc{}
	for {
		var doc NetworkTrafficDoc
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		// if !libMode || doc.Name == name {
			yamlDocs = append(yamlDocs, &doc)
		// }
	}
	return yamlDocs, nil
}
