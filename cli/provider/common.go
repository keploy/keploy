package provider

import (
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/storage"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v2/pkg/platform/yaml/openAPIdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
)

type commonPlatformServices struct {
	YamlTestDB    *testdb.TestYaml
	YamlMockDb    *mockdb.MockYaml
	YamlOpenAPIDb *openAPIdb.OpenAPIYaml
	YamlReportDb  *reportdb.TestReport
	YamlTestSetDB *testset.Db[*models.TestSet]
	Storage       *storage.Storage
}
