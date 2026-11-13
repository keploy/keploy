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

// What a test writes to keploy's copy of its kubeconfig goes with the run:
// keploy never writes the user's file. As the run ends keploy says what was
// lost, by name (never what the copy holds), and names a cluster the test
// added whose CA leaves keploy's out: kubectl refused keploy's certificate
// for it. A copy left as keploy wrote it, or written again as it was, says
// nothing.
func TestSetupCaCertEnv_SaysWhatTheTestWroteToItsKubeconfigCopy(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	user := filepath.Join(t.TempDir(), "config")
	clusterCA, kindCA := mustGenerateTestCA().certPEM, mustGenerateTestCA().certPEM
	src := []byte("apiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n- name: kind-a\n  cluster:\n    server: https://127.0.0.1:36205\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(clusterCA) + "\ncontexts:\n- name: kind-a\n  context:\n    cluster: kind-a\n    user: kind-a\nusers:\n- name: kind-a\n  user:\n    client-key-data: c2VjcmV0\n")
	if err := os.WriteFile(user, src, 0o600); err != nil {
		t.Fatal(err)
	}
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
	// edit has f change the copy as kubectl does: read whole, written again
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
				t.Fatal("the row writes the copy as it was, byte for byte: it tests nothing")
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
	setNamespace := edit(func(cfg map[string]any) {
		cfg["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)["namespace"] = "web"
	})
	// A warning that names the kubeconfig: the start of its message, and its
	// fields but the kubeconfig and next_step (an error by its key alone).
	type warning struct{ msg, fields string }
	const changed = "the test changed its kubeconfig during the run; keploy had pointed it at a copy"
	rows := []struct {
		name string
		do   func(t *testing.T, cp string)
		want []warning
	}{
		{"left as keploy wrote it", nil, nil},
		{"written again as it was, in kubectl's order", again, nil},
		{"kind create cluster", kindCreate, []warning{
			{changed, `added=[cluster kind-b context kind-b user kind-b] current_context="kind-a" -> "kind-b"`},
			{"kubectl and helm do not trust keploy's certificate for a cluster the test added or changed", "cluster=[kind-b (https://127.0.0.1:40001)]"},
		}},
		{"kubectl config set-context --namespace", setNamespace, []warning{{changed, "changed=[context kind-a]"}}},
		{"removed", func(t *testing.T, cp string) {
			if err := os.Remove(cp); err != nil {
				t.Fatal(err)
			}
		}, []warning{{"the test removed its kubeconfig during the run; keploy had pointed it at a copy", ""}}},
		{"left as no YAML", write("clusters: [\n"), []warning{{"the test changed its kubeconfig during the run, in a way keploy cannot read", "error"}}},
		{"replaced by a directory", func(t *testing.T, cp string) {
			if err := os.Remove(cp); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cp, 0o700); err != nil {
				t.Fatal(err)
			}
		}, []warning{{"keploy could not read its copy of the kubeconfig as the run ended", "error"}}},
	}
	for _, r := range rows {
		ctx, cancel := context.WithCancel(context.Background())
		core, logs := observer.New(zap.WarnLevel)
		if err := SetupCaCertEnv(ctx, zap.New(core), testCACertPEM()); err != nil {
			cancel()
			t.Fatal(err)
		}
		dirs := runDirsIn(t, tmp)
		if len(dirs) != 1 {
			cancel()
			t.Fatalf("%s: %d run directories; want one", r.name, len(dirs))
		}
		live := liveRunOf(t, dirs[0])
		if r.do != nil {
			r.do(t, os.Getenv(kubeconfig.EnvVar))
		}
		if n := logs.FilterField(zap.String("kubeconfig", user)).Len(); n != 0 {
			cancel()
			t.Fatalf("%s: %d warnings name the kubeconfig before the run ended; want none", r.name, n)
		}
		cancel()
		waitRunGone(t, live)
		var got []warning
		for _, e := range logs.FilterField(zap.String("kubeconfig", user)).All() {
			fields := e.ContextMap()
			var fs []string
			for _, k := range []string{"added", "changed", "removed", "current_context", "cluster", "error"} {
				if v, ok := fields[k]; ok && k == "error" {
					fs = append(fs, k)
				} else if ok {
					fs = append(fs, fmt.Sprintf("%s=%v", k, v))
				}
			}
			_, says := fields["next_step"]
			if !says && !strings.HasPrefix(e.Message, "keploy could not read") {
				t.Errorf("%s: %q says no next step", r.name, e.Message)
			}
			got = append(got, warning{e.Message, strings.Join(fs, " ")})
		}
		ok := len(got) == len(r.want)
		for i := 0; ok && i < len(got); i++ {
			ok = strings.HasPrefix(got[i].msg, r.want[i].msg) && got[i].fields == r.want[i].fields
		}
		if !ok {
			t.Errorf("%s: warnings %+v; want %+v (each message's start)", r.name, got, r.want)
		}
		if got, err := os.ReadFile(user); err != nil || !bytes.Equal(got, src) {
			t.Fatalf("%s: the user's kubeconfig changed (%v)", r.name, err)
		}
	}
}
