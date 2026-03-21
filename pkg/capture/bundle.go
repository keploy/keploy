package capture

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// BundleManifest describes the contents of a debug bundle.
type BundleManifest struct {
	Version     string    `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	Mode        string    `json:"mode"`
	AppName     string    `json:"app_name,omitempty"`
	CaptureFile string    `json:"capture_file"`
	MockDir     string    `json:"mock_dir,omitempty"`
	TestDir     string    `json:"test_dir,omitempty"`
	LogFile     string    `json:"log_file,omitempty"`
	ConfigFile  string    `json:"config_file,omitempty"`
	Notes       string    `json:"notes,omitempty"`
}

// BundleOptions configures what gets included in the debug bundle.
type BundleOptions struct {
	CaptureFile string // path to .kpcap file (required)
	MockDir     string // path to mock directory
	TestDir     string // path to test directory
	LogFile     string // path to debug log file
	ConfigFile  string // path to keploy config file
	OutputPath  string // where to write the .tar.gz bundle
	AppName     string
	Mode        string
	Notes       string // optional notes from the user about the issue
}

// CreateBundle packages a capture file, mocks, tests, logs, and config into a
// single .tar.gz archive that can be shared for debugging.
func CreateBundle(logger *zap.Logger, opts BundleOptions) (string, error) {
	if opts.CaptureFile == "" {
		return "", fmt.Errorf("capture file path is required")
	}

	// Verify capture file exists and is valid
	if _, err := os.Stat(opts.CaptureFile); err != nil {
		return "", fmt.Errorf("capture file not found: %w", err)
	}

	// Generate output path if not specified
	if opts.OutputPath == "" {
		opts.OutputPath = fmt.Sprintf("keploy-debug-bundle_%s_%s.tar.gz",
			opts.Mode,
			time.Now().Format("20060102_150405"))
	}

	// Create the tar.gz file
	outFile, err := os.Create(opts.OutputPath)
	if err != nil {
		return "", fmt.Errorf("failed to create bundle file: %w", err)
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	bundleDir := "keploy-debug-bundle"

	// Add capture file
	captureName, err := addFileToTar(tarWriter, opts.CaptureFile, filepath.Join(bundleDir, "capture", filepath.Base(opts.CaptureFile)))
	if err != nil {
		return "", fmt.Errorf("failed to add capture file to bundle: %w", err)
	}

	manifest := BundleManifest{
		Version:     "1",
		CreatedAt:   time.Now(),
		Mode:        opts.Mode,
		AppName:     opts.AppName,
		CaptureFile: captureName,
		Notes:       opts.Notes,
	}

	// Add mock directory (optional — log at debug level if missing/unreadable)
	if opts.MockDir != "" {
		if err := addDirToTar(tarWriter, opts.MockDir, filepath.Join(bundleDir, "mocks")); err != nil {
			logger.Debug("skipping mock directory in bundle", zap.Error(err))
		} else {
			manifest.MockDir = "mocks"
		}
	}

	// Add test directory (optional)
	if opts.TestDir != "" {
		if err := addDirToTar(tarWriter, opts.TestDir, filepath.Join(bundleDir, "tests")); err != nil {
			logger.Debug("skipping test directory in bundle", zap.Error(err))
		} else {
			manifest.TestDir = "tests"
		}
	}

	// Add log file (optional)
	if opts.LogFile != "" {
		if _, err := addFileToTar(tarWriter, opts.LogFile, filepath.Join(bundleDir, "logs", filepath.Base(opts.LogFile))); err != nil {
			logger.Debug("skipping log file in bundle", zap.Error(err))
		} else {
			manifest.LogFile = filepath.Join("logs", filepath.Base(opts.LogFile))
		}
	}

	// Add config file
	if opts.ConfigFile != "" {
		if _, err := addFileToTar(tarWriter, opts.ConfigFile, filepath.Join(bundleDir, filepath.Base(opts.ConfigFile))); err != nil {
			logger.Warn("failed to add config file to bundle", zap.Error(err))
		} else {
			manifest.ConfigFile = filepath.Base(opts.ConfigFile)
		}
	}

	// Write manifest
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal manifest: %w", err)
	}

	manifestHeader := &tar.Header{
		Name:    filepath.Join(bundleDir, "manifest.json"),
		Size:    int64(len(manifestJSON)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tarWriter.WriteHeader(manifestHeader); err != nil {
		return "", fmt.Errorf("failed to write manifest header: %w", err)
	}
	if _, err := tarWriter.Write(manifestJSON); err != nil {
		return "", fmt.Errorf("failed to write manifest: %w", err)
	}

	logger.Info("Debug bundle created",
		zap.String("path", opts.OutputPath),
		zap.String("capture", opts.CaptureFile))

	return opts.OutputPath, nil
}

// ExtractBundle extracts a debug bundle to a target directory.
func ExtractBundle(bundlePath, targetDir string) (*BundleManifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open bundle: %w", err)
	}
	defer f.Close()

	gzReader, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to open gzip reader: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	var manifest *BundleManifest

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Sanitize path to prevent directory traversal (zip-slip).
		// filepath.Clean alone is insufficient for absolute paths; we must also
		// verify the resolved destination stays inside targetDir.
		cleanName := filepath.Clean(header.Name)
		if filepath.IsAbs(cleanName) || strings.Contains(cleanName, "..") {
			return nil, fmt.Errorf("unsafe tar entry path: %q", header.Name)
		}

		targetPath := filepath.Join(targetDir, cleanName)
		// Final safeguard: ensure the path is still inside targetDir after Join.
		absTarget, err := filepath.Abs(targetPath)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve path %q: %w", targetPath, err)
		}
		absBase, err := filepath.Abs(targetDir)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve base dir %q: %w", targetDir, err)
		}
		if !strings.HasPrefix(absTarget+string(filepath.Separator), absBase+string(filepath.Separator)) {
			return nil, fmt.Errorf("tar entry %q would escape extraction directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return nil, fmt.Errorf("failed to create directory %s: %w", targetPath, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return nil, fmt.Errorf("failed to create parent directory for %s: %w", targetPath, err)
			}

			outFile, err := os.Create(targetPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create file %s: %w", targetPath, err)
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return nil, fmt.Errorf("failed to write file %s: %w", targetPath, err)
			}
			outFile.Close()

			// Parse manifest if this is it
			if filepath.Base(cleanName) == "manifest.json" {
				data, err := os.ReadFile(targetPath)
				if err == nil {
					var m BundleManifest
					if err := json.Unmarshal(data, &m); err == nil {
						manifest = &m
					}
				}
			}
		}
	}

	if manifest == nil {
		return nil, fmt.Errorf("manifest.json not found in bundle")
	}

	return manifest, nil
}

// addFileToTar adds a single file to the tar archive.
func addFileToTar(tw *tar.Writer, srcPath, tarPath string) (string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	header := &tar.Header{
		Name:    tarPath,
		Size:    info.Size(),
		Mode:    int64(info.Mode()),
		ModTime: info.ModTime(),
	}

	if err := tw.WriteHeader(header); err != nil {
		return "", err
	}

	if _, err := io.Copy(tw, f); err != nil {
		return "", err
	}

	return tarPath, nil
}

// addDirToTar adds a directory and its contents to the tar archive.
func addDirToTar(tw *tar.Writer, srcDir, tarDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		tarPath := filepath.Join(tarDir, relPath)

		if info.IsDir() {
			header := &tar.Header{
				Name:     tarPath + "/",
				Typeflag: tar.TypeDir,
				Mode:     int64(info.Mode()),
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(header)
		}

		// Skip files larger than 100MB
		if info.Size() > 100*1024*1024 {
			return nil
		}

		_, err = addFileToTar(tw, path, tarPath)
		return err
	})
}
