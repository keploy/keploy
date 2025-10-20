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

// GetMappings converts models.Mapping to map[string][]string
func GetMappings(mapping *models.Mapping, logger *zap.Logger) map[string][]string {
	testMockMappings := make(map[string][]string)

	for _, test := range mapping.Tests {
		testMockMappings[test.ID] = test.Mocks.ToSlice()
	}

	logger.Debug("Converted mapping to test-mock mappings",
		zap.String("mappingID", mapping.TestSetID),
		zap.Int("numTests", len(testMockMappings)))

	return testMockMappings
}

// CreateMappings converts map[string][]string to models.Mapping
func CreateMappingStructure(testSetID string, testMockMappings map[string][]string, logger *zap.Logger) *models.Mapping {
	mapping := &models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}

	// Convert the map to the structured format
	for testName, mockNames := range testMockMappings {
		test := models.Test{
			ID:    testName,
			Mocks: models.FromSlice(mockNames),
		}
		mapping.Tests = append(mapping.Tests, test)
	}

	return mapping
}
