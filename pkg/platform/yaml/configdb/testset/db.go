// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Db is a generic struct to read and write testset config file
type Db[T any] struct {
	logger *zap.Logger
	path   string
}

func New[T any](logger *zap.Logger, path string) *Db[T] {
	return &Db[T]{
		logger: logger,
		path:   path,
	}
}

func (db *Db[T]) Read(ctx context.Context, testSetID string) (T, error) {
	filePath := filepath.Join(db.path, testSetID)

	var config T

	// Try to read config.yaml, but continue if it doesn't exist
	data, err := yaml.ReadFile(ctx, db.logger, filePath, "config")
	if err != nil {
		// Config file missing, create default config and continue with secret loading
		db.logger.Debug("Config file not found, using default config", zap.String("testSet", testSetID), zap.String("filePath", filePath), zap.Error(err))

		// Since T is *models.TestSet, initialize a new TestSet instance
		// Use type assertion to ensure we're working with the right type
		emptyTestSet := &models.TestSet{}
		testSetPtr, ok := any(emptyTestSet).(T)
		if ok {
			config = testSetPtr
			db.logger.Debug("Initialized empty TestSet for missing config", zap.String("testSet", testSetID))
		} else {
			// If T is not *models.TestSet, log warning but continue with zero value
			db.logger.Warn("Generic type T is not *models.TestSet, using zero value", zap.String("testSet", testSetID))
		}
	} else {
		// Config file exists, unmarshal it
		err := yamlLib.Unmarshal(data, &config)
		if err != nil {
			utils.LogError(db.logger, err, "failed to unmarshal test-set config file", zap.String("testSet", testSetID))
			// Don't return early - continue with secret loading even if config is malformed
			// Use default config instead
			emptyTestSet := &models.TestSet{}
			testSetPtr, ok := any(emptyTestSet).(T)
			if ok {
				config = testSetPtr
				db.logger.Warn("Using default config due to unmarshal error, continuing with secret loading", zap.String("testSet", testSetID))
			}
		}
	}

	// Always try to load secrets, regardless of whether config.yaml existed
	secretValues, err := db.ReadSecret(ctx, testSetID)
	if err != nil {
		db.logger.Warn("Failed to read secret values, continuing without secrets", zap.String("testSet", testSetID), zap.Error(err))
		// Don't return error here - missing secrets shouldn't fail the config loading
		return config, nil
	}

	// Set secrets into the config struct if supported
	secretConfig, ok := any(config).(models.Secret)
	if ok && len(secretValues) > 0 {
		db.logger.Debug("Setting secrets into config", zap.String("testSet", testSetID), zap.Int("secretCount", len(secretValues)))
		secretConfig.SetSecrets(secretValues)
	} else {
		db.logger.Debug("Not setting secrets", zap.String("testSet", testSetID), zap.Bool("configSupportsSecrets", ok), zap.Int("secretCount", len(secretValues)))
	}

	return config, nil
}

func (db *Db[T]) Write(ctx context.Context, testSetID string, config T) error {
	filePath := filepath.Join(db.path, testSetID)

	// Clear secrets before writing to config.yaml to avoid leaking them in config.yaml
	if testSetPtr, ok := any(config).(*models.TestSet); ok {
		// Create a shallow copy of the TestSet to avoid modifying the original
		testSetCopy := *testSetPtr
		// Clear the secrets in the copy
		testSetCopy.Secret = nil
		// Marshal the copy instead of the original
		data, err := yamlLib.Marshal(&testSetCopy)
		if err != nil {
			utils.LogError(db.logger, err, "failed to marshal test-set config file", zap.String("testSet", testSetID))
			return err
		}
		err = yaml.WriteFile(ctx, db.logger, filePath, "config", data, false)
		if err != nil {
			utils.LogError(db.logger, err, "failed to write test-set configuration in yaml file", zap.String("testSet", testSetID))
			return err
		}
		return nil
	}

	data, err := yamlLib.Marshal(config)
	if err != nil {
		utils.LogError(db.logger, err, "failed to marshal test-set config file", zap.String("testSet", testSetID))
		return err
	}
	err = yaml.WriteFile(ctx, db.logger, filePath, "config", data, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write test-set configuration in yaml file", zap.String("testSet", testSetID))
		return err
	}

	return nil
}

// ReadSecret reads the secret configuration for a test set
func (db *Db[T]) ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error) {
	filePath := filepath.Join(db.path, testSetID)

	secretPath := filepath.Join(filePath, "secret.yaml")
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		return make(map[string]interface{}), nil
	}

	data, err := yaml.ReadFile(ctx, db.logger, filePath, "secret")
	if err != nil {
		return nil, err
	}

	var secretConfig map[string]interface{}
	if err := yamlLib.Unmarshal(data, &secretConfig); err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal test-set secret file", zap.String("testSet", testSetID))
		return nil, err
	}

	return secretConfig, nil
}
