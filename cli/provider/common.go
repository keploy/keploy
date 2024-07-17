package provider

import (
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
)

type commonPlatformServices struct {
	YamlTestDB    *testdb.TestYaml
	YamlMockDb    *mockdb.MockYaml
	YamlReportDb  *reportdb.TestReport
	YamlTestSetDB *testset.Db[*models.TestSet]
}
