package tls

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// copyCA is the CA (decoded certificate-authority-data) of the first cluster
// of the kubeconfig at path.
func copyCA(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Data string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil || len(cfg.Clusters) == 0 {
		t.Fatalf("the copy %s is no kubeconfig with a cluster (%v)", path, err)
	}
	ca, err := base64.StdEncoding.DecodeString(cfg.Clusters[0].Cluster.Data)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// runUntilCleanup sets up a native run that ends with the test: its removal
// renames the run's directory, so the test waits for it before t.TempDir's
// cleanup lists the temp directory, or that cleanup finds a name it did not
// list and fails with "directory not empty".
func runUntilCleanup(t *testing.T, logger *zap.Logger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := SetupCaCertEnv(ctx, logger, testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	run := liveRunOf(t, filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS")))
	t.Cleanup(func() {
		cancel()
		waitRunGone(t, run)
	})
}

// A native run points kubectl and helm (KUBECONFIG) at a copy of the user's
// kubeconfig that trusts keploy's CA beside the cluster's own, readable by its
// owner alone (it holds the user's client key), in the run's directory. The
// user's file is the same byte for byte after the run, and the copy goes with
// the run. A second run in the same process copies the user's file again, not
// the first run's copy.
func TestSetupCaCertEnv_KubectlGetsACopyThatTrustsKeploysCA(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	home := t.TempDir()
	user := filepath.Join(home, "config")
	clusterCA := mustGenerateTestCA().certPEM
	src := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: kind-kp\n  cluster:\n    server: https://127.0.0.1:36205\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(clusterCA) + "\nusers:\n- name: kind-kp\n  user:\n    client-key-data: c2VjcmV0\n")
	if err := os.WriteFile(user, src, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kubeconfig.EnvVar, user)
	var given [][2]string
	orig := chownCopy
	chownCopy = func(ref, path string) error { given = append(given, [2]string{ref, path}); return nil }
	t.Cleanup(func() { chownCopy = orig })

	for run := 1; run <= 2; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		core, logs := observer.New(zap.InfoLevel)
		given = nil
		if err := SetupCaCertEnv(ctx, zap.New(core), testCACertPEM()); err != nil {
			cancel()
			t.Fatal(err)
		}
		dirs := runDirsIn(t, tmp)
		if len(dirs) != 1 {
			cancel()
			t.Fatalf("run %d: %d run directories; want one", run, len(dirs))
		}
		live := liveRunOf(t, dirs[0])
		cp := os.Getenv(kubeconfig.EnvVar)
		if filepath.Dir(cp) != dirs[0] {
			cancel()
			t.Fatalf("run %d: KUBECONFIG=%q is not a copy in the run's directory %s", run, cp, dirs[0])
		}
		if len(given) != 1 || given[0] != [2]string{user, cp} {
			cancel()
			t.Fatalf("run %d: the copy was given to the owner of %v; want the copy %s to the owner of %s", run, given, cp, user)
		}
		if n := logs.FilterMessageSnippet("pointing kubectl and helm at a copy").Len(); n != 1 {
			cancel()
			t.Fatalf("run %d: %d logs say kubectl is pointed at a copy; want 1", run, n)
		}
		ca := copyCA(t, cp)
		if !bytes.HasPrefix(ca, clusterCA) || !bytes.Contains(ca, testCACertPEM()) {
			cancel()
			t.Fatalf("run %d: the copy's CA is not the cluster's followed by keploy's", run)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			cancel()
			t.Fatalf("run %d: the copy's CA holds no certificate", run)
		}
		if body, _ := os.ReadFile(cp); !strings.Contains(string(body), "client-key-data: c2VjcmV0") {
			cancel()
			t.Fatalf("run %d: the copy lost the user's client key, which kubectl sends its certificate with", run)
		}
		if runtime.GOOS != "windows" {
			if st, err := os.Stat(cp); err != nil || st.Mode().Perm() != 0o600 {
				cancel()
				t.Fatalf("run %d: the copy's mode is %v (%v); want 0600: it holds the user's client key", run, st.Mode().Perm(), err)
			}
		}
		cancel()
		waitRunGone(t, live)
		if _, err := os.Stat(cp); !os.IsNotExist(err) {
			t.Fatalf("run %d: the copy %s outlived the run (%v)", run, cp, err)
		}
		if got, err := os.ReadFile(user); err != nil || !bytes.Equal(got, src) {
			t.Fatalf("run %d: the user's kubeconfig changed (%v)", run, err)
		}
	}
}

// A copy keploy cannot give to its file's owner (keploy under sudo) is not
// used: KUBECONFIG stays the user's, and the run says why.
func TestSetupCaCertEnv_ACopyItsOwnerCannotReadIsNotUsed(t *testing.T) {
	isolateCertEnv(t)
	useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	user := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(user, []byte("clusters: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kubeconfig.EnvVar, user)
	orig := chownCopy
	chownCopy = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { chownCopy = orig })
	core, logs := observer.New(zap.WarnLevel)
	runUntilCleanup(t, zap.New(core))
	if got := os.Getenv(kubeconfig.EnvVar); got != user {
		t.Fatalf("KUBECONFIG=%q; want the user's %q", got, user)
	}
	if logs.FilterMessageSnippet("could not write a copy of the kubeconfig").Len() != 1 {
		t.Fatal("the run does not say why kubectl is not pointed at a copy")
	}
}

// With no kubeconfig, KUBECONFIG is left as it is (unset stays unset).
func TestSetupCaCertEnv_NoKubeconfigLeavesKUBECONFIGAlone(t *testing.T) {
	isolateCertEnv(t)
	useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv(kubeconfig.EnvVar, "")
	if err := os.Unsetenv(kubeconfig.EnvVar); err != nil {
		t.Fatal(err)
	}
	runUntilCleanup(t, zap.NewNop())
	if v, ok := os.LookupEnv(kubeconfig.EnvVar); ok {
		t.Fatalf("KUBECONFIG=%q with no kubeconfig; want it unset", v)
	}
}

// logLine is a log line about one kubeconfig: the start of its message, and
// its fields but the kubeconfig, next_step, written and backup (error,
// test_changes by their key alone).
type logLine struct{ msg, fields string }

// linesFor is each line logs hold that names the kubeconfig file, at Info or
// above; every warning says a next step, but one that says keploy could not
// read its copy.
func linesFor(t *testing.T, logs *observer.ObservedLogs, file string) []logLine {
	t.Helper()
	var got []logLine
	for _, e := range logs.FilterField(zap.String("kubeconfig", file)).All() {
		fields := e.ContextMap()
		var fs []string
		for _, k := range []string{"added", "changed", "removed", "current_context", "preferences", "conflict", "cluster", "error", "test_changes"} {
			if v, ok := fields[k]; ok && (k == "error" || k == "test_changes") {
				fs = append(fs, k)
			} else if ok {
				fs = append(fs, fmt.Sprintf("%s=%v", k, v))
			}
		}
		if _, says := fields["next_step"]; !says && e.Level > zap.InfoLevel && !strings.HasPrefix(e.Message, "keploy could not read") {
			t.Errorf("%q says no next step", e.Message)
		}
		got = append(got, logLine{e.Message, strings.Join(fs, " ")})
	}
	return got
}

func sameLines(got, want []logLine) bool {
	ok := len(got) == len(want)
	for i := 0; ok && i < len(got); i++ {
		ok = strings.HasPrefix(got[i].msg, want[i].msg) && got[i].fields == want[i].fields
	}
	return ok
}

// kubeconfigRun runs SetupCaCertEnv until do, given the copy (or copies)
// KUBECONFIG names, has written what the test writes, and returns what the
// run logged at Info and above.
func kubeconfigRun(t *testing.T, tmp string, do func(copies []string)) *observer.ObservedLogs {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, logs := observer.New(zap.InfoLevel)
	if err := SetupCaCertEnv(ctx, zap.New(core), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	dirs := runDirsIn(t, tmp)
	if len(dirs) != 1 {
		t.Fatalf("%d run directories; want one", len(dirs))
	}
	live := liveRunOf(t, dirs[0])
	if do != nil {
		do(filepath.SplitList(os.Getenv(kubeconfig.EnvVar)))
	}
	if n := logs.FilterMessageSnippet("during the run").Len() + logs.FilterMessageSnippet("as the run ended").Len(); n != 0 {
		t.Fatalf("%d lines on what the test wrote before the run ended; want none", n)
	}
	cancel()
	waitRunGone(t, live)
	return logs
}

// As a native run ends, what the test wrote to keploy's copy of its
// kubeconfig is written back to the user's file, a backup kept beside it,
// and one line says what changed and where the backup is; a test that
// changed nothing writes nothing. A cluster the test added whose CA leaves
// keploy's out is named: its calls were neither recorded nor mocked. When the
// user's file changed during the run in the entry the test changed, nothing
// is written and the warning names the conflict and the test's changes.
func TestSetupCaCertEnv_WritesWhatTheTestWroteToItsKubeconfigBack(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	dir := t.TempDir()
	user := filepath.Join(dir, "config")
	clusterCA, kindCA := mustGenerateTestCA().certPEM, mustGenerateTestCA().certPEM
	src := []byte("apiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n- name: kind-a\n  cluster:\n    server: https://127.0.0.1:36205\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(clusterCA) + "\ncontexts:\n- name: kind-a\n  context:\n    cluster: kind-a\n    user: kind-a\nusers:\n- name: kind-a\n  user:\n    client-key-data: c2VjcmV0\n")
	t.Setenv(kubeconfig.EnvVar, user)
	orig := chownCopy
	chownCopy = func(string, string) error { return nil }
	t.Cleanup(func() { chownCopy = orig })

	write := func(data string) func(t *testing.T, cp string) {
		return func(t *testing.T, cp string) {
			if err := os.WriteFile(cp, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// edit has f change the file as kubectl does: read whole, written again
	// whole (in its own order).
	edit := func(f func(cfg map[string]any)) func(t *testing.T, cp string) {
		return func(t *testing.T, cp string) {
			data, err := os.ReadFile(cp)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			f(cfg)
			out, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(out, data) {
				t.Fatal("the row writes the file as it was, byte for byte: it tests nothing")
			}
			write(string(out))(t, cp)
		}
	}
	kindCreate := edit(func(cfg map[string]any) {
		cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{
			"server": "https://127.0.0.1:40001", "certificate-authority-data": base64.StdEncoding.EncodeToString(kindCA)}})
		cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "kind-b", "context": map[string]any{"cluster": "kind-b", "user": "kind-b"}})
		cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "kind-b", "user": map[string]any{"client-key-data": "b3RoZXI="}})
		cfg["current-context"] = "kind-b"
	})
	// Written again in sorted order, with preferences: {} as kubectl adds.
	again := edit(func(cfg map[string]any) { cfg["preferences"] = map[string]any{} })
	namespace := func(ns string) func(t *testing.T, cp string) {
		return edit(func(cfg map[string]any) {
			cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)["namespace"] = ns
		})
	}
	const wroteBack = "keploy wrote to the kubeconfig what the test changed in it during the run"
	rows := []struct {
		name    string
		do      func(t *testing.T, cp string)
		outside func(t *testing.T, file string) // someone else's write to the user's file during the run
		want    []logLine
		written func(cfg map[string]any) // the user's file after; nil: as it was
	}{
		{"left as keploy wrote it", nil, nil, nil, nil},
		{"written again as it was, in kubectl's order", again, nil, nil, nil},
		{"kind create cluster", kindCreate, nil, []logLine{
			{wroteBack, `added=[cluster kind-b context kind-b user kind-b] current_context="kind-a" -> "kind-b"`},
			{"kubectl and helm do not trust keploy's certificate for a cluster the test added or changed", "cluster=[kind-b (https://127.0.0.1:40001)]"},
		}, func(cfg map[string]any) {
			if cfg["current-context"] != "kind-b" || len(cfg["clusters"].([]any)) != 2 || len(cfg["users"].([]any)) != 2 {
				t.Errorf("kind create cluster: the user's file holds %v; want kind-b's cluster, context and user, and current", cfg)
			}
		}},
		{"kubectl config set-context --namespace", namespace("web"), nil, []logLine{{wroteBack, "changed=[context kind-a]"}}, func(cfg map[string]any) {
			if ns := cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)["namespace"]; ns != "web" {
				t.Errorf("set-context: namespace %v; want web", ns)
			}
			ca, _ := base64.StdEncoding.DecodeString(cfg["clusters"].([]any)[0].(map[string]any)["cluster"].(map[string]any)["certificate-authority-data"].(string))
			if !bytes.Equal(ca, clusterCA) {
				t.Errorf("set-context: the cluster's CA is now\n%s\nwant the user's own alone", ca)
			}
		}},
		{"the context changed outside the test too", namespace("web"), namespace("ops"), []logLine{
			{"the kubeconfig was changed during the run, outside the test, in entries the test changed too", "changed=[context kind-a] conflict=[context kind-a] test_changes"},
		}, nil},
		{"removed", func(t *testing.T, cp string) {
			if err := os.Remove(cp); err != nil {
				t.Fatal(err)
			}
		}, nil, []logLine{{"the test removed its kubeconfig during the run", ""}}, nil},
		{"left as no YAML", write("clusters: [\n"), nil, []logLine{{"keploy could not write what the test changed in its kubeconfig", "error"}}, nil},
		{"replaced by a directory", func(t *testing.T, cp string) {
			if err := os.Remove(cp); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cp, 0o700); err != nil {
				t.Fatal(err)
			}
		}, nil, []logLine{{"keploy could not read its copy of the kubeconfig as the run ended", "error"}}, nil},
	}
	for _, r := range rows {
		for _, e := range mustReadDir(t, dir) {
			_ = os.Remove(filepath.Join(dir, e))
		}
		if err := os.WriteFile(user, src, 0o600); err != nil {
			t.Fatal(err)
		}
		before := src
		logs := kubeconfigRun(t, tmp, func(copies []string) {
			if r.do != nil {
				r.do(t, copies[0])
			}
			if r.outside != nil {
				r.outside(t, user)
				before, _ = os.ReadFile(user)
			}
		})
		if got := linesFor(t, logs, user); !sameLines(got, r.want) {
			t.Errorf("%s: logged %+v; want %+v (each message's start)", r.name, got, r.want)
		}
		after, err := os.ReadFile(user)
		if err != nil {
			t.Fatal(err)
		}
		backups := 0
		for _, e := range mustReadDir(t, dir) {
			if strings.HasPrefix(e, "config.keploy-backup-") {
				backups++
				if b, _ := os.ReadFile(filepath.Join(dir, e)); !bytes.Equal(b, src) {
					t.Errorf("%s: the backup does not hold the user's file as it was", r.name)
				}
			}
		}
		if r.written == nil {
			if !bytes.Equal(after, before) || backups != 0 {
				t.Errorf("%s: the user's file was written (%d backups); want it left as it was", r.name, backups)
			}
			continue
		}
		if backups != 1 {
			t.Errorf("%s: %d backups; want one", r.name, backups)
		}
		var cfg map[string]any
		if err := yaml.Unmarshal(after, &cfg); err != nil {
			t.Fatal(err)
		}
		r.written(cfg)
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// kubectl writes a new entry to the first file KUBECONFIG names and a change
// to the file that holds the entry: keploy's copies are in the same order, so
// each change reaches its own file, relative KUBECONFIG entries made absolute
// from where keploy started.
func TestSetupCaCertEnv_WritesEachChangeToTheKubeconfigItCameFrom(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	orig := chownCopy
	chownCopy = func(string, string) error { return nil }
	t.Cleanup(func() { chownCopy = orig })
	work := t.TempDir()
	t.Chdir(work)
	a := "apiVersion: v1\nkind: Config\ncurrent-context: a\ncontexts:\n- name: a\n  context:\n    cluster: a\n"
	b := "apiVersion: v1\nkind: Config\ncontexts:\n- name: b\n  context:\n    cluster: b\n"
	for _, list := range [][]string{{"a/config"}, {"a/config", "b/config"}} {
		for _, f := range []string{"a/config", "b/config"} {
			if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile("a/config", []byte(a), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("b/config", []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(kubeconfig.EnvVar, strings.Join(list, string(os.PathListSeparator)))
		kubeconfigRun(t, tmp, func(copies []string) {
			if len(copies) != len(list) {
				t.Fatalf("KUBECONFIG names %v; want a copy of each of %v", copies, list)
			}
			// kubectl config set-context new: a new entry, to the first file.
			kubectlWrites(t, copies[0], func(cfg map[string]any) {
				cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "new", "context": map[string]any{"cluster": "new"}})
			})
			if len(copies) > 1 { // set-context b --namespace: to b's file
				kubectlWrites(t, copies[1], func(cfg map[string]any) {
					cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)["namespace"] = "web"
				})
			}
		})
		gotA, gotB := string(mustRead(t, "a/config")), string(mustRead(t, "b/config"))
		if !strings.Contains(gotA, "name: new") || strings.Contains(gotA, "web") {
			t.Errorf("KUBECONFIG=%v: a/config is\n%s\nwant the new context alone added", list, gotA)
		}
		if wantB := len(list) > 1; strings.Contains(gotB, "namespace: web") != wantB || strings.Contains(gotB, "new") {
			t.Errorf("KUBECONFIG=%v: b/config is\n%s\nwant b's namespace: %v, and no new context", list, gotB, wantB)
		}
	}
}

// kubectlWrites has f change the kubeconfig at p as kubectl does: read whole,
// written again whole.
func kubectlWrites(t *testing.T, p string, f func(cfg map[string]any)) {
	t.Helper()
	var cfg map[string]any
	if err := yaml.Unmarshal(mustRead(t, p), &cfg); err != nil {
		t.Fatal(err)
	}
	f(cfg)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// dieWithoutCleanup leaves run as a run killed outright (SIGKILL) leaves it:
// its lock let go, as the system does at its exit, and nothing of its end
// done, its directory and records left.
func dieWithoutCleanup(run *runTrustDir) {
	run.once.Do(func() {})
	_ = run.lock.Close()
	liveRunDirs.Delete(run)
}

// What the test of a run that ended without its cleanup (killed, crashed)
// changed in its kubeconfig is written back by the next run, before it removes
// the ended run's directory, as the run's own end would have: merged onto a
// file changed since elsewhere, refused when changed in the same entry; each
// line names the ended run's pid and start. A run that ends as it should, or
// whose test changed nothing, leaves the next run nothing to write; a live
// run's directory is left alone; a record keploy cannot read keeps the
// directory, whose copy holds the test's changes, and says so.
func TestReapEndedRunDirs_WritesADeadRunsKubeconfigChangesBack(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	dir := t.TempDir()
	user := filepath.Join(dir, "config")
	kindCA := mustGenerateTestCA().certPEM
	src := []byte("apiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n- name: kind-a\n  cluster:\n    server: https://127.0.0.1:36205\ncontexts:\n- name: kind-a\n  context:\n    cluster: kind-a\n    user: kind-a\nusers:\n- name: kind-a\n  user:\n    client-key-data: c2VjcmV0\n")
	t.Setenv(kubeconfig.EnvVar, user)
	orig := chownCopy
	chownCopy = func(string, string) error { return nil }
	t.Cleanup(func() { chownCopy = orig })

	edit := func(f func(cfg map[string]any)) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			f(cfg)
			out, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	kindCreate := edit(func(cfg map[string]any) {
		cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{
			"server": "https://127.0.0.1:40001", "certificate-authority-data": base64.StdEncoding.EncodeToString(kindCA)}})
		cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "kind-b", "context": map[string]any{"cluster": "kind-b", "user": "kind-b"}})
		cfg["current-context"] = "kind-b"
	})
	namespace := func(ns string) func(t *testing.T, path string) {
		return edit(func(cfg map[string]any) {
			cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)["namespace"] = ns
		})
	}
	addUser := edit(func(cfg map[string]any) {
		cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "ops", "user": map[string]any{"token": "dG9r"}})
	})
	ctxOf := func(cfg map[string]any) map[string]any {
		return cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)
	}
	const wroteBack = "keploy wrote to the kubeconfig what the test changed in it during the run"
	rows := []struct {
		name    string
		do      func(t *testing.T, cp string)
		outside func(t *testing.T, file string) // a write to the user's file after the run, before the next
		ownEnd  bool                            // the run's end wrote back before it was killed
		want    []logLine
		written func(cfg map[string]any) // the user's file after; nil: as it was
		kept    bool                     // the ended run's directory stays
	}{
		{name: "its test changed nothing"},
		{name: "kind create cluster", do: kindCreate, want: []logLine{
			{wroteBack, `added=[cluster kind-b context kind-b] current_context="kind-a" -> "kind-b"`},
			{"kubectl and helm do not trust keploy's certificate for a cluster the test added or changed", "cluster=[kind-b (https://127.0.0.1:40001)]"},
		}, written: func(cfg map[string]any) {
			if cfg["current-context"] != "kind-b" || len(cfg["clusters"].([]any)) != 2 || len(cfg["contexts"].([]any)) != 2 {
				t.Errorf("kind create cluster: the user's file holds %v; want kind-b's cluster and context, and current", cfg)
			}
		}},
		{name: "the user's file changed since in another entry", do: namespace("web"), outside: addUser, want: []logLine{
			{wroteBack, "changed=[context kind-a]"},
		}, written: func(cfg map[string]any) {
			if ns := ctxOf(cfg)["namespace"]; ns != "web" || len(cfg["users"].([]any)) != 2 {
				t.Errorf("merged: namespace %v and users %v; want web, and kind-a and ops", ns, cfg["users"])
			}
		}},
		{name: "the user's file changed since in the same entry", do: namespace("web"), outside: namespace("ops"), want: []logLine{
			{"the kubeconfig was changed during the run, outside the test, in entries the test changed too", "changed=[context kind-a] conflict=[context kind-a] test_changes"},
		}},
		{name: "killed after its own end wrote back", do: namespace("web"), ownEnd: true, written: func(cfg map[string]any) {
			if ns := ctxOf(cfg)["namespace"]; ns != "web" {
				t.Errorf("own end: namespace %v; want web", ns)
			}
		}},
		{name: "a record keploy cannot read", do: func(t *testing.T, cp string) {
			namespace("web")(t, cp)
			if err := os.WriteFile(cp+recordSuffix, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, kept: true},
	}
	for _, r := range rows {
		for _, e := range mustReadDir(t, dir) {
			_ = os.Remove(filepath.Join(dir, e))
		}
		if err := os.WriteFile(user, src, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		runLog, _ := observer.New(zap.InfoLevel)
		if err := SetupCaCertEnv(ctx, zap.New(runLog), testCACertPEM()); err != nil {
			cancel()
			t.Fatal(err)
		}
		runDir := filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS"))
		live := liveRunOf(t, runDir)
		started := live.started
		cp := os.Getenv(kubeconfig.EnvVar)
		if r.do != nil {
			r.do(t, cp)
		}
		// A live run's directory is left alone: its records too.
		reapEndedRunDirs(zap.NewNop())
		if got, _ := os.ReadFile(user); !bytes.Equal(got, src) {
			t.Errorf("%s: the user's file was written while the run lived", r.name)
		}
		if _, err := os.Stat(cp + recordSuffix); err != nil {
			t.Errorf("%s: the live run's record: %v", r.name, err)
		}
		if r.ownEnd {
			live.mu.Lock()
			for _, f := range live.atEnd {
				f()
			}
			live.mu.Unlock()
		}
		dieWithoutCleanup(live)
		cancel()
		before, _ := os.ReadFile(user)
		if r.outside != nil {
			r.outside(t, user)
			before, _ = os.ReadFile(user)
		}

		core, logs := observer.New(zap.InfoLevel)
		reapEndedRunDirs(zap.New(core))
		if got := linesFor(t, logs, user); !sameLines(got, r.want) {
			t.Errorf("%s: logged %+v; want %+v (each message's start)", r.name, got, r.want)
		}
		ended := fmt.Sprintf("pid %d, started %s; it ended without its cleanup (killed or crashed)", os.Getpid(), started.Format(time.RFC3339))
		for _, e := range logs.FilterField(zap.String("kubeconfig", user)).All() {
			if got := e.ContextMap()["ended_run"]; got != ended {
				t.Errorf("%s: %q names the ended run %v; want %q", r.name, e.Message, got, ended)
			}
		}
		unread := logs.FilterMessageSnippet("keploy could not read what a run that ended without its cleanup").FilterField(zap.String("dir", runDir)).Len()
		_, statErr := os.Stat(filepath.Join(runDir, "kubeconfig-0"))
		if r.kept {
			if unread != 1 || statErr != nil {
				t.Errorf("%s: %d warnings name the directory, and its copy: %v; want one, and the copy kept", r.name, unread, statErr)
			}
			if err := os.RemoveAll(runDir); err != nil {
				t.Fatal(err)
			}
		} else if unread != 0 || len(runDirsIn(t, tmp)) != 0 {
			t.Errorf("%s: %d warnings on an unreadable record, run directories %q left; want none", r.name, unread, runDirsIn(t, tmp))
		}

		after, err := os.ReadFile(user)
		if err != nil {
			t.Fatal(err)
		}
		backups, keptChanges := 0, 0
		for _, e := range mustReadDir(t, dir) {
			backups += strings.Count(e, "config.keploy-backup-")
			keptChanges += strings.Count(e, "config.keploy-test-changes-")
		}
		if wantKept := len(r.want) > 0 && r.written == nil; (keptChanges == 1) != wantKept {
			t.Errorf("%s: %d files keep the test's changes; want them kept: %v", r.name, keptChanges, wantKept)
		}
		if r.written == nil {
			if !bytes.Equal(after, before) || backups != 0 {
				t.Errorf("%s: the user's file was written (%d backups); want it left as it was", r.name, backups)
			}
			continue
		}
		if backups != 1 {
			t.Errorf("%s: %d backups; want one", r.name, backups)
		}
		var cfg map[string]any
		if err := yaml.Unmarshal(after, &cfg); err != nil {
			t.Fatal(err)
		}
		r.written(cfg)
	}
}
