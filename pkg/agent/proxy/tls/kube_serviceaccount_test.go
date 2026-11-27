//go:build !windows

package tls

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

const testClusterCA = "-----BEGIN CERTIFICATE-----\nY2x1c3Rlci1jYQ==\n-----END CERTIFICATE-----\n"

// projectedSA lays out a service-account directory the way the kubelet's atomic
// writer does: the files live in a timestamped directory, "..data" links to it,
// and each name the app reads links through "..data".
func projectedSA(t *testing.T, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	writeProjection(t, src, "..2026_11_26_10_00_00.000000001", files)
	for name := range files {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// writeProjection writes files into a new timestamped directory of src and
// swaps "..data" to it, as the kubelet does when it refreshes the token.
func writeProjection(t *testing.T, src, stamp string, files map[string]string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(src, stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, stamp, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(src, "..data_tmp")
	if err := os.Symlink(stamp, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(src, "..data")); err != nil {
		t.Fatal(err)
	}
}

func setKubeSAEnv(t *testing.T, src, dir, namespace string) {
	t.Helper()
	t.Setenv(EnvKubeServiceAccountSource, src)
	t.Setenv(EnvKubeServiceAccountDir, dir)
	t.Setenv(EnvKubeServiceAccountNamespace, namespace)
	t.Setenv(EnvKubeServiceAccountCopy, "")
}

// setKubeSACopyEnv is setKubeSAEnv for a replay pod, which copies.
func setKubeSACopyEnv(t *testing.T, src, dir, namespace string) {
	t.Helper()
	setKubeSAEnv(t, src, dir, namespace)
	t.Setenv(EnvKubeServiceAccountCopy, "true")
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestExportKubeServiceAccount_OffWithoutDir(t *testing.T) {
	setKubeSAEnv(t, "", "", "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("no directory asked for: want nil, got %v", err)
	}
}

func TestExportKubeServiceAccount_AddsKeployCAAndLinksTheRest(t *testing.T) {
	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "token-1", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSAEnv(t, src, dir, "")

	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("exportKubeServiceAccount: %v", err)
	}

	want := append([]byte(testClusterCA), testCACertPEM()...)
	if got := readString(t, filepath.Join(dir, "ca.crt")); got != string(want) {
		t.Fatalf("ca.crt must be the cluster CA then keploy's CA; got %q", got)
	}
	for name, body := range map[string]string{"token": "token-1", "namespace": "shop"} {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s must be a symlink: %v", name, err)
		}
		if target != filepath.Join(src, name) {
			t.Fatalf("%s links to %q, want the absolute source path %q", name, target, filepath.Join(src, name))
		}
		if got := readString(t, filepath.Join(dir, name)); got != body {
			t.Fatalf("%s reads %q, want %q", name, got, body)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("left %q in the directory; the atomic-writer internals and temp files must not come across", e.Name())
		}
	}
	if len(entries) != 3 {
		t.Fatalf("want ca.crt, namespace and token; got %d entries", len(entries))
	}
}

// The kubelet refreshes the projected token by writing a new timestamped
// directory and swapping "..data". The app must read the new token through the
// exported directory without the agent doing anything.
func TestExportKubeServiceAccount_TokenRefreshReachesTheApp(t *testing.T) {
	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "token-1", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSAEnv(t, src, dir, "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}

	writeProjection(t, src, "..2026_11_26_10_48_00.000000002", map[string]string{"ca.crt": testClusterCA, "token": "token-2", "namespace": "shop"})

	if got := readString(t, filepath.Join(dir, "token")); got != "token-2" {
		t.Fatalf("after the kubelet's refresh the app reads token %q, want token-2", got)
	}
}

func TestExportKubeServiceAccount_NamespaceOverride(t *testing.T) {
	t.Run("replaces the source's namespace", func(t *testing.T) {
		src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "t", "namespace": "keploy"})
		dir := t.TempDir()
		setKubeSAEnv(t, src, dir, "shop")
		if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Readlink(filepath.Join(dir, "namespace")); err == nil {
			t.Fatal("an overridden namespace must be a file of its own, not a link to the source's")
		}
		if got := readString(t, filepath.Join(dir, "namespace")); got != "shop" {
			t.Fatalf("namespace = %q, want shop", got)
		}
	})
	t.Run("added when the source has none", func(t *testing.T) {
		src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "t"})
		dir := t.TempDir()
		setKubeSAEnv(t, src, dir, "shop")
		if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
			t.Fatal(err)
		}
		if got := readString(t, filepath.Join(dir, "namespace")); got != "shop" {
			t.Fatalf("namespace = %q, want shop", got)
		}
	})
}

// An agent restarted in the pod exports again over what the app is reading.
func TestExportKubeServiceAccount_RerunReplacesInPlace(t *testing.T) {
	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "t", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSAEnv(t, src, dir, "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if got := readString(t, filepath.Join(dir, "ca.crt")); !strings.HasSuffix(got, string(testCACertPEM())) {
		t.Fatalf("ca.crt kept the first run's CA: %q", got)
	}
	if got := readString(t, filepath.Join(dir, "token")); got != "t" {
		t.Fatalf("token = %q after a second export", got)
	}
}

// Without a readable cluster CA the app keeps exactly what it had.
func TestExportKubeServiceAccount_UnreadableClusterCAIsLinked(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "token"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "missing"), filepath.Join(src, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	setKubeSAEnv(t, src, dir, "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("exportKubeServiceAccount: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dir, "ca.crt"))
	if err != nil || target != filepath.Join(src, "ca.crt") {
		t.Fatalf("ca.crt must link to the source's when it cannot be read; target %q, err %v", target, err)
	}
}

func TestExportKubeServiceAccount_RefusesWhatWouldLeaveTheAppWithoutFiles(t *testing.T) {
	empty := t.TempDir()
	for _, tc := range []struct {
		name, src string
	}{
		{"no source", ""},
		{"relative source", "var/run/secrets/keploy.io/serviceaccount"},
		{"missing source", filepath.Join(empty, "nope")},
		{"empty source", empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setKubeSAEnv(t, tc.src, t.TempDir(), "")
			if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err == nil {
				t.Fatal("want an error: the app would start with an empty service-account directory")
			}
		})
	}
}

func TestExportKubeServiceAccount_CopyModeCopiesTheFiles(t *testing.T) {
	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "token-1", "namespace": "keploy"})
	dir := t.TempDir()
	setKubeSACopyEnv(t, src, dir, "shop")

	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("exportKubeServiceAccount: %v", err)
	}

	want := append([]byte(testClusterCA), testCACertPEM()...)
	if got := readString(t, filepath.Join(dir, "ca.crt")); got != string(want) {
		t.Fatalf("ca.crt must be the cluster CA then keploy's CA; got %q", got)
	}
	for name, body := range map[string]string{"token": "token-1", "namespace": "shop"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s must be a file of its own: the app cannot see the agent's mount a link would name (mode %v, err %v)", name, fi.Mode(), err)
		}
		if got := readString(t, filepath.Join(dir, name)); got != body {
			t.Fatalf("%s reads %q, want %q", name, got, body)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("want ca.crt, namespace and token, no atomic-writer internals or temp files; got %d entries", len(entries))
	}
}

// A copy does not follow the kubelet's refresh by itself: the agent must.
func TestExportKubeServiceAccount_CopyModeFollowsTheKubeletsRefresh(t *testing.T) {
	interval := kubeSARefreshInterval
	kubeSARefreshInterval = 10 * time.Millisecond
	t.Cleanup(func() { kubeSARefreshInterval = interval })

	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "token-1", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSACopyEnv(t, src, dir, "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}

	writeProjection(t, src, "..2026_11_26_10_48_00.000000002", map[string]string{"ca.crt": testClusterCA, "token": "token-2", "namespace": "shop"})

	deadline := time.Now().Add(5 * time.Second)
	for readString(t, filepath.Join(dir, "token")) != "token-2" {
		if time.Now().After(deadline) {
			t.Fatal("the copied token never followed the kubelet's refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A refresh that finds nothing new leaves the files the app reads alone.
func TestExportKubeServiceAccount_CopyModeKeepsUnchangedFiles(t *testing.T) {
	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "t", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSACopyEnv(t, src, dir, "")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	before := map[string]os.FileInfo{}
	for _, name := range []string{"ca.crt", "token", "namespace"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = fi
	}
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	for name, fi := range before {
		after, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !os.SameFile(fi, after) {
			t.Fatalf("%s was rewritten though nothing changed (err %v)", name, err)
		}
	}
}

// A replay pod admission gave no token: the app's directory stays empty, as it
// would be without keploy, and the agent still comes up.
func TestExportKubeServiceAccount_CopyModeWithoutAToken(t *testing.T) {
	dir := t.TempDir()
	setKubeSACopyEnv(t, filepath.Join(t.TempDir(), "serviceaccount"), dir, "shop")
	if err := exportKubeServiceAccount(t.Context(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("no token mounted: want nil, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("want an empty directory, got %d entries", len(entries))
	}
}

func TestSetupSharedVolume_ExportsKubeServiceAccount(t *testing.T) {
	isolateCertEnv(t)
	orig := loadSystemCABundleFn
	defer func() { loadSystemCABundleFn = orig }()
	loadSystemCABundleFn = func(_ *zap.Logger) ([]byte, string) { return []byte(systemBundleA), "/stub" }

	src := projectedSA(t, map[string]string{"ca.crt": testClusterCA, "token": "t", "namespace": "shop"})
	dir := t.TempDir()
	setKubeSAEnv(t, src, dir, "")

	if err := setupSharedVolume(t.Context(), zap.NewNop(), t.TempDir(), testCA); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("the service-account ca.crt was not exported: %v", err)
	}
	if !bytes.HasPrefix(got, []byte(testClusterCA)) || !bytes.HasSuffix(got, testCACertPEM()) {
		t.Fatalf("service-account ca.crt = %q", got)
	}
	if bytes.Contains(got, []byte(systemBundleA)) {
		t.Fatal("the service-account ca.crt must carry the cluster CA and keploy's, not the public roots")
	}
}
