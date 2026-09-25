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
	downloadURL, err := updateDownloadURL(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	err = t.downloadAndUpdate(ctx, t.logger, downloadURL)
	if err != nil {
		return err
	}

	t.logger.Info("Update Successful!")
	fmt.Println(releaseNotes(t.logger, changelog))
	return nil
}

// releaseNotes is the changelog as the terminal shows it after an update:
// rendered, or as it came when it cannot be.
//
// The update has already happened by then -- the binary is replaced -- so
// nothing here may fail it. Returning the renderer's error did: cli/update.go
// exits non-zero on any Update error, and a GLAMOUR_STYLE naming a missing
// file made `keploy update` report a failed update over one that had
// succeeded.
func releaseNotes(logger *zap.Logger, changelog string) string {
	changelog = "\n" + changelog
	renderer, err := glamour.NewTermRenderer(glamour.WithEnvironmentConfig(), glamour.WithWordWrap(0))
	if err == nil {
		var rendered string
		if rendered, err = renderer.Render(changelog); err == nil {
			return rendered
		}
	}
	logger.Warn("could not format the release notes; showing them unformatted", zap.Error(err))
	return changelog
}

// updateDownloadURL picks the latest-release archive for the running
// platform. Asset names follow release.yml's keploy_<os>_<arch>.tar.gz.
// macOS is published for arm64 only, so an Intel Mac gets an error rather
// than a download that cannot run. That refusal also sets the process exit
// code here, at the point where the platform is judged unsupported:
// cli/update.go arms utils.ExitCodeFor(err) for an Update error, which is the
// generic 1 for every error Update returns -- none carries a tag -- so without
// it `keploy update` on an Intel Mac could not say "no build for this
// platform".
func updateDownloadURL(goos, goarch string) (string, error) {
	const base = "https://github.com/keploy/keploy/releases/latest/download/"
	switch goos {
	case "linux":
		if goarch == "amd64" {
			return base + "keploy_linux_amd64.tar.gz", nil
		}
		return base + "keploy_linux_arm64.tar.gz", nil
	case "darwin":
		if goarch != "arm64" {
			// Retrying cannot help -- there is no asset for this OS/arch -- so
			// give the caller the code that says so rather than a generic 1.
			utils.SetExitCodeOnce(utils.ExitUnsupportedPlatform)
			return "", fmt.Errorf("keploy's native macOS build is Apple Silicon (arm64) only; this Mac is %s. On an Intel Mac, run Keploy inside Lima: https://keploy.io/docs/installation/macos-installation/#option-2-install-keploy-with-lima", goarch)
		}
		return base + "keploy_darwin_arm64.tar.gz", nil
	}
	return "", nil
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

	// A non-200 body is NOT an archive. Unchecked, GitHub's 404 page was
	// io.Copy'd into the .tar.gz below and only surfaced as "failed to extract
	// tar.gz file" -- and cli/update.go, which then returned nil on every
	// Update error, exited 0 over it. That is how a release missing an asset
	// for this platform looks to every keploy already installed, so it has to
	// be the download's own error, named, with the exit code that says so
	// behind it: cli/update.go's utils.ExitCodeFor(err) is the generic 1 for
	// this untagged error.
	if resp.StatusCode != http.StatusOK {
		utils.SetExitCodeOnce(utils.ExitUnsupportedPlatform)
		return fmt.Errorf("release asset %s is not available (HTTP %s) -- this release may not publish a binary for %s/%s",
			downloadURL, resp.Status, runtime.GOOS, runtime.GOARCH)
	}

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
	// Never through a link to somewhere the user did not name. os.WriteFile
	// follows one, so a keploy.yml that is a symlink -- dangling or not --
	// sent the generated config outside the project with no prompt and exit
	// 0. A link resolving INSIDE the directory is an ordinary monorepo
	// layout and is honoured; anything else is refused by name.
	//
	// The write then goes to the RESOLVED path, not through the link: the
	// path that was checked has to be the path that is opened, or the check
	// is describing a different file from the one that gets written.
	filePath, err := ResolveConfigTarget(filePath)
	if err != nil {
		return err
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
	//
	// O_NOFOLLOW, because the path was checked a moment ago and os.WriteFile
	// follows whatever is there NOW. When the file does not exist yet --
	// which is the ordinary first run -- a link planted between the check and
	// the write sends the config wherever it points: measured at roughly one
	// attempt in six against a process doing nothing more exotic than
	// `ln -sf` in a loop. filePath here is already fully resolved, so it can
	// never legitimately BE a link, and refusing to follow one costs nothing.
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|oNoFollow, 0644)
	if err != nil {
		utils.LogError(logger, err, "failed to write config file",
			zap.String("path", filePath),
			zap.String("next_step", "verify the directory exists and the user running keploy has write permission; remove any read-only keploy.yml left over from a prior run before re-invoking `keploy config --generate`"))
		return err
	}
	if _, werr := f.Write(body); werr != nil {
		_ = f.Close()
		utils.LogError(logger, werr, "failed to write config file", zap.String("path", filePath))
		return werr
	}
	// Mode and ownership on the OPEN FILE, before it is closed.
	//
	// Doing them by name afterwards re-resolves the path, and os.Stat,
	// os.Chmod and os.Chown all follow links: the same `ln -sf` race the open
	// itself refuses, landing a moment later, made Keploy chmod and chown a
	// file OUTSIDE the directory. Under `sudo keploy config --generate` --
	// the documented way to run Keploy on Linux -- that chown hands a
	// root-owned file to the unprivileged invoking user. The descriptor
	// cannot be redirected, so nothing can be substituted for it.
	if err := restrictConfigFile(f); err != nil {
		_ = f.Close()
		utils.LogError(logger, err, "failed to set the permission of config file")
		return fmt.Errorf("wrote %s but could not set its permissions: %w", filePath, err)
	}
	utils.RestoreFileOwnershipOf(logger, f, filePath)
	if cerr := f.Close(); cerr != nil {
		utils.LogError(logger, cerr, "failed to write config file", zap.String("path", filePath))
		return cerr
	}
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

// withinDir walks up at most this far. filepath.Dir reaches the root and stays
// there, so the walk terminates on its own; this only bounds a pathological
// one, and is far past any real project depth.
const maxDirDepth = 4096

// ResolveConfigTarget answers the only question that matters before writing a
// config: which file would this actually land on, and is it inside the
// directory the user named?
//
// It returns that real path, or refuses. Callers write to what it returns --
// never through the original path -- so the file that was checked is the file
// that is opened.
//
// What it decides is where the PATH leads. A hard link, or a directory bind-
// mounted into the project, is a second name for a file that genuinely is
// inside: no amount of path resolution can see past that, and writing to it
// writes to the other name too. The guarantee is "this path does not lead out
// of the directory you named", not "nothing outside it can be reached".
//
// Every component that EXISTS is resolved by filepath.EvalSymlinks, and only
// a final name that does not exist yet is ever joined on. That division is the
// whole correctness argument. filepath.Join and filepath.Abs clean ".."
// LEXICALLY, before the component in front of it has been resolved, while the
// kernel resolves the link first and then takes ".." of the REAL parent -- so
// a target spelled "a/sub/../b.yml", where a/sub is a link out of the
// project, was computed as being inside it and written to anyway.
// EvalSymlinks gets this right, because it strips ".." from the already
// resolved prefix; the job here is to let it, and to stop cleaning paths by
// hand.
func ResolveConfigTarget(filePath string) (string, error) {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return "", err
	}
	// The directory the user named, by identity. It has to exist -- nothing
	// can be written into it otherwise -- so this resolution is exact.
	dirReal, err := resolveAbs(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("%s cannot be resolved: %w", filepath.Dir(filePath), err)
	}
	dirInfo, err := os.Stat(dirReal)
	if err != nil {
		return "", err
	}

	target, err := resolveLeaf(filepath.Join(dirReal, filepath.Base(abs)))
	if err != nil {
		// Only say "symbolic link" when there is one. A directory that is
		// really a file, or one this user cannot read, failed here too and
		// was reported as a link problem with no link anywhere in the path --
		// and the message displaced the one that says to check the directory
		// and its permissions.
		if st, lerr := os.Lstat(abs); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link whose target could not be resolved: %w", filePath, err)
		}
		return "", fmt.Errorf("%s cannot be resolved: %w; verify the directory exists and the user running keploy can read it", filePath, err)
	}
	if !withinDir(filepath.Dir(target), dirInfo) {
		return "", fmt.Errorf("%s is a symbolic link pointing outside %s; Keploy will not write through it -- remove the link, or generate the config elsewhere with --path",
			filePath, filepath.Dir(filePath))
	}
	return target, nil
}

// resolveLeaf follows a chain of links to the file a write would land on. The
// last hop is allowed not to exist yet -- a keploy.yml committed as a link
// points at the file the generator has not written BY DEFINITION -- but every
// directory along the way must, or no write could land there either.
func resolveLeaf(p string) (string, error) {
	cur := p
	// The kernel's own ceiling is in this range; a cycle otherwise spins here
	// for ever.
	for i := 0; i < 40; i++ {
		dest, err := os.Readlink(cur)
		if err != nil {
			// Not a link. Either it exists -- resolve it exactly -- or it is
			// the missing leaf, and only its name may be joined on.
			if real, rerr := filepath.EvalSymlinks(cur); rerr == nil {
				return real, nil
			} else if !errors.Is(rerr, fs.ErrNotExist) {
				return "", rerr
			}
			parentRaw, leaf := splitLast(cur)
			if leaf == "" || leaf == "." || leaf == ".." {
				return "", fmt.Errorf("%s does not name a file", p)
			}
			parent, perr := filepath.EvalSymlinks(parentRaw)
			if perr != nil {
				return "", perr
			}
			return filepath.Join(parent, leaf), nil
		}
		if !filepath.IsAbs(dest) {
			// The directory the LINK sits in, resolved first, then the target
			// appended WITHOUT cleaning: filepath.Join would collapse a ".."
			// against the unresolved spelling. The next turn of this loop
			// hands the uncleaned path back to the OS, which walks it the way
			// the kernel will.
			parentRaw, _ := splitLast(cur)
			parent, perr := filepath.EvalSymlinks(parentRaw)
			if perr != nil {
				return "", perr
			}
			dest = parent + string(os.PathSeparator) + dest
		}
		cur = dest
	}
	return "", fmt.Errorf("too many levels of symbolic links under %s", p)
}

// splitLast cuts the last component off a path WITHOUT cleaning it.
// filepath.Dir cleans, which turns ".../a/sub/.." into ".../a" before `sub`
// has been resolved -- the very mistake this file now exists to avoid.
func splitLast(p string) (dir, last string) {
	// "C:" is drive-RELATIVE on Windows -- it means the current directory on
	// that drive, not its root -- so the volume has to stay attached to the
	// separator. On Unix VolumeName is always empty and this is a no-op.
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	i := strings.LastIndex(rest, string(os.PathSeparator))
	if i < 0 {
		if vol == "" {
			return ".", rest
		}
		return vol + ".", rest
	}
	if i == 0 {
		return vol + string(os.PathSeparator), rest[1:]
	}
	return vol + rest[:i], rest[i+1:]
}

// withinDir reports whether `child` is `dir` or sits under it, by IDENTITY.
//
// A string prefix is both too strict and too loose: on a case-insensitive
// volume two spellings of one directory compare unequal (a link squarely
// inside was refused with a message saying it pointed outside), and any
// spelling difference at all defeats it. os.SameFile compares the inode, so
// how the path was typed stops mattering.
func withinDir(child string, dir os.FileInfo) bool {
	cur := child
	for i := 0; i < maxDirDepth; i++ {
		info, err := os.Stat(cur)
		if err != nil {
			return false
		}
		if os.SameFile(info, dir) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

// RestrictConfigMode is restrictConfig, for the CLI's own defaults writer.
func RestrictConfigMode(path string) error { return restrictConfig(path) }

func restrictConfig(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return narrowMode(info.Mode().Perm(), func(m os.FileMode) error { return os.Chmod(path, m) })
}

// restrictConfigFile is restrictConfig on an open descriptor, so no link can
// be swapped in between deciding the mode and applying it.
func restrictConfigFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return narrowMode(info.Mode().Perm(), f.Chmod)
}

func narrowMode(mode os.FileMode, chmod func(os.FileMode) error) error {
	if mode&0o022 == 0 {
		return nil
	}
	return chmod(mode &^ 0o022)
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
