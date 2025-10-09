package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// SanitizationManager handles the sanitization and desanitization of test cases
type SanitizationManager struct {
	logger *zap.Logger
}

// NewSanitizationManager creates a new SanitizationManager
func NewSanitizationManager(logger *zap.Logger) *SanitizationManager {
	return &SanitizationManager{
		logger: logger,
	}
}

// IsTestSetSanitized checks if a test set has been sanitized by looking for secrets.yaml
func (sm *SanitizationManager) IsTestSetSanitized(testSetDir string) bool {
	secretsPath := filepath.Join(testSetDir, "secret.yaml")
	_, err := os.Stat(secretsPath)
	return err == nil
}

// LoadSecrets loads secrets from secrets.yaml
func (sm *SanitizationManager) LoadSecrets(testSetDir string) (map[string]string, error) {
	secretsPath := filepath.Join(testSetDir, "secret.yaml")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}
	
	var secrets map[string]string
	if err := yaml.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to unmarshal secrets: %w", err)
	}
	return secrets, nil
}

// ProcessTestCases processes test cases in batches with a worker pool
func (sm *SanitizationManager) ProcessTestCases(
	ctx context.Context,
	testCases []*models.TestCase,
	batchSize int,
	processFunc func(*models.TestCase) error,
) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(testCases))
	semaphore := make(chan struct{}, 10) // Limit concurrency

	for i := 0; i < len(testCases); i += batchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			end := i + batchSize
			if end > len(testCases) {
				end = len(testCases)
			}
			batch := testCases[i:end]

			wg.Add(1)
			semaphore <- struct{}{} // Acquire semaphore

			go func(batch []*models.TestCase) {
				defer wg.Done()
				defer func() { <-semaphore }() // Release semaphore

				for _, tc := range batch {
					if err := processFunc(tc); err != nil {
						select {
						case errChan <- fmt.Errorf("error processing test case %s: %w", tc.Name, err):
						default: // Don't block if error channel is full
						}
						return
					}
				}
			}(batch)
		}
	}

	// Wait for all workers to finish
	go func() {
		wg.Wait()
		close(errChan)
	}()

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("encountered %d errors during processing, first error: %w", len(errs), errs[0])
	}

	return nil
}

// CreateBackup creates a timestamped backup of a test set directory
func (sm *SanitizationManager) CreateBackup(testSetDir string) (string, error) {
	if _, err := os.Stat(testSetDir); os.IsNotExist(err) {
		return "", fmt.Errorf("source directory does not exist: %s", testSetDir)
	}

	timestamp := time.Now().Format("20060102-150405")
	backupDir := fmt.Sprintf("%s_backup_%s", testSetDir, timestamp)

	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	if err := filepath.Walk(testSetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(testSetDir, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(backupDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		return copyFile(path, dstPath, info.Mode())
	}); err != nil {
		return "", fmt.Errorf("failed to copy files to backup: %w", err)
	}

	return backupDir, nil
}

// RestoreFromBackup restores a test set from a backup
func (sm *SanitizationManager) RestoreFromBackup(testSetDir, backupDir string) error {
	// Remove existing directory if it exists
	if err := os.RemoveAll(testSetDir); err != nil {
		return fmt.Errorf("failed to remove target directory: %w", err)
	}

	// Restore from backup
	if err := filepath.Walk(backupDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(backupDir, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(testSetDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		return copyFile(path, dstPath, info.Mode())
	}); err != nil {
		return fmt.Errorf("failed to restore from backup: %w", err)
	}

	return nil
}

// Helper function to copy files
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, mode)
}

// DesanitizeTestCase replaces placeholders with original values
func (sm *SanitizationManager) DesanitizeTestCase(tc *models.TestCase, secrets map[string]string) error {
	if tc.HTTPResp != nil {
		body := tc.HTTPResp.Body
		for placeholder, value := range secrets {
			body = strings.ReplaceAll(body, placeholder, value)
		}
		tc.HTTPResp.Body = body
	}
	// Handle other test case types (gRPC, etc.) similarly
	return nil
}

// ResanitizeTestCase replaces original values with placeholders
func (sm *SanitizationManager) ResanitizeTestCase(tc *models.TestCase, secrets map[string]string) error {
	if tc.HTTPResp != nil {
		body := tc.HTTPResp.Body
		for placeholder, value := range secrets {
			body = strings.ReplaceAll(body, value, placeholder)
		}
		tc.HTTPResp.Body = body
	}
	// Handle other test case types (gRPC, etc.) similarly
	return nil
}
