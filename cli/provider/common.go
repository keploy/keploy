package provider

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/storage"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	mapdb "go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v3/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v3/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v3/pkg/platform/yaml/testdb"
)

type commonPlatformServices struct {
	YamlTestDB    *testdb.TestYaml
	YamlMockDb    *mockdb.MockYaml
	YamlMappingDb *mapdb.MappingDb
	YamlOpenAPIDb *openapidb.OpenAPIYaml
	YamlReportDb  *reportdb.TestReport
	YamlTestSetDB *testset.Db[*models.TestSet]
	Storage       *storage.Storage
}
