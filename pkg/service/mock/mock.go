package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-test/deep"
	"github.com/google/uuid"
	grpcMock "go.keploy.io/server/grpc/mock"
	proto "go.keploy.io/server/grpc/regression"

	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg"
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

// GetAll returns an array of mocks which are captured in unit-tests
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

func (m *Mock) upsert(ctx context.Context, mock *proto.Mock, path, name string, updateCount int) error {
	mocks, err := m.mockFS.Read(ctx, path, name, true)
	if err != nil {
		m.log.Error(err.Error())
		return err
	}
	newMock, err := grpcMock.Encode(mock)
	if err != nil {
		m.log.Error(err.Error())
		return err
	}

	err = os.Remove(filepath.Join(path, name+".yaml"))
	if err != nil {
		m.log.Error("failed to remove mocks from", zap.String("file", name), zap.Error(err))
		return err
	}
	mocks[len(mocks)-updateCount] = newMock
	err = m.mockFS.WriteAll(ctx, path, name, mocks)
	if err != nil {
		m.log.Error("failed to write updated mocks", zap.Error(err))
		return err
	}
	// for i := 0; i < len(arr)-updateCount; i++ {
	// 	err := m.mockFS.Write(ctx, path, arr[i])
	// 	if err != nil {
	// 		m.log.Error(err.Error())
	// 		return err
	// 	}
	// }

	return nil
}

func (m *Mock) insertAt(ctx context.Context, mock *proto.Mock, path, name string, updateCount int) error {
	mocks, err := m.mockFS.Read(ctx, path, name, true)
	if err != nil {
		m.log.Error(err.Error())
		return err
	}
	newMock, err := grpcMock.Encode(mock)
	if err != nil {
		m.log.Error(err.Error())
		return err
	}
	i := len(mocks) - updateCount

	//insert the new mock at index i
	mocks = append(mocks, newMock)
	copy(mocks[i+1:], mocks[i:])
	mocks[i] = newMock

	// update the yaml file
	err = os.Remove(filepath.Join(path, name+".yaml"))
	if err != nil {
		m.log.Error("failed to remove mocks from", zap.String("file", name), zap.Error(err))
		return err
	}
	err = m.mockFS.WriteAll(ctx, path, name, mocks)
	if err != nil {
		m.log.Error("failed to write updated mocks", zap.Error(err))
		return err
	}
	return nil
}

func (m *Mock) CompareMockResponses(old, new *proto.Mock) bool {
	matched := true

	if old.Version != new.Version || old.Name != new.Name {
		matched = false
	}
	switch old.Kind {
	case string(models.GENERIC):
		if deep.Equal(old.Spec.Objects, new.Spec.Objects) != nil {
			matched = false
		}
	case string(models.HTTP):
		// old.Spec.Res.ProtoMinor = 0
		// if deep.Equal(old.Spec.Assertions, new.Spec.Assertions) != nil ||
		if old.Spec.Res.StatusCode != new.Spec.Res.StatusCode {
			matched = false
		}
		var (
			bodyNoise   []string
			headerNoise = map[string]string{}
		)
		assertions := utils.GetStringMap(old.Spec.Assertions)
		for _, n := range assertions["noise"] {
			a := strings.Split(n, ".")
			if len(a) > 1 && a[0] == "body" {
				x := strings.Join(a[1:], ".")
				bodyNoise = append(bodyNoise, x)
			} else if a[0] == "header" {
				// if len(a) == 2 {
				//  headerNoise[a[1]] = a[1]
				//  continue
				// }
				headerNoise[a[len(a)-1]] = a[len(a)-1]
				// headerNoise[a[0]] = a[0]
			}
		}
		if !pkg.Contains(assertions["noise"], "body") {
			bodyType := models.BodyTypePlain
			if json.Valid([]byte(new.Spec.Res.Body)) != json.Valid([]byte(old.Spec.Res.Body)) {
				matched = false
			}
			if json.Valid([]byte(old.Spec.Res.Body)) {
				bodyType = models.BodyTypeJSON
			}

			if bodyType == models.BodyTypeJSON {
				_, _, pass, err := pkg.Match(old.Spec.Res.Body, new.Spec.Res.Body, bodyNoise, m.log)
				if err != nil || !pass {
					matched = false
				}
			} else {
				if old.Spec.Res.Body != new.Spec.Res.Body {
					matched = false
				}
			}
		}

		hRes := &[]models.HeaderResult{}

		if !pkg.CompareHeaders(utils.GetHttpHeader(old.Spec.Res.Header), utils.GetHttpHeader(new.Spec.Res.Header), hRes, headerNoise) {
			matched = false
		}
	case string(models.SQL):
		if deep.Equal(old.Spec, new.Spec) != nil {
			matched = false
		}
	}
	return matched
}

func (m *Mock) trimMocks(ctx context.Context, mocks []models.Mock, path, name string, fromIndex, toIndex int) error {
	mocks = append(mocks[0:fromIndex], mocks[toIndex:]...)

	err := os.Remove(filepath.Join(path, name+".yaml"))
	if err != nil {
		m.log.Error("failed to remove mocks from", zap.String("file", name), zap.Error(err))
		return err
	}
	err = m.mockFS.WriteAll(ctx, path, name, mocks)
	if err != nil {
		m.log.Error("failed to write updated mocks", zap.Error(err))
		return err
	}
	return nil
}

func (m *Mock) IsEqual(ctx context.Context, old, new *proto.Mock, path, name string, updateCount int) error {

	if old.Kind != new.Kind || deep.Equal(old.Spec.Metadata, new.Spec.Metadata) != nil {
		mArr, err := m.mockFS.Read(ctx, path, name, true)
		if err != nil {
			m.log.Error(err.Error())
			return err
		}
		mocks, err := grpcMock.Decode(mArr)
		if err != nil {
			m.log.Error(err.Error())
			return err
		}

		for i := len(mocks) - updateCount + 1; i < len(mocks); i++ {
			if mocks[i].Kind == new.Kind && deep.Equal(mocks[i].Spec.Metadata, new.Spec.Metadata) == nil && m.CompareMockResponses(mocks[i], new) {
				err := m.trimMocks(ctx, mArr, path, name, len(mocks)-updateCount, i)
				if err != nil {
					return err
				}
				return errors.New(ERR_DEP_REQ_UNEQUAL_REMOVE)
			}
		}

		err = m.insertAt(ctx, new, path, name, updateCount)
		if err != nil {
			return err
		}

		m.log.Info("Request of dmocks not matches: ", zap.Any("", old.Kind), zap.Any("", new.Kind))
		return errors.New(ERR_DEP_REQ_UNEQUAL_INSERT)
	}

	matched := m.CompareMockResponses(old, new)
	if !matched {
		err := m.upsert(ctx, new, path, name, updateCount)
		if err != nil {
			return err
		}
	}
	return nil
}

func mapToStrLog(meta map[string]string) string {
	res := ""
	for k, v := range meta {
		res += "     " + k + ": " + v + "\n"
	}
	return res
}
