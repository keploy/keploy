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
	listener     net.Listener
	// socketCoverageDetected tracks if we've successfully received socket-based coverage data
	socketCoverageDetected bool
}

type DedupRecord struct {
	ID                  string           `json:"id"`
	ExecutedLinesByFile map[string][]int `json:"executedLinesByFile"`
}

// CoverageReport defines the nested structure for the dedupData.yaml file.
// It maps a test set ID to its collection of test cases.
type CoverageReport map[string]TestSetData

// TestSetData maps a test case ID to its coverage information, which itself is a
// map of file paths to the lines executed in that file.
type TestSetData map[string]map[string][]int

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

	cov.g.Go(cov.startDataReceiver)
	return cov
}

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

	// Wait for the errgroup to finish. This ensures that any pending data
	// from the socket has been fully processed and written to dedupData.yaml.
	if err := g.g.Wait(); err != nil && err != context.Canceled {
		g.logger.Warn("Error during coverage service shutdown", zap.Error(err))
	}

	g.mu.Lock()
	dataReceived := len(g.coverageData) > 0
	g.mu.Unlock()

	if dataReceived {
		g.logger.Info("Coverage data received via socket, 'dedupData.yaml' has been generated.")
	} else {
		g.logger.Info("No coverage data received via socket.")
	}

	g.logger.Info("Calculating final coverage report using the GOCOVERDIR method.")
	return g.getGoCoverDirCoverage()
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
		if uln, ok := ln.(*net.UnixListener); ok {
			uln.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := ln.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				select {
				case <-g.gctx.Done():
					return g.gctx.Err()
				default:
					continue
				}
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return nil
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
		// Delete the old dedupData.yaml file only when we first receive socket-based coverage data
		// This confirms that the user has imported the SDK and socket connection is working
		if !g.socketCoverageDetected {
			if err := os.Remove(dedupFileName); err != nil && !os.IsNotExist(err) {
				g.logger.Warn("Failed to remove old dedupData.yaml file", zap.Error(err))
				return
			} else {
				g.logger.Info("Removed old dedupData.yaml file as socket-based coverage is now active")
			}
			g.socketCoverageDetected = true
		}
		g.coverageData[record.ID] = data
		g.mu.Unlock()

		// The record's ID is already in the correct "test-set/test-case" format.
		// Do not modify it. Pass it directly to the file writer.
		if err := g.appendToDedupFile(record); err != nil {
			g.logger.Error("Failed to write to dedupData.yaml", zap.Error(err))
		}
	} else {
		g.logger.Debug("Received coverage report with no executed lines", zap.String("testID", record.ID))
	}
}

func (g *Golang) appendToDedupFile(record DedupRecord) error {
	g.dedupFileMu.Lock()
	defer g.dedupFileMu.Unlock()

	parts := strings.SplitN(record.ID, "/", 2)
	if len(parts) != 2 {
		g.logger.Warn("Received malformed test ID, skipping dedup file update", zap.String("testID", record.ID))
		return nil
	}
	testSetID, testCaseID := parts[0], parts[1]

	report := make(CoverageReport)
	yamlFile, err := os.ReadFile(dedupFileName)
	// If the file exists and is not empty, try to unmarshal it.
	if err == nil && len(yamlFile) > 0 {
		if err := yaml.Unmarshal(yamlFile, &report); err != nil {
			g.logger.Warn("failed to unmarshal existing dedup file, starting fresh", zap.Error(err))
			// Reset report to ensure a clean state if unmarshaling fails.
			report = make(CoverageReport)
		}
	}

	// Ensure the test set map exists.
	if _, ok := report[testSetID]; !ok {
		report[testSetID] = make(TestSetData)
	}

	// Add or update the test case's coverage data.
	report[testSetID][testCaseID] = record.ExecutedLinesByFile

	// Marshal the updated report back to YAML.
	newData, err := yaml.Marshal(&report)
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

func (g *Golang) getGoCoverDirCoverage() (models.TestCoverage, error) {
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
		if strings.HasPrefix(line, "mode") || line == "" {
			continue
		}
		lineFields := strings.Fields(line)
		malformedErrMsg := "go coverage file is malformed"
		if len(lineFields) == 3 {
			noOfLines, err := strconv.Atoi(lineFields[1])
			if err != nil {
				return testCov, err
			}
			coveredOrNot, err := strconv.Atoi(lineFields[2])
			if err != nil {
				return testCov, err
			}
			i := strings.Index(line, ":")
			var filename string
			if i > 0 {
				filename = line[:i]
			} else {
				return testCov, fmt.Errorf("%s at %d", malformedErrMsg, idx)
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
