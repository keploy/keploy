package update

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
		u.Logger.Warn(ErrInDockerEnv.Error())
		return false, ErrInDockerEnv
	}

	if u.Config.IsDevVersion {
		u.Logger.Info("Running development version, skipping update.")
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
		u.Logger.Warn("No download URL configured for this platform", zap.String("platform", osArch))
		return false, nil // Not an error, just no update path
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
		return fmt.Errorf("failed to download file: http status %d %s", resp.StatusCode, resp.Status)
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
	if strings.Contains(fileType, ".dmg") {
		fileType = ".dmg"
	}

	switch fileType {
	case ".gz":
		if err := u.extractTarGzAndApply(tmpFile.Name(), binPath); err != nil {
			return fmt.Errorf("failed to extract and apply .tar.gz update: %w", err)
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


func findBinaryPath(binaryName string) (string, error) {
	if binaryName == "" {
		return "", errors.New("binary name cannot be empty")
	}

	execPath, err := os.Executable()
	if err == nil && execPath != "" {
		resolvedPath, err := filepath.EvalSymlinks(execPath)
		if err == nil {
			return resolvedPath, nil
		}
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

