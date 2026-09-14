package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	glamour "charm.land/glamour/v2"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/service/export"
	postmanimport "go.keploy.io/server/v3/pkg/service/import"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewTools(logger *zap.Logger, testsetConfig TestSetConfig, testDB TestDB, reportDB ReportDB, telemetry teleDB, config *config.Config) Service {
	return &Tools{
		logger:      logger,
		telemetry:   telemetry,
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
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

func (t *Tools) SendTelemetry(event string, output ...map[string]interface{}) {
	t.telemetry.SendTelemetry(event, output...)
}

func (t *Tools) Export(ctx context.Context) error {
	return export.Export(ctx, t.logger)
}

func (t *Tools) Import(ctx context.Context, path, basePath string) error {
	postmanImport := postmanimport.NewPostmanImporterWithFormat(ctx, t.logger, yaml.ParseFormat(t.config.StorageFormat))
	return postmanImport.Import(path, basePath)
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
	termRendererOpts = append(termRendererOpts, glamour.WithEnvironmentConfig(), glamour.WithWordWrap(0))

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
	// Create a new request with context
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Create a HTTP client and execute the request
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

	// Create a temporary file to store the downloaded tar.gz
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

	// Write the downloaded content to the temporary file
	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write to temporary file: %v", err)
	}

	// Extract the tar.gz file
	if err := extractTarGz(tmpFile.Name(), "/tmp"); err != nil {
		return fmt.Errorf("failed to extract tar.gz file: %v", err)
	}

	// Determine the path based on the alias "keploy"
	aliasPath := "/usr/local/bin/keploy" // Default path

	keployPath, err := exec.LookPath("keploy")
	if err == nil && keployPath != "" {
		aliasPath = keployPath
	}

	// Check if the aliasPath is a valid path
	_, err = os.Stat(aliasPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("alias path %s does not exist", aliasPath)
	}

	// Check if the aliasPath is a directory
	if fileInfo, err := os.Stat(aliasPath); err == nil && fileInfo.IsDir() {
		return fmt.Errorf("alias path %s is a directory, not a file", aliasPath)
	}

	// Move the extracted binary to the alias path
	if err := os.Rename("/tmp/keploy", aliasPath); err != nil {
		return fmt.Errorf("failed to move keploy binary to %s: %v", aliasPath, err)
	}

	if err := os.Chmod(aliasPath, 0777); err != nil {
		return fmt.Errorf("failed to set execute permission on %s: %v", aliasPath, err)
	}

	return nil
}

// maxExtractedArchiveBytes caps the decompressed size of the self-update
// tarball. A keploy release archive is well under this; the bound stops a
// crafted artifact from exhausting disk or RAM (#3867).
const maxExtractedArchiveBytes = 1 << 30 // 1 GiB

func extractTarGz(gzipPath, destDir string) error {
	return extractTarGzWithLimit(gzipPath, destDir, maxExtractedArchiveBytes)
}

// extractTarGzWithLimit is the testable seam: the production cap stays a
// constant while tests exercise the bound without gigabyte archives.
func extractTarGzWithLimit(gzipPath, destDir string, limit int64) error {
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

	// Cap total decompressed output: the update artifact is a keploy
	// release tarball (well under the cap), and without a bound a crafted
	// archive can exhaust disk/RAM via io.Copy below (#3867). Note /tmp is
	// commonly tmpfs, so the copy target may be RAM-backed.
	tarReader := tar.NewReader(pkg.NewCappedReader(gzipReader, limit))

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

// WriteMinimalConfig writes a keploy.yml holding only what a developer
// DECIDED -- every setting that differs from Keploy's defaults -- topped by a
// header saying so and naming the command that prints the rest.
//
// A generated config used to be the whole struct: 251 lines on a real repo,
// of which two were the developer's. That buries the content, and it freezes
// every default into a repository that never chose it, so a later release
// cannot change the default for anyone who ran the generator.
//
// Deliberately NOT part of CreateConfig, which writes the document it is
// given and must keep doing so: one caller (the enterprise "ignore this test"
// resolver) reads keploy.yml into a ZERO-valued struct, edits one field and
// writes it back, so a document that omits the defaults would come back with
// every default replaced by a Go zero -- proxyPort 0, mocking off. Stripping
// is a decision about a file a human will read, so it belongs where that
// decision is made: the two places that GENERATE a config.
//
// cfgYAML is the config to write, or "" for "nothing has been decided yet".
func WriteMinimalConfig(logger *zap.Logger, filePath string, cfgYAML string) error {
	// Never THROUGH a link. os.WriteFile follows one, so a keploy.yml that is
	// a symlink -- dangling or not -- sent the generated config to a path the
	// user never named, outside the project, with no prompt and exit 0.
	// Removing their link would be just as presumptuous, so say what is in
	// the way and stop.
	if info, lerr := os.Lstat(filePath); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		// A link that resolves INSIDE the same directory is an ordinary
		// layout (keploy.yml -> config/keploy.yml in a monorepo); refusing it
		// took the generator away from those repositories entirely. What must
		// not happen is writing through a link to somewhere the user did not
		// name -- including a DANGLING one, where Stat reports "no file here"
		// and the overwrite prompt never fires.
		// Resolved AND absolute: "keploy.yml" and "." compare as strings
		// otherwise, and every relative path looks like an escape.
		target, rErr := resolveAbs(filePath)
		dir, dErr := resolveAbs(filepath.Dir(filePath))
		if rErr != nil || dErr != nil || !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			return fmt.Errorf("%s is a symbolic link pointing outside %s; Keploy will not write through it -- remove the link, or generate the config elsewhere with --path",
				filePath, filepath.Dir(filePath))
		}
	}
	defaults, err := config.DefaultsYAML()
	if err != nil {
		utils.LogError(logger, err, "failed to read the defaults")
		return err
	}
	if cfgYAML == "" {
		cfgYAML = defaults
	}
	// `agent` is Keploy's to resolve per run. The defaults document does not
	// publish it, so without this it reads as a key Keploy knows nothing
	// about -- and 45 lines of internals survive into the developer's file.
	cfgYAML, err = config.WithoutKey(cfgYAML, "agent")
	if err != nil {
		utils.LogError(logger, err, "failed to drop the agent block from the config")
		return err
	}
	short, err := config.StripDefaults(cfgYAML, defaults)
	if err != nil {
		utils.LogError(logger, err, "failed to strip the defaults from the config")
		return err
	}
	body := []byte(utils.ConfigHeader())
	body = append(body, []byte(short)...)
	body = append(body, []byte(placeholders(short))...)
	body = append(body, []byte(utils.ConfigGuide)...)
	// 0644, not 0777. keploy.yml carries `command:` -- the process Keploy
	// executes -- so a world-writable one hands every local user a way to
	// change what runs on the next `keploy test`.
	if err := os.WriteFile(filePath, body, 0644); err != nil {
		utils.LogError(logger, err, "failed to write config file",
			zap.String("path", filePath),
			zap.String("next_step", "verify the directory exists and the user running keploy has write permission; remove any read-only keploy.yml left over from a prior run before re-invoking `keploy config --generate`"))
		return err
	}
	if err := restrictConfig(filePath); err != nil {
		utils.LogError(logger, err, "failed to set the permission of config file")
		return fmt.Errorf("wrote %s but could not set its permissions: %w", filePath, err)
	}
	utils.RestoreFileOwnership(logger, filePath)
	return nil
}

// restrictConfig takes the group and world WRITE bits off a config file,
// leaving everything else as the user had it.
//
// keploy.yml names the process Keploy executes, so a world-writable one hands
// every local user a way to change what runs on the next `keploy test`.
// Forcing an exact 0644 instead would WIDEN a file somebody had deliberately
// restricted -- the config can carry mongoPassword -- so only the dangerous
// bits come off.
// resolveAbs follows every link in a path and makes the result absolute, so
// two paths can be compared for containment.
func resolveAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// RestrictConfigMode is restrictConfig, for the CLI's own defaults writer.
func RestrictConfigMode(path string) error { return restrictConfig(path) }

func restrictConfig(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode&0o022 == 0 {
		return nil
	}
	return os.Chmod(path, mode&^0o022)
}

// placeholders are the few settings a human actually opens this file to set,
// commented out, for the case where they have set none of them.
//
// A config of nothing but what differs is right, and for a fresh repository
// that is two lines -- both of them settings nobody should touch. The file
// then has nothing in it to edit and no clue about what could be. Commented
// lines are not settings: they change nothing until somebody uncomments one.
func placeholders(short string) string {
	// Flat keys only, and only ones the file does not already set.
	// Uncommenting a nested block (`test:` with a child) next to a `test:`
	// the file already has would give the document two of that key, and YAML
	// refuses a document with a duplicated key outright -- a placeholder
	// that can break the file it is trying to help with is worse than none.
	offer := [][2]string{
		{"command", `# command: ""            # the test command Keploy wraps, e.g. "npm test"`},
		{"containerName", `# containerName: ""      # the container your tests run in, if they run in one`},
		{"appName", `# appName: ""            # what this service is called in reports`},
	}
	// Settings only. Scanning the whole document let a user's own note --
	// "# command: npm test" jotted above their config -- suppress the
	// placeholder for a key they had not actually set.
	set := map[string]bool{}
	for _, line := range strings.Split(short, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if i := strings.Index(t, ":"); i > 0 {
			set[strings.TrimSpace(t[:i])] = true
		}
	}
	var lines []string
	for _, o := range offer {
		if !set[o[0]] {
			lines = append(lines, o[1])
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n# The settings most projects set. Uncomment and fill in what you need;\n" +
		"# everything else keeps the default (keploy config defaults prints them all).\n#\n" +
		strings.Join(lines, "\n") + "\n"
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
			return fmt.Errorf("failed to assemble the default config: %w", err)
		}
		data = []byte(configData)
	}

	if err := yamlLib.Unmarshal(data, &node); err != nil {
		// Returned, not swallowed: the caller prints "Config file generated
		// successfully" on a nil error, and did so for a document it had
		// never written.
		utils.LogError(t.logger, err, "failed to unmarshal the config")
		return fmt.Errorf("failed to read the config document: %w", err)
	}

	// A document with no content -- all comments, or empty -- has no root
	// mapping to marshal below. Unreachable while every generated config
	// was a full document; now that a config holds only what a developer
	// decided, a file of nothing but the header is the ordinary shape, and
	// the first caller to read one back and hand it here would crash.
	if len(node.Content) == 0 {
		// The SAME file every other path writes -- header, body, guide,
		// 0644. Writing the raw bytes here instead gave a comments-only
		// config a different shape from every other one, and left it
		// world-writable.
		body := append([]byte(utils.GetVersionAsComment()), []byte(configData)...)
		body = append(body, []byte(utils.ConfigGuide)...)
		if err := os.WriteFile(filePath, body, 0644); err != nil {
			utils.LogError(t.logger, err, "failed to write config file", zap.String("path", filePath))
			return err
		}
		if err := restrictConfig(filePath); err != nil {
			utils.LogError(t.logger, err, "failed to set the permission of config file")
			return fmt.Errorf("wrote %s but could not set its permissions: %w", filePath, err)
		}
		utils.RestoreFileOwnership(t.logger, filePath)
		return nil
	}

	if len(node.Content) > 0 { // we don't need agent config in the config file. All the config of the agent will be managed internally
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
		return fmt.Errorf("failed to write the config document: %w", err)
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)
	finalOutput = append([]byte(utils.GetVersionAsComment()), finalOutput...)

	// 0644: keploy.yml names the process Keploy executes, so a
	// world-writable one hands every local user a way to change it.
	err = os.WriteFile(filePath, finalOutput, 0644)
	if err != nil {
		// Return the error so callers (cli/config.go handler,
		// CmdConfigurator.CreateConfigFile) don't falsely claim
		// "Config file generated successfully" — they each check
		// the error already. Prior behavior of returning nil
		// here was a latent lie that let CI scripts see a
		// "success" line plus an ERROR line for the same op.
		utils.LogError(t.logger, err, "failed to write config file",
			zap.String("path", filePath),
			zap.String("next_step", "verify the directory exists and the user running keploy has write permission; remove any read-only keploy.yml left over from a prior run (e.g. via sudo chown or rm) before re-invoking `keploy config --generate`"),
		)
		return err
	}

	// 0644, matching the write above. This used to be 0777, which undid it.
	// Returned, not logged past: os.WriteFile leaves an existing file's mode
	// alone, so a failed chmod means "Config file generated successfully"
	// over a keploy.yml still at 0777 -- and keploy.yml names the process
	// Keploy executes.
	if err := restrictConfig(filePath); err != nil {
		utils.LogError(t.logger, err, "failed to set the permission of config file")
		return fmt.Errorf("wrote %s but could not set its permissions: %w", filePath, err)
	}
	utils.RestoreFileOwnership(t.logger, filePath)

	return nil
}

func (t *Tools) IgnoreTests(_ context.Context, _ string, _ []string) error {
	return nil
}

func (t *Tools) IgnoreTestSet(_ context.Context, _ string) error {
	return nil
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
		t.logger.Debug("No test sets found to templatize")
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
			t.logger.Debug("The test set is empty. Please record some test cases to templatize.", zap.String("testSet", testSetID))
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
