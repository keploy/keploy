// Package java implements the methods for java coverage services.
package java

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Java struct {
	ctx             context.Context
	logger          *zap.Logger
	reportDB        coverage.ReportDB
	cmd             string
	jacocoAgentPath string
	executable      string
}

func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd, jacocoAgentPath, executable string) *Java {
	return &Java{
		ctx:             ctx,
		logger:          logger,
		reportDB:        reportDB,
		cmd:             cmd,
		jacocoAgentPath: jacocoAgentPath,
		executable:      executable,
	}
}

func (j *Java) PreProcess(_ bool) (string, error) {
	// default location for jar of jacoco agent
	jacocoAgentPath := "~/.m2/repository/org/jacoco/org.jacoco.agent/0.8.8/org.jacoco.agent-0.8.8-runtime.jar"
	if j.jacocoAgentPath != "" {
		jacocoAgentPath = j.jacocoAgentPath
	}
	var err error
	jacocoAgentPath, err = utils.ExpandPath(jacocoAgentPath)
	if err == nil {
		isFileExist, err := utils.FileExists(jacocoAgentPath)
		if err == nil && isFileExist {
			j.cmd = strings.Replace(
				j.cmd,
				j.executable,
				fmt.Sprintf("%s -javaagent:%s=destfile=target/${TESTSETID}.exec", j.executable, jacocoAgentPath), 1,
			)
		}
	}
	if err != nil {
		j.logger.Warn("failed to find jacoco agent. If jacoco agent is present in a different path, please set it using --jacocoAgentPath")
		return j.cmd, err
	}
	// downlaod jacoco cli
	jacocoPath := filepath.Join(os.TempDir(), "jacoco")
	err = os.MkdirAll(jacocoPath, 0777)
	if err != nil {
		j.logger.Debug("failed to create jacoco directory", zap.Error(err))
		return j.cmd, err
	}
	err = downloadAndExtractJaCoCoCli(j.logger, "0.8.12", jacocoPath)
	if err != nil {
		j.logger.Debug("failed to download and extract jacoco binaries", zap.Error(err))
		return j.cmd, err
	}
	return j.cmd, nil
}

func (j *Java) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	// Define the path to the CSV file
	csvPath := filepath.Join("target", "site", "keployE2E", "e2e.csv")

	file, err := os.Open(csvPath)
	if err != nil {
		return testCov, fmt.Errorf("failed to open CSV file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(j.logger, err, "Error closing coverage csv file")
		}
	}()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return testCov, fmt.Errorf("failed to read CSV file: %w", err)
	}

	var totalInstructions, coveredInstructions int

	// Skip header row and process each record
	for i, record := range records {
		if i == 0 {
			continue // Skip header
		}

		// Parse instructions coverage data
		instructionsMissed, err := strconv.Atoi(record[3])
		if err != nil {
			return testCov, err
		}
		instructionsCovered, err := strconv.Atoi(record[4])
		if err != nil {
			return testCov, err
		}

		// Calculate total instructions and covered instructions
		totalInstructions += instructionsMissed + instructionsCovered
		coveredInstructions += instructionsCovered

		// Calculate coverage percentage for each class
		if instructionsCovered > 0 {
			coverage := float64(instructionsCovered) / float64(instructionsCovered+instructionsMissed) * 100
			classPath := strings.ReplaceAll(record[1], ".", string(os.PathSeparator))              // Replace dots with path separator
			testCov.FileCov[filepath.Join(classPath, record[2])] = fmt.Sprintf("%.2f%%", coverage) // Use class path as key
		}
	}
	if totalInstructions > 0 {
		totalCoverage := float64(coveredInstructions) / float64(totalInstructions) * 100
		testCov.TotalCov = fmt.Sprintf("%.2f%%", totalCoverage)
	}

	testCov.Loc = models.Loc{
		Total:   totalInstructions,
		Covered: coveredInstructions,
	}

	return testCov, nil
}

func (j *Java) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return j.reportDB.UpdateReport(j.ctx, testRunID, coverage)
}
