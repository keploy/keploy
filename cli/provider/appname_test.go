package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// inCheckoutFolder makes a git checkout of acme/orders-api, moves the test
// into its folder a/b, and returns that folder: a new app made there is
// "orders-api-a-b", and the name runs from it had before is "b".
func inCheckoutFolder(t *testing.T) string {
	t.Helper()
	top := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(top, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/orders-api.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(top, "a", "b")
	if err := os.MkdirAll(filepath.Join(work, "keploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	return work
}

// AppName keeps a name the run was given; names a run with no keploy.yml
// after its repository and path; and names a run whose keploy.yml names no
// app (an existing project) after its folder, as runs named it before, and
// writes that name into the file once, unless it may not write (the agent)
// or cannot (the file is one keploy does not rewrite: it warns).
func TestAppName(t *testing.T) {
	for _, c := range []struct {
		name, configured, doc string
		noFile, write, up     bool
		want, wantDoc, says   string
		level                 zapcore.Level
	}{
		{name: "a name given, no keploy.yml", configured: "billing", noFile: true, write: true, want: "billing"},
		{name: "a name given, a keploy.yml naming none", configured: "billing", doc: "path: x\n", write: true, want: "billing", wantDoc: "path: x\n"},
		{name: "no keploy.yml", noFile: true, write: true, want: "orders-api-a-b", says: `Naming the app "orders-api-a-b" after the git remote origin and the path a/b`, level: zapcore.InfoLevel},
		{name: "a keploy.yml naming no app", doc: "# mine\npath: x\n", write: true, want: "b", wantDoc: "# mine\nappName: \"b\"\npath: x\n", says: `named no app, so keploy wrote appName "b" into it`, level: zapcore.InfoLevel},
		{name: "the agent writes none", doc: "path: x\n", want: "b", wantDoc: "path: x\n"},
		{name: "a keploy.yml in another folder is not written", doc: "path: x\n", up: true, write: true, want: "b", wantDoc: "path: x\n"},
		{name: "a keploy.yml keploy does not rewrite", doc: "{path: x}\n", write: true, want: "b", wantDoc: "{path: x}\n", says: `names no app, so this run uses "b"`, level: zapcore.WarnLevel},
	} {
		t.Run(c.name, func(t *testing.T) {
			work := inCheckoutFolder(t)
			file := ""
			if !c.noFile {
				file = filepath.Join(work, "keploy.yml")
				if c.up {
					file = filepath.Join(filepath.Dir(work), "keploy.yml")
				}
				if err := os.WriteFile(file, []byte(c.doc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			core, logs := observer.New(zapcore.DebugLevel)
			got, err := AppName(zap.New(core), c.configured, file, c.write)
			if err != nil {
				t.Fatalf("AppName: %v", err)
			}
			if got != c.want {
				t.Errorf("AppName = %q, want %q", got, c.want)
			}
			if file != "" {
				if b, _ := os.ReadFile(file); string(b) != c.wantDoc {
					t.Errorf("keploy.yml =\n%q\nwant\n%q", b, c.wantDoc)
				}
			}
			said := logs.All()
			if c.says == "" {
				if len(said) != 0 {
					t.Errorf("AppName said %q, want nothing", said[0].Message)
				}
				return
			}
			if len(said) != 1 || said[0].Level != c.level || !strings.Contains(said[0].Message, c.says) {
				t.Errorf("AppName said %v, want one %v line with %q", said, c.level, c.says)
			}
		})
	}
}

// An existing project's keploy.yml that names no app, one a keploy before
// apps were named wrote (appName: "") among them, keeps the name its runs
// had, the folder's ("b"), not the new default ("orders-api-a-b"): record
// writes the name into it once, and the next run reads it and leaves the
// file as it is. The agent, which reads the keploy.yml its parent wrote,
// never writes it.
func TestRecordKeepsAnExistingProjectsAppName(t *testing.T) {
	for _, c := range []struct{ name, cmd, doc, want string }{
		{"no appName", "record", "# my project\ncommand: ./app\n", "# my project\nappName: \"b\"\ncommand: ./app\n"},
		{"appName \"\" of an older keploy", "record", "appName: \"\"\ncommand: ./app\n", "appName: \"b\"\ncommand: ./app\n"},
		{"the agent", "agent", "# my project\ncommand: ./app\n", "# my project\ncommand: ./app\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			original := NativeCommandSupported
			t.Cleanup(func() { NativeCommandSupported = original })
			RegisterNativeCommandSupport(func(string, string) bool { return true })
			t.Cleanup(viper.Reset)
			t.Cleanup(log.TestOnlyResetSink)
			// The agent pins the proven switch from its --proven flag.
			t.Cleanup(provenswitch.Unpin)
			found, file := IsConfigFileFound, ConfigFile
			t.Cleanup(func() { IsConfigFileFound, ConfigFile = found, file })

			work := inCheckoutFolder(t)
			path := filepath.Join(work, "keploy.yml")
			if err := os.WriteFile(path, []byte(c.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			for run := 1; run <= 2; run++ {
				viper.Reset()
				c2 := NewCmdConfigurator(zap.NewNop(), config.New())
				cmd := &cobra.Command{Use: c.cmd}
				if err := c2.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags: %v", err)
				}
				args := []string{"--config-path", work}
				if c.cmd == "record" {
					args = append(args, "-c", "./app")
				}
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				cmd.SetContext(context.Background())
				if err := c2.Validate(context.Background(), cmd); err != nil {
					t.Fatalf("run %d: Validate: %v", run, err)
				}
				if c2.cfg.AppName != "b" {
					t.Errorf("run %d: appName = %q, want the folder's \"b\"", run, c2.cfg.AppName)
				}
				if b, _ := os.ReadFile(path); string(b) != c.want {
					t.Errorf("run %d: keploy.yml =\n%q\nwant\n%q", run, b, c.want)
				}
			}
		})
	}
}
