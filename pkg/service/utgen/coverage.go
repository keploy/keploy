package utgen

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
)

// CoverageProcessor handles the processing of coverage reports
type CoverageProcessor struct {
	ReportPath string
	SrcPath    string
	Format     string
}

// NewCoverageProcessor initializes a CoverageProcessor object
func NewCoverageProcessor(reportPath, srcpath, format string) *CoverageProcessor {
	return &CoverageProcessor{
		ReportPath: reportPath,
		SrcPath:    srcpath,
		Format:     format,
	}
}

// ProcessCoverageReport verifies the report and parses it based on its type
func (cp *CoverageProcessor) ProcessCoverageReport(
	latestTime int64,
) (*models.CoverageResult, error) {
	err := cp.VerifyReportUpdate(latestTime)
	if err != nil {
		return nil, err
	}
	return cp.ParseCoverageReport()
}

// VerifyReportUpdate verifies the coverage report's existence and update time
func (cp *CoverageProcessor) VerifyReportUpdate(latestTime int64) error {
	if _, err := os.Stat(cp.ReportPath); os.IsNotExist(err) {
		return fmt.Errorf("fatal: coverage report \"%s\" was not generated", cp.ReportPath)
	}

	fileInfo, err := os.Stat(cp.ReportPath)
	if err != nil {
		return err
	}
	fileModTimeMs := fileInfo.ModTime().UnixNano() / int64(time.Millisecond)

	if fileModTimeMs < latestTime {
		time.Sleep(2 * time.Second)
		fileInfo, err = os.Stat(cp.ReportPath)
		if err != nil {
			return err
		}
		fileModTimeMs = fileInfo.ModTime().UnixNano() / int64(time.Millisecond)
		if fileModTimeMs < latestTime {
			return fmt.Errorf(
				"fatal: the coverage report file was not updated after the test command. file_mod_time_ms: %d, time_of_test_command: %d",
				fileModTimeMs,
				latestTime,
			)
		}
	}
	return nil
}

// ParseCoverageReport parses the coverage report based on its type
func (cp *CoverageProcessor) ParseCoverageReport() (*models.CoverageResult, error) {
	switch cp.Format {
	case "cobertura":
		return cp.ParseCoverageReportCobertura()
	case "jacoco":
		return cp.ParseCoverageReportJacoco()
	case "lcov":
		return nil, fmt.Errorf("parsing for %s coverage reports is not implemented yet", cp.Format)
	default:
		return nil, fmt.Errorf("unsupported coverage report type: %s", cp.Format)
	}
}

func (cp *CoverageProcessor) ParseCoverageReportCobertura() (*models.CoverageResult, error) {
	filesToCover := make([]string, 0)
	// Open the XML file
	xmlFile, err := os.Open(cp.ReportPath)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := xmlFile.Close(); err != nil {
			return
		}
	}()

	// Decode the XML file into a Coverage struct
	var cov models.Cobertura
	if err := xml.NewDecoder(xmlFile).Decode(&cov); err != nil {
		return nil, err
	}

	// Find coverage for the specified file
	var linesCovered, linesMissed []int
	var totalLines, coveredLines int
	var filteredClasses []models.Class
	for _, pkg := range cov.Packages {
		for _, cls := range pkg.Classes {
			if cp.SrcPath == "." {
				filesToCover = append(filesToCover, cls.FileName)
			}
			if strings.HasSuffix(cls.FileName, cp.SrcPath) {
				for _, line := range cls.Lines {
					totalLines++
					if line.Hits > 0 {
						coveredLines++
						linesCovered = append(linesCovered, line.Number)
					} else {
						linesMissed = append(linesMissed, line.Number)
					}
				}
				filteredClasses = append(filteredClasses, cls)
				break
			}
		}
	}

	var coveragePercentage float64
	if totalLines > 0 {
		coveragePercentage = float64(len(linesCovered)) / float64(totalLines)
	}

	// Reconstruct the coverage report with only the filtered class
	filteredCov := models.Cobertura{
		Packages: []models.Package{
			{
				Classes: filteredClasses,
			},
		},
	}

	// Encode the filtered coverage report to XML
	var filteredBuf bytes.Buffer
	xmlEncoder := xml.NewEncoder(&filteredBuf)
	xmlEncoder.Indent("", "  ")
	if err := xmlEncoder.Encode(filteredCov); err != nil {
		return nil, err
	}

	coverageResult := &models.CoverageResult{
		LinesCovered:  linesCovered,
		LinesMissed:   linesMissed,
		Coverage:      coveragePercentage,
		Files:         filesToCover,
		ReportContent: filteredBuf.String(),
	}

	return coverageResult, nil
}

func (cp *CoverageProcessor) ParseCoverageReportJacoco() (*models.CoverageResult, error) {
	filesToCover := make([]string, 0)
	// Open the XML file
	xmlFile, err := os.Open(cp.ReportPath)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := xmlFile.Close(); err != nil {
			return
		}
	}()

	// Decode the XML file into a Coverage struct
	var jacoco models.Jacoco
	if err := xml.NewDecoder(xmlFile).Decode(&jacoco); err != nil {
		return nil, err
	}

	// Find coverage for the specified file
	var linesCovered, linesMissed []int
	var totalLines, coveredLines int
	var filteredSourceFiles []models.JacocoSourceFile

	for _, pkg := range jacoco.Packages {
		for _, src := range pkg.SourceFiles {
			if cp.SrcPath == "." {
				filesToCover = append(filesToCover, src.Name)
			}
			if strings.HasSuffix(src.Name, cp.SrcPath) {
				for _, line := range src.Lines {
					totalLines++
					missedInstructions, errMissed := strconv.Atoi(line.MissedInstructions)
					coveredInstructions, errCovered := strconv.Atoi(line.CoveredInstructions)
					if errMissed != nil || errCovered != nil {
						// Handle conversion error
						continue
					}
					if coveredInstructions > 0 {
						coveredLines++
						lineNumber, err := strconv.Atoi(line.Number)
						if err == nil {
							linesCovered = append(linesCovered, lineNumber)
						}
					} else if missedInstructions > 0 {
						// Use missedInstructions to check if a line has any missed instructions
						lineNumber, err := strconv.Atoi(line.Number)
						if err == nil {
							linesMissed = append(linesMissed, lineNumber)
						}
					}
				}
				filteredSourceFiles = append(filteredSourceFiles, src)
				break
			}
		}
	}
	var coveragePercentage float64
	if totalLines > 0 {
		coveragePercentage = float64(len(linesCovered)) / float64(totalLines)
	}

	// Reconstruct the coverage report with only the filtered class
	filteredCov := models.Jacoco{
		Packages: []models.JacocoPackage{
			{
				SourceFiles: filteredSourceFiles,
			},
		},
	}

	// Encode the filtered coverage report to XML
	var filteredBuf bytes.Buffer
	xmlEncoder := xml.NewEncoder(&filteredBuf)
	xmlEncoder.Indent("", "  ")
	if err := xmlEncoder.Encode(filteredCov); err != nil {
		return nil, err
	}

	coverageResult := &models.CoverageResult{
		LinesCovered:  linesCovered,
		LinesMissed:   linesMissed,
		Coverage:      coveragePercentage,
		Files:         filesToCover,
		ReportContent: filteredBuf.String(),
	}

	return coverageResult, nil
}

func sendCoverageUpdate(
	ctx context.Context,
	auth service.Auth,
	apiBase string,
	logger *zap.Logger,
	sessionID string,
	oldCoverage, newCoverage float64,
	iterationCount int,
	functionUnderTest string,
) error {
	purpose := TestForFile
	if functionUnderTest != "" {
		purpose = TestForFunction
	}

	body, err := json.Marshal(map[string]interface{}{
		"sessionId":      sessionID,
		"initalCoverage": oldCoverage,
		"finalCoverage":  newCoverage,
		"iteration":      iterationCount,
		"requestPurpose": purpose,
	})
	if err != nil {
		return fmt.Errorf("error marshalling request body: %v", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		apiBase+"/ai/coverage/update",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	token, err := auth.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("error getting token: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error making request: %v", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(logger, err, "Error closing response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"unexpected status code: %v, response body: %s",
			resp.StatusCode,
			string(bodyBytes),
		)
	}

	logger.Debug("Coverage update sent successfully", zap.String("session_id", sessionID))
	return nil
}
