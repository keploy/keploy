package tls

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := SetupCaCertEnv(ctx, zap.New(core), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
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
	os.Unsetenv(kubeconfig.EnvVar)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := SetupCaCertEnv(ctx, zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	if v, ok := os.LookupEnv(kubeconfig.EnvVar); ok {
		t.Fatalf("KUBECONFIG=%q with no kubeconfig; want it unset", v)
	}
}
