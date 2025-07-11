// Package golang implements a hybrid coverage service for Go applications.
package golang

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

const (
	controlSocketPath = "/tmp/coverage_control.sock"
	dataSocketPath    = "/tmp/keploy-coverage.sock"
	dedupFileName     = "dedupData.yaml"
)

// Service interface now correctly inherits from the base coverage.Service
// and includes methods for per-test-case coverage control.
type Service interface {
	coverage.Service
	StartCoverage(testID string)
	EndCoverage(testID string)
}

type Golang struct {
	ctx                context.Context
	logger             *zap.Logger
	reportDB           coverage.ReportDB
	cmd                string
	coverageReportPath string
	commandType        string

	g            *errgroup.Group
	gctx         context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	coverageData map[string][]byte
	dedupFileMu  sync.Mutex
	// listener is protected by the mutex to prevent data races.
	listener net.Listener
}

type DedupRecord struct {
	ID                  string           `json:"id" yaml:"id"`
	ExecutedLinesByFile map[string][]int `json:"executedLinesByFile" yaml:"executedLinesByFile"`
}

// New creates and initializes the hybrid Go coverage service.
func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd, coverageReportPath, commandType string) Service {
	g, gctx := errgroup.WithContext(ctx)
	gctx, cancel := context.WithCancel(gctx)

	cov := &Golang{
		ctx:                ctx,
		logger:             logger,
		reportDB:           reportDB,
		cmd:                cmd,
		coverageReportPath: coverageReportPath,
		commandType:        commandType,
		g:                  g,
		gctx:               gctx,
		cancel:             cancel,
		coverageData:       make(map[string][]byte),
	}

	if err := os.Remove(dedupFileName); err != nil && !os.IsNotExist(err) {
		logger.Warn("Failed to remove old dedupData.yaml file", zap.Error(err))
	}

	cov.g.Go(cov.startDataReceiver)
	return cov
}

// GetCoverage gracefully shuts down the listeners and calculates coverage.
func (g *Golang) GetCoverage() (models.TestCoverage, error) {
	g.mu.Lock()
	if g.listener != nil {
		// Closing the listener will unblock the Accept() call in the goroutine.
		if err := g.listener.Close(); err != nil {
			g.logger.Warn("Error closing listener", zap.Error(err))
		}
		g.listener = nil
	}
	g.mu.Unlock()

	// Cancel the context to signal all goroutines to stop.
	g.cancel()

	// Wait for the errgroup to finish.
	if err := g.g.Wait(); err != nil && err != context.Canceled {
		g.logger.Warn("Error during coverage service shutdown", zap.Error(err))
	}

	g.mu.Lock()
	dataReceived := len(g.coverageData) > 0
	g.mu.Unlock()

	if dataReceived {
		g.logger.Info("Coverage data received via socket. Calculating coverage using socket-based method.")
		return g.getSocketCoverage()
	}

	g.logger.Info("No coverage data received. Attempting to calculate coverage using legacy GOCOVERDIR method.")
	return g.getLegacyCoverage()
}

// startDataReceiver listens for coverage data from the instrumented app.
func (g *Golang) startDataReceiver() error {
	if err := os.RemoveAll(dataSocketPath); err != nil {
		g.logger.Debug("Could not remove old data socket, may not exist", zap.Error(err))
	}
	ln, err := net.Listen("unix", dataSocketPath)
	if err != nil {
		g.logger.Error("Failed to start coverage data receiver", zap.Error(err))
		return err
	}

	// FIX: Protect the assignment of the listener to prevent a data race.
	g.mu.Lock()
	g.listener = ln
	g.mu.Unlock()

	defer func() {
		ln.Close()
		g.mu.Lock()
		g.listener = nil
		g.mu.Unlock()
	}()

	for {
		// FIX: Set a deadline on Accept to allow periodic checks of the context.
		// This makes the shutdown more responsive.
		if uln, ok := ln.(*net.UnixListener); ok {
			uln.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := ln.Accept()
		if err != nil {
			// Check for timeout error, which is expected.
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				select {
				case <-g.gctx.Done(): // Context was canceled, so exit.
					return g.gctx.Err()
				default:
					continue // No cancellation, continue listening.
				}
			}
			// Check for the error indicating the listener was closed.
			if strings.Contains(err.Error(), "use of closed network connection") {
				return nil // Graceful shutdown.
			}
			g.logger.Warn("Error accepting data connection", zap.Error(err))
			continue
		}
		g.g.Go(func() error {
			g.handleDataConnection(conn)
			return nil
		})
	}
}

// handleDataConnection reads the JSON payload and stores it.
func (g *Golang) handleDataConnection(conn net.Conn) {
	defer conn.Close()
	data, err := io.ReadAll(conn)
	if err != nil {
		g.logger.Warn("Failed to read coverage data from socket", zap.Error(err))
		return
	}

	var record DedupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		g.logger.Warn("Received invalid JSON from coverage agent", zap.Error(err))
		return
	}

	if len(record.ExecutedLinesByFile) > 0 {
		g.mu.Lock()
		// Store the original data with the full ID for internal processing.
		g.coverageData[record.ID] = data
		g.mu.Unlock()

		// The record's ID is already in the correct "test-set/test-case" format.
		// Do not modify it. Pass it directly to the file writer.
		if err := g.appendToDedupFile(record); err != nil {
			g.logger.Error("Failed to write to dedupdata.yaml", zap.Error(err))
		}
	} else {
		g.logger.Debug("Received coverage report with no executed lines", zap.String("testID", record.ID))
	}
}

func (g *Golang) appendToDedupFile(record DedupRecord) error {
	g.dedupFileMu.Lock()
	defer g.dedupFileMu.Unlock()

	var existingRecords []DedupRecord
	yamlFile, err := os.ReadFile(dedupFileName)
	if err == nil {
		if err := yaml.Unmarshal(yamlFile, &existingRecords); err != nil {
			g.logger.Warn("failed to unmarshal existing dedup file, starting fresh", zap.Error(err))
			existingRecords = []DedupRecord{}
		}
	}

	existingRecords = append(existingRecords, record)

	newData, err := yaml.Marshal(&existingRecords)
	if err != nil {
		return fmt.Errorf("failed to marshal new dedup data: %w", err)
	}
	return os.WriteFile(dedupFileName, newData, 0644)
}

func (g *Golang) PreProcess(_ bool) (string, error) {
	if !checkForCoverFlag(g.logger, g.cmd) {
		return g.cmd, fmt.Errorf("the application binary was not built with coverage flags")
	}
	if utils.CmdType(g.commandType) == utils.Native {
		goCovPath, err := utils.SetCoveragePath(g.logger, g.coverageReportPath)
		if err != nil {
			g.logger.Warn("Failed to set go coverage path for legacy mode", zap.Error(err))
			return g.cmd, err
		}
		err = os.Setenv("GOCOVERDIR", goCovPath)
		if err != nil {
			g.logger.Warn("Failed to set GOCOVERDIR for legacy mode", zap.Error(err))
			return g.cmd, err
		}
	}
	g.logger.Info("Go coverage pre-processing complete. Will attempt socket-based coverage first.")
	return g.cmd, nil
}

func (g *Golang) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return g.reportDB.UpdateReport(g.ctx, testRunID, coverage)
}

func (g *Golang) StartCoverage(testID string) {
	g.sendCommand(fmt.Sprintf("START %s", testID))
}

func (g *Golang) EndCoverage(testID string) {
	g.sendCommand(fmt.Sprintf("END %s", testID))
	// FIX: Add a small delay to ensure the agent has time to send the data back
	// before the test run proceeds to the next test or finishes.
	time.Sleep(100 * time.Millisecond)
}

func (g *Golang) sendCommand(command string) {
	conn, err := net.Dial("unix", controlSocketPath)
	if err != nil {
		g.logger.Debug("Could not send command to coverage agent; app may not be instrumented.", zap.String("command", strings.Fields(command)[0]), zap.Error(err))
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(command + "\n"))
}

func (g *Golang) getSocketCoverage() (models.TestCoverage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	aggregatedCoveredLines := make(map[string]map[int]bool)
	type coveragePayload struct {
		ExecutedLinesByFile map[string][]int `json:"executedLinesByFile"`
	}

	for _, data := range g.coverageData {
		var payload coveragePayload
		if err := json.Unmarshal(data, &payload); err != nil {
			g.logger.Warn("failed to unmarshal coverage payload", zap.Error(err))
			continue
		}
		for file, lines := range payload.ExecutedLinesByFile {
			if _, ok := aggregatedCoveredLines[file]; !ok {
				aggregatedCoveredLines[file] = make(map[int]bool)
			}
			for _, line := range lines {
				aggregatedCoveredLines[file][line] = true
			}
		}
	}

	finalCoverage := models.TestCoverage{FileCov: make(map[string]string)}
	totalLines := 0
	totalCoveredLines := 0

	for file, linesSet := range aggregatedCoveredLines {
		content, err := os.ReadFile(file)
		if err != nil {
			g.logger.Warn("Could not read source file for line count", zap.String("file", file), zap.Error(err))
			continue
		}
		fileTotalLines := len(strings.Split(string(content), "\n"))
		fileCoveredLines := len(linesSet)
		totalLines += fileTotalLines
		totalCoveredLines += fileCoveredLines

		if fileTotalLines > 0 {
			covPercentage := float64(fileCoveredLines*100) / float64(fileTotalLines)
			finalCoverage.FileCov[file] = fmt.Sprintf("%.2f%%", covPercentage)
		}
	}

	if totalLines > 0 {
		finalCoverage.TotalCov = fmt.Sprintf("%.2f%%", (float64(totalCoveredLines*100) / float64(totalLines)))
	} else {
		finalCoverage.TotalCov = "0.00%"
	}
	finalCoverage.Loc = models.Loc{Total: totalLines, Covered: totalCoveredLines}
	return finalCoverage, nil
}

func (g *Golang) getLegacyCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}
	coverageDir := os.Getenv("GOCOVERDIR")
	if coverageDir == "" {
		g.logger.Warn("GOCOVERDIR environment variable not set. Skipping legacy coverage.")
		return testCov, nil
	}
	f, err := os.Open(coverageDir)
	if err != nil {
		utils.LogError(g.logger, err, "failed to open coverage directory, skipping coverage calculation")
		return testCov, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		utils.LogError(g.logger, err, fmt.Sprintf("no coverage files found in %s, skipping coverage calculation", coverageDir))
		return testCov, err
	}
	totalCoveragePath := filepath.Join(coverageDir, "total-coverage.txt")
	generateCovTxtCmd := exec.CommandContext(g.ctx, "go", "tool", "covdata", "textfmt", "-i="+coverageDir, "-o="+totalCoveragePath)
	_, err = generateCovTxtCmd.Output()
	if err != nil {
		return testCov, err
	}
	coveragePerFileTmp := make(map[string][]int)
	covdata, err := os.ReadFile(totalCoveragePath)
	if err != nil {
		return testCov, err
	}
	for idx, line := range strings.Split(string(covdata), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		lineFields := strings.Fields(line)
		malformedErrMsg := "go coverage file is malformed"
		if len(lineFields) == 3 {
			noOfLines, _ := strconv.Atoi(lineFields[1])
			coveredOrNot, _ := strconv.Atoi(lineFields[2])
			i := strings.Index(line, ":")
			var filename string
			if i > 0 {
				filename = line[:i]
			} else {
				return testCov, fmt.Errorf("%s at line %d", malformedErrMsg, idx)
			}
			if _, ok := coveragePerFileTmp[filename]; !ok {
				coveragePerFileTmp[filename] = make([]int, 2)
			}
			coveragePerFileTmp[filename][0] += noOfLines
			if coveredOrNot != 0 {
				coveragePerFileTmp[filename][1] += noOfLines
			}
		} else {
			return testCov, fmt.Errorf("%s at %d", malformedErrMsg, idx)
		}
	}
	totalLines := 0
	totalCoveredLines := 0
	for filename, lines := range coveragePerFileTmp {
		totalLines += lines[0]
		totalCoveredLines += lines[1]
		if lines[0] > 0 {
			covPercentage := float64(lines[1]*100) / float64(lines[0])
			testCov.FileCov[filename] = fmt.Sprintf("%.2f%%", covPercentage)
		}
	}
	if totalLines > 0 {
		testCov.TotalCov = fmt.Sprintf("%.2f%%", float64(totalCoveredLines*100)/float64(totalLines))
	} else {
		testCov.TotalCov = "0.00%"
	}
	testCov.Loc = models.Loc{Total: totalLines, Covered: totalCoveredLines}
	return testCov, nil
}
