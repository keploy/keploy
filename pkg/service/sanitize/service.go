package sanitize

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Service interface {
	Sanitize(ctx context.Context, testSets []string) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
}

type service struct {
	logger *zap.Logger
	testDB TestDB
	config *config.Config
}

func New(logger *zap.Logger, testDB TestDB, cfg *config.Config) Service {
	return &service{
		logger: logger,
		testDB: testDB,
		config: cfg,
	}
}

func (s *service) Sanitize(ctx context.Context, _ []string) error {
	s.logger.Info("Starting sanitize process...")

	// Extract test sets from config (set by CLI flags)
	testSets := s.extractTestSetIDs()
	s.logger.Debug("Config test sets", zap.Any("config.Test.SelectedTests", s.config.Test.SelectedTests))
	s.logger.Debug("Extracted test sets", zap.Strings("testSets", testSets))

	// If no test sets specified, get all test sets
	if len(testSets) == 0 {
		var err error
		testSets, err = s.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			s.logger.Error("Failed to get test sets", zap.Error(err))
			return fmt.Errorf("failed to get test sets: %w", err)
		}
		s.logger.Info("No test sets specified, processing all test sets", zap.Int("count", len(testSets)))
	} else {
		s.logger.Info("Processing specified test sets", zap.Strings("testSets", testSets))
	}

	// Iterate through each test set
	for _, testSetID := range testSets {
		s.logger.Info("Processing test set", zap.String("testSetID", testSetID))

		// Get test cases for this test set
		testCases, err := s.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			s.logger.Error("Failed to get test cases for test set",
				zap.String("testSetID", testSetID),
				zap.Error(err))
			continue
		}

		s.logger.Info("Found test cases in test set",
			zap.String("testSetID", testSetID),
			zap.Int("count", len(testCases)))

		// Process each test case
		for i, testCase := range testCases {
			s.logger.Info("Processing test case",
				zap.String("testSetID", testSetID),
				zap.Int("index", i),
				zap.String("name", testCase.Name),
				zap.String("kind", string(testCase.Kind)),
				zap.String("type", testCase.Type))

			// TODO: Add your sanitization logic here
			// For now, just print the information
			fmt.Printf("Test Set: %s, Test Case %d: %s (%s)\n",
				testSetID, i+1, testCase.Name, testCase.Kind)
		}
	}

	s.logger.Info("Sanitize process completed")
	return nil
}

// extractTestSetIDs extracts and cleans test set IDs from config
func (s *service) extractTestSetIDs() []string {
	var testSetIDs []string
	for testSet := range s.config.Test.SelectedTests {
		testSetIDs = append(testSetIDs, testSet)
	}
	return testSetIDs
}
