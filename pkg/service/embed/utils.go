package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/pkoukk/tiktoken-go"
	"go.uber.org/zap"
)

// CountTokens returns the number of tokens in a text string.
func CountTokens(text string, encodingName string) (int, error) {
	tke, err := tiktoken.EncodingForModel(encodingName)
	if err != nil {
		tke, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			return 0, err
		}
	}
	tokens := tke.Encode(text, nil, nil)
	return len(tokens), nil
}

// LoadJSON loads data from a JSON file.
func LoadJSON(jsonFile string, v interface{}) error {
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// loadHashes loads the file hashes from the given path and logs debug info using the provided logger.
func loadHashes(path string, logger *zap.Logger) (map[string]string, error) {
	hashes := make(map[string]string)
	logger.Debug("[DEBUG] Loading hashes from", zap.String("path", path))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Debug("[DEBUG] Hash file does not exist", zap.String("path", path))
			return hashes, nil
		}
		logger.Debug("[DEBUG] Error reading hash file", zap.Error(err))
		return nil, err
	}
	err = json.Unmarshal(data, &hashes)
	if err != nil {
		logger.Debug("[DEBUG] Error unmarshalling hash file", zap.Error(err))
		return nil, err
	}
	logger.Debug("[DEBUG] Loaded hashes", zap.Any("hashes", hashes))
	return hashes, nil
}

func saveHashes(path string, hashes map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(hashes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func calculateFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
