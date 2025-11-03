package update

// This is a generic auto update package

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/glamour"
	"go.uber.org/zap"
)

var (
	ErrInDockerEnv = errors.New("updates are not supported in Docker - please pull the latest image instead")
	ErrDevVersion  = errors.New("updates are not supported for development versions")
)

// Config holds the configuration for the update process
type Config struct {
	BinaryName     string            // e.g. "keploy" or "keploy-agent"
	CurrentVersion string            // e.g. "v1.0.0"
	IsDevVersion   bool              // Whether this is a development version
	IsInDocker     bool              // Whether running in Docker
	DownloadURLs   map[string]string // Map of OS_ARCH to download URL pattern
	LatestVersion  string            // Latest version from GitHub
	Changelog      string            // Release notes/changelog
}

// UpdateManager handles the update process
type UpdateManager struct {
	Logger *zap.Logger
	Config Config
}

// NewUpdateManager creates a new update manager instance
func NewUpdateManager(logger *zap.Logger, cfg Config) *UpdateManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &UpdateManager{
		Logger: logger,
		Config: cfg,
	}
}

// CheckAndUpdate checks for new releases and updates the binary if a newer version exists
func (u *UpdateManager) CheckAndUpdate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	if u.Config.IsInDocker {
		return ErrInDockerEnv
	}

	if u.Config.IsDevVersion {
		return ErrDevVersion
	}

	if u.Config.CurrentVersion == u.Config.LatestVersion {
		fmt.Printf("✅ You are already on the latest version of %s: %s\n",
			u.Config.BinaryName, u.Config.CurrentVersion)
		return nil
	}

	u.Logger.Info("Updating to version", zap.String("version", u.Config.LatestVersion))

	// Get platform-specific download URL
	osArch := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "darwin" {
		osArch = "darwin_all" // Special case for macOS
	}

	downloadURL, ok := u.Config.DownloadURLs[osArch]
	if !ok {
		return fmt.Errorf("no download URL configured for platform: %s", osArch)
	}

	if err := u.downloadAndUpdate(ctx, downloadURL); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}

	if u.Config.Changelog != "" {
		if err := renderChangelog(u.Config.Changelog); err != nil {
			u.Logger.Warn("Failed to render changelog", zap.Error(err))
		}
	}

	return nil
}

func (u *UpdateManager) downloadAndUpdate(ctx context.Context, downloadURL string) error {
	// Create request
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Download file
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	// Create temporary file
	tmpFile, err := os.CreateTemp("", u.Config.BinaryName+"-*.tar.gz")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	// Copy download to temp file
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return fmt.Errorf("failed to save download: %w", err)
	}

	// Extract archive
	if err := extractTarGz(tmpFile.Name(), "/tmp"); err != nil {
		return fmt.Errorf("failed to extract archive: %w", err)
	}

	// Find binary path
	binPath, err := findBinaryPath(u.Config.BinaryName)
	if err != nil {
		return fmt.Errorf("failed to locate binary: %w", err)
	}

	// Replace binary
	if err := os.Rename("/tmp/"+u.Config.BinaryName, binPath); err != nil {
		return fmt.Errorf("failed to install binary: %w", err)
	}

	// Set permissions
	if err := os.Chmod(binPath, 0755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	return nil
}

// extractTarGz extracts a tar.gz archive to the specified destination
func extractTarGz(gzipPath, destDir string) error {
	file, err := os.Open(gzipPath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, filepath.Clean(header.Name))
		// Ensure the target path is within the destination directory to prevent Zip Slip vulnerabilities.
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		destDirAbs, err := filepath.Abs(destDir)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(destDirAbs, targetAbs)
		if err != nil {
			return err
		}
		// If rel starts with ".." or is absolute, it's outside destDir; skip or error.
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("tar file entry %q is outside the target directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

// findBinaryPath finds where the current binary is located
func findBinaryPath(binaryName string) (string, error) {
	if binaryName == "" {
		return "", errors.New("binary name cannot be empty")
	}

	// Try to get the current executable path
	execPath, err := os.Executable()
	if err == nil && execPath != "" {
		return execPath, nil
	}

	// Try to find in PATH
	if path, err := exec.LookPath(binaryName); err == nil && path != "" {
		return path, nil
	}

	// Fallback to default path with platform-specific binary name
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	defaultPath := filepath.Join("/usr/local/bin", binaryName)

	// Verify the path exists
	if _, err := os.Stat(defaultPath); err != nil {
		return "", fmt.Errorf("binary not found at %s: %w", defaultPath, err)
	}

	return defaultPath, nil
}

// renderChangelog pretty-prints the changelog in terminal using Glamour
func renderChangelog(changelog string) error {
	if changelog == "" {
		return nil
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(0),
	)
	if err != nil {
		return fmt.Errorf("failed to initialize renderer: %w", err)
	}

	out, err := renderer.Render("\n" + changelog)
	if err != nil {
		return fmt.Errorf("failed to render changelog: %w", err)
	}

	fmt.Println(out)
	return nil
}
