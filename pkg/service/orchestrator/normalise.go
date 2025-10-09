//go:build linux

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func (o *Orchestrator) Normalize(ctx context.Context) error {

	var testRun string
	if o.config.Normalize.TestRun == "" {
		testRunIDs, err := o.reportDB.GetAllTestRunIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test run ids: %w", err)
		}
		testRun = pkg.LastID(testRunIDs, models.TestRunTemplateName)
	}

	if len(o.config.Normalize.SelectedTests) == 0 {
		testSetIDs, err := o.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test set ids: %w", err)
		}
		for _, testSetID := range testSetIDs {
			o.config.Normalize.SelectedTests = append(o.config.Normalize.SelectedTests, config.SelectedTests{TestSet: testSetID})
		}
	}

	for _, testSet := range o.config.Normalize.SelectedTests {
		testSetID := testSet.TestSet
		testCases := testSet.Tests

		// Check if test set is sanitized (has secret.yaml)
		// If yes, desanitize before normalization
		desanitized, err := o.tools.DesanitizeTestSet(testSetID)
		if err != nil {
			o.logger.Error("Failed to desanitize test set before normalization",
				zap.String("testSetID", testSetID),
				zap.Error(err))
			return fmt.Errorf("failed to desanitize test set %s: %w", testSetID, err)
		}
		if desanitized {
			o.logger.Info("Desanitized test set before normalization",
				zap.String("testSetID", testSetID))
		}

		// Normalize test cases
		err = o.replay.NormalizeTestCases(ctx, testRun, testSetID, testCases, nil)
		if err != nil {
			return err
		}

		// Re-sanitize after normalization if it was originally sanitized
		if desanitized {
			testSetDir := filepath.Join(o.config.Path, testSetID)
			err = o.tools.SanitizeTestSetDir(ctx, testSetDir)
			if err != nil {
				o.logger.Error("Failed to re-sanitize test set after normalization",
					zap.String("testSetID", testSetID),
					zap.Error(err))
				return fmt.Errorf("failed to re-sanitize test set %s: %w", testSetID, err)
			}
			o.logger.Info("Re-sanitized test set after normalization",
				zap.String("testSetID", testSetID))
		}
	}
	o.logger.Info("Normalized test cases successfully. Please run keploy tests to verify the changes.")
	return nil
}
