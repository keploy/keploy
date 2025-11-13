package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var osReadFile224 = os.ReadFile
var osCreate224 = os.Create
var timeSleep224 = time.Sleep
var extractClaimsWithoutVerification224 = extractClaimsWithoutVerification
var getLatestPlan224 = getLatestPlan

const configPushPath = "/mock/pr" // The API endpoint for pushing config changes

type mock struct {
	cfg        *config.Config
	storage    Storage
	logger     *zap.Logger
	tsConfigDB TestSetConfig
	token      string
}

type MockChangeReq struct {
	Config    *models.TestSet `json:"config"`
	TestSetID string          `json:"testSetId"`
	Branch    string          `json:"branch"`
	Owner     string          `json:"owner"`
}
type MockChangeResp struct {
	CommitURL string `json:"commit_url"`
	Message   string `json:"message"`
}

func (m *mock) setToken(token string) {
	m.token = token
}

// download function remains the same...
func (m *mock) download(ctx context.Context, testSetID string) error {
	// Add nil check protection to prevent segmentation fault
	if m.storage == nil {
		m.logger.Error("Storage service is not initialized, cannot download mocks")
		return fmt.Errorf("storage service is not initialized")
	}

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
	mockContent, err := osReadFile224(localMockPath)
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
	downloadFunc := func() (io.Reader, error) {
		return m.storage.Download(ctx, tsConfig.MockRegistry.Mock, tsConfig.MockRegistry.App, tsConfig.MockRegistry.User, m.token)
	}

	err = m.downloadAndSaveMock(downloadFunc, localMockPath)
	if err != nil {
		// The error is already logged by the helper function.
		return err
	}

	m.logger.Info("Mock file downloaded successfully")

	err = utils.AddToGitIgnore(m.logger, m.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(m.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}

func (m *mock) upload(ctx context.Context, testSetID string) error {
	// Add nil check protection to prevent segmentation fault
	if m.storage == nil {
		m.logger.Error("Storage service is not initialized, cannot upload mocks")
		return fmt.Errorf("storage service is not initialized")
	}

	claims, err := extractClaimsWithoutVerification224(m.token)
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
	plan, err := getLatestPlan224(ctx, m.logger, m.cfg.APIServerURL, m.token)
	if err != nil {
		m.logger.Error("Failed to get latest plan of the user", zap.Error(err))
		return err
	}

	m.logger.Debug("The latest plan", zap.Any("Plan", plan))

	// Inspect local mock file
	localMockPath := filepath.Join(m.cfg.Path, testSetID, "mocks.yaml")
	mockFileContent, err := osReadFile224(localMockPath)
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
		var metadata map[string]interface{}
		if tsConfig != nil {
			prescript = tsConfig.PreScript
			postscript = tsConfig.PostScript
			template = tsConfig.Template
			metadata = tsConfig.Metadata
		}
		tsConfig = &models.TestSet{
			PreScript:  prescript,
			PostScript: postscript,
			Template:   template,
			MockRegistry: &models.MockRegistry{
				Mock: mockHash,
				App:  m.cfg.AppName,
			},
			Metadata: metadata,
		}

		if plan == "Free" {
			if username == "" {
				m.logger.Error("Username not found in the token for Free plan")
				return fmt.Errorf("failed to upload mock file: username not found in the token")
			}
			tsConfig.MockRegistry.User = username
		}

		m.logger.Info("uploading mock file...", zap.String("testSet", testSetID))

		err = m.storage.Upload(ctx, mockFileReader, mockHash, m.cfg.AppName, m.token)
		if err != nil {
			m.logger.Error("Failed to upload mock file", zap.Error(err))
			return err
		}

		err = m.tsConfigDB.Write(ctx, testSetID, tsConfig)
		if err != nil {
			m.logger.Error("Failed to write test set config", zap.Error(err))
			return err
		}

		// After successfully writing the config, push it to the repo
		if m.cfg.ReRecord.Branch != "" && m.cfg.ReRecord.Owner != "" {
			err := m.pushConfigChange(ctx, testSetID, tsConfig, m.cfg.ReRecord.Owner, m.cfg.ReRecord.Branch)
			if err != nil {
				m.logger.Error("Failed to push config change", zap.Error(err))
				return err
			}
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

	m.logger.Info("uploading mock file...", zap.String("testSet", testSetID))
	err = m.storage.Upload(ctx, mockFileReader, mockHash, m.cfg.AppName, m.token)
	if err != nil {
		m.logger.Error("Failed to upload mock file", zap.Error(err))
		return err
	}

	tsConfig.MockRegistry.Mock = mockHash
	if plan == "Free" {
		if username == "" {
			m.logger.Error("Username not found in the token for Free plan")
			return fmt.Errorf("failed to upload mock file: username not found in the token")
		}
		tsConfig.MockRegistry.User = username
	}
	err = m.tsConfigDB.Write(ctx, testSetID, tsConfig)
	if err != nil {
		m.logger.Error("Failed to write updated test set config", zap.Error(err))
		return err
	}

	// After successfully writing the config, push it to the repo
	if m.cfg.ReRecord.Branch != "" && m.cfg.ReRecord.Owner != "" {
		err := m.pushConfigChange(ctx, testSetID, tsConfig, m.cfg.ReRecord.Owner, m.cfg.ReRecord.Branch)
		if err != nil {
			m.logger.Error("Failed to push config change", zap.Error(err))
			return err
		}
	}

	err = utils.AddToGitIgnore(m.logger, m.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(m.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}

// pushConfigChange sends a request to the api-server to push the updated config to a git branch.
func (m *mock) pushConfigChange(ctx context.Context, testSetID string, tsConfig *models.TestSet, owner, branch string) error {
	m.logger.Info("Attempting to push config change to git", zap.String("testSetID", testSetID), zap.String("branch", branch))

	// 1. Construct the request payload
	payload := MockChangeReq{
		Config:    tsConfig,
		TestSetID: testSetID,
		Branch:    branch,
		Owner:     owner,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		m.logger.Error("Failed to marshal config push request payload", zap.Error(err))
		return err
	}

	// 2. Create the HTTP request
	url := m.cfg.APIServerURL + configPushPath
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		m.logger.Error("Failed to create HTTP request for config push", zap.Error(err))
		return err
	}

	// 3. Set necessary headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)

	// 4. Send the request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		m.logger.Error("Failed to send config push request to API server", zap.Error(err))
		return err
	}
	defer resp.Body.Close()

	// 5. Handle the response
	if resp.StatusCode != http.StatusOK {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			m.logger.Error("Failed to read error response body from config push", zap.Error(err))
			return err
		}
		m.logger.Error("API server returned an error for config push",
			zap.Int("statusCode", resp.StatusCode),
			zap.String("response", string(respBody)))
		return err
	}

	var respData MockChangeResp
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		m.logger.Error("Failed to decode successful config push response", zap.Error(err))
		return err
	}

	m.logger.Info("Successfully pushed config change to git",
		zap.String("testSetID", testSetID),
		zap.String("commitURL", respData.CommitURL))

	return nil
}

// downloadByRegistryID downloads mocks directly using a registry ID
func (m *mock) downloadByRegistryID(ctx context.Context, registryID string, appName string) error {
	// Add nil check protection
	if m.storage == nil {
		m.logger.Error("Storage service is not initialized, cannot download mocks")
		return fmt.Errorf("storage service is not initialized")
	}

	// If app name is empty, get it from current directory
	if appName == "" {
		var err error
		appName, err = utils.GetLastDirectory()
		if err != nil {
			m.logger.Error("Failed to get app name from current directory", zap.Error(err))
			return fmt.Errorf("failed to get app name: %w", err)
		}
		m.logger.Info("Using current directory name as app name", zap.String("app", appName))
	}

	// Determine the output path - save at repository root
	outputPath := fmt.Sprintf("%s.mocks.yaml", registryID)

	claims, err := extractClaimsWithoutVerification224(m.token)
	if err != nil {
		m.logger.Error("Failed to extract claims from token for mock download by registry ID", zap.Error(err))
		return err
	}

	var (
		username string
		ok       bool
	)
	if username, ok = claims["username"].(string); !ok {
		m.logger.Error("Username not found in the token, skipping mock download")
		return fmt.Errorf("failed to download mock file: username not found in the token")
	}

	// Download the mock file from cloud using registry ID
	downloadFunc := func() (io.Reader, error) {
		// We pass registryID as mockName and an empty string for userName
		return m.storage.Download(ctx, registryID, appName, username, m.token)
	}

	err = m.downloadAndSaveMock(downloadFunc, outputPath)
	if err != nil {
		m.logger.Error("Failed to download mock file using registry ID",
			zap.String("registryID", registryID),
			zap.Error(err))
		return err
	}

	m.logger.Info("Mock file downloaded successfully",
		zap.String("registryID", registryID),
		zap.String("savedAs", outputPath))

	// Add to .gitignore if needed
	err = utils.AddToGitIgnore(m.logger, ".", "*.mocks.yaml")
	if err != nil {
		utils.LogError(m.logger, err, "failed to add *.mocks.yaml to .gitignore file")
	}

	return nil
}

// downloadAndSaveMock is a helper function to download from a reader and save to a file.
func (m *mock) downloadAndSaveMock(downloadFunc func() (io.Reader, error), outputPath string) error {
	cloudFile, err := downloadFunc()
	if err != nil {
		m.logger.Error("Failed to download mock file from storage", zap.Error(err))
		return err
	}

	// Save the downloaded mock file to the specified path
	file, err := osCreate224(outputPath)
	if err != nil {
		m.logger.Error("Failed to create local file", zap.String("path", outputPath), zap.Error(err))
		return err
	}
	defer func() {
		err := file.Close()
		if err != nil {
			utils.LogError(m.logger, err, "failed to close the file")
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
				timeSleep224(100 * time.Millisecond)
			}
		}
	}()

	_, err = io.Copy(file, cloudFile)
	close(done)
	if err != nil {
		return err
	}

	return nil
}
