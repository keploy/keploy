package update

import (
	"archive/tar"
	"archive/zip"
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
	"github.com/minio/selfupdate"
	"go.uber.org/zap"
)

var (
	ErrInDockerEnv         = errors.New("updates are not supported in Docker - please pull the latest image instead")
	ErrDevVersion          = errors.New("updates are not supported for development versions")
	ErrUnsupportedFiletype = errors.New("unsupported file type for auto-update")
)

type Config struct {
	BinaryName     string
	CurrentVersion string
	IsDevVersion   bool
	IsInDocker     bool
	DownloadURLs   map[string]string
	LatestVersion  string
	Changelog      string
}

type UpdateManager struct {
	Logger *zap.Logger
	Config Config
}

func NewUpdateManager(logger *zap.Logger, cfg Config) *UpdateManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &UpdateManager{
		Logger: logger,
		Config: cfg,
	}
}

func (u *UpdateManager) CheckAndUpdate(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	u.Logger.Info("Checking for new version...", zap.String("current", u.Config.CurrentVersion), zap.String("latest", u.Config.LatestVersion))

	if u.Config.IsInDocker {
		u.Logger.Debug("Skipping update check: running in Docker.")
		return false, nil
	}

	if u.Config.IsDevVersion {
		u.Logger.Debug("Skipping update check: running 'dev' version.")
		return false, nil
	}

	if u.Config.CurrentVersion == u.Config.LatestVersion {
		fmt.Printf("✅ You are already on the latest version of %s: %s\n",
			u.Config.BinaryName, u.Config.CurrentVersion)
		return false, nil
	}

	u.Logger.Info("New version found, downloading update...", zap.String("version", u.Config.LatestVersion))

	osArch := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "darwin" {
		osArch = "darwin_all"
	}
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		osArch = "windows_amd64"
	}

	downloadURL, ok := u.Config.DownloadURLs[osArch]
	if !ok {
		return false, fmt.Errorf("no download URL configured for platform: %s", osArch)
	}

	if err := u.downloadAndUpdate(ctx, downloadURL); err != nil {
		return false, fmt.Errorf("update failed: %w", err)
	}

	if u.Config.Changelog != "" {
		if err := renderChangelog(u.Config.Changelog); err != nil {
			u.Logger.Warn("Failed to render changelog", zap.Error(err))
		}
	}

	return true, nil
}

func (u *UpdateManager) downloadAndUpdate(ctx context.Context, downloadURL string) error {
	binPath, err := findBinaryPath(u.Config.BinaryName)
	if err != nil {
		return fmt.Errorf("failed to locate binary: %w", err)
	}
	u.Logger.Info("Found binary to update", zap.String("path", binPath))

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download file: http status %s (url: %s)", resp.Status, downloadURL)
	}

	tmpFile, err := os.CreateTemp("", u.Config.BinaryName+"-*"+filepath.Ext(downloadURL))
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return fmt.Errorf("failed to save download: %w", err)
	}
	tmpFile.Close()

	fileType := filepath.Ext(downloadURL)
	if strings.HasSuffix(downloadURL, ".tar.gz") {
		fileType = ".tar.gz"
	}
	if strings.Contains(fileType, ".dmg") {
		fileType = ".dmg"
	}

	switch fileType {
	case ".tar.gz":
		if err := u.extractTarGzAndApply(tmpFile.Name(), binPath); err != nil {
			return fmt.Errorf("failed to extract and apply .tar.gz update: %w", err)
		}
	case ".zip":
		if err := u.extractZipAndApply(tmpFile.Name(), binPath); err != nil {
			return fmt.Errorf("failed to extract and apply .zip update: %w", err)
		}
	case ".dmg":
		u.Logger.Warn("Downloaded .dmg, but cannot auto-install. Please install manually.", zap.String("path", tmpFile.Name()))
		return fmt.Errorf("%w: .dmg files must be installed manually", ErrUnsupportedFiletype)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedFiletype, fileType)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binPath, 0755); err != nil {
			return fmt.Errorf("failed to set permissions: %w", err)
		}
	}

	return nil
}

func (u *UpdateManager) extractTarGzAndApply(tarballPath, finalBinPath string) error {
	file, err := os.Open(tarballPath)
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

		if header.Typeflag == tar.TypeReg && filepath.Clean(header.Name) == u.Config.BinaryName {
			u.Logger.Info("Found binary in archive, applying safe update...",
				zap.String("binary", header.Name),
				zap.String("targetPath", finalBinPath),
			)
			err = selfupdate.Apply(tarReader, selfupdate.Options{
				TargetPath: finalBinPath,
			})
			if err != nil {
				return fmt.Errorf("safe update failed: %w", err)
			}
			return nil
		}
	}

	return fmt.Errorf("binary %q not found in downloaded archive", u.Config.BinaryName)
}

func (u *UpdateManager) extractZipAndApply(zipPath, finalBinPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer r.Close()

	binaryBaseName := u.Config.BinaryName
	if runtime.GOOS == "windows" && !strings.HasSuffix(binaryBaseName, ".exe") {
		binaryBaseName += ".exe"
	}

	for _, f := range r.File {
		cleanedName := filepath.Clean(f.Name)

		if f.Mode().IsRegular() && (cleanedName == u.Config.BinaryName || cleanedName == binaryBaseName) {
			u.Logger.Info("Found binary in zip archive, applying safe update...",
				zap.String("binary", f.Name),
				zap.String("targetPath", finalBinPath),
			)

			binaryReader, err := f.Open()
			if err != nil {
				return fmt.Errorf("failed to open binary from zip: %w", err)
			}
			defer binaryReader.Close()

			err = selfupdate.Apply(binaryReader, selfupdate.Options{
				TargetPath: finalBinPath,
			})
			if err != nil {
				return fmt.Errorf("safe update failed: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("binary %q not found in downloaded zip archive", u.Config.BinaryName)
}

func findBinaryPath(binaryName string) (string, error) {
	if binaryName == "" {
		return "", errors.New("binary name cannot be empty")
	}

	execPath, err := os.Executable()
	if err == nil && execPath != "" {
		// Resolve symlinks to get the actual file path
		resolvedPath, err := filepath.EvalSymlinks(execPath)
		if err == nil && resolvedPath != "" {
			return resolvedPath, nil
		}
		// Fallback to original execPath if symlink resolution fails
		return execPath, nil
	}

	if path, err := exec.LookPath(binaryName); err == nil && path != "" {
		return path, nil
	}

	defaultPath := filepath.Join("/usr/local/bin", binaryName)
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(binaryName, ".exe") {
			binaryName += ".exe"
		}
		defaultPath = filepath.Join("C:", "Program Files", binaryName)
	}

	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath, nil
	}

	return "", fmt.Errorf("binary not found. Looked for %s, %s, and in PATH", execPath, defaultPath)
}

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

