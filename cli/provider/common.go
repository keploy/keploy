package provider

import (
	"go.keploy.io/server/v2/pkg/models"
	HTTP "go.keploy.io/server/v2/pkg/platform/http"
	"go.keploy.io/server/v2/pkg/platform/storage"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v2/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
)

type commonPlatformServices struct {
	YamlTestDB    *testdb.TestYaml
	YamlMockDb    *mockdb.MockYaml
	YamlOpenAPIDb *openapidb.OpenAPIYaml
	YamlReportDb  *reportdb.TestReport
	YamlTestSetDB *testset.Db[*models.TestSet]
	HttpClient    *HTTP.HTTP
	Storage       *storage.Storage
}
