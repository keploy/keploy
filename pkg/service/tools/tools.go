// Package tools provides functionality for utilities and helpers like config generation, imports, exports etc.
package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/pkg/service/export"
	postmanimport "go.keploy.io/server/v3/pkg/service/import"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewTools(logger *zap.Logger, testsetConfig TestSetConfig, testDB TestDB, reportDB ReportDB, telemetry teleDB, auth service.Auth, config *config.Config) Service {
	return &Tools{
		logger:      logger,
		telemetry:   telemetry,
		auth:        auth,
		testSetConf: testsetConfig,
		testDB:      testDB,
		reportDB:    reportDB,
		config:      config,
	}
}

type Tools struct {
	logger      *zap.Logger
	telemetry   teleDB
	testSetConf TestSetConfig
	testDB      TestDB
	reportDB    ReportDB
	config      *config.Config
	auth        service.Auth
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

func (t *Tools) SendTelemetry(event string, output ...*sync.Map) {
	t.telemetry.SendTelemetry(event, output...)
}

func (t *Tools) Export(ctx context.Context) error {
	return export.Export(ctx, t.logger)
}

func (t *Tools) Import(ctx context.Context, path, basePath string) error {
	postmanImport := postmanimport.NewPostmanImporter(ctx, t.logger)
	return postmanImport.Import(path, basePath)
}

// ✅ NEW: update delay in keploy.yml automatically
// ✅ NEW: update delay in keploy.yml automatically
func (t *Tools) UpdateTestDelayInConfig(ctx context.Context, configPath string, delay uint64) error {
	configFile := filepath.Join(configPath, "keploy.yml")

	b, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", configFile, err)
	}

	var node yamlLib.Node
	if err := yamlLib.Unmarshal(b, &node); err != nil {
		return fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	// ✅ update test.delay directly using yaml node traversal
	updated := false

	// root must exist
	if len(node.Content) == 0 {
		node.Content = []*yamlLib.Node{{
			Kind: yamlLib.MappingNode,
		}}
	}

	root := node.Content[0]

	// find/create "test" mapping
	var testNode *yamlLib.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "test" {
			testNode = root.Content[i+1]
			break
		}
	}

	if testNode == nil {
		testNode = &yamlLib.Node{Kind: yamlLib.MappingNode}
		root.Content = append(root.Content,
			&yamlLib.Node{Kind: yamlLib.ScalarNode, Value: "test"},
			testNode,
		)
	}

	// set delay inside test node
	for i := 0; i < len(testNode.Content); i += 2 {
		if testNode.Content[i].Value == "delay" {
			testNode.Content[i+1].Value = fmt.Sprintf("%d", delay)
			updated = true
			break
		}
	}

	if !updated {
		testNode.Content = append(testNode.Content,
			&yamlLib.Node{Kind: yamlLib.ScalarNode, Value: "delay"},
			&yamlLib.Node{Kind: yamlLib.ScalarNode, Value: fmt.Sprintf("%d", delay)},
		)
	}

	out, err := yamlLib.Marshal(&node)
	if err != nil {
		return fmt.Errorf("failed to marshal yaml: %w", err)
	}

	if err := os.WriteFile(configFile, out, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// Update initiates the tools process for the Keploy binary file.
func (t *Tools) Update(ctx context.Context) error {
	currentVersion := "v" + utils.Version
	isKeployInDocker := len(os.Getenv("KEPLOY_INDOCKER")) > 0
	if isKeployInDocker {
		fmt.Println("As you are using docker version of keploy, please pull the latest Docker image of keploy to update keploy")
		return nil
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		fmt.Println("you are using a development version of Keploy. Skipping update")
		return nil
	}

	releaseInfo, err := utils.GetLatestGitHubRelease(ctx, t.logger)
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			return errors.New("gitHub API is unresponsive. Update process cannot continue")
		}
		return fmt.Errorf("failed to fetch latest GitHub release version: %v", err)
	}

	latestVersion := releaseInfo.TagName
	changelog := releaseInfo.Body

	if currentVersion == latestVersion {
		fmt.Println("✅You are already on the latest version of Keploy: " + latestVersion)
		return nil
	}

	t.logger.Info("Updating to Version: " + latestVersion)
	downloadURL := ""

	if runtime.GOOS == "linux" {
		if runtime.GOARCH == "amd64" {
			downloadURL = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
		} else {
			downloadURL = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
		}
	}

	if runtime.GOOS == "darwin" {
		downloadURL = "https://github.com/keploy/keploy/releases/latest/download/keploy_darwin_all.tar.gz"
	}

	err = t.downloadAndUpdate(ctx, t.logger, downloadURL)
	if err != nil {
		return err
	}

	t.logger.Info("Update Successful!")

	changelog = "\n" + string(changelog)
	var renderer *glamour.TermRenderer

	var termRendererOpts []glamour.TermRendererOption
	termRendererOpts = append(termRendererOpts, glamour.WithAutoStyle(), glamour.WithWordWrap(0))

	renderer, err = glamour.NewTermRenderer(termRendererOpts...)
	if err != nil {
		utils.LogError(t.logger, err, "failed to initialize renderer")
		return err
	}
	changelog, err = renderer.Render(changelog)
	if err != nil {
		utils.LogError(t.logger, err, "failed to render release notes")
		return err
	}
	fmt.Println(changelog)
	return nil
}

func (t *Tools) downloadAndUpdate(ctx context.Context, logger *zap.Logger, downloadURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			utils.LogError(logger, cerr, "failed to close response body")
		}
	}()

	tmpFile, err := os.CreateTemp("", "keploy-download-*.tar.gz")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer func() {
		if err := tmpFile.Close(); err != nil {
			utils.LogError(logger, err, "failed to close temporary file")
		}
		if err := os.Remove(tmpFile.Name()); err != nil {
			utils.LogError(logger, err, "failed to remove temporary file")
		}
	}()

	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write to temporary file: %v", err)
	}

	if err := extractTarGz(tmpFile.Name(), "/tmp"); err != nil {
		return fmt.Errorf("failed to extract tar.gz file: %v", err)
	}

	aliasPath := "/usr/local/bin/keploy"

	keployPath, err := exec.LookPath("keploy")
	if err == nil && keployPath != "" {
		aliasPath = keployPath
	}

	_, err = os.Stat(aliasPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("alias path %s does not exist", aliasPath)
	}

	if fileInfo, err := os.Stat(aliasPath); err == nil && fileInfo.IsDir() {
		return fmt.Errorf("alias path %s is a directory, not a file", aliasPath)
	}

	if err := os.Rename("/tmp/keploy", aliasPath); err != nil {
		return fmt.Errorf("failed to move keploy binary to %s: %v", aliasPath, err)
	}

	if err := os.Chmod(aliasPath, 0777); err != nil {
		return fmt.Errorf("failed to set execute permission on %s: %v", aliasPath, err)
	}

	return nil
}

func extractTarGz(gzipPath, destDir string) error {
	file, err := os.Open(gzipPath)
	if err != nil {
		return err
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(nil, err, "failed to close file")
		}
	}()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}

	defer func() {
		if err := gzipReader.Close(); err != nil {
			utils.LogError(nil, err, "failed to close gzip reader")
		}
	}()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		fileName := filepath.Clean(header.Name)
		if strings.Contains(fileName, "..") {
			return fmt.Errorf("invalid file path: %s", fileName)
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0777); err != nil {
				return err
			}
		case tar.TypeReg:
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				if err := outFile.Close(); err != nil {
					return err
				}
				return err
			}
			if err := outFile.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tools) CreateConfig(_ context.Context, filePath string, configData string) error {
	var node yamlLib.Node
	var data []byte
	var err error

	if configData != "" {
		data = []byte(configData)
	} else {
		configData, err = config.Merge(config.InternalConfig, config.GetDefaultConfig())
		if err != nil {
			utils.LogError(t.logger, err, "failed to create default config string")
			return nil
		}
		data = []byte(configData)
	}

	if err := yamlLib.Unmarshal(data, &node); err != nil {
		utils.LogError(t.logger, err, "failed to unmarshal the config")
		return nil
	}

	if len(node.Content) > 0 { // remove agent config
		rootContent := node.Content[0].Content
		for i := 0; i < len(rootContent)-1; i += 2 {
			keyNode := rootContent[i]
			if keyNode.Value == "agent" {
				node.Content[0].Content = append(rootContent[:i], rootContent[i+2:]...)
				break
			}
		}
	}

	results, err := yamlLib.Marshal(node.Content[0])
	if err != nil {
		utils.LogError(t.logger, err, "failed to marshal the config")
		return nil
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)
	finalOutput = append([]byte(utils.GetVersionAsComment()), finalOutput...)

	err = os.WriteFile(filePath, finalOutput, fs.ModePerm)
	if err != nil {
		utils.LogError(t.logger, err, "failed to write config file")
		return nil
	}

	err = os.Chmod(filePath, 0777)
	if err != nil {
		utils.LogError(t.logger, err, "failed to set the permission of config file")
		return nil
	}

	return nil
}

func (t *Tools) IgnoreTests(_ context.Context, _ string, _ []string) error {
	return nil
}

func (t *Tools) IgnoreTestSet(_ context.Context, _ string) error {
	return nil
}

func (t *Tools) Login(ctx context.Context) bool {
	return t.auth.Login(ctx)
}


func (t *Tools) Templatize(ctx context.Context) error {

	testSets := t.config.Templatize.TestSets
	if len(testSets) == 0 {
		all, err := t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get all test sets")
			return err
		}
		testSets = all
	}

	if len(testSets) == 0 {
		t.logger.Warn("No test sets found to templatize")
		return nil
	}

	for _, testSetID := range testSets {

		testSet, err := t.testSetConf.Read(ctx, testSetID)
		if err == nil && (testSet != nil && testSet.Template != nil) {
			utils.TemplatizedValues = testSet.Template
		} else {
			utils.TemplatizedValues = make(map[string]interface{})
		}

		if err == nil && (testSet != nil && testSet.Secret != nil) {
			utils.SecretValues = testSet.Secret
		} else {
			utils.SecretValues = make(map[string]interface{})
		}

		// Get test cases from the database
		tcs, err := t.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get test cases")
			return err
		}

		if len(tcs) == 0 {
			t.logger.Warn("The test set is empty. Please record some test cases to templatize.", zap.String("testSet", testSetID))
			continue
		}

		err = t.ProcessTestCasesV2(ctx, tcs, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to process test cases")
			return err
		}
	}
	return nil
}



// setYAMLValue updates YAML value at a given path like ["test","delay"]
func setYAMLValue(root *yamlLib.Node, path []string, value uint64) bool {
	if root == nil || len(root.Content) == 0 {
		return false
	}

	// root.Content[0] should be Document->Mapping
	curr := root.Content[0]
	if curr.Kind != yamlLib.MappingNode {
		return false
	}

	for i := 0; i < len(path)-1; i++ {
		key := path[i]
		nextKey := path[i+1]

		// find key in mapping
		found := false
		for j := 0; j < len(curr.Content); j += 2 {
			k := curr.Content[j]
			v := curr.Content[j+1]

			if k.Value == key {
				// if v is not mapping, cannot continue
				if v.Kind != yamlLib.MappingNode {
					return false
				}
				curr = v
				found = true
				break
			}
		}

		// path doesn't exist
		if !found {
			_ = nextKey
			return false
		}
	}

	// set final key
	finalKey := path[len(path)-1]
	for j := 0; j < len(curr.Content); j += 2 {
		k := curr.Content[j]
		v := curr.Content[j+1]

		if k.Value == finalKey {
			v.Kind = yamlLib.ScalarNode
			v.Tag = "!!int"
			v.Value = fmt.Sprintf("%d", value)
			return true
		}
	}
	return false
}

// createYAMLPath creates missing mapping nodes for path and sets value
func createYAMLPath(root *yamlLib.Node, path []string, value uint64) {
	if root == nil {
		return
	}

	// if empty YAML, create document node
	if len(root.Content) == 0 {
		root.Kind = yamlLib.DocumentNode
		root.Content = []*yamlLib.Node{
			{
				Kind: yamlLib.MappingNode,
				Tag:  "!!map",
			},
		}
	}

	curr := root.Content[0]

	// ensure curr is mapping
	if curr.Kind != yamlLib.MappingNode {
		curr.Kind = yamlLib.MappingNode
		curr.Tag = "!!map"
		curr.Content = []*yamlLib.Node{}
	}

	for i := 0; i < len(path)-1; i++ {
		key := path[i]

		// try to find key
		var next *yamlLib.Node
		for j := 0; j < len(curr.Content); j += 2 {
			if curr.Content[j].Value == key {
				next = curr.Content[j+1]
				break
			}
		}

		// create key if missing
		if next == nil {
			keyNode := &yamlLib.Node{
				Kind:  yamlLib.ScalarNode,
				Tag:   "!!str",
				Value: key,
			}
			mapNode := &yamlLib.Node{
				Kind:    yamlLib.MappingNode,
				Tag:     "!!map",
				Content: []*yamlLib.Node{},
			}
			curr.Content = append(curr.Content, keyNode, mapNode)
			next = mapNode
		}

		// ensure mapping
		if next.Kind != yamlLib.MappingNode {
			next.Kind = yamlLib.MappingNode
			next.Tag = "!!map"
			next.Content = []*yamlLib.Node{}
		}

		curr = next
	}

	// set final key
	finalKey := path[len(path)-1]

	// check exists
	for j := 0; j < len(curr.Content); j += 2 {
		if curr.Content[j].Value == finalKey {
			curr.Content[j+1].Kind = yamlLib.ScalarNode
			curr.Content[j+1].Tag = "!!int"
			curr.Content[j+1].Value = fmt.Sprintf("%d", value)
			return
		}
	}

	// create if missing
	curr.Content = append(curr.Content,
		&yamlLib.Node{
			Kind:  yamlLib.ScalarNode,
			Tag:   "!!str",
			Value: finalKey,
		},
		&yamlLib.Node{
			Kind:  yamlLib.ScalarNode,
			Tag:   "!!int",
			Value: fmt.Sprintf("%d", value),
		},
	)
}
