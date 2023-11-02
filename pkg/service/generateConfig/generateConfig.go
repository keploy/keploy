package generateConfig

import (
	"sync"
	"os"
	"path/filepath"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	
	"gopkg.in/yaml.v3"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type generatorConfig struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewGeneratorConfig(logger *zap.Logger) GeneratorConfig {
	return &generatorConfig {
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

func (g *generatorConfig) GenerateConfig(path string) {

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

	filePath := filepath.Join(path, "keploy-config.yaml")

	if utils.CheckFileExists(filePath) {
		override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
		if err != nil {
			g.logger.Fatal("Failed to ask for confirmation", zap.Error(err))
			return
		}
		if !override {
			return
		}
	}

	err = os.WriteFile(filePath, data, os.ModePerm)
	if err != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	g.logger.Info("Config file generated successfully")
}
