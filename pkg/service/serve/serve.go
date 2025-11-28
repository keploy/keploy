package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	agentSvc "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type serve struct {
	logger       *zap.Logger
	config       *config.Config
	proxy        agentSvc.Proxy
	mockDb       *mockdb.MockYaml
	isProxyReady atomic.Bool
}

func New(logger *zap.Logger, config *config.Config, proxy agentSvc.Proxy) Service {
	mockPath := filepath.Join(config.Path, "keploy", "mocks")
	mockDb := mockdb.New(logger, mockPath, "mocks")

	return &serve{
		logger: logger,
		config: config,
		proxy:  proxy,
		mockDb: mockDb,
	}
}

type StatusResponse struct {
	Status          string        `json:"status"`
	Port            uint32        `json:"port"`
	ProxyPort       uint32        `json:"proxy_port,omitempty"`
	TotalMocks      int           `json:"total_mocks"`
	FilteredMocks   int           `json:"filtered_mocks"`
	UnfilteredMocks int           `json:"unfiltered_mocks"`
	MockTypes       MockTypeCount `json:"mock_types"`
}

type MockTypeCount struct {
	HTTP     int `json:"http"`
	GRPC     int `json:"grpc"`
	MySQL    int `json:"mysql"`
	Postgres int `json:"postgres"`
	Redis    int `json:"redis"`
	Generic  int `json:"generic"`
}

func (s *serve) Start(ctx context.Context) error {
	s.logger.Info("Starting Keploy mock server in standalone mode")

	mockPath := filepath.Join(s.config.Path, "keploy", "mocks")
	if _, err := os.Stat(mockPath); os.IsNotExist(err) {
		return fmt.Errorf("mocks directory does not exist: %s. Please record mocks first", mockPath)
	}

	port := s.config.ServerPort
	if port == 0 {
		port = config.DefaultServePort
	}

	mocks, err := s.loadMocks(ctx)
	if err != nil {
		return fmt.Errorf("failed to load mocks: %w", err)
	}

	if len(mocks) == 0 {
		s.logger.Warn("No mocks found to serve. The server will start but will not serve any mocks.")
	}

	s.logger.Info("Loaded mocks from filesystem",
		zap.Int("total_mocks", len(mocks)),
		zap.Uint32("port", port))

	filteredMocks, unfilteredMocks := s.separateMocksByFilter(mocks)

	s.logger.Info("Separated mocks",
		zap.Int("filtered_mocks", len(filteredMocks)),
		zap.Int("unfiltered_mocks", len(unfilteredMocks)))

	mockOpts := models.OutgoingOptions{
		Rules:          s.config.BypassRules,
		MongoPassword:  s.config.Test.MongoPassword,
		SQLDelay:       time.Duration(s.config.Test.Delay) * time.Second,
		FallBackOnMiss: s.config.Test.FallBackOnMiss,
		Mocking:        true,
		NoiseConfig:    nil,
	}

	// Mock() and SetMocks() are synchronous setup calls, not long-running operations
	if err := s.proxy.Mock(ctx, mockOpts); err != nil {
		return fmt.Errorf("failed to configure proxy mock mode: %w", err)
	}

	if err := s.proxy.SetMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
		return fmt.Errorf("failed to set mocks on proxy: %w", err)
	}

	eg, gCtx := errgroup.WithContext(ctx)
	gCtx = context.WithValue(gCtx, models.ErrGroupKey, eg)

	server := s.createHTTPServer(port, mocks, filteredMocks, unfilteredMocks)

	eg.Go(func() error {
		proxyOpts := agentSvc.ProxyOptions{}
		if err := s.proxy.StartProxy(gCtx, proxyOpts); err != nil {
			return fmt.Errorf("failed to start proxy server: %w", err)
		}
		s.isProxyReady.Store(true)
		return nil
	})

	eg.Go(func() error {
		errCh := make(chan error, 1)
		go func() {
			s.logger.Info("Starting HTTP server", zap.Uint32("port", port))
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("http server failed: %w", err)
				return
			}
			errCh <- nil
		}()

		select {
		case err := <-errCh:
			return err
		case <-gCtx.Done():
			s.logger.Info("Shutting down mock server...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				utils.LogError(s.logger, err, "HTTP server shutdown failed")
			}
			return gCtx.Err()
		}
	})

	s.logger.Info("Mock server started successfully",
		zap.Uint32("port", port),
		zap.Uint32("proxy_port", s.config.ProxyPort),
		zap.String("health_endpoint", fmt.Sprintf("http://localhost:%d/health", port)),
		zap.String("status_endpoint", fmt.Sprintf("http://localhost:%d/status", port)))

	return eg.Wait()
}

func (s *serve) createHTTPServer(port uint32, mocks, filteredMocks, unfilteredMocks []*models.Mock) *http.Server {
	router := chi.NewRouter()

	router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if s.isProxyReady.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Proxy not ready"))
		}
	})

	router.Get("/status", func(w http.ResponseWriter, r *http.Request) {
		status := StatusResponse{
			Status:          "running",
			Port:            port,
			TotalMocks:      len(mocks),
			FilteredMocks:   len(filteredMocks),
			UnfilteredMocks: len(unfilteredMocks),
			MockTypes: MockTypeCount{
				HTTP:     s.countMocksByKind(mocks, models.HTTP),
				GRPC:     s.countMocksByKind(mocks, models.GRPC_EXPORT),
				MySQL:    s.countMocksByKind(mocks, models.MySQL),
				Postgres: s.countMocksByKind(mocks, models.Postgres),
				Redis:    s.countMocksByKind(mocks, models.REDIS),
				Generic:  s.countMocksByKind(mocks, models.GENERIC),
			},
		}
		if s.isProxyReady.Load() {
			status.ProxyPort = s.config.ProxyPort
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(status); err != nil {
			utils.LogError(s.logger, err, "failed to encode status response")
		}
	})

	return &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", port),
		Handler: router,
	}
}

func (s *serve) loadMocks(ctx context.Context) ([]*models.Mock, error) {
	var allMocks []*models.Mock

	testSets, err := s.getTestSets()
	if err != nil {
		return nil, fmt.Errorf("failed to get test sets: %w", err)
	}

	if len(testSets) == 0 {
		s.logger.Warn("No test sets found or specified")
		return allMocks, nil
	}

	s.logger.Info("Loading mocks from test sets", zap.Strings("test_sets", testSets))

	for _, testSet := range testSets {
		s.logger.Debug("Loading mocks from test set", zap.String("test_set", testSet))

		filteredMocks, err := s.mockDb.GetFilteredMocks(ctx, testSet, models.BaseTime, time.Now())
		if err != nil {
			s.logger.Warn("Failed to load filtered mocks",
				zap.String("test_set", testSet),
				zap.Error(err))
		} else {
			allMocks = append(allMocks, filteredMocks...)
		}

		unfilteredMocks, err := s.mockDb.GetUnFilteredMocks(ctx, testSet, models.BaseTime, time.Now())
		if err != nil {
			s.logger.Warn("Failed to load unfiltered mocks",
				zap.String("test_set", testSet),
				zap.Error(err))
		} else {
			allMocks = append(allMocks, unfilteredMocks...)
		}
	}

	return allMocks, nil
}

func (s *serve) getTestSets() ([]string, error) {
	if len(s.config.Test.SelectedTests) > 0 {
		var testSets []string
		for testSet := range s.config.Test.SelectedTests {
			testSets = append(testSets, testSet)
		}
		return testSets, nil
	}

	mockPath := filepath.Join(s.config.Path, "keploy", "mocks")

	if _, err := os.Stat(mockPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("mocks directory does not exist: %s", mockPath)
	}

	entries, err := os.ReadDir(mockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read mocks directory: %w", err)
	}

	var testSets []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			name := entry.Name()
			if strings.Contains(name, string(os.PathSeparator)) || name == ".." {
				s.logger.Warn("invalid test set name, skipping", zap.String("name", name))
				continue
			}
			testSets = append(testSets, name)
		}
	}

	return testSets, nil
}

func (s *serve) separateMocksByFilter(mocks []*models.Mock) ([]*models.Mock, []*models.Mock) {
	var filteredMocks, unfilteredMocks []*models.Mock

	for _, mock := range mocks {
		if isUnfiltered(mock) {
			unfilteredMocks = append(unfilteredMocks, mock)
		} else {
			filteredMocks = append(filteredMocks, mock)
		}
	}
	return filteredMocks, unfilteredMocks
}

func isUnfiltered(mock *models.Mock) bool {
	if mock.Spec.Metadata["type"] == "config" {
		return true
	}
	switch mock.Kind {
	case models.GENERIC, models.Postgres, models.HTTP, models.REDIS, models.MySQL:
		return true
	}
	return false
}

func (s *serve) countMocksByKind(mocks []*models.Mock, kind models.Kind) int {
	count := 0
	for _, mock := range mocks {
		if mock.Kind == kind {
			count++
		}
	}
	return count
}
