package update

import (
	"context"
	"errors"
	"os"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Service interface {
	Update(ctx context.Context) error
}

type Updater struct {
	logger *zap.Logger
}

func NewUpdater(logger *zap.Logger) Service {
	return &Updater{
		logger: logger,
	}
}

func (u *Updater) Update(ctx context.Context) error {
	currentVersion := "v" + utils.Version

	downloadURLs := map[string]string{
		"linux_amd64": "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz",
		"linux_arm64": "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz",
		"darwin_all":  "https://github.com/keploy/keploy/releases/latest/download/keploy_darwin_all.tar.gz",
	}

	releaseInfo, err := utils.GetLatestGitHubRelease(ctx, u.logger)
	if err != nil {
		u.logger.Error("failed to fetch latest release info", zap.Error(err))
		return err
	}

	updateMgr := NewUpdateManager(u.logger, Config{
		BinaryName:     "keploy",
		CurrentVersion: currentVersion,
		IsDevVersion:   strings.HasSuffix(currentVersion, "-dev"),
		IsInDocker:     len(os.Getenv("KEPLOY_INDOCKER")) > 0,
		DownloadURLs:   downloadURLs,
		LatestVersion:  releaseInfo.TagName,
		Changelog:      releaseInfo.Body,
	})

	_, err = updateMgr.CheckAndUpdate(ctx)

	if errors.Is(err, ErrDmgNeedsManualInstall) {
		u.logger.Warn("Update downloaded but requires manual installation. Please find the .dmg file in your temporary directory and install it manually.", zap.Error(err))
		return nil
	}

	return err
}