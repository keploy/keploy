package config

import (
	"log"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	MongoURI         string `envconfig:"MONGO_URI" default:"mongodb://localhost:27017"`
	DB               string `envconfig:"DB" default:"keploy"`
	TestCaseTable    string `envconfig:"TEST_CASE_TABLE" default:"test-cases"`
	TestRunTable     string `envconfig:"TEST_RUN_TABLE" default:"test-runs"`
	TestTable        string `envconfig:"TEST_TABLE" default:"tests"`
	TelemetryTable   string `envconfig:"TELEMETRY_TABLE" default:"telemetry"`
	APIKey           string `envconfig:"API_KEY"`
	EnableDeDup      bool   `envconfig:"ENABLE_DEDUP" default:"false"`
	EnableTelemetry  bool   `envconfig:"ENABLE_TELEMETRY" default:"true"`
	EnableDebugger   bool   `envconfig:"ENABLE_DEBUG" default:"false"`
	EnableTestExport bool   `envconfig:"ENABLE_TEST_EXPORT" default:"true"`
	KeployApp        string `envconfig:"APP_NAME" default:"Keploy-Test-App"`
	Port             string `envconfig:"PORT" default:"6789"`
	ReportPath       string `envconfig:"REPORT_PATH" default:""`
	PathPrefix       string `envconfig:"KEPLOY_PATH_PREFIX" default:"/"`
}

func NewConfig() *Config {
	var conf Config
	err := envconfig.Process("keploy", &conf)
	if err != nil {
		log.Fatalf("failed to read/process configuration. error: %v", err)
		return nil
	}
	return &conf
}
