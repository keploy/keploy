package vectorstore

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.keploy.io/server/v2/pkg/service/embedding"
	"go.uber.org/zap"
)

type FileWatcher struct {
	watcher          *fsnotify.Watcher
	logger           *zap.Logger
	scanner          *CodeScanner
	milvusStore      *MilvusStore
	embeddingService *embedding.EmbeddingService
	ignorePatterns   []string
	debounceTime     time.Duration
	changes          map[string]time.Time
	stopCh           chan struct{}
}

func NewFileWatcher(
	logger *zap.Logger,
	milvusStore *MilvusStore,
	embeddingService *embedding.EmbeddingService,
	ignorePatterns []string,
) (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	scanner := NewCodeScanner(logger, milvusStore, embeddingService)

	return &FileWatcher{
		watcher:          watcher,
		logger:           logger,
		scanner:          scanner,
		milvusStore:      milvusStore,
		embeddingService: embeddingService,
		ignorePatterns:   ignorePatterns,
		debounceTime:     500 * time.Millisecond,
		changes:          make(map[string]time.Time),
		stopCh:           make(chan struct{}),
	}, nil
}

func (fw *FileWatcher) WatchDirectory(ctx context.Context, dir string) error {
	// Add the directory and all subdirectories to the watcher
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}

		// Check if directory should be ignored
		for _, pattern := range fw.ignorePatterns {
			matched, err := filepath.Match(pattern, info.Name())
			if err != nil {
				fw.logger.Debug("Error matching pattern", zap.String("pattern", pattern), zap.Error(err))
				continue
			}
			if matched {
				return filepath.SkipDir
			}
		}

		return fw.watcher.Add(path)
	})

	if err != nil {
		return err
	}

	// Start the watcher in a goroutine
	go fw.watch(ctx)

	return nil
}

func (fw *FileWatcher) watch(ctx context.Context) {
	ticker := time.NewTicker(fw.debounceTime)
	defer ticker.Stop()

	for {
		select {
		case <-fw.stopCh:
			return
		case <-ctx.Done():
			return
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}

			// Only process create and write events
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				// Skip directories
				fileInfo, err := os.Stat(event.Name)
				if err != nil || fileInfo.IsDir() {
					continue
				}

				fw.changes[event.Name] = time.Now()
			}
		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			fw.logger.Error("Watcher error", zap.Error(err))
		case <-ticker.C:
			// Process batched changes
			if len(fw.changes) > 0 {
				now := time.Now()
				filesToProcess := make([]string, 0, len(fw.changes))

				// Collect files that haven't changed recently
				for file, lastChange := range fw.changes {
					if now.Sub(lastChange) >= fw.debounceTime {
						filesToProcess = append(filesToProcess, file)
						delete(fw.changes, file)
					}
				}

				// Process files in batch
				if len(filesToProcess) > 0 {
					go fw.processFiles(ctx, filesToProcess)
				}
			}
		}
	}
}

func (fw *FileWatcher) processFiles(ctx context.Context, files []string) {
	for _, file := range files {
		ext := filepath.Ext(file)
		lang, ok := fw.scanner.languageMap[ext]
		if !ok {
			// Not a code file we're interested in
			continue
		}

		// Read file content
		content, err := os.ReadFile(file)
		if err != nil {
			fw.logger.Debug("Error reading file", zap.String("path", file), zap.Error(err))
			continue
		}

		// Skip empty files
		if len(content) == 0 {
			continue
		}

		// Generate embedding
		embedding, err := fw.embeddingService.GenerateEmbedding(ctx, string(content))
		if err != nil {
			fw.logger.Debug("Error generating embedding", zap.String("path", file), zap.Error(err))
			continue
		}

		// Store in Milvus
		err = fw.milvusStore.IndexCode(ctx, file, lang, string(content), embedding)
		if err != nil {
			fw.logger.Debug("Error indexing code", zap.String("path", file), zap.Error(err))
			continue
		}

		fw.logger.Debug("Indexed file", zap.String("path", file))
	}
}

func (fw *FileWatcher) Stop() {
	close(fw.stopCh)
	fw.watcher.Close()
}
