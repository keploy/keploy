package coverage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// SetupCoverageCommands first checks whether language specific coverage tool is installed in the system 
// for python, javascript and java, and checks whether the go binary was built with -cover flag in case of go
// It then appends the coverage executable to the command and sets the coverage command in the config
func SetupCoverageCommands(logger *zap.Logger, conf *config.Config, executable string) {
	var err error
	switch conf.Test.Language {
	case models.Python:
		err = utils.RunCommand("coverage")
		if err != nil {
			conf.Test.SkipCoverage = true
			logger.Warn("coverage tool not found, skipping coverage caluclation. Please install coverage tool using 'pip install coverage'")
		} else {
			utils.CreatePyCoverageConfig(logger)
			conf.CoverageCommand = strings.Replace(conf.Command, executable, "coverage run $APPEND --data-file=.coverage.keploy", 1)
		}
	case models.Node:
		err = utils.RunCommand("nyc", "--version")
		if err != nil {
			conf.Test.SkipCoverage = true
			logger.Warn("coverage tool not found, skipping coverage caluclation. please install coverage tool using 'npm install -g nyc'")
		} else {
			conf.CoverageCommand = "nyc --clean=$CLEAN " + conf.Command
		}
	case models.Go:
		if !utils.CheckGoBinaryForCoverFlag(logger, conf.Command) {
			conf.Test.SkipCoverage = true
			logger.Warn("go binary was not built with -cover flag")
		}
		if utils.CmdType(conf.CommandType) == utils.Native {
			goCovPath, err := utils.SetCoveragePath(logger, conf.Test.CoverageReportPath)
			if err != nil {
				conf.Test.SkipCoverage = true
				logger.Warn("failed to set go coverage path", zap.Error(err))
			}
			conf.Test.CoverageReportPath = goCovPath
			err = os.Setenv("GOCOVERDIR", goCovPath)
			if err != nil {
				logger.Warn("failed to set GOCOVERDIR", zap.Error(err))
			}

		}
	case models.Java:
		// default location for jar of java agent
		javaAgentPath := "~/.m2/repository/org/jacoco/org.jacoco.agent/0.8.8/org.jacoco.agent-0.8.8-runtime.jar"
		if conf.Test.JacocoAgentPath != "" {
			javaAgentPath = conf.Test.JacocoAgentPath
		}
		javaAgentPath, err = utils.ExpandPath(javaAgentPath)
		if err == nil {
			isFileExist, err := utils.FileExists(javaAgentPath)
			if err == nil && isFileExist {
				conf.CoverageCommand = strings.Replace(conf.Command, executable, fmt.Sprintf("%s -javaagent:%s=destfile=target/${TESTSETID}.exec", executable, javaAgentPath), 1)
			}
		}
		if err != nil {
			conf.Test.SkipCoverage = true
			logger.Warn("failed to find jacoco agent. If jacoco agent is present in a different path, please set it using --jacocoAgentPath")
		}
		// downlaod jacoco cli
		jacocoPath := filepath.Join(os.TempDir(), "jacoco")
		err = os.MkdirAll(jacocoPath, 0777)
		if err != nil {
			logger.Debug("failed to create jacoco directory", zap.Error(err))
		} else {
			err := utils.DownloadAndExtractJaCoCoCli(logger, "0.8.12", jacocoPath)
			if err != nil {
				conf.Test.SkipCoverage = true
				logger.Debug("failed to download and extract jacoco binaries", zap.Error(err))
			}
		}
	}
}

func MergeAndGenerateJacocoReport(ctx context.Context, logger *zap.Logger) error {
	jacocoPath := filepath.Join(os.TempDir(), "jacoco")
	jacocoCliPath := filepath.Join(jacocoPath, "jacococli.jar")
	err := MergeJacocoCoverageFiles(ctx, jacocoCliPath)
	if err == nil {
		err = generateJacocoReport(ctx, jacocoCliPath)
		if err != nil {
			logger.Debug("failed to generate jacoco report", zap.Error(err))
		}
	} else {
		logger.Debug("failed to merge jacoco coverage data", zap.Error(err))
	}
	if err != nil {
		return err
	}
	return nil
}

func MergeJacocoCoverageFiles(ctx context.Context, jacocoCliPath string) error {
	// Find all .exec files in the target directory
	sourceFiles, err := filepath.Glob("target/*.exec")
	if err != nil {
		return fmt.Errorf("error finding coverage files: %w", err)
	}
	if len(sourceFiles) == 0 {
		return errors.New("no coverage files found")
	}

	// Construct the command arguments
	args := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"merge",
	}

	args = append(args, sourceFiles...)

	// Specify the output file
	args = append(args, "--destfile", "target/keploy-e2e.exec")

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to merge coverage files: %w", err)
	}

	return nil
}

func generateJacocoReport(ctx context.Context, jacocoCliPath string) error {
	reportDir := "target/site/keployE2E"

	// Ensure the report directory exists
	if err := os.MkdirAll(reportDir, 0777); err != nil {
		return fmt.Errorf("failed to create report directory: %w", err)
	}

	command := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"report",
		"target/keploy-e2e.exec",
		"--classfiles",
		"target/classes",
		"--csv",
		reportDir + "/e2e.csv",
		"--html",
		reportDir,
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate report: %w", err)
	}

	return nil
}
