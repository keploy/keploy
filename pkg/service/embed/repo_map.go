package embed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
)

// SymbolInfo holds information about a single code symbol.
type SymbolInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // e.g., "Function", "Struct", "Method"
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// RepoMap stores a map of all symbols in the repository.
// The key is the symbol name, and the value is a list of definitions
// (since some symbols like methods can have the same name on different structs).
type RepoMap struct {
	Symbols map[string][]SymbolInfo `json:"symbols"`
	logger  *zap.Logger
	mu      sync.RWMutex
}

// NewRepoMap creates and initializes a new RepoMap.
func NewRepoMap(logger *zap.Logger) *RepoMap {
	return &RepoMap{
		Symbols: make(map[string][]SymbolInfo),
		logger:  logger,
	}
}

// AddSymbol adds a new symbol to the map in a thread-safe way.
func (rm *RepoMap) AddSymbol(info SymbolInfo) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.Symbols[info.Name] = append(rm.Symbols[info.Name], info)
}

// AddSymbols adds multiple symbols at once.
func (rm *RepoMap) AddSymbols(infos []SymbolInfo) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, info := range infos {
		rm.Symbols[info.Name] = append(rm.Symbols[info.Name], info)
	}
}

// Save serializes the RepoMap to a JSON file.
func (rm *RepoMap) Save(path string) error {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	data, err := json.MarshalIndent(rm.Symbols, "", "  ")
	if err != nil {
		rm.logger.Error("Failed to marshal RepoMap", zap.Error(err))
		return err
	}

	// Ensure the directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		rm.logger.Error("Failed to create directory for RepoMap", zap.String("path", dir), zap.Error(err))
		return err
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		rm.logger.Error("Failed to write RepoMap to file", zap.String("path", path), zap.Error(err))
		return err
	}

	rm.logger.Info("Successfully saved repository map", zap.String("path", path))
	return nil
}

// Load deserializes the RepoMap from a JSON file.
func (rm *RepoMap) Load(path string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rm.logger.Info("RepoMap file does not exist, starting with an empty map.", zap.String("path", path))
			rm.Symbols = make(map[string][]SymbolInfo)
			return nil
		}
		rm.logger.Error("Failed to read RepoMap file", zap.String("path", path), zap.Error(err))
		return err
	}

	if err := json.Unmarshal(data, &rm.Symbols); err != nil {
		rm.logger.Error("Failed to unmarshal RepoMap", zap.String("path", path), zap.Error(err))
		return err
	}

	rm.logger.Info("Successfully loaded repository map", zap.String("path", path))
	return nil
}