package mockdb

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// MockYAMLMapper lets non-OSS parser packages own their YAML representation
// without adding parser-specific fields to the shared OSS mock model. A mapper
// translates a mock to/from its on-disk YAML document for a single mock kind.
type MockYAMLMapper struct {
	Encode func(mock *models.Mock, doc *yaml.NetworkTrafficDoc) error
	Decode func(doc *yaml.NetworkTrafficDoc, mock *models.Mock) error
}

var mockYAMLMappers sync.Map // map[models.Kind]MockYAMLMapper

func RegisterMockYAMLMapper(kind models.Kind, mapper MockYAMLMapper) {
	if kind == "" || mapper.Encode == nil || mapper.Decode == nil {
		return
	}
	mockYAMLMappers.Store(kind, mapper)
}

func encodeWithMapper(mock *models.Mock, doc *yaml.NetworkTrafficDoc) (bool, error) {
	if mock == nil {
		return false, nil
	}
	v, ok := mockYAMLMappers.Load(mock.Kind)
	if !ok {
		return false, nil
	}
	return true, v.(MockYAMLMapper).Encode(mock, doc)
}

func decodeWithMapper(doc *yaml.NetworkTrafficDoc, mock *models.Mock) (bool, error) {
	if doc == nil {
		return false, nil
	}
	v, ok := mockYAMLMappers.Load(doc.Kind)
	if !ok {
		return false, nil
	}
	return true, v.(MockYAMLMapper).Decode(doc, mock)
}
