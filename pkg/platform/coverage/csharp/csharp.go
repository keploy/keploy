// Package Csharp impliments methods for Csharp coverage services.
package csharp

import (
	"context"
	"encoding/xml"
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

type Csharp struct {
	ctx                context.Context
	logger             *zap.Logger
	reportdb           coverage.ReportDB
	cmd                string
	executable         string
	dotnetCoveragePath string
}

type CoverageStructure struct {
	LineRate float64 `xml:"line-rate,attr"`
}

func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd, dotnetCoveragePath, executable string) *Csharp {
	return &Csharp{
		ctx:                ctx,
		logger:             logger,
		reportdb:           reportDB,
		cmd:                cmd,
		dotnetCoveragePath: dotnetCoveragePath,
		executable:         executable,
	}
}

func (cs *Csharp) PreProcess(_ bool) (string, error) {
	// default location for dotnet-coverage
	dotnetCoveragePath := "~/.dotnet/tools/dotnet-coverage"
	if cs.dotnetCoveragePath != "" {
		dotnetCoveragePath = cs.dotnetCoveragePath
	}

	isFileExists, err := utils.FileExists(dotnetCoveragePath)
	if err != nil {
		cs.logger.Warn("error checking dotnet-coverage tool existance: %s", zap.Error(err))
		return cs.cmd, err
	}

	if !isFileExists {
		return cs.cmd, fmt.Errorf("dotnet coverage tool not found at: %s", dotnetCoveragePath)
	}

	cs.cmd = strings.Replace(cs.cmd, cs.executable, fmt.Sprintf("%s collect --output target/${TESTSETID}.cobertura --output-format cobertura", cs.executable), 1)

	// download dotnet coverage
	dotnetPath := filepath.Join(os.TempDir(), "dotnet")
	err = os.MkdirAll(dotnetPath, 0777)

	if err != nil {
		cs.logger.Debug("failed to create dotnet directory: %s", zap.Error(err))
		return cs.cmd, err
	}

	err = downloadDotnetCoverage(cs.ctx)
	if err != nil {
		cs.logger.Debug("failed to download and extract dotnet binaries: %s", zap.Error(err))
		return cs.cmd, err
	}

	return cs.cmd, nil
}

func (cs *Csharp) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	// Define the path to cobertura file
	coberturaPath := filepath.Join("target", "site", "KeployE2E", "e2e.cobertura")

	file, err := os.Open(coberturaPath)
	if err != nil {
		return testCov, fmt.Errorf("failed to open cobertura file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(cs.logger, err, "Error closing coverage cobertura file")
		}
	}()

	coverageStruct := CoverageStructure{}
	if err := xml.Unmarshal([]byte(coberturaPath), &coverageStruct); err != nil {
		return testCov, fmt.Errorf("failed to unmarshal file: %w", err)
	}
	testCov.TotalCov = strconv.FormatFloat(coverageStruct.LineRate*100, 'E', -1, 64)

	return testCov, nil
}
