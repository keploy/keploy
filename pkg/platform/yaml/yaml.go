package yaml

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var Emoji = "\U0001F430" + " Keploy:"

type Yaml struct {
	TcsPath  string
	MockPath string
	MockName string
	TcsName  string
	Logger   *zap.Logger
}

// func NewYamlStore(tcsPath, mockPath string, Logger *zap.Logger) platform.TestCaseDB {
func NewYamlStore(tcsPath string, mockPath string, tcsName string, mockName string, Logger *zap.Logger) platform.TestCaseDB {
	return &Yaml{
		TcsPath:  tcsPath,
		MockPath: mockPath,
		MockName: mockName,
		TcsName:  tcsName,
		Logger:   Logger,
	}
}

// createYamlFile is used to create the yaml file along with the path directory (if does not exists)
func createYamlFile(path string, fileName string, Logger *zap.Logger) (bool, error) {
	// checks id the yaml exists
	if _, err := os.Stat(filepath.Join(path, fileName+".yaml")); err != nil {
		// creates the path director if does not exists
		err = os.MkdirAll(filepath.Join(path), fs.ModePerm)
		if err != nil {
			Logger.Error("failed to create a directory for the yaml file", zap.Error(err), zap.Any("path directory", path), zap.Any("yaml", fileName))
			return false, err
		}

		// create the yaml file
		_, err := os.Create(filepath.Join(path, fileName+".yaml"))
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

// findLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func findLastIndex(path string, Logger *zap.Logger) (int, error) {

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
		if v.Name() == "mocks.yaml" || v.Name() == "config.yaml" {
			continue
		}
		fileName := filepath.Base(v.Name())
		fileNameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		if len(strings.Split(fileNameWithoutExt, "-")) < 2 {
			Logger.Error("failed to decode the last sequence number from yaml test", zap.Any("for the file", fileName), zap.Any("at path", path))
			return 0, errors.New("failed to decode the last sequence number from yaml test")
		}
		indxStr := strings.Split(fileNameWithoutExt, "-")[1]
		indx, err := strconv.Atoi(indxStr)
		if err != nil {
			Logger.Error("failed to read the sequence number from the yaml file name", zap.Error(err), zap.Any("for the file", fileName))
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
func (ys *Yaml) Write(path, fileName string, doc NetworkTrafficDoc) error {
	//
	isFileEmpty, err := createYamlFile(path, fileName, ys.Logger)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(path, fileName+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the created yaml file", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		ys.Logger.Error("failed to marshal the recorded calls into yaml", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		ys.Logger.Error("failed to write the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	defer file.Close()

	return nil
}

// func (ys *yaml) Insert(tc *models.Mock, mocks []*models.Mock) error {
func (ys *Yaml) WriteTestcase(tc *models.TestCase) error {

	var tcsName string
	if ys.TcsName == "" {
		// finds the recently generated testcase to derive the sequence number for the current testcase
		lastIndx, err := findLastIndex(ys.TcsPath, ys.Logger)
		if err != nil {
			return err
		}
		tcsName = fmt.Sprintf("test-%v", lastIndx)
	} else {
		tcsName = ys.TcsName
	}

	// encode the testcase and its mocks into yaml docs
	// yamlTc, yamlMocks, err := EncodeTestcase(*tc, ys.Logger)
	yamlTc, err := EncodeTestcase(*tc, ys.Logger)
	if err != nil {
		return err
	}

	// write testcase yaml
	yamlTc.Name = tcsName
	err = ys.Write(ys.TcsPath, tcsName, *yamlTc)
	if err != nil {
		ys.Logger.Error("failed to write testcase yaml file", zap.Error(err))
		return err
	}
	ys.Logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", ys.TcsPath), zap.String("testcase name", tcsName))

	// write the mock yamls
	// mockName := fmt.Sprintf("mock-%v", lastIndx)
	// for i, v := range yamlMocks {
	// 	v.Name = mockName + fmt.Sprintf("-%v", i)
	// 	err = ys.write(ys.mockPath, mockName, v)
	// 	if err != nil {
	// 		ys.Logger.Error("failed to write the yaml for mock", zap.Any("mockId", v.Name), zap.Error(err))
	// 		return err
	// 	}
	// }
	// if len(yamlMocks) > 0 {
	// 	ys.Logger.Info("🟠 Keploy has recorded mocks for the external calls of user's application", zap.String("path", ys.mockPath), zap.String("mock name", mockName))
	// }
	return nil
}

// func (ys *yaml) Read (options interface{}) ([]models.Mock,  map[string][]models.Mock, error) {
func (ys *Yaml) ReadTestcase(path string, options interface{}) ([]*models.TestCase, error) {

	if path == "" {
		path = ys.TcsPath
	}

	tcs := []*models.TestCase{}
	// tcsPath := filepath.Join(path, "tests")

	_, err := os.Stat(path)
	if err != nil {
		dirNames := strings.Split(path, "/")
		// fmt.Println(dirNames)
		suitName := ""
		if len(dirNames) > 1 {
			suitName = dirNames[len(dirNames)-2]
		}
		ys.Logger.Debug("no tests are recorded for the session", zap.String("index", suitName))
		return tcs, nil
	}

	dir, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", path))
		return nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.Logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", path))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		yamlTestcase, err := read(path, name)
		if err != nil {
			ys.Logger.Error("failed to read the testcase from yaml", zap.Error(err))
			return nil, err
		}

		// yamlMocks := []*NetworkTrafficDoc{}
		// mockName := "mock-" + strings.Split(name, "-")[1]
		// check if mocks exists for the current testcase. read the yaml documents if mock exists.
		// if _, err := os.Stat(filepath.Join(ys.mockPath, mockName+".yaml")); err == nil {
		// 	yamlMocks, err = read(ys.mockPath, mockName)
		// 	if err != nil {
		// 		ys.Logger.Error("failed to read the mocks from yaml", zap.Error(err), zap.Any("mocks for testcase", yamlTestcase[0].Name))
		// 		return nil, err
		// 	}
		// }

		// Unmarshal the yaml doc into Testcase
		tc, err := Decode(yamlTestcase[0], ys.Logger)
		if err != nil {
			return nil, err
		}
		// Append the encoded testcase
		tcs = append(tcs, tc)
	}

	sort.Slice(tcs, func(i, j int) bool {
		return tcs[i].Created < tcs[j].Created
	})

	// if _, err := os.Stat(filepath.Join(path, "mocks.yaml")); err == nil {
	// 	mockYamls, err := read(path, "mocks")
	// 	if err != nil {
	// 		ys.Logger.Error("failed to read the mocks from yaml", zap.Error(err))
	// 		return nil, nil, err
	// 	}
	// 	mocks, err := decodeMocks(mockYamls, ys.Logger)
	// 	return tcs, mocks, nil
	// }
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

func (ys *Yaml) WriteMock(mock *models.Mock) error {

	if ys.MockName != "" {
		mock.Name = ys.MockName
	}

	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}

	if mock.Name == "" {
		mock.Name = "mocks"
	}

	err = ys.Write(ys.MockPath, mock.Name, *mockYaml)
	if err != nil {
		return err
	}

	return nil
}

func (ys *Yaml) ReadMocks(path string) ([]*models.Mock, []*models.Mock, error) {
	var (
		configMocks = []*models.Mock{}
		tcsMocks    = []*models.Mock{}
	)

	if path == "" {
		path = ys.MockPath
	}

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	mockPath, err := ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		// _, err := os.Stat(filepath.Join(path, "config.yaml"))
		// if err != nil {
		// 	ys.Logger.Error("failed to find the config yaml", zap.Error(err))
		// 	return nil, nil, err
		// }

		yamls, err := read(path, mockName)
		if err != nil {
			ys.Logger.Error("failed to read the mocks from config yaml", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, nil, err
		}
		mocks, err := decodeMocks(yamls, ys.Logger)
		if err != nil {
			ys.Logger.Error("failed to decode the config mocks from yaml docs", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, nil, err
		}


		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] == "config" {
				configMocks = append(configMocks, mock)
			} else {
				tcsMocks = append(tcsMocks, mock)
			}
		}
	}

	return configMocks, tcsMocks, nil

}
