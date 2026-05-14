// Package yaml provides utility functions for working with YAML files.
package yaml

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]models.HeaderResult, noise map[string]string) bool {
	if res == nil {
		return false
	}
	match := true
	_, isHeaderNoisy := noise["header"]
	for k, v := range h1 {
		_, isNoisy := noise[k]
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h2[k]
		if !isNoisy {
			if !ok {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: val,
						},
					})
				}
				match = false
				continue
			}
			for i, e := range v {
				if val[i] != e {
					if checkKey(res, k) {
						*res = append(*res, models.HeaderResult{
							Normal: false,
							Expected: models.Header{
								Key:   k,
								Value: v,
							},
							Actual: models.Header{
								Key:   k,
								Value: val,
							},
						})
					}
					match = false
					continue
				}
			}
		}
		if checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: v,
				},
				Actual: models.Header{
					Key:   k,
					Value: val,
				},
			})
		}
	}
	for k, v := range h2 {
		_, isNoisy := noise[k]
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h1[k]
		if isNoisy && checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: val,
				},
				Actual: models.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			if checkKey(res, k) {
				*res = append(*res, models.HeaderResult{
					Normal: false,
					Expected: models.Header{
						Key:   k,
						Value: nil,
					},
					Actual: models.Header{
						Key:   k,
						Value: v,
					},
				})
			}

			match = false
		}
	}
	return match
}

func checkKey(res *[]models.HeaderResult, key string) bool {
	for _, v := range *res {
		if key == v.Expected.Key {
			return false
		}
	}
	return true
}

func Contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}

func NewSessionIndex(path string, Logger *zap.Logger) (string, error) {
	indx := 0
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return fmt.Sprintf("%s%v", models.TestSetPattern, indx), nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return "", err
	}

	for _, v := range files {
		// fmt.Println("name for the file", v.Name())
		fileName := filepath.Base(v.Name())
		fileNamePackets := strings.Split(fileName, "-")
		if len(fileNamePackets) == 3 {
			fileIndx, err := strconv.Atoi(fileNamePackets[2])
			if err != nil {
				Logger.Debug("failed to convert the index string to integer", zap.Error(err))
				continue
			}
			if indx < fileIndx+1 {
				indx = fileIndx + 1
			}
		}
	}
	return fmt.Sprintf("%s%v", models.TestSetPattern, indx), nil
}

func ValidatePath(path string) (string, error) {
	// Validate the input to prevent directory traversal attack
	if strings.Contains(path, "..") {
		return "", errors.New("invalid path: contains '..' indicating directory traversal")
	}
	return path, nil
}

// NextIndexForPrefix scans path for testcase files named
// "{prefix}-{N}.{ext}" (where {ext} is either yaml or json) and returns
// the next sequential index (max+1, starting at 1). It is used to
// disambiguate descriptive test case slugs when multiple recordings
// share the same endpoint.
//
// Both .yaml and .json files are counted into the same index space so
// a tests/ directory containing a mix of formats (e.g. a yaml-record +
// json-record dual pass) does not hand out colliding indices to a
// JSON recorder. Without this, claimName loops 256× on every second
// JSON-format capture for any repeating slug because the existing
// .json sibling is invisible to the index scan and generateName keeps
// re-suggesting the same N.
//
// A missing directory is treated as "no existing files" and returns 1
// (first recording in a new test set). Any other IO error is returned
// to the caller so we never silently overwrite an existing testcase
// file because of a transient read failure.
func NextIndexForPrefix(path, prefix string) (int, error) {
	if prefix == "" {
		return 1, nil
	}
	// Reject a prefix that could escape its containing directory
	// (path separators or parent references). The slug builder never
	// emits these, but NextIndexForPrefix is exported so keep the
	// guard in place for future callers.
	if strings.ContainsAny(prefix, `/\`) || strings.Contains(prefix, "..") {
		return 0, fmt.Errorf("invalid prefix %q: must not contain path separators or parent references", prefix)
	}
	// The directory path itself is what we actually read from, so
	// validate that here instead of validating the slug prefix.
	// Capture and reuse the (potentially normalised) return value
	// so a future hardening of ValidatePath — e.g. calling
	// filepath.Clean — automatically flows through to the ReadDir
	// and HasPrefix checks below without leaving this function
	// silently using the raw input.
	validatedPath, err := ValidatePath(path)
	if err != nil {
		return 0, err
	}
	path = validatedPath
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	defer func() { _ = dir.Close() }()
	files, err := dir.ReadDir(0)
	if err != nil {
		return 0, err
	}
	lastIndex := 0
	for _, v := range files {
		name := filepath.Base(v.Name())
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".json" {
			continue
		}
		stem := name[:len(name)-len(ext)]
		if !strings.HasPrefix(stem, prefix+"-") {
			continue
		}
		numStr := stem[len(prefix)+1:]
		idx, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if idx > lastIndex {
			lastIndex = idx
		}
	}
	return lastIndex + 1, nil
}

// FindLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func FindLastIndex(path string, logger *zap.Logger) (int, error) {
	return FindLastIndexF(path, logger, FormatYAML)
}

func FindLastIndexF(path string, logger *zap.Logger, format Format) (int, error) {
	// Delegate to the format-agnostic scanner: when allocating the next
	// test-N index we must see BOTH .yaml and .json files so we don't
	// hand out a number that collides with an existing file of the other
	// format after a StorageFormat switch.
	_ = format
	return FindLastIndexAny(path, logger)
}

// FindLastIndexAny scans `path` for both test-N.yaml and test-N.json (and
// report-N.*) and returns the next index, ensuring newly-created files never
// collide with pre-existing files of the other format.
func FindLastIndexAny(path string, _ *zap.Logger) (int, error) {
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		return 1, nil
	}
	files, err := dir.ReadDir(0)
	if err != nil {
		return 1, nil
	}

	lastIndex := 0
	for _, v := range files {
		name := v.Name()
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".json" {
			continue
		}
		// Skip well-known non-test files in either format.
		base := name[:len(name)-len(ext)]
		if base == "mocks" || base == "config" {
			continue
		}
		fileNameParts := strings.Split(base, "-")
		if len(fileNameParts) != 2 || (fileNameParts[0] != "test" && fileNameParts[0] != "report") {
			continue
		}
		indx, convErr := strconv.Atoi(fileNameParts[1])
		if convErr != nil {
			continue
		}
		if indx > lastIndex {
			lastIndex = indx
		}
	}
	lastIndex++

	return lastIndex, nil
}

func ReadDir(path string, fileMode fs.FileMode) (*os.File, error) {
	dir, err := os.OpenFile(path, os.O_RDONLY, fileMode)
	if err != nil {
		return nil, err
	}
	return dir, nil
}

// CreateDir to create a directory if it doesn't exist
func CreateDir(path string, logger *zap.Logger) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		err := os.MkdirAll(path, os.ModePerm)
		if err != nil {
			utils.LogError(logger, err, "failed to create directory", zap.String("directory", path))
			return err
		}
	}
	return nil
}

// ReadYAMLFile to read and parse YAML file
func ReadYAMLFile(ctx context.Context, logger *zap.Logger, filePath string, fileName string, v interface{}, extType bool) error {
	if !extType {
		filePath = filepath.Join(filePath, fileName+".yml")

	} else {
		filePath = filepath.Join(filePath, fileName+".yaml")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to read the file: %v", err)
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

	configData, err := io.ReadAll(cr)
	if err != nil {
		if err == ctx.Err() {
			return err // Ignore context cancellation error
		}
		return fmt.Errorf("failed to read the file: %v", err)
	}

	err = yaml.Unmarshal(configData, v)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal YAML", zap.String("file", filePath))
		return err
	}
	return nil
}

// CopyFile copies a single file from src to dst
func CopyFile(src, dst string, rename bool, logger *zap.Logger) error {
	srcFile, err := os.Open(src)

	if err != nil {
		return err
	}
	defer func() {
		if err := srcFile.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", srcFile.Name()))
		}
	}()
	// If rename is true, generate a new name for the destination file
	if rename {
		dst = generateSchemaName(dst)
	}
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := dstFile.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", dstFile.Name()))
		}
	}()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	// Ensure the copied file has the same permissions as the original file
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	err = os.Chmod(dst, srcInfo.Mode())
	if err != nil {
		return err
	}

	return nil
}

// CopyDir recursively copies a directory tree, attempting to preserve permissions
func CopyDir(srcDir, destDir string, rename bool, logger *zap.Logger) error {
	// Ensure the destination directory exists
	if _, err := os.Stat(destDir); os.IsNotExist(err) {
		err := os.MkdirAll(destDir, os.ModePerm)
		if err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		destPath := filepath.Join(destDir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if info.IsDir() {
			err = os.MkdirAll(destPath, info.Mode())
			if err != nil {
				return err
			}
			err = CopyDir(srcPath, destPath, rename, logger)
			if err != nil {
				return err
			}
		} else {
			err = CopyFile(srcPath, destPath, rename, logger)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// generateSchemaName generates a new schema name
func generateSchemaName(src string) string {
	dir := filepath.Dir(src)
	newName := "schema" + filepath.Ext(src)
	return filepath.Join(dir, newName)
}

func FileExists(_ context.Context, logger *zap.Logger, path string, fileName string) (bool, error) {
	return FileExistsF(nil, logger, path, fileName, FormatYAML)
}

func FileExistsF(_ context.Context, logger *zap.Logger, path string, fileName string, format Format) (bool, error) {
	filePath, err := ValidatePath(filepath.Join(path, fileName+"."+format.FileExtension()))
	if err != nil {
		utils.LogError(logger, err, "failed to validate the file path", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		utils.LogError(logger, err, "failed to check if the file exists", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}

	return true, nil
}

// FileExistsAny checks whether <path>/<fileName>.<ext> exists for either
// supported format, preferring `preferred`. Returns true + the format that
// was found. Use this on read paths where the stored file may be in a
// different format than the current StorageFormat.
func FileExistsAny(ctx context.Context, logger *zap.Logger, path string, fileName string, preferred Format) (bool, Format, error) {
	other := otherFormat(preferred)
	for _, f := range [2]Format{preferred, other} {
		exists, err := FileExistsF(ctx, logger, path, fileName, f)
		if err != nil {
			return false, "", err
		}
		if exists {
			return true, f, nil
		}
	}
	return false, preferred, nil
}
