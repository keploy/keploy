package mock

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewMockService(mockFS models.MockFS, log *zap.Logger) *Mock {
	return &Mock{
		log:    log,
		mockFS: mockFS,
	}
}

// Mock is a service to read-write mocks during record and replay in unit-tests only.
type Mock struct {
	log    *zap.Logger
	mockFS models.MockFS
}

func (m *Mock) FileExists(ctx context.Context, path string) bool {
	return m.mockFS.Exists(ctx, path)
}

func (m *Mock) Put(ctx context.Context, path string, doc models.Mock, meta interface{}) error {

	isGenerated := false
	if doc.Name == "" {
		doc.Name = uuid.New().String()
		isGenerated = true
	}
	err := m.mockFS.Write(ctx, path, doc)
	if err != nil {
		m.log.Error(err.Error())
	}
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

func (m *Mock) GetAll(ctx context.Context, path string, name string) ([]models.Mock, error) {
	arr, err := m.mockFS.Read(ctx, path, name, true)
	if err != nil {
		m.log.Error("failed to read then yaml file", zap.Any("error", err))
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
