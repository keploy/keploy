package testdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestNew_Initialization_123 tests the initialization of the TestYaml struct.
func TestNew_Initialization_123(t *testing.T) {
	logger := zap.NewNop()
	tcsPath := "/path/to/testcases"

	testYaml := New(logger, tcsPath)

	assert.NotNil(t, testYaml)
	assert.Equal(t, tcsPath, testYaml.TcsPath)
	assert.Equal(t, logger, testYaml.logger)
}

// TestChangePath_Update_123 tests if the TcsPath is correctly updated.
func TestChangePath_Update_123(t *testing.T) {
	// Setup
	logger := zap.NewNop()
	tdb := New(logger, "/initial/path")
	newPath := "/new/path"

	// Execute
	tdb.ChangePath(newPath)

	// Assert
	assert.Equal(t, newPath, tdb.TcsPath)
}

// TestGetReportTestSets_EmptyRunID_456 tests the behavior when latestRunID is empty.
func TestGetReportTestSets_EmptyRunID_456(t *testing.T) {
	// Setup
	ctx := context.Background()
	logger := zap.NewNop()
	tdb := New(logger, "/tmp/keploy")

	// Execute
	sets, err := tdb.GetReportTestSets(ctx, "")

	// Assert
	assert.NoError(t, err)
	assert.Empty(t, sets)
}
