package mapdb

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// EncodeMapping encodes a mapping structure into a YAML document
func EncodeMapping(mapping *models.Mapping, logger *zap.Logger) ([]byte, error) {
	yamlData, err := yamlLib.Marshal(mapping)
	if err != nil {
		logger.Error("failed to marshal mapping to yaml", zap.Error(err))
		return nil, err
	}
	return yamlData, nil
}

// DecodeMapping decodes YAML data into a mapping structure
func DecodeMapping(yamlData []byte, logger *zap.Logger) (*models.Mapping, error) {
	var mapping models.Mapping
	err := yamlLib.Unmarshal(yamlData, &mapping)
	if err != nil {
		logger.Error("failed to unmarshal yaml to mapping", zap.Error(err))
		return nil, err
	}
	return &mapping, nil
}

// GetMappings converts models.Mapping to map[string][]string (names only, for backward compat)
func GetMappings(mapping *models.Mapping, logger *zap.Logger) map[string][]string {
	testMockEntrys := make(map[string][]string)

	for _, test := range mapping.Tests {
		testMockEntrys[test.ID] = test.MockNames()
	}

	logger.Debug("Converted mapping to test-mock mappings",
		zap.String("mappingID", mapping.TestSetID),
		zap.Int("numTests", len(testMockEntrys)))

	return testMockEntrys
}

// CreateMappingStructure converts map[string][]models.MockEntry to models.Mapping
func CreateMappingStructure(testSetID string, testMockEntrys map[string][]models.MockEntry, logger *zap.Logger) *models.Mapping {
	mapping := &models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}

	// Convert the map to the structured format
	for testName, mocks := range testMockEntrys {
		test := models.Test{
			ID:    testName,
			Mocks: mocks,
		}
		mapping.Tests = append(mapping.Tests, test)
	}

	return mapping
}
