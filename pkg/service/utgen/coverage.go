package utgen

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"
)

// CoverageProcessor handles the processing of coverage reports
type CoverageProcessor struct {
	FilePath     string
	Filename     string
	CoverageType string
}

// NewCoverageProcessor initializes a CoverageProcessor object
func NewCoverageProcessor(filePath, filename, coverageType string) *CoverageProcessor {
	return &CoverageProcessor{
		FilePath:     filePath,
		Filename:     filename,
		CoverageType: coverageType,
	}
}

// ProcessCoverageReport verifies the report and parses it based on its type
func (cp *CoverageProcessor) ProcessCoverageReport(timeOfTestCommand int64) ([]int, []int, float64, []string, error) {
	err := cp.VerifyReportUpdate(timeOfTestCommand)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	return cp.ParseCoverageReport()
}

// VerifyReportUpdate verifies the coverage report's existence and update time
func (cp *CoverageProcessor) VerifyReportUpdate(timeOfTestCommand int64) error {
	if _, err := os.Stat(cp.FilePath); os.IsNotExist(err) {
		return fmt.Errorf("fatal: coverage report \"%s\" was not generated", cp.FilePath)
	}

	fileInfo, err := os.Stat(cp.FilePath)
	if err != nil {
		return err
	}
	fileModTimeMs := fileInfo.ModTime().UnixNano() / int64(time.Millisecond)

	if fileModTimeMs <= timeOfTestCommand {
		return fmt.Errorf("fatal: the coverage report file was not updated after the test command. file_mod_time_ms: %d, time_of_test_command: %d", fileModTimeMs, timeOfTestCommand)
	}
	return nil
}

// ParseCoverageReport parses the coverage report based on its type
func (cp *CoverageProcessor) ParseCoverageReport() ([]int, []int, float64, []string, error) {
	switch cp.CoverageType {
	case "cobertura":
		return cp.ParseCoverageReportCobertura()
	case "lcov":
		return nil, nil, 0, nil, fmt.Errorf("parsing for %s coverage reports is not implemented yet", cp.CoverageType)
	default:
		return nil, nil, 0, nil, fmt.Errorf("unsupported coverage report type: %s", cp.CoverageType)
	}
}

type Coverage struct {
	XMLName  xml.Name  `xml:"coverage"`
	Sources  []string  `xml:"sources>source"`
	Packages []Package `xml:"packages>package"`
}

type Package struct {
	Name    string  `xml:"name,attr"`
	Classes []Class `xml:"classes>class"`
}

type Class struct {
	Name     string `xml:"name,attr"`
	FileName string `xml:"filename,attr"`
	Lines    []Line `xml:"lines>line"`
}

type Line struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

func (cp *CoverageProcessor) ParseCoverageReportCobertura() ([]int, []int, float64, []string, error) {

	filesToCover := make([]string, 0)
	// Open the XML file
	xmlFile, err := os.Open(cp.FilePath)
	if err != nil {
		return nil, nil, 0, filesToCover, err
	}

	defer func() {
		if err := xmlFile.Close(); err != nil {
			return
		}
	}()

	// Decode the XML file into a Coverage struct
	var cov Coverage
	if err := xml.NewDecoder(xmlFile).Decode(&cov); err != nil {
		return nil, nil, 0, filesToCover, err
	}

	// Find coverage for the specified file
	var linesCovered, linesMissed []int
	var totalLines, coveredLines int
	for _, pkg := range cov.Packages {
		for _, cls := range pkg.Classes {
			if cp.Filename == "." {
				filesToCover = append(filesToCover, cls.FileName)
			}
			if strings.HasSuffix(cls.FileName, cp.Filename) {
				for _, line := range cls.Lines {
					totalLines++
					if line.Hits > 0 {
						coveredLines++
						linesCovered = append(linesCovered, line.Number)
					} else {
						linesMissed = append(linesMissed, line.Number)
					}
				}
				break
			}
		}
	}

	var coveragePercentage float64
	if totalLines > 0 {
		coveragePercentage = float64(len(linesCovered)) / float64(totalLines)
	}

	return linesCovered, linesMissed, coveragePercentage, filesToCover, nil
}
