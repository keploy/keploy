package mapdb

import (
	"context"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
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

// Insert saves test-mock mappings to a YAML file
func (db *MappingDb) Insert(ctx context.Context, testSetID string, testMockMappings map[string][]string) error {
	// Create mapping structure from the test-mock mappings
	mapping := CreateMappingStructure(testSetID, testMockMappings, db.logger)

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

// Upsert updates a single test-mock mapping.
// If the file exists, it appends or updates the specific test case.
// If the file doesn't exist, it creates it.
func (db *MappingDb) Upsert(ctx context.Context, testSetID string, testID string, mockIDs []string) error {

	// 1. Define File Path
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	var mapping *models.Mapping

	// 2. Check if file exists
	exists, err := yaml.FileExists(ctx, db.logger, mappingPath, fileName)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists",
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return err
	}

	if exists {
		// 3. READ: Load existing mappings
		yamlData, err := yaml.ReadFile(ctx, db.logger, mappingPath, fileName)
		if err != nil {
			utils.LogError(db.logger, err, "failed to read mapping file for upsert",
				zap.String("testSetID", testSetID))
			return err
		}

		mapping, err = DecodeMapping(yamlData, db.logger)
		if err != nil {
			utils.LogError(db.logger, err, "failed to decode mapping from yaml",
				zap.String("testSetID", testSetID))
			return err
		}
	} else {
		// 4. CREATE: Initialize new mapping structure if file doesn't exist
		mapping = &models.Mapping{
			Version:   string(models.V1Beta1),
			Kind:      models.MappingKind,
			TestSetID: testSetID,
			Tests:     []models.Test{},
		}
	}

	// 5. MODIFY: Update or Append the specific test case
	found := false
	for i, t := range mapping.Tests {
		if t.ID == testID {
			// Update existing entry
			mapping.Tests[i].Mocks = models.FromSlice(mockIDs)
			found = true
			break
		}
	}

	if !found {
		// Append new entry
		newTest := models.Test{
			ID:    testID,
			Mocks: models.FromSlice(mockIDs),
		}
		mapping.Tests = append(mapping.Tests, newTest)
	}

	// 6. WRITE: Save back to disk
	yamlData, err := EncodeMapping(mapping, db.logger)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping to yaml during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	// Add version comment if we are creating a fresh file
	if !exists {
		yamlData = append([]byte(utils.GetVersionAsComment()), yamlData...)
	}

	err = yaml.WriteFile(ctx, db.logger, mappingPath, fileName, yamlData, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping to yaml file during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	db.logger.Debug("Successfully upserted test-mock mapping",
		zap.String("testSetID", testSetID),
		zap.String("testID", testID),
		zap.Int("mockCount", len(mockIDs)))

	return nil
}

// Get reads test-mock mappings from a YAML file
// Returns: testMockMappings, mappingFilePresent, error
func (db *MappingDb) Get(ctx context.Context, testSetID string) (map[string][]string, bool, error) {
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
		return nil, false, err
	}

	if !exists {
		db.logger.Debug("Mapping file does not exist, returning empty mappings",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath))
		return make(map[string][]string), false, nil
	}

	// Read the file
	yamlData, err := yaml.ReadFile(ctx, db.logger, mappingPath, fileName)
	if err != nil {
		utils.LogError(db.logger, err, "failed to read mapping file",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return nil, false, err
	}

	// Decode the YAML data
	mapping, err := DecodeMapping(yamlData, db.logger)
	if err != nil {
		utils.LogError(db.logger, err, "failed to decode mapping from yaml",
			zap.String("testSetID", testSetID))
		return nil, false, err
	}

	// Convert to map format
	testMockMappings := GetMappings(mapping, db.logger)

	// Check if we have any meaningful mappings (non-empty test cases with mocks)
	hasMeaningfulMappings := false
	for _, mocks := range testMockMappings {
		if len(mocks) > 0 {
			hasMeaningfulMappings = true
			break
		}
	}

	db.logger.Info("Successfully loaded test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.String("filePath", filepath.Join(mappingPath, fileName+".yaml")),
		zap.Int("numTests", len(testMockMappings)),
		zap.Bool("hasMeaningfulMappings", hasMeaningfulMappings))

	return testMockMappings, hasMeaningfulMappings, nil
}
