package sandbox

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type fakeCloudStore struct {
	manifest *models.SandboxManifest
	err      error
}

func (f *fakeCloudStore) GetManifest(_ context.Context, _ string) (*models.SandboxManifest, error) {
	return f.manifest, f.err
}

func (f *fakeCloudStore) UploadArtifact(_ context.Context, _ *models.SandboxManifest, _ io.Reader) error {
	return nil
}

func (f *fakeCloudStore) DownloadArtifact(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func TestSyncReturnsManifestNotFoundSentinel(t *testing.T) {
	t.Parallel()

	service := New(&fakeCloudStore{}, zap.NewNop())
	err := service.Sync(context.Background(), "owner/service:v1.2.3", ".")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("expected ErrManifestNotFound, got: %v", err)
	}
}
