package utgen

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
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
func (cp *CoverageProcessor) ProcessCoverageReport(latestTime int64) (*models.CoverageResult, error) {
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

	if fileModTimeMs <= latestTime {
		return fmt.Errorf("fatal: the coverage report file was not updated after the test command. file_mod_time_ms: %d, time_of_test_command: %d", fileModTimeMs, latestTime)
	}
	return nil
}

// ParseCoverageReport parses the coverage report based on its type
func (cp *CoverageProcessor) ParseCoverageReport() (*models.CoverageResult, error) {
	switch cp.Format {
	case "cobertura":
		return cp.ParseCoverageReportCobertura()
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

	fmt.Println(filteredBuf.String())

	coverageResult := &models.CoverageResult{
		LinesCovered:  linesCovered,
		LinesMissed:   linesMissed,
		Coverage:      coveragePercentage,
		Files:         filesToCover,
		ReportContent: filteredBuf.String(),
	}

	return coverageResult, nil
}
