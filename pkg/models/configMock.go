package models

import (
	"context"
	"os"
	"path/filepath"

	yamlLib "gopkg.in/yaml.v3"

	"go.uber.org/zap"
)

type ConfigMockRepository struct {
	Mocks []string `yaml:"mocks"`
}

func WriteConfigFile(ctx context.Context, logger *zap.Logger, path, fileName string, mocks ConfigMockRepository) error {
	filePath := filepath.Join(path, fileName+".yaml")
	data, err := yamlLib.Marshal(mocks)
	if err != nil {
		logger.Error("Failed to marshal the test report", zap.Error(err))
		return err
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		logger.Error("Failed to open config file", zap.Error(err))
		return err
	}
	if ctx.Err() != nil {
		logger.Info("Operation cancelled by context before writing", zap.Error(ctx.Err()))
		return ctx.Err()
	}

	if _, err := file.Write(data); err != nil {
		logger.Error("Failed to write to the report file", zap.Error(err))
		return err
	}

	err = file.Close()
	if err != nil {
		logger.Error("failed to close the yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
		return err
	}

	return nil
}

func ReadConfigFile(logger *zap.Logger, path, fileName string) *ConfigMockRepository {
	filePath := filepath.Join(path, fileName+".yaml")
	var config ConfigMockRepository
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		logger.Error("Failed to read configuration file", zap.Error(err))
		return nil
	}
	if err := yamlLib.Unmarshal(fileContent, &config); err != nil {
		logger.Info("The file does not match the required structure or failed to parse", zap.Error(err))
		return nil
	}
	return &config
}
