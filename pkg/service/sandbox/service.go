package sandbox

import (
	"context"
	"io"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for the sandbox cloud sync service.
type Service interface {
	// Upload runs tests, and if they pass, creates a manifest + artifact zip and uploads to cloud.
	Upload(ctx context.Context, ref string, basePath string) error

	// Sync checks the cloud manifest against local files and downloads if needed.
	// Returns true if tests can proceed (either already synced or successfully downloaded).
	Sync(ctx context.Context, ref string, basePath string) error
}

// CloudStore provides methods to interact with the cloud API for sandbox operations.
type CloudStore interface {
	// GetManifest fetches the sandbox manifest from the API server.
	// Returns nil, nil if the manifest does not exist.
	GetManifest(ctx context.Context, ref string) (*models.SandboxManifest, error)
	// UploadArtifact uploads the manifest and artifact zip to the API server.
	UploadArtifact(ctx context.Context, manifest *models.SandboxManifest, artifactData io.Reader) error
	// DownloadArtifact downloads the artifact zip from the API server.
	DownloadArtifact(ctx context.Context, ref string) (io.ReadCloser, error)
}
