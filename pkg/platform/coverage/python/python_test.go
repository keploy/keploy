package python

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gotest.tools/v3/assert"
)

// TestPreProcess_CoverageToolNotFound_456 simulates a scenario where the 'coverage'
// command fails, expecting the method to return the original command and an error.
func TestPreProcess_CoverageToolNotFound_456(t *testing.T) {
	originalExec := execCommand123
	defer func() { execCommand123 = originalExec }()

	execCommand123 = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("false")
	}

	logger := zap.NewNop()
	p := New(context.Background(), logger, nil, "my-cmd", "python3")

	cmd, err := p.PreProcess(false)

	require.Error(t, err)
	assert.Equal(t, "my-cmd", cmd)
}
