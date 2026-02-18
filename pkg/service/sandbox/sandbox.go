package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ErrManifestNotFound indicates that no sandbox manifest exists in cloud for the given ref.
var ErrManifestNotFound = errors.New("sandbox manifest not found in cloud")

// sandboxService implements the Service interface.
type sandboxService struct {
	cloud  CloudStore
	logger *zap.Logger
}

// New creates a new sandbox service.
func New(cloud CloudStore, logger *zap.Logger) Service {
	return &sandboxService{
		cloud:  cloud,
		logger: logger,
	}
}

// Upload implements the upload flow (Path A):
// 1. Scan local files, compute hashes, build manifest.
// 2. Zip mock files (excluding test files).
// 3. Upload artifact to Azure (via API server).
// 4. Save manifest to MongoDB (via API server) - only after successful upload.
func (s *sandboxService) Upload(ctx context.Context, ref string, basePath string) error {
	if ref == "" {
		return fmt.Errorf("sandbox ref is required for upload")
	}

	company, appName, tag, err := ParseRef(ref)
	if err != nil {
		return fmt.Errorf("invalid sandbox ref %q: %w", ref, err)
	}

	if basePath == "" {
		basePath = "."
	}
	basePath, err = filepath.Abs(basePath)
	if err != nil {
		return fmt.Errorf("failed to resolve base path: %w", err)
	}

	s.logger.Info("scanning local files for sandbox upload",
		zap.String("ref", ref),
		zap.String("basePath", basePath),
	)

	// Step 1: Scan local files and compute hashes.
	fileHashes, err := scanAndHash(basePath)
	if err != nil {
		return fmt.Errorf("failed to scan and hash files: %w", err)
	}

	if len(fileHashes) == 0 {
		return fmt.Errorf("no mock files found in %q to upload", basePath)
	}

	s.logger.Info("found mock files for sandbox",
		zap.Int("fileCount", len(fileHashes)),
		zap.String("ref", ref),
	)

	// Step 2: Create zip archive (excluding test files).
	zipData, err := createZip(basePath, fileHashes)
	if err != nil {
		return fmt.Errorf("failed to create artifact zip: %w", err)
	}

	// Step 3: Build manifest (not persisted to disk, only sent to MongoDB).
	manifest := &models.SandboxManifest{
		Ref:     ref,
		Company: company,
		AppName: appName,
		Tag:     tag,
		Files:   fileHashes,
	}

	// Step 4: Upload artifact + manifest to cloud (crucial order: Azure first, then MongoDB).
	s.logger.Info("uploading sandbox artifact and manifest",
		zap.String("ref", ref),
		zap.Int("fileCount", len(fileHashes)),
	)

	err = s.cloud.UploadArtifact(ctx, manifest, bytes.NewReader(zipData))
	if err != nil {
		return fmt.Errorf("failed to upload sandbox artifact: %w", err)
	}

	s.logger.Info("sandbox upload completed successfully",
		zap.String("ref", ref),
		zap.Int("fileCount", len(fileHashes)),
	)

	return nil
}

// Sync implements the sync flow (Path B):
// 1. Fetch manifest from MongoDB (via API server).
// 2. Compare local files against manifest hashes.
// 3. If all match, proceed directly. If any mismatch/missing, download artifact from Azure.
// 4. Unzip and overwrite local files.
func (s *sandboxService) Sync(ctx context.Context, ref string, basePath string) error {
	if ref == "" {
		return fmt.Errorf("sandbox ref is required for sync")
	}

	if basePath == "" {
		basePath = "."
	}
	basePath, err := filepath.Abs(basePath)
	if err != nil {
		return fmt.Errorf("failed to resolve base path: %w", err)
	}

	s.logger.Info("checking sandbox manifest in cloud",
		zap.String("ref", ref),
	)

	// Step 1: Fetch manifest from MongoDB.
	manifest, err := s.cloud.GetManifest(ctx, ref)
	if err != nil {
		return fmt.Errorf("failed to get sandbox manifest: %w", err)
	}

	if manifest == nil {
		// Path A: Manifest doesn't exist yet - this is handled by the caller (upload flow).
		return fmt.Errorf("%w for ref %q; run sandbox record first to create it", ErrManifestNotFound, ref)
	}

	s.logger.Info("sandbox manifest found in cloud",
		zap.String("ref", ref),
		zap.Int("fileCount", len(manifest.Files)),
	)

	// Step 2: Local verification - compare local files against manifest.
	syncResult := verifyLocal(basePath, manifest.Files)

	if !syncResult.NeedsDownload {
		s.logger.Info("local files match cloud manifest, no download needed",
			zap.String("ref", ref),
			zap.Int("matchedFiles", len(syncResult.MatchedFiles)),
		)
		return nil
	}

	s.logger.Info("local files do not match cloud manifest, downloading artifact",
		zap.String("ref", ref),
		zap.Int("mismatched", len(syncResult.MismatchedFiles)),
		zap.Int("missing", len(syncResult.MissingFiles)),
	)

	// Step 3: Download artifact from Azure.
	reader, err := s.cloud.DownloadArtifact(ctx, ref)
	if err != nil {
		return fmt.Errorf("failed to download sandbox artifact: %w", err)
	}
	defer reader.Close()

	// Read all data into memory for zip processing.
	artifactData, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read artifact data: %w", err)
	}

	// Step 4: Unzip and overwrite local files.
	err = extractZip(basePath, artifactData)
	if err != nil {
		return fmt.Errorf("failed to extract artifact zip: %w", err)
	}

	s.logger.Info("sandbox sync completed, local files updated from cloud",
		zap.String("ref", ref),
	)

	return nil
}

// ParseTag validates a sandbox tag value (e.g. v1.0.0, 1.2.3).
func ParseTag(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", fmt.Errorf("tag cannot be empty")
	}

	// Enforce semantic versioning on the tag (e.g. v1.0.0, 2.1.3).
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(tag, "v")); err != nil {
		return "", fmt.Errorf("tag %q is not valid semantic versioning (expected format like v1.0.0): %w", tag, err)
	}

	return tag, nil
}

// BuildRef builds a sandbox ref in the format <company>/<service>:<tag>.
func BuildRef(company, appName, tag string) (string, error) {
	company = strings.TrimSpace(company)
	appName = strings.TrimSpace(appName)

	if company == "" || appName == "" {
		return "", fmt.Errorf("company and service must be non-empty")
	}

	parsedTag, err := ParseTag(tag)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/%s:%s", company, appName, parsedTag), nil
}

// ParseRef parses a sandbox ref string in the format <company>/<service>:<tag>.
func ParseRef(ref string) (company, appName, tag string, err error) {
	// Split on ":" to get tag.
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", "", fmt.Errorf("ref must be in format <company>/<service>:<tag>, got %q", ref)
	}
	tag, err = ParseTag(parts[1])
	if err != nil {
		return "", "", "", err
	}

	// Split the prefix on "/" to get company and service.
	prefixParts := strings.SplitN(parts[0], "/", 2)
	if len(prefixParts) != 2 || prefixParts[0] == "" || prefixParts[1] == "" {
		return "", "", "", fmt.Errorf("ref must be in format <company>/<service>:<tag>, got %q", ref)
	}
	company = prefixParts[0]
	appName = prefixParts[1]

	return company, appName, tag, nil
}

// scanAndHash walks the base directory and computes SHA-256 hashes for sandbox mock files.
func scanAndHash(basePath string) ([]models.SandboxFileHash, error) {
	var fileHashes []models.SandboxFileHash

	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		base := filepath.Base(path)
		if !strings.HasSuffix(base, ".sb.yaml") && !strings.HasSuffix(base, ".sb.yml") {
			return nil
		}

		relPath, err := filepath.Rel(basePath, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %q: %w", path, err)
		}

		hash, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("failed to hash file %q: %w", path, err)
		}

		fileHashes = append(fileHashes, models.SandboxFileHash{
			Path: filepath.ToSlash(relPath), // Use forward slashes for cross-platform consistency.
			Hash: hash,
		})

		return nil
	})

	return fileHashes, err
}

// hashFile computes the SHA-256 hash of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// createZip creates a zip archive of all mock files, preserving directory structure.
func createZip(basePath string, files []models.SandboxFileHash) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	for _, fh := range files {
		absPath := filepath.Join(basePath, filepath.FromSlash(fh.Path))
		data, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %q: %w", absPath, err)
		}

		header := &zip.FileHeader{
			Name:     fh.Path,
			Method:   zip.Deflate,
			Modified: time.Now(),
		}

		fw, err := w.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("failed to create zip entry for %q: %w", fh.Path, err)
		}

		if _, err := fw.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write zip entry for %q: %w", fh.Path, err)
		}
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to close zip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// verifyLocal compares local files against the manifest hashes.
func verifyLocal(basePath string, manifestFiles []models.SandboxFileHash) models.SandboxSyncResult {
	result := models.SandboxSyncResult{}

	for _, mf := range manifestFiles {
		localPath := filepath.Join(basePath, filepath.FromSlash(mf.Path))

		localHash, err := hashFile(localPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Scenario 3: File is missing.
				result.MissingFiles = append(result.MissingFiles, mf.Path)
				result.NeedsDownload = true
			} else {
				// Treat read errors as mismatches.
				result.MismatchedFiles = append(result.MismatchedFiles, mf.Path)
				result.NeedsDownload = true
			}
			continue
		}

		if localHash != mf.Hash {
			// Scenario 2: Hash mismatch - file is "dirty".
			result.MismatchedFiles = append(result.MismatchedFiles, mf.Path)
			result.NeedsDownload = true
		} else {
			// Scenario 1: Match - no need to download this file.
			result.MatchedFiles = append(result.MatchedFiles, mf.Path)
		}
	}

	return result
}

// extractZip extracts a zip archive to the given base path, overwriting existing files.
func extractZip(basePath string, zipData []byte) error {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("failed to open zip reader: %w", err)
	}

	for _, f := range r.File {
		destPath := filepath.Join(basePath, filepath.FromSlash(f.Name))

		// Security check: ensure the file doesn't escape the base path.
		if !strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(basePath)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry %q escapes the target directory", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return fmt.Errorf("failed to create directory %q: %w", destPath, err)
			}
			continue
		}

		// Ensure parent directory exists.
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("failed to create parent directory for %q: %w", destPath, err)
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("failed to open zip entry %q: %w", f.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("failed to read zip entry %q: %w", f.Name, err)
		}

		if err := os.WriteFile(destPath, data, 0o644); err != nil {
			return fmt.Errorf("failed to write file %q: %w", destPath, err)
		}
	}

	return nil
}
