package load

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
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

func (e *Exporter) GetMetrics(vuReport VUReport) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vusReport[vuReport.VUID] = vuReport
	e.logger.Debug("VU Report collected", zap.Int("VUID", vuReport.VUID))
}

func (e *Exporter) StartServer(ctx context.Context) error {
	r := mux.NewRouter()
	r.HandleFunc("/metrics", e.metricsHandler).Methods("GET")

	port := 9090
	portOK := false
	for !portOK {
		listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			e.logger.Warn("Failed to listen on port", zap.String("port", strconv.Itoa(port)), zap.Error(err))
			port++
			continue
		}
		portOK = true
		listener.Close()
	}

	server := &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: r,
	}

	e.ltToken.URL = "http://localhost:" + strconv.Itoa(port) + "/metrics"

	go func() {
		defer func() {
			if r := recover(); r != nil {
				e.logger.Error("Metrics server panicked", zap.Any("recover", r))
			}
		}()
		e.logger.Info("Metrics server starting on port", zap.Int("port", port))
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			e.logger.Error("Failed to start metrics server", zap.Error(err))
		}
	}()

	go func() {
		<-ctx.Done()
		e.logger.Info("Shutting down metrics server...")
		// wait 1 second for the server to shutdown gracefully
		ctxShutdown, cancel := context.WithTimeout(context.Background(), 1*time.Second)
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

	// vusReportCopy := make([]VUReport, len(e.vusReport))
	// for i, report := range e.vusReport {
	// 	vusReportCopy[i].VUID = report.VUID
	// 	vusReportCopy[i].TSExecCount = report.TSExecCount
	// 	vusReportCopy[i].TSExecFailure = report.TSExecFailure
	// 	vusReportCopy[i].TSExecTime = make([]time.Duration, len(report.TSExecTime))
	// 	copy(vusReportCopy[i].TSExecTime, report.TSExecTime)
	// 	vusReportCopy[i].Steps = make([]StepReport, len(report.Steps))
	// 	for j, step := range report.Steps {
	// 		vusReportCopy[i].Steps[j].StepName = step.StepName
	// 		vusReportCopy[i].Steps[j].StepCount = step.StepCount
	// 		vusReportCopy[i].Steps[j].StepFailure = step.StepFailure
	// 		vusReportCopy[i].Steps[j].StepResponseTime = make([]time.Duration, len(step.StepResponseTime))
	// 		copy(vusReportCopy[i].Steps[j].StepResponseTime, step.StepResponseTime)
	// 		vusReportCopy[i].Steps[j].StepBytesIn = step.StepBytesIn
	// 		vusReportCopy[i].Steps[j].StepBytesOut = step.StepBytesOut
	// 	}
	// }

	encoder := json.NewEncoder(res)
	encoder.SetEscapeHTML(false)
	e.mu.RLock()
	err := encoder.Encode(e.vusReport)
	defer e.mu.RUnlock()
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
