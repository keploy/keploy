package mockdb

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// MockCodec lets non-OSS parser packages own their YAML representation without
// adding parser-specific fields to the shared OSS mock model.
type MockCodec struct {
	Encode func(mock *models.Mock, doc *yaml.NetworkTrafficDoc) error
	Decode func(doc *yaml.NetworkTrafficDoc, mock *models.Mock) error
}

var mockCodecs sync.Map // map[models.Kind]MockCodec

func RegisterMockCodec(kind models.Kind, codec MockCodec) {
	if kind == "" || codec.Encode == nil || codec.Decode == nil {
		return
	}
	mockCodecs.Store(kind, codec)
}

func encodeRegisteredMock(mock *models.Mock, doc *yaml.NetworkTrafficDoc) (bool, error) {
	if mock == nil {
		return false, nil
	}
	v, ok := mockCodecs.Load(mock.Kind)
	if !ok {
		return false, nil
	}
	return true, v.(MockCodec).Encode(mock, doc)
}

func decodeRegisteredMock(doc *yaml.NetworkTrafficDoc, mock *models.Mock) (bool, error) {
	if doc == nil {
		return false, nil
	}
	v, ok := mockCodecs.Load(doc.Kind)
	if !ok {
		return false, nil
	}
	return true, v.(MockCodec).Decode(doc, mock)
}
