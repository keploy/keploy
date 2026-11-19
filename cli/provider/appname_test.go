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

// AppName keeps a name the run was given, unless it is blank (a blank one
// names no app); names a run with no keploy.yml
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
		{name: "a blank name given, no keploy.yml", configured: "  ", noFile: true, write: true, want: "orders-api-a-b", says: `Naming the app "orders-api-a-b" after the git remote origin and the path a/b`, level: zapcore.InfoLevel},
		{name: "a blank name given, a keploy.yml naming none", configured: " \t", doc: "path: x\n", write: true, want: "b", wantDoc: "appName: \"b\" " + config.KeptAppNameNote + "\npath: x\n", says: `named no app, so keploy wrote appName "b" into it`, level: zapcore.InfoLevel},
		{name: "a blank name written as an escape is not rewritten", configured: "\t", doc: "appName: \"\\t\"\n", write: true, want: "b", wantDoc: "appName: \"\\t\"\n", says: `names no app, so this run uses "b"`, level: zapcore.WarnLevel},
		{name: "a name between spaces is kept as given", configured: " billing ", noFile: true, write: true, want: " billing "},
		{name: "a keploy.yml naming no app", doc: "# mine\npath: x\n", write: true, want: "b", wantDoc: "# mine\nappName: \"b\" " + config.KeptAppNameNote + "\npath: x\n", says: `named no app, so keploy wrote appName "b" into it`, level: zapcore.InfoLevel},
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
// apps were named wrote (appName: "") and a null or blank appName among
// them, keeps the name its runs
// had, the folder's ("b"), not the new default ("orders-api-a-b"): record
// writes the name into it once, and the next run reads it and leaves the
// file as it is. The agent, which reads the keploy.yml its parent wrote,
// never writes it, nor does any other command that neither records nor
// tests (templatize here): it reads the name and leaves the file byte for
// byte.
func TestRecordKeepsAnExistingProjectsAppName(t *testing.T) {
	for _, c := range []struct{ name, cmd, doc, want string }{
		{"no appName", "record", "# my project\ncommand: ./app\n", "# my project\nappName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName \"\" of an older keploy", "record", "appName: \"\"\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName null", "record", "appName: null\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName ~", "record", "appName: ~\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName Null", "record", "appName: Null\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName of spaces", "record", "appName: \"  \"\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName of a tab, single-quoted", "record", "appName: '\t'\ncommand: ./app\n", "appName: \"b\" " + config.KeptAppNameNote + "\ncommand: ./app\n"},
		{"appName of spaces, the agent", "agent", "appName: \"  \"\ncommand: ./app\n", "appName: \"  \"\ncommand: ./app\n"},
		{"the agent", "agent", "# my project\ncommand: ./app\n", "# my project\ncommand: ./app\n"},
		{"a command that neither records nor tests", "templatize", "# my project\ncommand: ./app\n", "# my project\ncommand: ./app\n"},
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

// A blank or empty --app-name or KEPLOY_APPNAME names no app, so it is no
// choice: keploy.yml's appName names the run, and with no keploy.yml the git
// repository's name and path, else the folder's. The run warns once for each
// blank name it ignores, never that keploy.yml names no app, and leaves
// keploy.yml as it is. The flag then reads as not given, and a blank variable
// is unset, while a KEPLOY_APPNAME that names an app stays set and names the
// run. A name given that is not blank still wins over keploy.yml, and an
// empty KEPLOY_APPNAME, which viper never read, is not warned about. A blank,
// empty or null appName in the override file <folder>.keploy.yml is no choice
// either: keploy.yml's appName names the run, with no warning; an override
// that names an app still wins.
func TestABlankGivenNameIsNoChoice(t *testing.T) {
	const orders = "appName: orders\ncommand: ./app\n"
	for _, c := range []struct {
		name, doc string
		flag      []string
		env       *string
		override  string
		noRepo    bool
		want      string
		warns     int
	}{
		{name: "--app-name of spaces over keploy.yml", doc: orders, flag: []string{"--app-name", "  "}, want: "orders", warns: 1},
		{name: "--app-name empty over keploy.yml", doc: orders, flag: []string{"--app-name", ""}, want: "orders", warns: 1},
		{name: "--app-name of a tab over keploy.yml", doc: orders, flag: []string{"--app-name=\t"}, want: "orders", warns: 1},
		{name: "KEPLOY_APPNAME of spaces over keploy.yml", doc: orders, env: strPtr("  "), want: "orders", warns: 1},
		{name: "KEPLOY_APPNAME empty over keploy.yml", doc: orders, env: strPtr(""), want: "orders"},
		{name: "nothing given over keploy.yml", doc: orders, want: "orders"},
		{name: "--app-name of spaces, no keploy.yml, in a repository", flag: []string{"--app-name", "  "}, want: "orders-api-a-b", warns: 1},
		{name: "KEPLOY_APPNAME of spaces, no keploy.yml, in a repository", env: strPtr(" "), want: "orders-api-a-b", warns: 1},
		{name: "--app-name of spaces, no keploy.yml, outside a repository", flag: []string{"--app-name", "  "}, noRepo: true, want: "svc", warns: 1},
		{name: "a name given still wins over keploy.yml", doc: orders, flag: []string{"--app-name", "billing"}, want: "billing"},
		{name: "a KEPLOY_APPNAME still wins over keploy.yml", doc: orders, env: strPtr("billing"), want: "billing"},
		{name: "--app-name and KEPLOY_APPNAME both of spaces over keploy.yml", doc: orders, flag: []string{"--app-name", "  "}, env: strPtr("  "), want: "orders", warns: 2},
		{name: "--app-name of spaces with a KEPLOY_APPNAME that names an app", doc: orders, flag: []string{"--app-name", "  "}, env: strPtr("billing"), want: "billing", warns: 1},
		{name: "KEPLOY_APPNAME of a tab over keploy.yml", doc: orders, env: strPtr("\t"), want: "orders", warns: 1},
		{name: "an override file's appName of spaces over keploy.yml", doc: orders, override: "appName: \"  \"\n", want: "orders"},
		{name: "an override file's empty appName over keploy.yml", doc: orders, override: "appName: \"\"\n", want: "orders"},
		{name: "an override file's null appName over keploy.yml", doc: orders, override: "appName:\n", want: "orders"},
		{name: "an override file's blank appName with --app-name of spaces", doc: orders, flag: []string{"--app-name", "  "}, override: "appName: \" \"\n", want: "orders", warns: 1},
		{name: "an override file's appName still wins over keploy.yml", doc: orders, override: "appName: billing\n", want: "billing"},
		{name: "a KEPLOY_APPNAME still wins over an override file's blank appName", doc: orders, env: strPtr("billing"), override: "appName: \"  \"\n", want: "billing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			original := NativeCommandSupported
			t.Cleanup(func() { NativeCommandSupported = original })
			RegisterNativeCommandSupport(func(string, string) bool { return true })
			t.Cleanup(viper.Reset)
			t.Cleanup(log.TestOnlyResetSink)
			found, file := IsConfigFileFound, ConfigFile
			t.Cleanup(func() { IsConfigFileFound, ConfigFile = found, file })
			// t.Setenv puts back whatever the run unset.
			t.Setenv("KEPLOY_APPNAME", "")
			if err := os.Unsetenv("KEPLOY_APPNAME"); err != nil {
				t.Fatal(err)
			}
			if c.env != nil {
				t.Setenv("KEPLOY_APPNAME", *c.env)
			}

			var work string
			if c.noRepo {
				root := t.TempDir()
				t.Setenv("HOME", root)
				work = filepath.Join(root, "svc")
				if err := os.MkdirAll(filepath.Join(work, "keploy"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Chdir(work)
			} else {
				work = inCheckoutFolder(t)
			}
			path := filepath.Join(work, "keploy.yml")
			if c.doc != "" {
				if err := os.WriteFile(path, []byte(c.doc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.override != "" {
				if err := os.WriteFile(filepath.Join(work, filepath.Base(work)+".keploy.yml"), []byte(c.override), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			viper.Reset()
			core, logs := observer.New(zapcore.InfoLevel)
			c2 := NewCmdConfigurator(zap.New(core), config.New())
			cmd := &cobra.Command{Use: "record"}
			if err := c2.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			if err := cmd.ParseFlags(append([]string{"--config-path", work, "-c", "./app"}, c.flag...)); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			cmd.SetContext(context.Background())
			if err := c2.Validate(context.Background(), cmd); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if c2.cfg.AppName != c.want {
				t.Errorf("appName = %q, want %q", c2.cfg.AppName, c.want)
			}
			if c.doc != "" {
				if b, _ := os.ReadFile(path); string(b) != c.doc {
					t.Errorf("keploy.yml =\n%q\nwant it as it was:\n%q", b, c.doc)
				}
			}
			warned := 0
			for _, e := range logs.All() {
				if strings.Contains(e.Message, "names no app") || strings.Contains(e.Message, "named no app") {
					t.Errorf("the run said %q, but keploy.yml was not what named it", e.Message)
				}
				if e.Level >= zapcore.WarnLevel && strings.HasPrefix(e.Message, "ignoring ") {
					warned++
				}
			}
			if warned != c.warns {
				t.Errorf("the run warned %d times that it ignores a blank name, want %d: %v", warned, c.warns, logs.All())
			}
			if c.env != nil && *c.env != "" {
				v, set := os.LookupEnv("KEPLOY_APPNAME")
				if named := strings.TrimSpace(*c.env) != ""; named && v != *c.env {
					t.Errorf("KEPLOY_APPNAME after the run = %q (set %v), want %q kept", v, set, *c.env)
				} else if !named && set {
					t.Errorf("KEPLOY_APPNAME after the run = %q, want the blank variable unset", v)
				}
			}
			if len(c.flag) > 0 && strings.TrimSpace(strings.TrimPrefix(c.flag[len(c.flag)-1], "--app-name=")) == "" {
				f := cmd.Flags().Lookup("app-name")
				if f.Changed || f.Value.String() != f.DefValue {
					t.Errorf("--app-name after the run: given %v, value %q; want it not given, at its default %q", f.Changed, f.Value.String(), f.DefValue)
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// Only keploy record and keploy test, and their sandbox subcommands, write
// the app's name into keploy.yml, each by its path below the root. mock record
// and mock replay do not: their app is not keploy.yml's appName.
func TestRecordsOrTests(t *testing.T) {
	root := &cobra.Command{Use: "keploy"}
	leaf := func(parent *cobra.Command, name string) *cobra.Command {
		c := &cobra.Command{Use: name}
		parent.AddCommand(c)
		return c
	}
	mock, contract, app := leaf(root, "mock"), leaf(root, "contract"), leaf(root, "app")
	record, test := leaf(root, "record"), leaf(root, "test")
	local := leaf(leaf(root, "sandbox"), "local")
	for c, want := range map[*cobra.Command]bool{
		record:                  true,
		test:                    true,
		leaf(record, "sandbox"): true,
		leaf(test, "sandbox"):   true,
		leaf(mock, "record"):    false,
		leaf(mock, "replay"):    false,
		leaf(local, "replay"):   false,
		leaf(root, "agent"):     false,
		leaf(root, "report"):    false,
		leaf(root, "replay"):    false,
		leaf(contract, "test"):  false,
		leaf(mock, "upload"):    false,
		leaf(mock, "test"):      false,
		leaf(app, "create"):     false,
		leaf(local, "record"):   false,
		root:                    false,
		mock:                    false,
		// A command with no parent, as rows build one, by its own name.
		{Use: "record"}: true,
		{Use: "agent"}:  false,
	} {
		if got := RecordsOrTests(c); got != want {
			t.Errorf("RecordsOrTests(%q) = %v, want %v", c.CommandPath(), got, want)
		}
	}
}

// A working directory that is gone fails the run, as utils.WorkingDirectory
// does, whatever name the run was given.
func TestAppNameInAGoneWorkingDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("this system keeps a working directory from being removed: %v", err)
	}
	for _, configured := range []string{"", "orders"} {
		if name, err := AppName(zap.NewNop(), configured, "", false); err == nil {
			t.Errorf("AppName(%q) in a gone working directory = %q, want an error", configured, name)
		}
	}
}
