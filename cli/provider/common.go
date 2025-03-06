package provider

import (
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/storage"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	idempotencydb "go.keploy.io/server/v2/pkg/platform/yaml/idempotencydb"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v2/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
)

type commonPlatformServices struct {
	YamlTestDB    *testdb.TestYaml
	YamlIdemDB    *idempotencydb.IdempotencyReportYaml
	YamlMockDb    *mockdb.MockYaml
	YamlOpenAPIDb *openapidb.OpenAPIYaml
	YamlReportDb  *reportdb.TestReport
	YamlTestSetDB *testset.Db[*models.TestSet]
	Storage       *storage.Storage
}
