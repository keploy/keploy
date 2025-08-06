package log

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNew_LoggerInitialization_123 tests the New function for logger initialization and error handling.
func TestNew_LoggerInitialization_123(t *testing.T) {
	// Mock os.OpenFile to simulate file creation success and failure
	originalOsOpenFile := osOpenFile234
	defer func() { osOpenFile234 = originalOsOpenFile }()

	t.Run("success", func(t *testing.T) {
		osOpenFile234 = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			return &os.File{}, nil
		}
		logger, logFile, err := New()
		require.NoError(t, err)
		assert.NotNil(t, logger)
		assert.NotNil(t, logFile)
	})

	t.Run("failure", func(t *testing.T) {
		osOpenFile234 = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			return nil, fmt.Errorf("mock error")
		}
		logger, logFile, err := New()
		require.Error(t, err)
		assert.Nil(t, logger)
		assert.Nil(t, logFile)
	})
}
