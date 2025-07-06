package load

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

// Exporter Load Test Token, it contains unique identifier for the load test with the load test information.
type LTToken struct {
	ID          string                `json:"id"`
	URL         string                `json:"url"`
	Title       string                `json:"title"`
	CreatedAt   time.Time             `json:"created_at"`
	Description string                `json:"description"`
	LoadOptions testsuite.LoadOptions `json:"load_options"`
}

type Exporter struct {
	config       *config.Config
	logger       *zap.Logger
	dashboardURL string
	ltToken      *LTToken
	vusReport    []VUReport
	mu           sync.RWMutex
}

// Exporter is responsible for providing endpoint to export the load test report in JSON format.
func NewExporter(cfg *config.Config, logger *zap.Logger, vus int, ltToken *LTToken) *Exporter {
	return &Exporter{
		config:       cfg,
		logger:       logger,
		dashboardURL: "http://localhost:3000",
		ltToken:      ltToken,
		vusReport:    make([]VUReport, vus),
	}
}

func (e *Exporter) GetMetrics(vuReport *VUReport) {
	e.mu.Lock()
	e.vusReport[vuReport.VUID] = *vuReport
	e.mu.Unlock()
	e.logger.Debug("VU Report collected", zap.Int("VUID", vuReport.VUID))
}

func (e *Exporter) StartServer(ctx context.Context) error {
	r := mux.NewRouter()
	r.HandleFunc("/metrics", e.metricsHandler).Methods("GET")

	server := &http.Server{
		Addr:    ":9090",
		Handler: r,
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				e.logger.Error("Metrics server panicked", zap.Any("recover", r))
			}
		}()
		e.logger.Info("Metrics server starting on :9090")
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			e.logger.Error("Failed to start metrics server", zap.Error(err))
		}
	}()

	go func() {
		<-ctx.Done()
		e.logger.Info("Shutting down metrics server...")
		// wait 5 seconds for the server to shutdown gracefully
		ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctxShutdown); err != nil {
			e.logger.Error("Failed to shutdown metrics server", zap.Error(err))
		}
	}()

	return nil
}

func (e *Exporter) metricsHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Set("Access-Control-Allow-Origin", e.dashboardURL)
	res.Header().Set("Access-Control-Allow-Methods", "GET")
	res.Header().Set("Content-Type", "application/json")

	e.mu.RLock()
	vusReportCopy := make([]VUReport, len(e.vusReport))
	copy(vusReportCopy, e.vusReport)
	e.mu.RUnlock()

	if len(vusReportCopy) == 0 {
		e.logger.Warn("No VU reports available")
		res.WriteHeader(http.StatusNoContent)
		return
	}
	encoder := json.NewEncoder(res)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(vusReportCopy)
	if err != nil {
		e.logger.Error("Failed to encode VU reports", zap.Error(err))
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (e *Exporter) ExportLoadTestToken() {
	tokenURL := e.dashboardURL + "/api/load"

	tokenData, err := json.Marshal(e.ltToken)
	if err != nil {
		e.logger.Error("Failed to marshal LTToken", zap.Error(err))
		return
	}

	req, err := http.NewRequest("POST", tokenURL, bytes.NewBuffer(tokenData))
	if err != nil {
		e.logger.Error("Failed to create request for LTToken", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		e.logger.Error("Failed to send LTToken to dashboard", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		e.logger.Error("Failed to send LTToken to dashboard", zap.Int("statusCode", resp.StatusCode))
		return
	}
}
