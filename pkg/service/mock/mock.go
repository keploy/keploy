package mock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func NewMockService(log *zap.Logger) *Mock {
	return &Mock{
		log: log,
	}
}

type Mock struct {
	log *zap.Logger
}

func (m *Mock) CreateMockFile(path string, fileName string) bool {
	if _, err := os.Stat(filepath.Join(path, fileName+".yaml")); err != nil {
		err := os.MkdirAll(filepath.Join(path), os.ModePerm)
		if err != nil {
			m.log.Error("failed to create a mock dir", zap.Error(err))
			return false
		}
		_, err = os.Create(filepath.Join(path, fileName+".yaml"))
		if err != nil {
			m.log.Error("failed to create a yaml file", zap.Error(err))
			return false
		}
		return true
	}
	return false
}

func (m *Mock) FileExists(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path)); err == nil {
		return true
	}
	return false
}

func (m *Mock) Put(ctx context.Context, path string, doc models.Mock, meta interface{}) error {

	isGenerated := false
	if doc.Name == "" {
		doc.Name = uuid.New().String()
		isGenerated = true
	}
	isFileEmpty := m.CreateMockFile(path, doc.Name)
	file, err := os.OpenFile(filepath.Join(path, doc.Name+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		m.log.Error("failed to open the file", zap.Any("error", err))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yaml.Marshal(&doc)
	if err != nil {
		m.log.Error("failed to marshal document to yaml", zap.Any("error", err))
		return err
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		m.log.Error("failed to embed document into yaml file", zap.Any("error", err))
		return err
	}
	defer file.Close()
	MockPathStr := fmt.Sprint("\n✅ Mocks are successfully written in yaml file at path: ", path, "/", doc.Name, ".yaml", "\n")
	if isGenerated {
		MockConfigStr := fmt.Sprint("\n\n🚨 Note: Please set the mock.Config.Name to auto generated name in your unit test. Ex: \n    mock.Config{\n      Name: ", doc.Name, "\n    }\n")
		MockNameStr := fmt.Sprint("\n💡 Auto generated name for your mock: ", doc.Name, " for ", doc.Kind, " with meta: {\n", mapToStrLog(meta.(map[string]string)), "   }")
		m.log.Info(fmt.Sprint(MockNameStr, MockConfigStr, MockPathStr))
	} else {
		m.log.Info(MockPathStr)
	}
	return nil
}

func ReadAll(log *zap.Logger, path, name string, libMode bool) ([]models.Mock, error) {
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	arr := []models.Mock{}
	for {
		var node models.Mock
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Error("failed to decode the yaml file documents into mock", zap.Any("error", err))
			return nil, err
		}
		if !libMode || node.Name == name {
			arr = append(arr, node)
		}
	}
	return arr, nil
}

func (m *Mock) GetAll(ctx context.Context, path string, name string) ([]models.Mock, error) {

	// file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	// if err != nil {
	// 	m.log.Error("failed to open the yaml file", zap.Any("error", err))
	// 	return nil, err
	// }
	// defer file.Close()
	// decoder := yaml.NewDecoder(file)
	// arr := []models.Mock{}
	// for {
	// 	var node models.Mock
	// 	err := decoder.Decode(&node)
	// 	if errors.Is(err, io.EOF) {
	// 		break
	// 	}
	// 	if err != nil {
	// 		m.log.Error("failed to decode the yaml file documents into mock", zap.Any("error", err))
	// 		return nil, err
	// 	}
	// 	if node.Name == name {
	// 		arr = append(arr, node)
	// 	}
	// }
	arr, err := ReadAll(m.log, path, name, true)
	if err != nil {
		return nil, err
	}
	MockPathStr := fmt.Sprint("\n✅ Mocks are read successfully from yaml file at path: ", path, "/", name, ".yaml", "\n")
	m.log.Info(MockPathStr)

	return arr, nil
}

func mapToStrLog(meta map[string]string) string {
	res := ""
	for k, v := range meta {
		res += "     " + k + ": " + v + "\n"
	}
	return res
}
