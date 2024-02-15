package generateConfig

import (
	"os"
	"sync"

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

	if options.ConfigStr != "" {
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

	err = utils.SetUmask(0)
	if err != nil {
		g.logger.Error("Failed to set umask", zap.Error(err))
	}
	err = os.WriteFile(filePath, finalOutput, os.ModePerm)
	if err != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}
	err = utils.SetUmask(0022)
	if err != nil {
		g.logger.Error("Failed to set umask", zap.Error(err))
	}
	g.logger.Info("Config file generated successfully")
}
