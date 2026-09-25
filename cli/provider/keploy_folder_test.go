package provider

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// keploy keeps its tests, mocks and reports in <path>/keploy, and the binary
// is called keploy. Under the default --path, a project the binary was
// downloaded into has a FILE there. Every command whose folder cmd.go resolves,
// and that opens it, has to refuse it there (keployFolder). Before, only
// native record, test and mock met a check, the permission check. The rest
// went on, failed later with "readdirent <path>/keploy: not a directory",
// which names neither the cause nor the way out, or did nothing, and exited 0.
//
// This drives the real flag wiring -- AddFlags, PreProcessFlags, ValidateFlags
// -- for every command whose folder cmd.go resolves. record, test and mock run
// in docker mode, which skips the permission check, so only the folder check
// can stop them.
//
// `report --report-path <file>` is the one run that never opens the folder: it
// reports on that file alone. It must go through whatever sits at
// <path>/keploy, as it did before the check existed.
func TestEveryCommandRefusesAFileAtTheKeployFolder(t *testing.T) {
	const (
		stale         = "stale file"
		binary        = "the running keploy binary"
		oldCopy       = "another copy of keploy"
		folder        = "a keploy folder"
		folderSymlink = "a symlink to a keploy folder"
		nothing       = "no keploy folder yet"
	)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// A report file for --report-path. ValidateFlags only checks that it is a
	// file; it is never parsed here.
	reportFile := filepath.Join(t.TempDir(), "test-set-0-report.yaml")
	if err := os.WriteFile(reportFile, []byte("name: test-set-0-report\ntests: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A keploy that is not the one running: an old download beside an
	// installed keploy, say. It is a byte-for-byte copy with the exec bit, a
	// new file, so only file identity tells it from the running binary.
	// Telling its owner to move it into /usr/local/bin would overwrite the
	// keploy they actually run. Made once and shared: every run refuses it.
	copyProject := t.TempDir()
	copyExecutable(t, exe, filepath.Join(copyProject, "keploy"))

	docker := []string{"-c", "docker compose up", "--container-name", "app"}
	for _, sub := range []struct {
		parent, name string
		args         []string
		docker       bool // args run the app in docker mode
		contract     bool // also resolves cfg.Contract.Path
		noFolder     bool // never opens the folder: nothing there is refused
	}{
		{name: "record", args: docker, docker: true},
		{name: "test", args: docker, docker: true},
		{parent: "mock", name: "record", args: docker, docker: true},
		{parent: "mock", name: "replay", args: docker, docker: true},
		{name: "report"},
		{name: "report", args: []string{"--report-path", reportFile}, noFolder: true},
		{name: "diff"},
		{name: "sanitize"},
		{name: "normalize"},
		{name: "templatize"},
		{parent: "contract", name: "generate", contract: true},
		{parent: "contract", name: "download", contract: true},
		{parent: "contract", name: "test", contract: true},
	} {
		for _, what := range []string{stale, binary, oldCopy, folder, folderSymlink, nothing} {
			// `keploy test` refuses a run with no keploy folder at all ("No
			// test-sets found"), so it has no "nothing" case here; that
			// refusal is TestValidateFlags_TestWithNoKeployFolderReturnsInsteadOfExiting.
			if sub.parent == "" && sub.name == "test" && what == nothing {
				continue
			}
			label := strings.TrimSpace(sub.parent + " " + sub.name)
			if sub.noFolder {
				label += " " + sub.args[0]
			}
			t.Run(label+"/"+what, func(t *testing.T) {
				viper.Reset()
				savedFound := IsConfigFileFound
				t.Cleanup(func() { IsConfigFileFound = savedFound; viper.Reset() })

				project := t.TempDir()
				switch what {
				case stale:
					if err := os.WriteFile(filepath.Join(project, "keploy"), []byte("an old download, or anything"), 0o600); err != nil {
						t.Fatal(err)
					}
				case binary:
					// os.Executable is this test binary: a symlink to it
					// stats as the same file, as the downloaded keploy
					// would be to itself.
					if err := os.Symlink(exe, filepath.Join(project, "keploy")); err != nil {
						t.Skipf("cannot symlink here: %v", err)
					}
				case oldCopy:
					project = copyProject
				case folder:
					if err := os.Mkdir(filepath.Join(project, "keploy"), 0o755); err != nil {
						t.Fatal(err)
					}
				case folderSymlink:
					// A test store kept elsewhere (shared, or mounted) and
					// linked in: a folder, reached through a link.
					if err := os.Symlink(t.TempDir(), filepath.Join(project, "keploy")); err != nil {
						t.Skipf("cannot symlink here: %v", err)
					}
				}
				keployPath := filepath.Join(project, "keploy")

				cfg := config.New()
				c := NewCmdConfigurator(zap.NewNop(), cfg)
				cmd := &cobra.Command{Use: sub.name}
				if sub.parent != "" {
					(&cobra.Command{Use: sub.parent}).AddCommand(cmd)
				}
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags: %v", err)
				}
				args := append(append([]string{}, sub.args...),
					"--path", project,
					// An empty directory: no keploy.yml, so nothing but the
					// flags decides the run.
					"--config-path", t.TempDir(),
				)
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				err := c.ValidateFlags(context.Background(), cmd)
				if sub.docker && cfg.CommandType != "docker-compose" {
					t.Fatalf("precondition: command type resolved to %q, want docker-compose", cfg.CommandType)
				}

				// cmd.go joins "/keploy" onto the absolute --path, so on
				// Windows its separators are mixed; compare paths cleaned.
				if sub.noFolder {
					if err != nil {
						t.Fatalf("refused a run that never opens the keploy folder, over %s there: %v", what, err)
					}
					if cfg.Report.ReportPath != reportFile {
						t.Errorf("the report file resolved to %q, want %q", cfg.Report.ReportPath, reportFile)
					}
					// The stores are still built from cfg.Path; it names the
					// folder as it always did, checked or not.
					if filepath.Clean(cfg.Path) != keployPath {
						t.Errorf("the keploy folder resolved to %q, want %q", cfg.Path, keployPath)
					}
					return
				}

				switch what {
				case folder, folderSymlink, nothing:
					if err != nil {
						t.Fatalf("refused a run whose keploy folder is fine (%s): %v", what, err)
					}
					if filepath.Clean(cfg.Path) != keployPath {
						t.Errorf("the keploy folder resolved to %q, want %q", cfg.Path, keployPath)
					}
					if sub.contract && filepath.Clean(cfg.Contract.Path) != keployPath {
						t.Errorf("the contract folder resolved to %q, want %q", cfg.Contract.Path, keployPath)
					}
					return
				case stale, oldCopy:
					wantAll(t, err, keployPath, "is a file, not a folder", "remove or rename that file", "--path")
					if err != nil && strings.Contains(err.Error(), "/usr/local/bin") {
						t.Errorf("told to move a file that is not the running keploy into /usr/local/bin, over the installed one: %v", err)
					}
				case binary:
					wantAll(t, err, keployPath, "is this keploy binary, not a folder", "move the keploy binary out of this folder", "--path")
				}
				// A refused run resolves no folder. main restores the keploy
				// folder's ownership after the command (chown -R, under sudo)
				// wherever cfg.Path points; it must not point at the file.
				if cfg.Path != "" {
					t.Errorf("a refused run left the keploy folder set to %q", cfg.Path)
				}
			})
		}
	}
}

// wantAll matches separator-blind: on Windows the path in the error is joined
// with "/" onto a path that uses backslashes.
func wantAll(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted a file where keploy keeps its tests; want an error saying %q", wants)
	}
	for _, w := range wants {
		if !strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(w)) {
			t.Errorf("the error does not say %q: %v", w, err)
		}
	}
}

// copyExecutable writes a byte-for-byte copy of src to dst, executable: a new
// file, never a link to src.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
