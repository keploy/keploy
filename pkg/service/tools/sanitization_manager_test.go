package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"gotest.tools/assert"
)

func TestSanitizationManager(t *testing.T) {
	// Setup
	logger, _ := zap.NewDevelopment()
	tempDir, err := os.MkdirTemp("", "keploy-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a test secrets file
	secrets := map[string]string{
		"{{SECRET_1}}": "sensitive-value-1",
		"{{SECRET_2}}": "sensitive-value-2",
	}

	t.Run("Test IsTestSetSanitized", func(t *testing.T) {
		sm := NewSanitizationManager(logger)
		assert.Equal(t, false, sm.IsTestSetSanitized(tempDir))
	})

	t.Run("Test ProcessTestCases", func(t *testing.T) {
		sm := NewSanitizationManager(logger)
		testCases := []*models.TestCase{
			{Name: "test1"},
			{Name: "test2"},
		}

		err := sm.ProcessTestCases(context.Background(), testCases, 1, func(tc *models.TestCase) error {
			tc.Name = tc.Name + "_processed"
			return nil
		})

		assert.NilError(t, err)
		assert.Equal(t, "test1_processed", testCases[0].Name)
		assert.Equal(t, "test2_processed", testCases[1].Name)
	})

	t.Run("Test Backup and Restore", func(t *testing.T) {
		sm := NewSanitizationManager(logger)
		
		// Create a test file
		testFile := filepath.Join(tempDir, "test.txt")
		err := os.WriteFile(testFile, []byte("test content"), 0644)
		assert.NilError(t, err)

		// Create backup
		backupDir, err := sm.CreateBackup(tempDir)
		defer os.RemoveAll(backupDir)
		assert.NilError(t, err)

		// Modify the original
		err = os.WriteFile(testFile, []byte("modified content"), 0644)
		assert.NilError(t, err)

		// Restore from backup
		err = sm.RestoreFromBackup(tempDir, backupDir)
		assert.NilError(t, err)

		// Verify content was restored
		content, err := os.ReadFile(testFile)
		assert.NilError(t, err)
		assert.Equal(t, "test content", string(content))
	})
}
