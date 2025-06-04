package replay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type mock struct {
	cfg        *config.Config
	storage    Storage
	logger     *zap.Logger
	tsConfigDB TestSetConfig
	token      string
}

func (m *mock) setToken(token string) {
	m.token = token
}

func (m *mock) download(ctx context.Context, testSetID string) error {

	// Check if test-set config is present
	tsConfig, err := m.tsConfigDB.Read(ctx, testSetID)
	if err != nil || tsConfig == nil || tsConfig.MockRegistry == nil {
		m.logger.Error("test set mock config not found", zap.String("testSetID", testSetID), zap.Error(err))
		return fmt.Errorf("mock registry config not found")
	}

	if tsConfig.MockRegistry.Mock == "" {
		m.logger.Error("Mock is empty in test-set config", zap.String("testSetID", testSetID))
		return fmt.Errorf("mock is empty in test-set config")
	}

	if tsConfig.MockRegistry.App == "" {
		m.logger.Warn("App name is empty in test-set config", zap.String("testSetID", testSetID))
		return fmt.Errorf("app name is empty in test-set config")
	}

	// Check if mock file is already downloaded by previous test runs
	localMockPath := filepath.Join(m.cfg.Path, testSetID, "mocks.yaml")
	mockContent, err := os.ReadFile(localMockPath)
	if err == nil {
		if tsConfig.MockRegistry.Mock == utils.Hash(mockContent) {
			m.logger.Info("Mock file already exists, downloading from cloud is not necessary", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
			return nil
		}
		var response string

		if len(mockContent) == 0 {
			m.logger.Warn("Local mock file is empty, proceeding with download from keploy registry", zap.String("testSetID", testSetID))
			response = "y"
		} else {
			m.logger.Warn("Local mock file is different from the one in the Keploy registry.")
			// Prompt user for confirmation to override the local mock file
			fmt.Print("The mock file present locally is different from the one in the Keploy registry. Do you want to override the local mock file with the version from the registry? (y/n): ")
		}

		// Create a channel to listen for context cancellation (Ctrl+C)
		cancelChan := make(chan struct{})

		// Start a goroutine to wait for user input asynchronously
		go func() {

			if len(mockContent) == 0 {
				cancelChan <- struct{}{}
				return
			}

			_, err := fmt.Scanln(&response)
			if err != nil {
				response = "n" // Default to 'no' if there's an error reading input
			}
			cancelChan <- struct{}{}
		}()

		select {
		case <-cancelChan:
			// Normalize user input
			response = strings.ToLower(strings.TrimSpace(response))
			if response != "y" && response != "yes" {
				m.logger.Info("Keeping the local mock file", zap.String("testSetID", testSetID))
				return nil
			}

			m.logger.Info("Overriding the local mock file with the version from the Keploy registry", zap.String("testSetID", testSetID))

		case <-ctx.Done(): // context cancellation (Ctrl+C)
			m.logger.Warn("Download interrupted by user")
			return ctx.Err() // Return the context cancellation error
		}
	}

	if tsConfig.MockRegistry.App != m.cfg.AppName {
		m.logger.Warn("App name in the keploy.yml does not match with the app name in the config.yml in the test-set", zap.String("test-set-config-AppName", tsConfig.MockRegistry.App), zap.String("global-config-Appname", m.cfg.AppName))
		m.logger.Warn("Using app name from the test-set's config.yml for mock retrieval", zap.String("appName", tsConfig.MockRegistry.App))
	}

	m.logger.Info("Downloading mock file from cloud...", zap.String("testSetID", testSetID))
	cloudFile, err := m.storage.Download(ctx, tsConfig.MockRegistry.Mock, tsConfig.MockRegistry.App, tsConfig.MockRegistry.User, m.token)
	if err != nil {
		m.logger.Error("Failed to download mock file", zap.Error(err))
		return err
	}

	// Save the downloaded mock file to local
	file, err := os.Create(localMockPath)
	if err != nil {
		m.logger.Error("Failed to create local file", zap.String("path", localMockPath), zap.Error(err))
		return err
	}
	defer func() {
		err := file.Close()
		if err != nil {
			utils.LogError(m.logger, err, "failed to close the http response body")
		}
	}()

	done := make(chan struct{})

	// Spinner goroutine
	go func() {
		spinnerChars := []rune{'|', '/', '-', '\\'}
		i := 0
		for {
			select {
			case <-done:
				fmt.Print("\r") // Clear spinner line after done
				return
			default:
				fmt.Printf("\rDownloading... %c", spinnerChars[i%len(spinnerChars)])
				i++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	_, err = io.Copy(file, cloudFile)
	if err != nil {
		close(done)
		return err
	}
	close(done)
	m.logger.Info("Mock file downloaded successfully")

	err = utils.AddToGitIgnore(m.logger, m.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(m.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}

func (m *mock) upload(ctx context.Context, testSetID string) error {

	claims, err := extractClaimsWithoutVerification(m.token)
	var role, username string
	var ok bool
	if err != nil {
		m.logger.Error("Failed to extract claim from token for mock upload", zap.Error(err))
		return err
	}

	if role, ok = claims["role"].(string); !ok || role == "" {
		m.logger.Error("Role not found in the token, skipping mock upload")
		return fmt.Errorf("failed to upload mock file: role not found in the token")
	}

	if username, ok = claims["username"].(string); !ok {
		m.logger.Error("Username not found in the token, skipping mock upload")
		return fmt.Errorf("failed to upload mock file: username not found in the token")
	}

	// get the plan of the current user
	plan, err := getLatestPlan(ctx, m.logger, m.cfg.APIServerURL, m.token)
	if err != nil {
		m.logger.Error("Failed to get latest plan of the user", zap.Error(err))
		return err
	}

	m.logger.Debug("The latest plan", zap.Any("Plan", plan))

	// Inspect local mock file
	localMockPath := filepath.Join(m.cfg.Path, testSetID, "mocks.yaml")
	mockFileContent, err := os.ReadFile(localMockPath)
	if err != nil {
		m.logger.Error("Failed to read mock file for mock upload", zap.String("path", localMockPath), zap.Error(err))
		return err
	}

	// If mock file is empty, return error
	if len(mockFileContent) == 0 {
		m.logger.Warn("Mock file is empty, skipping upload", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
		return nil
	}

	mockHash := utils.Hash(mockFileContent)
	mockFileReader := bytes.NewReader(mockFileContent)

	// Cross verify the local mock file with the test-set config
	tsConfig, err := m.tsConfigDB.Read(ctx, testSetID)
	// If test-set config is not found, upload the mock file
	if err != nil || tsConfig == nil || tsConfig.MockRegistry == nil {
		// create ts config
		var prescript, postscript string
		var template map[string]interface{}
		if tsConfig != nil {
			prescript = tsConfig.PreScript
			postscript = tsConfig.PostScript
			template = tsConfig.Template
		}
		tsConfig = &models.TestSet{
			PreScript:  prescript,
			PostScript: postscript,
			Template:   template,
			MockRegistry: &models.MockRegistry{
				Mock: mockHash,
				App:  m.cfg.AppName,
			},
		}

		if plan == "Free" {
			if username == "" {
				m.logger.Error("Username not found in the token for Free plan")
				return fmt.Errorf("failed to upload mock file: username not found in the token")
			}
			tsConfig.MockRegistry.User = username
		}

		m.logger.Info("uploading mock file...", zap.Any("testSet", testSetID))

		err = m.storage.Upload(ctx, mockFileReader, mockHash, m.cfg.AppName, m.token)
		if err != nil {
			m.logger.Error("Failed to upload mock file", zap.Error(err))
			return err
		}

		err := m.tsConfigDB.Write(ctx, testSetID, tsConfig)
		if err != nil {
			m.logger.Error("Failed to write test set config", zap.Error(err))
			return err
		}
		return nil
	}

	// If mock file is already uploaded, skip the upload
	if tsConfig.MockRegistry.Mock == mockHash {
		m.logger.Info("Mock file is already uploaded, skipping upload", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
		return nil
	}

	// If mock file is changed, upload the new mock file
	m.logger.Debug("Mock file has changed, uploading new mock", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))

	m.logger.Info("uploading mock file...", zap.Any("testSet", testSetID))
	err = m.storage.Upload(ctx, mockFileReader, mockHash, m.cfg.AppName, m.token)
	if err != nil {
		m.logger.Error("Failed to upload mock file", zap.Error(err))
		return err
	}

	err = utils.AddToGitIgnore(m.logger, m.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(m.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}
