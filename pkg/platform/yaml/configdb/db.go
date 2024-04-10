// Package configdb provides a config database implementation.
package configdb

import (
	"context"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type ConfigYaml struct {
	ConfigPath string
	Logger     *zap.Logger
}

func New(logger *zap.Logger, configPath string) *ConfigYaml {
	return &ConfigYaml{
		ConfigPath: configPath,
		Logger:     logger,
	}
}

func (c *ConfigYaml) InsertConfig(ctx context.Context, testSetID string, testSetConfig config.TestSetConfig) error {
	configPath := filepath.Join(c.ConfigPath, testSetID)
	configFileName := "config"
	data, err := yamlLib.Marshal(&testSetConfig)
	if err != nil {
		return err
	}
	err = yaml.WriteFile(ctx, c.Logger, configPath, configFileName, data, false)
	if err != nil {
		return err
	}
	return nil
}

func (c *ConfigYaml) GetConfig(ctx context.Context, testSetID string) (config.TestSetConfig, error) {
	configPath := filepath.Join(c.ConfigPath, testSetID)
	configFileName := "config"
	data, err := yaml.ReadFile(ctx, c.Logger, configPath, configFileName)
	if err != nil {
		return config.TestSetConfig{}, err
	}
	var testSetConfig config.TestSetConfig
	err = yamlLib.Unmarshal(data, &testSetConfig)
	if err != nil {
		return config.TestSetConfig{}, err
	}
	return testSetConfig, nil
}
