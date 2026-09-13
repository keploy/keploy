package mapdb

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"sort"
)

// EncodeMapping encodes a mapping structure into a YAML document
func EncodeMapping(mapping *models.Mapping, logger *zap.Logger) ([]byte, error) {
	return EncodeMappingF(mapping, logger, yaml.FormatYAML)
}

func EncodeMappingF(mapping *models.Mapping, logger *zap.Logger, format yaml.Format) ([]byte, error) {
	data, err := yaml.MarshalGeneric(format, mapping)
	if err != nil {
		logger.Error("failed to marshal mapping", zap.Error(err))
		return nil, err
	}
	return data, nil
}

// DecodeMapping decodes YAML data into a mapping structure
func DecodeMapping(yamlData []byte, logger *zap.Logger) (*models.Mapping, error) {
	return DecodeMappingF(yamlData, logger, yaml.FormatYAML)
}

func DecodeMappingF(data []byte, logger *zap.Logger, format yaml.Format) (*models.Mapping, error) {
	var mapping models.Mapping
	err := yaml.UnmarshalGeneric(format, data, &mapping)
	if err != nil {
		logger.Error("failed to unmarshal mapping data", zap.Error(err))
		return nil, err
	}
	return &mapping, nil
}

func GetMappings(mapping *models.Mapping, logger *zap.Logger) map[string][]models.MockEntry {
	testMockMappings := make(map[string][]models.MockEntry)

	for _, test := range mapping.TestCases {
		testMockMappings[test.ID] = test.Mocks
	}

	logger.Debug("Converted mapping to test-mock mappings",
		zap.String("mappingID", mapping.TestSetID),
		zap.Int("numTests", len(testMockMappings)))

	return testMockMappings
}

// CreateMappingStructure converts map[string][]models.MockEntry to models.Mapping
func CreateMappingStructure(testSetID string, testMockMappings map[string][]models.MockEntry, logger *zap.Logger) *models.Mapping {
	mapping := &models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}

	// Sorted, because Go randomises map iteration and mappings.yaml is a recorded
	// artifact that gets diffed, reviewed and committed. Without this every
	// rewrite reshuffles `tests:` and produces a large diff with no content
	// change. UpsertBatch already sorts its appends for the same reason.
	testNames := make([]string, 0, len(testMockMappings))
	for testName := range testMockMappings {
		testNames = append(testNames, testName)
	}
	sort.Strings(testNames)

	for _, testName := range testNames {
		mapping.TestCases = append(mapping.TestCases, models.MappedTestCase{
			ID:    testName,
			Mocks: testMockMappings[testName],
		})
	}

	return mapping
}
