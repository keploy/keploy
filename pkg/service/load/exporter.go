package load

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type Exporter struct {
	config    *config.Config
	logger    *zap.Logger
	vusReport []VUReport
}

// Exporter is responsible for providing endpoint to export the load test report in JSON format.
func NewExporter(cfg *config.Config, logger *zap.Logger, vus int) *Exporter {
	return &Exporter{
		config:    cfg,
		logger:    logger,
		vusReport: make([]VUReport, vus),
	}
}

func (e *Exporter) GetMetrics(vuReport *VUReport) {
	e.vusReport[vuReport.VUID] = *vuReport
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
	res.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
	res.Header().Set("Access-Control-Allow-Methods", "GET")

	res.Header().Set("Content-Type", "application/json")
	if len(e.vusReport) == 0 {
		e.logger.Warn("No VU reports available")
		res.WriteHeader(http.StatusNoContent)
		return
	}
	encoder := json.NewEncoder(res)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(e.vusReport)
	if err != nil {
		e.logger.Error("Failed to encode VU reports", zap.Error(err))
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
}
