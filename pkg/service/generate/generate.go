package generate

import (
	"sync"
	"os"
	"path/filepath"

	"go.keploy.io/server/pkg/models"
	
	"gopkg.in/yaml.v3"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type generator struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewGenerator(logger *zap.Logger) Generator {
	return &generator{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

var globalNoise = `
{
	"global": {
		"body": {},
		"header": {}
	},
	"test-sets": {
		"test-set-name": {
			"body": {},
			"header": {}
		}
	}
}`

func (g *generator) Generate(configPath string) {

	testConfig := models.Test {
		Path: "",
		Command: "",
		ProxyPort: 0,
		ContainerName: "",
		NetworkName: "",
		TestSets: []string{},
		GlobalNoise: globalNoise,
		Delay: 5,
		ApiTimeout: 5,
		PassThroughPorts: []uint{},
	}

	recordConfig := models.Record {
		Path: "",
		Command: "",
		ProxyPort: 0,
		ContainerName: "",
		NetworkName: "",
		Delay: 5,
		PassThroughPorts: []uint{},
	}

	config := models.Config{
		Record: recordConfig,
		Test: testConfig,
	}

	data, err := yaml.Marshal(&config)
	if err != nil {
		g.logger.Fatal("Failed to marshal the config", zap.Error(err))
	}

	err1 := os.WriteFile(filepath.Join(configPath, "keploy-config.yaml"), data, 0644)
	if err1 != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	g.logger.Info("Config file generated successfully")
}
