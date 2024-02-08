package generateConfig

import (
	"os"
	"sync"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var Emoji = "\U0001F430" + " Keploy:"

type generatorConfig struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

type GenerateConfigOptions struct {
  ConfigStr string
}

func NewGeneratorConfig(logger *zap.Logger) GeneratorConfig {
	return &generatorConfig{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

func (g *generatorConfig) GenerateConfig(filePath string, options GenerateConfigOptions) {
	var node yaml.Node
	data := []byte(models.DefaultConfig)

  if options.ConfigStr != ""{
    data = []byte(options.ConfigStr)
  }

	if err := yaml.Unmarshal(data, &node); err != nil {
		g.logger.Fatal("Unmarshalling failed %s", zap.Error(err))
	}
	results, err := yaml.Marshal(node.Content[0])
	if err != nil {
		g.logger.Fatal("Failed to marshal the config", zap.Error(err))
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)

	err = os.WriteFile(filePath, finalOutput, os.ModePerm)
	if err != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	err = pkg.SetChmodPermission(filePath)
	if err != nil {
		g.logger.Error("failed to set the permission of config file", zap.Error(err))
		return
	}

	g.logger.Info("Config file generated successfully")
}
