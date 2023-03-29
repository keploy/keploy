package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	v "github.com/hashicorp/go-version"
	"go.keploy.io/server/config"
	"go.keploy.io/server/keploycli"
	"go.keploy.io/server/pkg/service"
	"go.uber.org/zap"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string

func main() {
	// main method to start Keploy server
	if version == "" {
		version = getKeployVersion()
	}

	conf := config.NewConfig()

	// create a new logger
	logger := newLogger(conf)

	// default resultPath is current directory from which keploy binary is running
	if conf.ReportPath == "" {
		curr, err := os.Getwd()
		if err != nil {
			logger.Error("failed to get path of current directory from which keploy binary is running", zap.Error(err))
		}
		conf.ReportPath = curr
	} else if conf.ReportPath[0] != '/' {
		path, err := filepath.Abs(conf.ReportPath)
		if err != nil {
			logger.Error("Failed to get the absolute path from relative conf.path", zap.Error(err))
		}
		conf.ReportPath = path
	}
	conf.ReportPath += "/test-reports"

	kServices := service.NewServices(version, conf, logger)

	// run the cli
	keploycli.CLI(version, conf, kServices, logger)
}

// newLogger returns a new logger at info level
func newLogger(conf *config.Config) *zap.Logger {
	logConf := zap.NewDevelopmentConfig()
	logConf.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger, err := logConf.Build()
	if err != nil {
		log.Fatalf("failed to initialize logger. error: %v", err)
	}

	// Set logger on debug level when "ENABLE_DEBUG" env variable is true
	if conf.EnableDebugger {
		logConf.Level.SetLevel(zap.DebugLevel)
	}
	return logger
}

func getKeployVersion() string {

	repo, err := git.PlainOpen(".")
	if err != nil {
		return "v0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return "v0.1.0-dev"
	}

	var latestTag string
	var latestTagVersion *v.Version

	err = tagIter.ForEach(func(tagRef *plumbing.Reference) error {
		tagName := tagRef.Name().Short()
		tagVersion, err := v.NewVersion(tagName)
		if err == nil {
			if latestTagVersion == nil || latestTagVersion.LessThan(tagVersion) {
				latestTagVersion = tagVersion
				latestTag = tagName
			}
		}
		return nil
	})

	if err != nil {
		return "v0.1.0-dev"
	}

	return latestTag + "-dev"
}
