package mapdb

import (
	"context"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type MappingDb struct {
	logger      *zap.Logger
	path        string
	MapFileName string
}

func New(logger *zap.Logger, path string, mapFileName string) *MappingDb {
	return &MappingDb{
		logger:      logger,
		path:        path,
		MapFileName: mapFileName,
	}
}

// InsertMappings saves test-mock mappings to a YAML file
func (db *MappingDb) InsertMappings(ctx context.Context, testSetID string, testMockMappings map[string][]string) error {
	// Create mapping structure from the test-mock mappings
	mapping := CreateMappingFromTestMockMappings(testSetID, testMockMappings, db.logger)

	// Encode mapping to YAML
	yamlData, err := EncodeMapping(mapping, db.logger)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping to yaml", zap.String("testSetID", testSetID))
		return err
	}

	// Create the file path
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	// Check if file exists to determine if we should append
	exists, err := yaml.FileExists(ctx, db.logger, mappingPath, fileName)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists",
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return err
	}

	// Add version comment if file doesn't exist
	if !exists {
		yamlData = append([]byte(utils.GetVersionAsComment()), yamlData...)
	}

	// Write to file
	err = yaml.WriteFile(ctx, db.logger, mappingPath, fileName, yamlData, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping to yaml file",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return err
	}

	db.logger.Info("Successfully saved test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.String("filePath", filepath.Join(mappingPath, fileName+".yaml")),
		zap.Int("numTests", len(testMockMappings)))

	return nil
}

// GetMappings reads test-mock mappings from a YAML file
func (db *MappingDb) GetMappings(ctx context.Context, testSetID string) (map[string][]string, error) {
	// Create the file path
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	// Check if file exists
	exists, err := yaml.FileExists(ctx, db.logger, mappingPath, fileName)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists",
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return nil, err
	}

	if !exists {
		db.logger.Debug("Mapping file does not exist, returning empty mappings",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath))
		return make(map[string][]string), nil
	}

	// Read the file
	yamlData, err := yaml.ReadFile(ctx, db.logger, mappingPath, fileName)
	if err != nil {
		utils.LogError(db.logger, err, "failed to read mapping file",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return nil, err
	}

	// Decode the YAML data
	mapping, err := DecodeMapping(yamlData, db.logger)
	if err != nil {
		utils.LogError(db.logger, err, "failed to decode mapping from yaml",
			zap.String("testSetID", testSetID))
		return nil, err
	}

	// Convert to map format
	testMockMappings := ConvertMappingToTestMockMappings(mapping, db.logger)

	db.logger.Info("Successfully loaded test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.String("filePath", filepath.Join(mappingPath, fileName+".yaml")),
		zap.Int("numTests", len(testMockMappings)))

	return testMockMappings, nil
}
