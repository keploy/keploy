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

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
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

// FindLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func FindLastIndex(path string, _ *zap.Logger) (int, error) {
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
		if v.Name() == "mocks.yaml" || v.Name() == "config.yaml" {
			continue
		}
		fileName := filepath.Base(v.Name())
		fileNameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		fileNameParts := strings.Split(fileNameWithoutExt, "-")
		if len(fileNameParts) != 2 || (fileNameParts[0] != "test" && fileNameParts[0] != "report") {
			continue
		}
		indxStr := fileNameParts[1]
		indx, err := strconv.Atoi(indxStr)
		if err != nil {
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
			logger.Error("Error creating directory", zap.String("directory", path), zap.Error(err))
			return err
		}
	}
	return nil
}

// ReadYAMLFile to read and parse YAML file
func ReadYAMLFile(ctx context.Context, logger *zap.Logger, filePath string, fileName string, v interface{}) error {
	configData, err := ReadFile(ctx, logger, filePath, fileName)
	if err != nil {
		logger.Fatal("Error reading file", zap.Error(err))
		return err
	}

	err = yaml.Unmarshal(configData, v)
	if err != nil {
		logger.Error("Error parsing YAML", zap.Error(err))
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
