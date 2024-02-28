package yaml

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
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

// findLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func FindLastIndex(path string, Logger *zap.Logger) (int, error) {
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
	lastIndex += 1

	return lastIndex, nil
}

func ReadDir(path string, fileMode fs.FileMode) (*os.File, error) {
	dir, err := os.OpenFile(path, os.O_RDONLY, fileMode)
	if err != nil {
		return nil, err
	}
	return dir, nil
}
