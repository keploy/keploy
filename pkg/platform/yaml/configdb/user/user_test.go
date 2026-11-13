package user

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// newDbInEmptyHome returns a Db whose home folder is a fresh temporary
// directory, so a test never reads or writes the real ~/.keploy.
func newDbInEmptyHome(t *testing.T) (*Db, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return New(zap.NewNop(), &config.Config{}), home
}

// With no keploy.yaml yet, ReadKeployConfig returns the file system's
// not-exist error, which a caller tells apart from a broken file.
func TestReadKeployConfig_NoFileIsNotExist(t *testing.T) {
	db, _ := newDbInEmptyHome(t)

	got, err := db.ReadKeployConfig()

	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Nil(t, got)
}

func TestWriteKeployConfig_ReadsBackWhatItWrote(t *testing.T) {
	db, home := newDbInEmptyHome(t)
	// WriteKeployConfig does not create ~/.keploy; it expects the folder.
	require.NoError(t, os.Mkdir(filepath.Join(home, ".keploy"), 0o755))
	want := &KeployConfig{UpdatePrompt: "never"}

	require.NoError(t, db.WriteKeployConfig(want))
	got, err := db.ReadKeployConfig()

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// In Docker the machine id is the container's, so the installation id comes
// from INSTALLATION_ID instead.
func TestGetInstallationID_InDockerReadsInstallationID(t *testing.T) {
	db, _ := newDbInEmptyHome(t)
	t.Setenv("KEPLOY_INDOCKER", "true")
	t.Setenv("INSTALLATION_ID", "abc")

	got, err := db.GetInstallationID(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "abc", got)
}
