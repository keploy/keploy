package yaml

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type IndexMode string

const (
	ModeDir  IndexMode = "dir"
	ModeFile IndexMode = "file"
)

// Ignored folders
const (
	FolderReports     = "reports"
	FolderTestReports = "testReports"
	FolderSchema      = "schema"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version      models.Version      `json:"version" yaml:"version"`
	Kind         models.Kind         `json:"kind" yaml:"kind"`
	Name         string              `json:"name" yaml:"name"`
	Spec         yamlLib.Node        `json:"spec" yaml:"spec"`
	Noise        []string            `json:"noise,omitempty" yaml:"noise,omitempty"`
	LastUpdated  *models.LastUpdated `json:"last_updated,omitempty" yaml:"last_updated,omitempty"`
	Curl         string              `json:"curl" yaml:"curl,omitempty"`
	ConnectionID string              `json:"connectionId" yaml:"connectionId,omitempty"`
}

// ctxReader wraps an io.Reader with a context for cancellation support
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (n int, err error) {
	select {
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	default:
		return cr.r.Read(p)
	}
}

// ctxWriter wraps an io.Writer with a context for cancellation support
type ctxWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (cw *ctxWriter) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		var written int
		written, err = cw.writer.Write(p)
		n += written
		if err != nil {
			return n, err
		}
		p = p[written:]
	}
	return n, nil
}

func WriteFile(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool) error {
	return WriteFileF(ctx, logger, path, fileName, docData, isAppend, FormatYAML)
}

func WriteFileF(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool, format Format) error {
	isFileEmpty, err := CreateFileF(ctx, logger, path, fileName, format)
	if err != nil {
		utils.LogError(logger, err, "failed to create file", zap.String("path directory", path), zap.String("file", fileName))
		return err
	}
	flag := os.O_WRONLY | os.O_TRUNC
	if isAppend {
		var sep []byte
		if !isFileEmpty {
			if format == FormatJSON {
				sep = []byte("\n") // NDJSON: newline separator
			} else {
				sep = []byte("---\n") // YAML: document separator
			}
		}
		docData = append(sep, docData...)
		flag = os.O_WRONLY | os.O_APPEND
	}
	filePath := filepath.Join(path, fileName+"."+format.FileExtension())
	file, err := os.OpenFile(filePath, flag, fs.ModePerm)
	if err != nil {
		utils.LogError(logger, err, "failed to open file for writing", zap.String("file", filePath))
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	cw := &ctxWriter{
		ctx:    ctx,
		writer: file,
	}

	_, err = cw.Write(docData)
	if err != nil {
		if err == ctx.Err() {
			return nil // Ignore context cancellation error
		}
		utils.LogError(logger, err, "failed to write the document", zap.String("file name", fileName))
		return err
	}
	return nil
}

func ReadFile(ctx context.Context, logger *zap.Logger, path, name string) ([]byte, error) {
	return ReadFileF(ctx, logger, path, name, FormatYAML)
}

func ReadFileF(ctx context.Context, logger *zap.Logger, path, name string, format Format) ([]byte, error) {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	cr := &ctxReader{
		ctx: ctx,
		r:   file,
	}

	data, err := io.ReadAll(cr)
	if err != nil {
		if err == ctx.Err() {
			return nil, err // Ignore context cancellation error
		}
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}
	return data, nil
}

// ReadFileAny reads a persisted artifact file, preferring `preferred`'s
// extension but transparently falling back to the other format if a file of
// that extension exists instead. Returns the bytes and the format that was
// actually read — so the caller can decode with the matching unmarshaller.
//
// This is the read-side mechanism that makes replay backward-compatible when
// users switch StorageFormat (or when old YAML recordings are replayed by a
// JSON-defaulted binary).
func ReadFileAny(ctx context.Context, logger *zap.Logger, path, name string, preferred Format) ([]byte, Format, error) {
	other := otherFormat(preferred)
	for _, f := range [2]Format{preferred, other} {
		filePath := filepath.Join(path, name+"."+f.FileExtension())
		if _, statErr := os.Stat(filePath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, "", statErr
		}
		data, err := ReadFileF(ctx, logger, path, name, f)
		if err != nil {
			return nil, "", err
		}
		return data, f, nil
	}
	return nil, "", fs.ErrNotExist
}

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	return CreateFileF(ctx, Logger, path, fileName, FormatYAML)
}

func CreateFileF(ctx context.Context, Logger *zap.Logger, path string, fileName string, format Format) (bool, error) {
	filePath, err := ValidatePath(filepath.Join(path, fileName+"."+format.FileExtension()))
	if err != nil {
		utils.LogError(Logger, err, "failed to validate the file path", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}

	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			utils.LogError(Logger, err,
				"failed to stat file — check filesystem permissions and that the configured keploy path is readable/writable by this process",
				zap.String("path directory", path),
				zap.String("file", fileName))
			return false, err
		}
		// Honour context cancellation/deadline before doing any
		// filesystem mutations. Previously the check was
		// `ctx.Err() == nil || ctx.Err() == context.Canceled`,
		// which both let cancelled contexts proceed (defeating the
		// cancellation contract) and masked DeadlineExceeded by
		// returning the surrounding os.Stat error instead of
		// ctx.Err().
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		// 0o755/0o644 rather than the historical 0o777 — nothing
		// in the keploy tree needs world-writable perms and the
		// stricter modes match the rest of the new testdb code.
		if err := os.MkdirAll(filepath.Join(path), 0o755); err != nil {
			utils.LogError(Logger, err, "failed to create a directory for the file", zap.String("path directory", path), zap.String("file", fileName))
			return false, err
		}
		file, err := os.OpenFile(filePath, os.O_CREATE, 0o644)
		if err != nil {
			utils.LogError(Logger, err, "failed to create the file", zap.String("path directory", path), zap.String("file", fileName))
			return false, err
		}
		if err := file.Close(); err != nil {
			utils.LogError(Logger, err, "failed to close the file", zap.String("path directory", path), zap.String("file", fileName))
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func ReadSessionIndices(ctx context.Context, path string, logger *zap.Logger, mode IndexMode) ([]string, error) {
	return ReadSessionIndicesF(ctx, path, logger, mode, FormatYAML)
}

func ReadSessionIndicesF(ctx context.Context, path string, logger *zap.Logger, mode IndexMode, format Format) ([]string, error) {
	var indices []string

	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	ext := "." + format.FileExtension()
	for _, v := range files {
		// Skip ignored folders
		if v.Name() == FolderReports || v.Name() == FolderTestReports || v.Name() == FolderSchema {
			continue
		}

		name := v.Name()

		switch mode {
		case ModeDir:
			if v.IsDir() {
				indices = append(indices, name)
			}
		case ModeFile:
			fileExt := filepath.Ext(name)
			if fileExt != ext {
				continue
			}
			name = name[:len(name)-len(fileExt)]
			indices = append(indices, name)
		}
	}

	return indices, nil
}

// ReadSessionIndicesAny is the format-agnostic variant of
// ReadSessionIndicesF. In ModeFile it accepts both .yaml and .json files
// (deduplicating by stripped base name) so callers discover test/report
// files regardless of the format they were recorded in.
func ReadSessionIndicesAny(ctx context.Context, path string, logger *zap.Logger, mode IndexMode) ([]string, error) {
	var indices []string

	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	seen := make(map[string]struct{})
	for _, v := range files {
		if v.Name() == FolderReports || v.Name() == FolderTestReports || v.Name() == FolderSchema {
			continue
		}

		name := v.Name()

		switch mode {
		case ModeDir:
			if v.IsDir() {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					indices = append(indices, name)
				}
			}
		case ModeFile:
			fileExt := filepath.Ext(name)
			if fileExt != ".yaml" && fileExt != ".json" {
				continue
			}
			base := name[:len(name)-len(fileExt)]
			if _, ok := seen[base]; ok {
				continue
			}
			seen[base] = struct{}{}
			indices = append(indices, base)
		}
	}

	return indices, nil
}

func DeleteFile(_ context.Context, logger *zap.Logger, path, name string) error {
	return DeleteFileF(nil, logger, path, name, FormatYAML)
}

func DeleteFileF(_ context.Context, logger *zap.Logger, path, name string, format Format) error {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	err := os.Remove(filePath)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the file", zap.String("file", filePath))
		return fmt.Errorf("failed to delete the file: %v", err)
	}
	return nil
}

func DeleteDir(_ context.Context, logger *zap.Logger, path string) error {
	err := os.RemoveAll(path)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the directory", zap.String("path", path))
		return fmt.Errorf("failed to delete the directory: %v", err)
	}
	return nil
}
