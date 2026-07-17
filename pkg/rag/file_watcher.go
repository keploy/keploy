package rag

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

type DynamicIndexer struct {
	ragSystem  *RAGSystem
	watcher    *fsnotify.Watcher
	watchPaths []string
}

func NewDynamicIndexer(ragSystem *RAGSystem, paths []string) *DynamicIndexer {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	return &DynamicIndexer{
		ragSystem:  ragSystem,
		watcher:    watcher,
		watchPaths: paths,
	}
}

func (di *DynamicIndexer) StartWatching() {
	go func() {
		for {
			select {
			case event, ok := <-di.watcher.Events:
				if !ok {
					return
				}

				if event.Op&fsnotify.Write == fsnotify.Write {
					if strings.HasSuffix(event.Name, ".go") {
						log.Printf("File modified: %s - Re-indexing...", event.Name)
						if err := di.ragSystem.IndexGoFile(event.Name); err != nil {
							log.Printf("Error re-indexing %s: %v", event.Name, err)
						} else {
							log.Printf("Successfully re-indexed %s", event.Name)
						}
					}
				}

				if event.Op&fsnotify.Create == fsnotify.Create {
					if strings.HasSuffix(event.Name, ".go") {
						log.Printf("New file created: %s - Indexing...", event.Name)
						if err := di.ragSystem.IndexGoFile(event.Name); err != nil {
							log.Printf("Error indexing new file %s: %v", event.Name, err)
						}
					}
				}

			case err, ok := <-di.watcher.Errors:
				if !ok {
					return
				}
				log.Printf("Watcher error: %v", err)
			}
		}
	}()

	// Add paths to watcher
	for _, path := range di.watchPaths {
		err := filepath.Walk(path, func(walkPath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Skip hidden directories and files
			if strings.HasPrefix(filepath.Base(walkPath), ".") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Skip vendor directory
			if strings.Contains(walkPath, "vendor/") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if info.IsDir() {
				log.Printf("Watching directory: %s", walkPath)
				return di.watcher.Add(walkPath)
			}
			return nil
		})

		if err != nil {
			log.Printf("Error setting up watcher for %s: %v", path, err)
		}
	}
}

func (di *DynamicIndexer) Stop() {
	if di.watcher != nil {
		di.watcher.Close()
	}
}
