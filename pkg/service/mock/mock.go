package mock

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

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

func (m *Mock) Put(ctx context.Context, path string, doc models.Mock) error {

	file, err := os.OpenFile(filepath.Join(path, "mock.yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		m.log.Error("failed to open the file", zap.Any("error", err))
		return err
	}

	d, err := yaml.Marshal(&doc)
	if err != nil {
		m.log.Error("failed to marshal document to yaml", zap.Any("error", err))
		return err
	}
	data := []byte(`---
`)
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		m.log.Error("failed to embed document into yaml file", zap.Any("error", err))
		return err
	}
	defer file.Close()

	return nil
}

func (m *Mock) GetAll(ctx context.Context, path string, name string) ([]models.Mock, error) {

	file, err := os.OpenFile(filepath.Join(path, "mock.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		m.log.Error("failed to open the yaml file", zap.Any("error", err))
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
			m.log.Error("failed to decode the yaml file documents into mock", zap.Any("error", err))
			return nil, err
		}
		if node.Name == name {
			arr = append(arr, node)
		}
	}

	return arr, nil
}
