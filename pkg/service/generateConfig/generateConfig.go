package generateConfig

import (
	"os"
	"path/filepath"
	"sync"

	"go.keploy.io/server/utils"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
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

var config = `
record:
  path: ""
  # mandatory
  command: ""
  proxyport: 0
  containerName: ""
  networkName: ""
  delay: 5
  passThroughPorts: []
test:
  path: ""
  # mandatory
  command: ""
  proxyport: 0
  containerName: ""
  networkName: ""
  tests: |-
    {
      "test-set-name": ["test-case-name"]
    }
  globalNoise: |-
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
    }
  delay: 5
  apiTimeout: 5
  passThroughPorts: []
`

func (g *generatorConfig) GenerateConfig(path string) {
	var node yaml.Node

	data := []byte(config)

	if err := yaml.Unmarshal(data, &node); err != nil {
		g.logger.Fatal("Unmarshalling failed %s", zap.Error(err))
	}

	results, err := yaml.Marshal(node.Content[0])
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

	err = os.WriteFile(filePath, results, os.ModePerm)
	if err != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	g.logger.Info("Config file generated successfully")
}
