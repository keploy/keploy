// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"os"
	"path/filepath"
	"reflect"

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

type withoutSecrets[T any] interface {
	WithoutSecrets() T
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
		config = newValue[T]()
	} else {
		// Config file exists, unmarshal it
		err := yamlLib.Unmarshal(data, &config)
		if err != nil {
			utils.LogError(db.logger, err, "failed to unmarshal test-set config file", zap.String("testSet", testSetID))
			// Don't return early - continue with secret loading even if config is malformed
			// Use default config instead
			config = newValue[T]()
			db.logger.Debug("Using default config due to unmarshal error, continuing with secret loading", zap.String("testSet", testSetID))
		}
	}

	if isNilValue(config) {
		config = newValue[T]()
	}

	// Always try to load secrets, regardless of whether config.yaml existed
	secretValues, err := db.ReadSecret(ctx, testSetID)
	if err != nil {
		db.logger.Debug("Failed to read secret values, continuing without secrets", zap.String("testSet", testSetID), zap.Error(err))
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

	if isNilValue(config) {
		config = newValue[T]()
	}

	if secretlessConfig, ok := any(config).(withoutSecrets[T]); ok {
		config = secretlessConfig.WithoutSecrets()
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

func newValue[T any]() T {
	var zero T
	typ := reflect.TypeOf(zero)
	if typ == nil {
		return zero
	}

	if typ.Kind() == reflect.Pointer {
		return reflect.New(typ.Elem()).Interface().(T)
	}

	return zero
}

func isNilValue[T any](value T) bool {
	v := reflect.ValueOf(value)
	if !v.IsValid() {
		return true
	}

	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
