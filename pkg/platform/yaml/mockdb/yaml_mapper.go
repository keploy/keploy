package mockdb

import (
	"log"
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

// builtinYAMLKinds is the set of kinds whose YAML shape is owned by the
// OSS mockdb switch in util.go. RegisterMockYAMLMapper refuses to
// register a mapper for one of these so an out-of-tree parser cannot
// accidentally shadow OSS encode/decode and silently change the
// on-disk shape of an OSS protocol.
var builtinYAMLKinds = map[models.Kind]struct{}{
	models.HTTP:        {},
	models.HTTP2:       {},
	models.GENERIC:     {},
	models.MySQL:       {},
	models.Postgres:    {},
	models.PostgresV2:  {},
	models.Mongo:       {},
	models.GRPC_EXPORT: {},
	models.DNS:         {},
}

// RegisterMockYAMLMapper registers mapper as the encode/decode
// implementation for kind. It is intended to be called from an init()
// function so the mapper is available before any mock is loaded.
//
// The call is a no-op when:
//   - kind is empty or Encode/Decode is nil — clearly malformed, the
//     caller has a programming error and silent no-op matches the
//     existing best-effort contract,
//   - kind names an OSS built-in — OSS owns the YAML shape for these
//     kinds and a parser registering the same kind would silently
//     shadow it; a warning is logged so the bug is surfaced at
//     startup rather than at first mock-load.
//
// A duplicate registration for the same non-builtin kind replaces the
// previous mapper (last-writer wins, which matches Go init ordering)
// and logs a warning. Two packages claiming the same kind is almost
// always a bug; surfacing it at startup rather than at first load
// makes the failure mode obvious.
func RegisterMockYAMLMapper(kind models.Kind, mapper MockYAMLMapper) {
	if kind == "" || mapper.Encode == nil || mapper.Decode == nil {
		return
	}
	if _, reserved := builtinYAMLKinds[kind]; reserved {
		log.Printf("mockdb: refusing to register YAML mapper for OSS built-in kind %q; built-in kinds are owned by pkg/platform/yaml/mockdb/util.go and cannot be overridden", kind)
		return
	}
	if _, existed := mockYAMLMappers.LoadOrStore(kind, mapper); existed {
		log.Printf("mockdb: replacing previously-registered YAML mapper for kind %q; check for duplicate init() registrations", kind)
		mockYAMLMappers.Store(kind, mapper)
	}
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
