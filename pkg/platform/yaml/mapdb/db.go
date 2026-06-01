package mapdb

import (
	"context"
	"os"
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
	Format      yaml.Format
}

func New(logger *zap.Logger, path string, mapFileName string) *MappingDb {
	return NewWithFormat(logger, path, mapFileName, yaml.FormatYAML)
}

func NewWithFormat(logger *zap.Logger, path string, mapFileName string, format yaml.Format) *MappingDb {
	return &MappingDb{
		logger:      logger,
		path:        path,
		MapFileName: mapFileName,
		Format:      format,
	}
}

func (db *MappingDb) Insert(ctx context.Context, mapping *models.Mapping) error {
	testSetID := mapping.TestSetID
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	finalMappings := make(map[string][]models.MockEntry)

	// Detect whether a mappings file already exists in either format,
	// and remember which one so we write back in the same format.
	exists, detected, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists", zap.String("path", mappingPath))
		return err
	}

	effFormat := db.Format
	if exists {
		effFormat = detected
		data, err := os.ReadFile(filepath.Join(mappingPath, fileName+"."+effFormat.FileExtension()))
		if err != nil {
			utils.LogError(db.logger, err, "failed to read existing mapping file", zap.String("path", mappingPath))
			return err
		}

		var existingConfig models.Mapping
		if err := yaml.UnmarshalGeneric(effFormat, data, &existingConfig); err != nil {
			utils.LogError(db.logger, err, "failed to unmarshal existing mappings", zap.String("path", mappingPath))
			return err
		}

		for _, t := range existingConfig.TestCases {
			finalMappings[t.ID] = t.Mocks
		}
	}

	for _, t := range mapping.TestCases {
		finalMappings[t.ID] = t.Mocks
	}

	newMapping := CreateMappingStructure(testSetID, finalMappings, db.logger)

	encodedData, err := EncodeMappingF(newMapping, db.logger, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping", zap.String("testSetID", testSetID))
		return err
	}
	if effFormat == yaml.FormatYAML {
		encodedData = append([]byte(utils.GetVersionAsComment()), encodedData...)
	}
	err = yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping file", zap.String("path", mappingPath))
		return err
	}

	db.logger.Info("Successfully merged and saved test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.Int("totalTests", len(finalMappings)))

	return nil
}

// Upsert updates a single test-mock mapping.
// If the file doesn't exist, it creates it.
func (db *MappingDb) Upsert(ctx context.Context, testSetID string, testID string, mockEntries []models.MockEntry) error {

	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	var mapping *models.Mapping

	exists, detected, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists",
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return err
	}

	effFormat := db.Format
	if exists {
		effFormat = detected
		fileData, err := yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			utils.LogError(db.logger, err, "failed to read mapping file for upsert",
				zap.String("testSetID", testSetID))
			return err
		}

		mapping, err = DecodeMappingF(fileData, db.logger, effFormat)
		if err != nil {
			utils.LogError(db.logger, err, "failed to decode mapping",
				zap.String("testSetID", testSetID))
			return err
		}
	} else {
		mapping = &models.Mapping{
			Version:   string(models.V1Beta1),
			Kind:      models.MappingKind,
			TestSetID: testSetID,
			TestCases: []models.MappedTestCase{},
		}
	}

	found := false
	for i, t := range mapping.TestCases {
		if t.ID == testID {
			mapping.TestCases[i].Mocks = mockEntries
			found = true
			break
		}
	}

	if !found {
		newTest := models.MappedTestCase{
			ID:    testID,
			Mocks: mockEntries,
		}
		mapping.TestCases = append(mapping.TestCases, newTest)
	}

	encodedData, err := EncodeMappingF(mapping, db.logger, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	if !exists && effFormat == yaml.FormatYAML {
		encodedData = append([]byte(utils.GetVersionAsComment()), encodedData...)
	}

	err = yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping file during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	db.logger.Debug("Successfully upserted test-mock mapping",
		zap.String("testSetID", testSetID),
		zap.String("testID", testID),
		zap.Int("mockCount", len(mockEntries)))

	return nil
}

// Exists reports whether mappings.yaml is on disk for the given
// test-set. Used by the test-mode create-if-not-present write path —
// distinct from Get's second return (which is "has at least one
// non-empty test entry"). A file with only empty entries should still
// count as "exists" so we don't overwrite the operator's intentional
// empty mapping.
func (db *MappingDb) Exists(ctx context.Context, testSetID string) (bool, error) {
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	return yaml.FileExists(ctx, db.logger, mappingPath, fileName)
}

// Get reads test-mock mappings from a YAML file
// Returns: testMockMappings, mappingFilePresent, error
func (db *MappingDb) Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error) {
	// Create the file path
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	fileData, detected, err := yaml.ReadFileAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		if os.IsNotExist(err) {
			db.logger.Debug("Mapping file does not exist, returning empty mappings",
				zap.String("testSetID", testSetID),
				zap.String("path", mappingPath))
			return make(map[string][]models.MockEntry), false, nil
		}
		utils.LogError(db.logger, err, "failed to read mapping file",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return nil, false, err
	}

	mapping, err := DecodeMappingF(fileData, db.logger, detected)
	if err != nil {
		utils.LogError(db.logger, err, "failed to decode mapping",
			zap.String("testSetID", testSetID))
		return nil, false, err
	}

	testMockMappings := GetMappings(mapping, db.logger)

	hasMeaningfulMappings := false
	for _, mocks := range testMockMappings {
		if len(mocks) > 0 {
			hasMeaningfulMappings = true
			break
		}
	}

	db.logger.Info("Successfully loaded test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.String("filePath", filepath.Join(mappingPath, fileName+"."+detected.FileExtension())),
		zap.Int("numTests", len(testMockMappings)),
		zap.Bool("hasMeaningfulMappings", hasMeaningfulMappings))

	return testMockMappings, hasMeaningfulMappings, nil
}
