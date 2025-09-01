package sanitize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type service struct {
	logger *zap.Logger
	testDB TestDB
	config *config.Config
}

func New(logger *zap.Logger, testDB TestDB, cfg *config.Config) Service {
	return &service{
		logger: logger,
		testDB: testDB,
		config: cfg,
	}
}

func (s *service) Sanitize(ctx context.Context) error {
	s.logger.Info("Starting sanitize process...")

	// From CLI: SelectedTests
	testSets := s.extractTestSetIDs()
	if len(testSets) == 0 {
		var err error
		testSets, err = s.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			s.logger.Error("Failed to get test sets", zap.Error(err))
			return fmt.Errorf("failed to get test sets: %w", err)
		}
		s.logger.Info("No test sets specified, processing all test sets", zap.Int("count", len(testSets)))
	} else {
		s.logger.Info("Processing specified test sets", zap.Strings("testSets", testSets))
	}

	for _, testSetID := range testSets {
		// keploy/<testSetID>
		testSetDir, err := s.locateTestSetDir(testSetID)
		if err != nil {
			s.logger.Error("Could not locate test set directory; skipping",
				zap.String("testSetID", testSetID), zap.Error(err))
			continue
		}
		s.logger.Info("Sanitizing test set",
			zap.String("testSetID", testSetID),
			zap.String("dir", testSetDir))

		if err := s.sanitizeTestSetDir(testSetDir); err != nil {
			s.logger.Error("Sanitize failed for test set",
				zap.String("testSetID", testSetID),
				zap.String("dir", testSetDir),
				zap.Error(err))
			continue
		}
	}

	s.logger.Info("Sanitize process completed")
	return nil
}

func (s *service) extractTestSetIDs() []string {
	var ids []string
	if s.config == nil || s.config.Test.SelectedTests == nil {
		return ids
	}
	for ts := range s.config.Test.SelectedTests {
		ids = append(ids, ts)
	}
	return ids
}

// locateTestSetDir resolves ./keploy/<testSetID> by walking upward from CWD
// (so you can run the server in a subdir and it still finds the repo root).
func (s *service) locateTestSetDir(testSetID string) (string, error) {
	// Fast path: ./keploy/<id>
	if p := filepath.Join(".", "keploy", testSetID); isDir(p) {
		return p, nil
	}
	// Walk upwards to find a keploy root
	dir, _ := os.Getwd()
	for {
		try := filepath.Join(dir, "keploy", testSetID)
		if isDir(try) {
			return try, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("keploy/%s not found (starting from CWD)", testSetID)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (s *service) sanitizeTestSetDir(testSetDir string) error {
	// Aggregate secrets across ALL files in this test set
	aggSecrets := map[string]string{}

	testsDir := filepath.Join(testSetDir, "tests")
	var files []string

	// Prefer keploy/<set>/tests/*.yaml
	if isDir(testsDir) {
		ents, err := os.ReadDir(testsDir)
		if err != nil {
			return fmt.Errorf("read tests dir: %w", err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
				continue
			}
			files = append(files, filepath.Join(testsDir, name))
		}
	} else {
		// Fallback: scan keploy/<set> (will skip mocks.yaml below anyway)
		ents, err := os.ReadDir(testSetDir)
		if err != nil {
			return fmt.Errorf("read test-set dir: %w", err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
				continue
			}
			files = append(files, filepath.Join(testSetDir, name))
		}
	}

	// Filter out files we never want to touch
	out := files[:0]
	for _, f := range files {
		l := strings.ToLower(filepath.Base(f))
		if l == "mocks.yaml" || l == "secret.yaml" || strings.HasSuffix(l, "-report.yaml") {
			continue
		}
		out = append(out, f)
	}
	files = out

	if len(files) == 0 {
		// Not an error — just nothing to sanitize
		return nil
	}

	for _, f := range files {
		if err := SanitizeFileInPlace(f, aggSecrets); err != nil {
			// Continue to next file; log the failure
			s.logger.Error("Failed to sanitize file", zap.String("file", f), zap.Error(err))
			continue
		}
	}

	// Write/overwrite keploy/<set>/secret.yaml
	secretPath := filepath.Join(testSetDir, "secret.yaml")
	if err := WriteSecretsYAML(secretPath, aggSecrets); err != nil {
		return fmt.Errorf("write secret.yaml: %w", err)
	}
	s.logger.Info("Wrote secret.yaml", zap.String("path", secretPath))
	return nil
}
