package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
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
	ErrInDockerEnv           = errors.New("updates are not supported in Docker - please pull the latest image instead")
	ErrDevVersion            = errors.New("updates are not supported for development versions")
	ErrUnsupportedFiletype   = errors.New("unsupported file type for auto-update")
	ErrDmgNeedsManualInstall = errors.New("unsupported file type: .dmg files must be installed manually")
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
		u.Logger.Info("✅ You are already on the latest version",
			zap.String("binary", u.Config.BinaryName),
			zap.String("version", u.Config.CurrentVersion),
		)
		return false, nil
	}

	u.Logger.Info("New version found, downloading update...", zap.String("version", u.Config.LatestVersion))

	osArch := runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "darwin" {
		osArch = "darwin_all"
	}
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		osArch = "windows_amd64"
	}

	downloadURL, ok := u.Config.DownloadURLs[osArch]
	if !ok {
		u.Logger.Warn("No download URL configured for this platform", zap.String("platform", osArch))
		return false, nil
	}

	if err := u.downloadAndUpdate(ctx, downloadURL); err != nil {
		// Log the error unless it's the expected "manual install" error
		if !errors.Is(err, ErrDmgNeedsManualInstall) {
			u.Logger.Error("update failed", zap.Error(err))
		}
		// Propagate the original error up
		return false, err
	}

	if u.Config.Changelog != "" {
		if err := u.renderChangelog(u.Config.Changelog); err != nil {
			u.Logger.Warn("Failed to render changelog", zap.Error(err))
		}
	}

	return true, nil
}

func (u *UpdateManager) downloadAndUpdate(ctx context.Context, downloadURL string) error {
	binPath, err := findBinaryPath(u.Config.BinaryName, u.Logger)
	if err != nil {
		u.Logger.Error("failed to locate binary", zap.Error(err))
		return err
	}
	u.Logger.Info("Found binary to update", zap.String("path", binPath))

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		u.Logger.Error("failed to create request", zap.Error(err))
		return err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		u.Logger.Error("failed to download file", zap.Error(err))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		u.Logger.Error("failed to download file", zap.Int("status", resp.StatusCode), zap.String("status_text", resp.Status))
		return errors.New("download failed with status: " + resp.Status)
	}

	tmpFile, err := os.CreateTemp("", u.Config.BinaryName+"-*"+filepath.Ext(downloadURL))
	if err != nil {
		u.Logger.Error("failed to create temporary file", zap.Error(err))
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		u.Logger.Error("failed to save download", zap.Error(err))
		return err
	}
	tmpFile.Close()

	fileType := filepath.Ext(downloadURL)
	if strings.Contains(fileType, ".dmg") {
		fileType = ".dmg"
	}

	switch fileType {
	case ".gz":
		if err := u.extractTarGzAndApply(tmpFile.Name(), binPath); err != nil {
			u.Logger.Error("failed to extract and apply .tar.gz update", zap.Error(err))
			return err
		}
	case ".dmg":
		u.Logger.Warn("Downloaded .dmg, but cannot auto-install. Please install manually.", zap.String("path", tmpFile.Name()))
		return ErrDmgNeedsManualInstall
	default:
		u.Logger.Warn("unsupported file type for auto-update", zap.String("type", fileType))
		return ErrUnsupportedFiletype
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binPath, 0755); err != nil {
			u.Logger.Error("failed to set permissions", zap.Error(err))
			return err
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
				u.Logger.Error("safe update failed", zap.Error(err))
				return err
			}
			return nil
		}
	}

	u.Logger.Error("binary not found in downloaded archive", zap.String("binary", u.Config.BinaryName))
	return errors.New("binary not found in downloaded archive")
}

func findBinaryPath(binaryName string, logger *zap.Logger) (string, error) {
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

	logger.Warn("binary not found", zap.String("execPath", execPath), zap.String("defaultPath", defaultPath))
	return "", errors.New("binary not found in common paths")
}

func (u *UpdateManager) renderChangelog(changelog string) error {
	if changelog == "" {
		return nil
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(0),
	)
	if err != nil {
		u.Logger.Warn("failed to initialize changelog renderer", zap.Error(err))
		return err
	}

	out, err := renderer.Render("\n" + changelog)
	if err != nil {
		u.Logger.Warn("failed to render changelog", zap.Error(err))
		return err
	}

	u.Logger.Info(out)
	return nil
}
