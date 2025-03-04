package vectorstore

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"go.keploy.io/server/v2/pkg/service/embedding"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type CodeScanner struct {
	logger           *zap.Logger
	milvusStore      *MilvusStore
	embeddingService *embedding.EmbeddingService
	languageMap      map[string]string
}

func NewCodeScanner(logger *zap.Logger, milvusStore *MilvusStore, embeddingService *embedding.EmbeddingService) *CodeScanner {
	// Initialize language map from the TOML file
	langMap := make(map[string]string)

	// Try to load language map from the TOML file
	var langConfig map[string][]string
	tomlFile := filepath.Join("pkg", "service", "utgen", "assets", "language.toml")

	_, err := toml.DecodeFile(tomlFile, &langConfig)
	if err != nil {
		logger.Warn("Failed to load language.toml, using default language map", zap.Error(err))

		// Fallback to hardcoded defaults
		langMap[".go"] = "go"
		langMap[".js"] = "javascript"
		langMap[".ts"] = "typescript"
		langMap[".py"] = "python"
		langMap[".java"] = "java"
		// Add more defaults...
	} else {
		// Convert TOML format to our extension->language map
		for language, extensions := range langConfig {
			for _, ext := range extensions {
				langMap[ext] = language
			}
		}
		logger.Debug("Loaded language map", zap.Int("count", len(langMap)))
	}

	return &CodeScanner{
		logger:           logger,
		milvusStore:      milvusStore,
		embeddingService: embeddingService,
		languageMap:      langMap,
	}
}

func (s *CodeScanner) ScanDirectory(ctx context.Context, dirPath string, ignorePatterns []string) error {
	startTime := time.Now()
	fileCount := 0

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			// Check if directory should be ignored
			for _, pattern := range ignorePatterns {
				matched, err := filepath.Match(pattern, info.Name())
				if err != nil {
					s.logger.Debug("Error matching pattern", zap.String("pattern", pattern), zap.Error(err))
					continue
				}
				if matched {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Check if file should be processed
		ext := filepath.Ext(path)
		lang, ok := s.languageMap[ext]
		if !ok {
			// Not a code file we're interested in
			return nil
		}

		// Read file content
		content, err := os.ReadFile(path)
		if err != nil {
			s.logger.Debug("Error reading file", zap.String("path", path), zap.Error(err))
			return nil // Skip this file but continue processing
		}

		// Skip empty files
		if len(content) == 0 {
			return nil
		}

		// Generate embedding
		embedding, err := s.embeddingService.GenerateEmbedding(ctx, string(content))
		if err != nil {
			s.logger.Debug("Error generating embedding", zap.String("path", path), zap.Error(err))
			return nil // Skip this file but continue processing
		}

		// Store in Milvus
		err = s.milvusStore.IndexCode(ctx, path, lang, string(content), embedding)
		if err != nil {
			s.logger.Debug("Error indexing code", zap.String("path", path), zap.Error(err))
			return nil // Skip this file but continue processing
		}

		fileCount++
		return nil
	})

	if err != nil {
		utils.LogError(s.logger, err, "error scanning directory")
		return err
	}

	duration := time.Since(startTime)
	s.logger.Info("Indexed files",
		zap.Int("count", fileCount),
		zap.String("duration", duration.String()),
		zap.String("directory", dirPath))

	return nil
}
