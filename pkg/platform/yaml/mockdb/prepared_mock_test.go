package mockdb

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestPreparedMock_Dir(t *testing.T) {
	base := t.TempDir()
	store := New(zap.NewNop(), base, "mocks")
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    "temp-1",
	}
	p := store.PrepareMock(context.Background(), m, "test-set-0")
	defer p.Release()

	require.Equal(t, filepath.Join(base, "test-set-0"), p.Dir())
	require.Same(t, m, p.Mock())
}
