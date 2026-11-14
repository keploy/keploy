package kubeconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// userConfig is a user's kubeconfig with a comment, a kind cluster (CA data),
// a cluster whose CA is a file beside it, one that names no CA, a context, a
// client-key user and an exec user whose command is a path beside it.
func userConfig(clusterCA []byte) string {
	return "# the user's own kubeconfig\napiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n" +
		"- name: kind-a # made by kind\n  cluster:\n    server: https://127.0.0.1:40000\n    certificate-authority-data: " + b64(clusterCA) + "\n" +
		"- name: filed\n  cluster:\n    server: https://10.0.0.1:6443\n    certificate-authority: ca/filed.crt\n" +
		"- name: open\n  cluster:\n    server: https://10.0.0.2:6443\n" +
		"contexts:\n- name: kind-a\n  context:\n    cluster: kind-a\n    user: kind-a\n" +
		"users:\n- name: kind-a\n  user:\n    client-key-data: c2VjcmV0\n" +
		"- name: oidc\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: bin/kubelogin\n      args: [get-token]\n"
}

// kubectlEdit is data as kubectl leaves it after f: read whole, written again
// whole, in its own order.
func kubectlEdit(t *testing.T, data []byte, f func(cfg map[string]any)) []byte {
	t.Helper()
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	f(cfg)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func got(t *testing.T, data []byte) map[string]any { return asConfig(t, data) }

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("%q is no base64: %v", s, err)
	}
	return b
}

// asConfig is data read as kubectl reads it, empty fields left out.
func asConfig(t *testing.T, data []byte) map[string]any {
	t.Helper()
	m, err := configOf(data)
	if err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	return m
}

// itemOf is the item of cfg's list named name, as a mapping a row may edit.
func itemOf(cfg map[string]any, list, name string) map[string]any {
	items, _ := cfg[list].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func bodyOf(cfg map[string]any, list, name string) map[string]any {
	field := strings.TrimSuffix(list, "s")
	return itemOf(cfg, list, name)[field].(map[string]any)
}

func without(cfg map[string]any, list, name string) {
	var kept []any
	for _, it := range cfg[list].([]any) {
		if it.(map[string]any)["name"] != name {
			kept = append(kept, it)
		}
	}
	cfg[list] = kept
}

// WriteBack writes to the user's kubeconfig what the test changed in keploy's
// copy of it, field by field: what keploy set in the copy (its CA beside a
// cluster's own, paths made absolute) never reaches the user's file; the
// user's comments stay; the file it replaces is kept as a backup beside it,
// with its mode. A test that changed nothing writes nothing and leaves no
// backup.
func TestWriteBackWritesWhatTheTestChangedToTheUsersKubeconfig(t *testing.T) {
	keploy, cluster, filed, kindB, renewed := newCA(t, "keploy"), newCA(t, "kind-a"), newCA(t, "filed"), newCA(t, "kind-b"), newCA(t, "renewed")
	trust := Trust{CA: keploy.pem, Bundle: withPEM([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), keploy.pem)}
	orig := userConfig(cluster.pem)
	stamp := time.Date(2026, 11, 13, 21, 40, 0, 0, time.FixedZone("IST", 5*3600+1800))
	const backupName = "config.keploy-backup-20261113T161000Z"

	kubectl := func(f func(cfg map[string]any)) func(t *testing.T, wrote []byte) []byte {
		return func(t *testing.T, wrote []byte) []byte { return kubectlEdit(t, wrote, f) }
	}
	setNS := func(ns string) func(cfg map[string]any) {
		return func(cfg map[string]any) { bodyOf(cfg, "contexts", "kind-a")["namespace"] = ns }
	}
	kindCreate := func(cfg map[string]any) {
		cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{"server": "https://127.0.0.1:40001", "certificate-authority-data": b64(kindB.pem)}})
		cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "kind-b", "context": map[string]any{"cluster": "kind-b", "user": "kind-b"}})
		cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "kind-b", "user": map[string]any{"client-key-data": "b3RoZXI="}})
		cfg["current-context"] = "kind-b"
	}
	rows := []struct {
		name    string
		test    func(t *testing.T, wrote []byte) []byte // what the test leaves in the copy
		outside func(cfg map[string]any)                // someone else's write to the user's file during the run
		want    func(cfg map[string]any)                // the user's file after, from before; nil: left as it was
		changes string                                  // what WriteBack names
		// conflict: nothing written, the conflicting entries named, the
		// test's changes kept beside the user's file (with this change).
		conflict []string
		kept     func(t *testing.T, kept map[string]any)
	}{
		{name: "left as keploy wrote it", test: func(_ *testing.T, wrote []byte) []byte { return wrote }},
		{name: "written again as it was, with preferences: {}", test: kubectl(func(cfg map[string]any) { cfg["preferences"] = map[string]any{} })},
		{name: "kind create cluster", test: kubectl(kindCreate), changes: "added=[cluster kind-b context kind-b user kind-b] context=\"kind-a\" -> \"kind-b\"",
			want: func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{"server": "https://127.0.0.1:40001", "certificate-authority-data": b64(kindB.pem)}})
				cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "kind-b", "context": map[string]any{"cluster": "kind-b", "user": "kind-b"}})
				cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "kind-b", "user": map[string]any{"client-key-data": "b3RoZXI="}})
				cfg["current-context"] = "kind-b"
			}},
		{name: "kubectl config set-context --current --namespace", test: kubectl(setNS("web")), changes: "changed=[context kind-a]", want: setNS("web")},
		{name: "a context given a namespace, written with fields that hold nothing: those stay out", changes: "changed=[context kind-a]",
			test: kubectl(func(cfg map[string]any) {
				setNS("web")(cfg)
				b := bodyOf(cfg, "contexts", "kind-a")
				b["extensions"], b["cluster-extras"], b["note"] = []any{}, map[string]any{}, ""
			}),
			want: setNS("web")},
		{name: "kind delete cluster", changes: "removed=[cluster kind-a context kind-a user kind-a] context=\"kind-a\" -> \"\"",
			test: kubectl(func(cfg map[string]any) {
				without(cfg, "clusters", "kind-a")
				without(cfg, "contexts", "kind-a")
				without(cfg, "users", "kind-a")
				delete(cfg, "current-context")
			}),
			want: func(cfg map[string]any) {
				without(cfg, "clusters", "kind-a")
				delete(cfg, "contexts")
				without(cfg, "users", "kind-a")
				delete(cfg, "current-context")
			}},
		{name: "a cluster made again with a new CA", changes: "changed=[cluster kind-a]",
			test: kubectl(func(cfg map[string]any) {
				bodyOf(cfg, "clusters", "kind-a")["certificate-authority-data"] = b64(renewed.pem)
			}),
			want: func(cfg map[string]any) {
				bodyOf(cfg, "clusters", "kind-a")["certificate-authority-data"] = b64(renewed.pem)
			}},
		{name: "a new CA written beside keploy's: keploy's is taken out", changes: "changed=[cluster kind-a]",
			test: kubectl(func(cfg map[string]any) {
				bodyOf(cfg, "clusters", "kind-a")["certificate-authority-data"] = b64(withPEM(renewed.pem, keploy.pem))
			}),
			want: func(cfg map[string]any) {
				bodyOf(cfg, "clusters", "kind-a")["certificate-authority-data"] = b64(renewed.pem)
			}},
		{name: "the test's CA keploy's alone: the cluster's CA data is gone", changes: "changed=[cluster kind-a]",
			test: kubectl(func(cfg map[string]any) {
				bodyOf(cfg, "clusters", "kind-a")["certificate-authority-data"] = b64(keploy.pem)
			}),
			want: func(cfg map[string]any) { delete(bodyOf(cfg, "clusters", "kind-a"), "certificate-authority-data") }},
		{name: "kubectl config set-cluster --insecure-skip-tls-verify on a cluster whose CA is a file", changes: "changed=[cluster filed]",
			test: kubectl(func(cfg map[string]any) {
				b := bodyOf(cfg, "clusters", "filed")
				delete(b, "certificate-authority-data")
				b["insecure-skip-tls-verify"] = true
			}),
			want: func(cfg map[string]any) {
				b := bodyOf(cfg, "clusters", "filed")
				delete(b, "certificate-authority")
				b["insecure-skip-tls-verify"] = true
			}},
		{name: "kubectl config set-cluster --server on a cluster whose CA is a file", changes: "changed=[cluster filed]",
			test: kubectl(func(cfg map[string]any) { bodyOf(cfg, "clusters", "filed")["server"] = "https://10.0.0.9:6443" }),
			want: func(cfg map[string]any) { bodyOf(cfg, "clusters", "filed")["server"] = "https://10.0.0.9:6443" }},
		{name: "kubectl config set-cluster --server on a cluster that names no CA", changes: "changed=[cluster open]",
			test: kubectl(func(cfg map[string]any) { bodyOf(cfg, "clusters", "open")["server"] = "https://10.0.0.8:6443" }),
			want: func(cfg map[string]any) { bodyOf(cfg, "clusters", "open")["server"] = "https://10.0.0.8:6443" }},
		{name: "kubectl config set-cluster --certificate-authority (a file, the data kept)", changes: "changed=[cluster kind-a]",
			test: kubectl(func(cfg map[string]any) { bodyOf(cfg, "clusters", "kind-a")["certificate-authority"] = "/etc/kind.crt" }),
			want: func(cfg map[string]any) { bodyOf(cfg, "clusters", "kind-a")["certificate-authority"] = "/etc/kind.crt" }},
		{name: "kubectl config set-credentials --exec-arg: the command keploy made absolute stays as written", changes: "changed=[user oidc]",
			test: kubectl(func(cfg map[string]any) {
				bodyOf(cfg, "users", "oidc")["exec"].(map[string]any)["args"] = []any{"get-token", "--v=2"}
			}),
			want: func(cfg map[string]any) {
				bodyOf(cfg, "users", "oidc")["exec"].(map[string]any)["args"] = []any{"get-token", "--v=2"}
			}},
		{name: "a user's client key replaced", changes: "changed=[user kind-a]",
			test: kubectl(func(cfg map[string]any) { bodyOf(cfg, "users", "kind-a")["client-key-data"] = "bmV3" }),
			want: func(cfg map[string]any) { bodyOf(cfg, "users", "kind-a")["client-key-data"] = "bmV3" }},
		{name: "preferences", changes: "preferences",
			test: kubectl(func(cfg map[string]any) { cfg["preferences"] = map[string]any{"colors": true} }),
			want: func(cfg map[string]any) { cfg["preferences"] = map[string]any{"colors": true} }},
		{name: "a user added outside the test, beside the test's change: both", changes: "changed=[context kind-a]",
			test: kubectl(setNS("web")),
			outside: func(cfg map[string]any) {
				cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "ci", "user": map[string]any{"token": "t"}})
			},
			want: setNS("web")}, // from the file as it is now
		{name: "the context the test changed, changed outside too", changes: "changed=[context kind-a]",
			test: kubectl(setNS("web")), outside: setNS("ops"), conflict: []string{"context kind-a"},
			kept: func(t *testing.T, kept map[string]any) {
				if ns := bodyOf(kept, "contexts", "kind-a")["namespace"]; ns != "web" || kept["clusters"] != nil {
					t.Errorf("kept %v; want the context alone, with namespace web", kept)
				}
			}},
		{name: "a cluster of the name the test added, added outside too", changes: "added=[cluster kind-b context kind-b user kind-b] context=\"kind-a\" -> \"kind-b\"",
			test: kubectl(kindCreate),
			outside: func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{"server": "https://127.0.0.1:50000"}})
				cfg["current-context"] = "kind-b"
			},
			// The current context, made what the test made it, is none.
			conflict: []string{"cluster kind-b"},
			kept: func(t *testing.T, kept map[string]any) {
				if bodyOf(kept, "clusters", "kind-b")["certificate-authority-data"] != b64(kindB.pem) || kept["current-context"] != "kind-b" || len(kept["users"].([]any)) != 1 {
					t.Errorf("kept %v; want kind-b's cluster, context, user and current-context", kept)
				}
			}},
		{name: "a cluster the test added, added the same outside: it is there once", changes: "added=[cluster kind-b context kind-b user kind-b] context=\"kind-a\" -> \"kind-b\"",
			test: kubectl(kindCreate),
			outside: func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-b", "cluster": map[string]any{"server": "https://127.0.0.1:40001", "certificate-authority-data": b64(kindB.pem)}})
			},
			want: func(cfg map[string]any) {
				cfg["contexts"] = append(cfg["contexts"].([]any), map[string]any{"name": "kind-b", "context": map[string]any{"cluster": "kind-b", "user": "kind-b"}})
				cfg["users"] = append(cfg["users"].([]any), map[string]any{"name": "kind-b", "user": map[string]any{"client-key-data": "b3RoZXI="}})
				cfg["current-context"] = "kind-b"
			}},
		{name: "a cluster the test added with keploy's CA beside its own (a cluster copied from the copy): keploy's is taken out", changes: "added=[cluster kind-c]",
			test: kubectl(func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-c", "cluster": map[string]any{"server": "https://127.0.0.1:40002", "certificate-authority-data": b64(withPEM(kindB.pem, keploy.pem))}})
			}),
			want: func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-c", "cluster": map[string]any{"server": "https://127.0.0.1:40002", "certificate-authority-data": b64(kindB.pem)}})
			}},
		{name: "a cluster the test added whose CA is keploy's alone: it is added with no CA data", changes: "added=[cluster kind-c]",
			test: kubectl(func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-c", "cluster": map[string]any{"server": "https://127.0.0.1:40002", "certificate-authority-data": b64(keploy.pem)}})
			}),
			want: func(cfg map[string]any) {
				cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "kind-c", "cluster": map[string]any{"server": "https://127.0.0.1:40002"}})
			}},
		{name: "the context the test removed, changed outside: nothing written, nothing to keep", changes: "removed=[context kind-a]",
			test:    kubectl(func(cfg map[string]any) { without(cfg, "contexts", "kind-a") }),
			outside: setNS("ops"), conflict: []string{"context kind-a"}},
	}
	for _, r := range rows {
		dir := t.TempDir()
		file := filepath.Join(dir, "config")
		if err := os.MkdirAll(filepath.Join(dir, "ca"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca", "filed.crt"), filed.pem, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(orig), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0o640); err != nil {
			t.Fatal(err)
		}
		wrote, _, err := Rewrite(file, []byte(orig), trust)
		if err != nil {
			t.Fatal(err)
		}
		cur := []byte(orig)
		if r.outside != nil {
			cfg := asConfig(t, cur)
			r.outside(cfg)
			if cur, err = yaml.Marshal(cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, cur, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		var chowned []string
		opt := WriteOptions{Now: stamp, Chown: func(ref, p string) error {
			if ref != file {
				t.Errorf("%s: chown %s to the owner of %s; want of the user's file", r.name, p, ref)
			}
			chowned = append(chowned, filepath.Base(p))
			return nil
		}}
		w, err := WriteBack(file, filepath.Join(t.TempDir(), "kubeconfig-0"), []byte(orig), wrote, r.test(t, wrote), keploy.pem, opt)
		if err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		if got := describe(w.Changes); got != r.changes {
			t.Errorf("%s: changes %q; want %q", r.name, got, r.changes)
		}
		if !reflect.DeepEqual(w.Conflict, r.conflict) {
			t.Errorf("%s: conflict %v; want %v", r.name, w.Conflict, r.conflict)
		}
		after, _ := os.ReadFile(file)
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if r.want == nil || r.conflict != nil {
			files := 2 // the user's file and ca
			if r.kept != nil {
				files++
			}
			if !bytes.Equal(after, cur) || w.Backup != "" || len(names) != files {
				t.Errorf("%s: the user's file was written (backup %q, files %v); want it left as it was", r.name, w.Backup, names)
			}
			if r.kept == nil && w.Kept != "" {
				t.Errorf("%s: kept %q; want nothing kept: the test added and changed nothing", r.name, w.Kept)
			}
			if r.kept != nil {
				data, err := os.ReadFile(w.Kept)
				if err != nil || filepath.Dir(w.Kept) != dir || !strings.HasPrefix(filepath.Base(w.Kept), "config.keploy-test-changes-") {
					t.Fatalf("%s: kept %q (%v); want a file beside the user's", r.name, w.Kept, err)
				}
				if info, _ := os.Stat(w.Kept); info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
					t.Errorf("%s: kept with mode %v; want 0600 (it may hold keys)", r.name, info.Mode().Perm())
				}
				r.kept(t, asConfig(t, data))
			}
			continue
		}
		want := asConfig(t, cur)
		r.want(want)
		if got := asConfig(t, after); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the user's file is\n%v\nwant\n%v", r.name, got, want)
		}
		if f := noneAdded(cur, after); f != "" {
			t.Errorf("%s: the user's file gained a field that holds nothing (%q):\n%s", r.name, f, after)
		}
		clusters, _ := got(t, after)["clusters"].([]any)
		for _, c := range clusters {
			item, _ := c.(map[string]any)
			body, _ := item["cluster"].(map[string]any)
			if d, _ := body["certificate-authority-data"].(string); strings.Contains(string(mustB64(t, d)), string(bytes.TrimSpace(keploy.pem))) {
				t.Errorf("%s: keploy's CA reached the user's file:\n%s", r.name, after)
			}
		}
		if bytes.Contains(after, []byte(dir+string(os.PathSeparator)+"bin")) {
			t.Errorf("%s: keploy's CA or a path keploy made absolute reached the user's file:\n%s", r.name, after)
		}
		if r.outside == nil && !bytes.Contains(after, []byte("# the user's own kubeconfig")) {
			t.Errorf("%s: the user's comments are gone:\n%s", r.name, after)
		}
		if w.File != file || w.Backup != filepath.Join(dir, backupName) {
			t.Errorf("%s: wrote %q, backup %q; want %q, %q", r.name, w.File, w.Backup, file, backupName)
		}
		if b, err := os.ReadFile(w.Backup); err != nil || !bytes.Equal(b, cur) {
			t.Errorf("%s: the backup does not hold the file as it was (%v)", r.name, err)
		}
		for _, p := range []string{file, w.Backup} {
			if info, err := os.Stat(p); err != nil {
				t.Errorf("%s: %q: %v; want it there with the user's 0640", r.name, p, err)
			} else if info.Mode().Perm() != 0o640 && os.PathSeparator == '/' {
				t.Errorf("%s: %s has mode %v; want the user's 0640", r.name, p, info.Mode().Perm())
			}
		}
		if len(names) != 3 || len(chowned) != 2 || chowned[0] != backupName {
			t.Errorf("%s: files %v, chowned %v; want the user's file, its backup and ca, each given to its owner", r.name, names, chowned)
		}
	}
}

// noneAdded is a field of an entry that holds nothing (an empty list, mapping
// or string, or null) that after holds more often than before, or "": kubectl
// leaves such fields out, so what holds nothing in the test's copy is no
// change to write. A list of the config itself left empty by a removal is
// not counted.
func noneAdded(before, after []byte) string {
	count := func(data []byte) map[string]int {
		n := map[string]int{}
		for _, l := range strings.Split(string(data), "\n") {
			for _, f := range []string{": []", ": {}", `: ""`, ": ''", ": null"} {
				if strings.HasPrefix(l, " ") && strings.HasSuffix(l, f) {
					n[f]++
				}
			}
		}
		return n
	}
	was := count(before)
	for f, n := range count(after) {
		if n > was[f] {
			return f
		}
	}
	return ""
}

// describe names what Changes holds, as the rows above write it.
func describe(c Changes) string {
	var out []string
	for _, f := range []struct {
		key   string
		names []string
	}{{"added", c.Added}, {"changed", c.Changed}, {"removed", c.Removed}} {
		if len(f.names) > 0 {
			out = append(out, f.key+"=["+strings.Join(f.names, " ")+"]")
		}
	}
	if c.CurrentContext != "" {
		out = append(out, "context="+c.CurrentContext)
	}
	if c.Preferences {
		out = append(out, "preferences")
	}
	return strings.Join(out, " ")
}

// When keploy cannot write the user's file, it says why and keeps what the
// test changed beside it; a copy it cannot read is an error with nothing
// kept. A backup's name taken gets a number; a symlink is followed, and an
// empty file becomes a config.
func TestWriteBackRefusesAndKeepsWhatItCannotWrite(t *testing.T) {
	keploy, cluster := newCA(t, "keploy"), newCA(t, "kind-a")
	orig := []byte(userConfig(cluster.pem))
	stamp := time.Date(2026, 11, 13, 16, 10, 0, 0, time.UTC)
	wrote, _, err := Rewrite("/k/config", orig, Trust{CA: keploy.pem})
	if err != nil {
		t.Fatal(err)
	}
	now := kubectlEdit(t, wrote, func(cfg map[string]any) { bodyOf(cfg, "contexts", "kind-a")["namespace"] = "web" })
	for _, r := range []struct {
		name  string
		setup func(t *testing.T, file string) // the user's file as the run ends
		now   []byte
		err   string // the error's start; "" none
		kept  bool
		chown func(ref, p string) error
	}{
		{"the user's file removed during the run", func(t *testing.T, file string) { _ = os.Remove(file) }, now, "keploy could not find the kubeconfig", true, nil},
		{"the user's file now no YAML", func(t *testing.T, file string) { _ = os.WriteFile(file, []byte("clusters: [\n"), 0o600) }, now, "the kubeconfig", true, nil},
		{"the user's file now a list", func(t *testing.T, file string) { _ = os.WriteFile(file, []byte("- a\n"), 0o600) }, now, "the kubeconfig", true, nil},
		{"the copy left as no YAML", nil, []byte("clusters: [\n"), "the kubeconfig the test left at", false, nil},
		{"a backup of the name taken", func(t *testing.T, file string) {
			_ = os.WriteFile(file+".keploy-backup-20261113T161000Z", nil, 0o600)
		}, now, "", false, nil},
		{"a symlink to the user's file", func(t *testing.T, file string) {
			real := filepath.Join(filepath.Dir(file), "real")
			if err := os.Rename(file, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, file); err != nil {
				t.Skip("no symlinks here:", err)
			}
		}, now, "", false, nil},
		{"the user's file unreadable", func(t *testing.T, file string) {
			if os.PathSeparator != '/' || os.Geteuid() == 0 {
				t.Skip("a mode keploy cannot read past: not as root, not on Windows")
			}
			if err := os.Chmod(file, 0); err != nil {
				t.Fatal(err)
			}
		}, now, "keploy could not read the kubeconfig", true, nil},
		// keploy under sudo: nothing it writes can be given to the user's
		// owner; nothing is left beside the user's file, and Kept names none.
		{"no file given to the user's owner", nil, now, "keploy could not back up the kubeconfig", false, func(ref, p string) error {
			return errors.New("chown: operation not permitted")
		}},
		// keploy under sudo: the new file cannot be given to the user once
		// the backup is made; the backup, the file as it still is, goes.
		{"the new file not given to the user's owner", nil, now, "keploy could not write the kubeconfig", true, func(ref, p string) error {
			if strings.Contains(filepath.Base(p), ".keploy-backup-") || strings.Contains(filepath.Base(p), ".keploy-test-changes-") {
				return nil
			}
			return errors.New("chown: operation not permitted")
		}},
	} {
		t.Run(r.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "config")
			if err := os.WriteFile(file, orig, 0o600); err != nil {
				t.Fatal(err)
			}
			if r.setup != nil {
				r.setup(t, file)
			}
			w, err := WriteBack(file, "/k/copy", orig, wrote, r.now, keploy.pem, WriteOptions{Now: stamp, Chown: r.chown})
			if (err == nil) != (r.err == "") || (err != nil && !strings.HasPrefix(err.Error(), r.err)) {
				t.Fatalf("err %v; want one that begins %q", err, r.err)
			}
			if r.name == "no file given to the user's owner" && !strings.Contains(err.Error(), "keploy could not keep the test's changes beside it") {
				t.Errorf("err %v; want it to say the test's changes could not be kept either", err)
			}
			if err != nil {
				if data, rerr := os.ReadFile(file); rerr == nil && !bytes.Equal(data, orig) && r.setup == nil {
					t.Errorf("the user's file was written:\n%s", data)
				}
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if strings.Contains(e.Name(), ".keploy-backup-") || strings.HasPrefix(e.Name(), ".config.keploy-") || (!r.kept && strings.Contains(e.Name(), ".keploy-test-changes-")) {
						t.Errorf("%s left beside the user's file; want no backup or temporary file when keploy wrote nothing", e.Name())
					}
				}
			}
			if (w.Kept != "") != r.kept {
				t.Fatalf("kept %q; want one: %v", w.Kept, r.kept)
			}
			if r.kept {
				data, _ := os.ReadFile(w.Kept)
				if bodyOf(asConfig(t, data), "contexts", "kind-a")["namespace"] != "web" {
					t.Errorf("kept\n%s\nwant the test's context", data)
				}
			}
			if err != nil {
				return
			}
			if r.name == "a backup of the name taken" && w.Backup != file+".keploy-backup-20261113T161000Z-1" {
				t.Errorf("backup %q; want the name with -1", w.Backup)
			}
			if fi, err := os.Lstat(file); r.name == "a symlink to the user's file" && (err != nil || fi.Mode()&os.ModeSymlink == 0 || w.File != filepath.Join(dir, "real")) {
				t.Errorf("the symlink was replaced, or %q written; want the file it names", w.File)
			}
			data, _ := os.ReadFile(file)
			if bodyOf(asConfig(t, data), "contexts", "kind-a")["namespace"] != "web" {
				t.Errorf("the user's file\n%s\nwant the test's namespace", data)
			}
		})
	}

	// An empty kubeconfig the test made a cluster in.
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	made := []byte("apiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n- name: kind-a\n  cluster:\n    server: https://127.0.0.1:40000\n")
	if _, err := WriteBack(file, "/k/copy", nil, nil, made, keploy.pem, WriteOptions{Now: stamp}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if got := asConfig(t, data); !reflect.DeepEqual(got, asConfig(t, made)) {
		t.Errorf("the empty kubeconfig is now\n%s\nwant\n%s", data, made)
	}
}

// withoutCA takes keploy's CA out of a CA the test wrote, and nothing else.
func TestWithoutCATakesOutKeploysCAAlone(t *testing.T) {
	keploy, other := newCA(t, "keploy"), newCA(t, "other")
	for _, r := range []struct{ name, in, want string }{
		{"keploy's alone", b64(keploy.pem), ""},
		{"beside another", b64(withPEM(other.pem, keploy.pem)), b64(other.pem)},
		{"before another", b64(append(append([]byte{}, keploy.pem...), other.pem...)), b64(other.pem)},
		{"another alone", b64(other.pem), b64(other.pem)},
		{"no base64", "%%%", "%%%"},
		{"keploy's, then what is no base64", b64(keploy.pem) + "%%%", b64(keploy.pem) + "%%%"},
		{"another alone, no last newline", b64(bytes.TrimSpace(other.pem)), b64(bytes.TrimSpace(other.pem))},
	} {
		if got := withoutCA(r.in, keploy.pem); got != r.want {
			t.Errorf("%s: %q; want %q", r.name, got, r.want)
		}
	}
	for _, v := range []string{b64(other.pem), b64([]byte("x"))} {
		if got := withoutCA(v, nil); got != v {
			t.Errorf("no CA of keploy's: %q; want %q kept", got, v)
		}
	}
}

// same compares what two values hold; one it cannot decode (an int tag on
// what is no number) is compared as written.
func TestSameComparesWhatItCannotDecodeAsWritten(t *testing.T) {
	n := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v} }
	if same(n("abc"), n("abd")) {
		t.Error("same(!!int abc, !!int abd); want them different")
	}
	if !same(n("abc"), n("abc")) {
		t.Error("!same(!!int abc, !!int abc); want them the same")
	}
}

// WriteBack makes what the user's file lacks for the test's change, in the
// block style kubectl writes: a list that is null or written [], an item's
// body that is null or absent; and a value the test's copy aliases is written
// whole, its anchor left where it was. Where the user's file shares a node
// (an alias), the test's change reaches only the place it changed, and an
// alias whose node it removed or changed keeps what it stood for, so the
// file still parses.
func TestWriteBackMakesWhatTheUsersFileLacks(t *testing.T) {
	keploy := newCA(t, "keploy")
	stamp := time.Date(2026, 11, 13, 16, 10, 0, 0, time.UTC)
	const head = "apiVersion: v1\nkind: Config\n"
	for _, r := range []struct {
		name, orig string
		// cur is the user's file as the run ends, written during it outside
		// the test; "": orig.
		cur   string
		test  func(t *testing.T, wrote []byte) []byte
		want  string // the user's file after, as kubectl reads it
		check func(t *testing.T, after string)
	}{
		{name: "an unnamed context that holds nothing, filled by the test and removed outside: the test's is added",
			orig: head + "contexts:\n- {}\n- name: c\n  context:\n    cluster: x\n",
			cur:  head + "contexts:\n- name: c\n  context:\n    cluster: x\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					cfg["contexts"].([]any)[0] = map[string]any{"context": map[string]any{"namespace": "web"}}
				})
			},
			want: head + "contexts:\n- name: c\n  context:\n    cluster: x\n- context:\n    namespace: web\n"},
		{name: "clusters null, contexts []: the test adds one of each",
			orig: head + "clusters: null\ncontexts: []\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					cfg["clusters"] = []any{map[string]any{"name": "c", "cluster": map[string]any{"server": "https://10.0.0.3:6443"}}}
					cfg["contexts"] = []any{map[string]any{"name": "c", "context": map[string]any{"cluster": "c"}}}
				})
			},
			want: head + "clusters:\n- name: c\n  cluster:\n    server: https://10.0.0.3:6443\ncontexts:\n- name: c\n  context:\n    cluster: c\n",
			check: func(t *testing.T, after string) {
				if strings.Contains(after, "[{") || strings.Contains(after, "null") {
					t.Errorf("the lists are not written as kubectl writes them:\n%s", after)
				}
			}},
		{name: "a context whose body is null, and one with none: the test gives each a namespace",
			orig: head + "contexts:\n- name: bare\n  context: null\n- name: nokey\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					for _, it := range cfg["contexts"].([]any) {
						it.(map[string]any)["context"] = map[string]any{"namespace": "web"}
					}
				})
			},
			want: head + "contexts:\n- name: bare\n  context:\n    namespace: web\n- name: nokey\n  context:\n    namespace: web\n"},
		{name: "a context whose body is null: the test leaves it none and changes a key beside it",
			orig: head + "contexts:\n- name: bare # mine\n  context: null\n  x-note: a\n  x-keep: k\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					it := itemOf(cfg, "contexts", "bare")
					delete(it, "context")
					it["x-note"] = "b"
				})
			},
			want: head + "contexts:\n- name: bare\n  x-note: b\n  x-keep: k\n",
			check: func(t *testing.T, after string) {
				if !strings.Contains(after, "name: bare # mine") {
					t.Errorf("a key the test left as it was is not as the user wrote it:\n%s", after)
				}
			}},
		{name: "the test removes a key beside a context's body and changes the body",
			orig: head + "contexts:\n- name: c\n  context:\n    cluster: x\n  x-note: a\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					it := itemOf(cfg, "contexts", "c")
					delete(it, "x-note")
					it["context"].(map[string]any)["cluster"] = "y"
				})
			},
			want: head + "contexts:\n- name: c\n  context:\n    cluster: y\n"},
		{name: "the test removes a context: an item of its name that is no mapping stays",
			orig: head + "contexts:\n- [name, c]\n- name: c\n  context:\n    cluster: x\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) { cfg["contexts"] = cfg["contexts"].([]any)[:1] })
			},
			want: head + "contexts:\n- [name, c]\n"},
		{name: "the test adds a cluster whose server aliases another's",
			orig: head + "clusters:\n- name: a\n  cluster:\n    server: &srv https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: *srv\n",
			test: func(t *testing.T, wrote []byte) []byte {
				var doc yaml.Node
				if err := yaml.Unmarshal(wrote, &doc); err != nil {
					t.Fatal(err)
				}
				l := get(doc.Content[0], "clusters")
				srv := get(get(deref(l.Content[0]), "cluster"), "server")
				body := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				body.Content = append(body.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "server"}, &yaml.Node{Kind: yaml.AliasNode, Alias: srv, Value: srv.Anchor})
				it := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				set(it, "name", "c")
				it.Content = append(it.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cluster"}, body)
				l.Content = append(l.Content, it)
				out, err := encode(&doc)
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			want: head + "clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: https://10.0.0.1:6443\n- name: c\n  cluster:\n    server: https://10.0.0.1:6443\n",
			check: func(t *testing.T, after string) {
				if strings.Count(after, "&srv") != 1 || strings.Count(after, "*srv") != 1 {
					t.Errorf("want the user's anchor once and its alias once, the test's cluster written whole:\n%s", after)
				}
			}},
		{name: "the test removes the cluster whose server another's aliases: that one keeps the value whole",
			orig: head + "clusters:\n- name: a\n  cluster:\n    server: &srv https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: *srv\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) { cfg["clusters"] = cfg["clusters"].([]any)[1:] })
			},
			want: head + "clusters:\n- name: b\n  cluster:\n    server: https://10.0.0.1:6443\n"},
		{name: "the test changes the server another's aliases: that one keeps the old value",
			orig: head + "clusters:\n- name: a\n  cluster:\n    server: &srv https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: *srv\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					itemOf(cfg, "clusters", "a")["cluster"].(map[string]any)["server"] = "https://10.0.0.9:6443"
				})
			},
			want: head + "clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.9:6443\n- name: b\n  cluster:\n    server: https://10.0.0.1:6443\n"},
		{name: "the test changes a cluster another's aliases whole: that one keeps the old cluster",
			orig: head + "clusters:\n- name: a\n  cluster: &c\n    server: https://10.0.0.1:6443\n- name: b\n  cluster: *c\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					itemOf(cfg, "clusters", "a")["cluster"].(map[string]any)["server"] = "https://10.0.0.9:6443"
				})
			},
			want: head + "clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.9:6443\n- name: b\n  cluster:\n    server: https://10.0.0.1:6443\n"},
		{name: "the test changes a cluster that aliases another's whole: the other keeps its own",
			orig: head + "clusters:\n- name: a\n  cluster: &c\n    server: https://10.0.0.1:6443\n- name: b\n  cluster: *c\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					itemOf(cfg, "clusters", "b")["cluster"].(map[string]any)["server"] = "https://10.0.0.9:6443"
				})
			},
			want: head + "clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: https://10.0.0.9:6443\n"},
		{name: "an anchor named twice: the test removes the later one, whose value the alias keeps",
			orig: head + "clusters:\n- name: a\n  cluster:\n    server: &s https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: &s https://10.0.0.2:6443\n- name: c\n  cluster:\n    server: *s\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					l := cfg["clusters"].([]any)
					cfg["clusters"] = []any{l[0], l[2]}
				})
			},
			want: head + "clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: c\n  cluster:\n    server: https://10.0.0.2:6443\n"},
		{name: "the clusters list an alias of another key's: the test adds a cluster, the other key keeps its list",
			orig: head + "x-seed: &l\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\nclusters: *l\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) {
					cfg["clusters"] = append(cfg["clusters"].([]any), map[string]any{"name": "b", "cluster": map[string]any{"server": "https://10.0.0.2:6443"}})
				})
			},
			want: head + "x-seed:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n" +
				"clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: https://10.0.0.2:6443\n"},
		{name: "the clusters list an alias of another key's: the test removes a cluster, the other key keeps its list",
			orig: head + "x-seed: &l\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: https://10.0.0.2:6443\nclusters: *l\n",
			test: func(t *testing.T, wrote []byte) []byte {
				return kubectlEdit(t, wrote, func(cfg map[string]any) { cfg["clusters"] = cfg["clusters"].([]any)[:1] })
			},
			want: head + "x-seed:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n- name: b\n  cluster:\n    server: https://10.0.0.2:6443\n" +
				"clusters:\n- name: a\n  cluster:\n    server: https://10.0.0.1:6443\n"},
	} {
		t.Run(r.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(file, []byte(r.orig), 0o600); err != nil {
				t.Fatal(err)
			}
			wrote, _, err := Rewrite(file, []byte(r.orig), Trust{CA: keploy.pem})
			if err != nil {
				t.Fatal(err)
			}
			if r.cur != "" {
				if err := os.WriteFile(file, []byte(r.cur), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := WriteBack(file, "/k/copy", []byte(r.orig), wrote, r.test(t, wrote), keploy.pem, WriteOptions{Now: stamp}); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(file)
			if got, want := asConfig(t, after), asConfig(t, []byte(r.want)); !reflect.DeepEqual(got, want) {
				t.Errorf("the user's file is\n%s\nwant\n%s", after, r.want)
			}
			if r.check != nil {
				r.check(t, string(after))
			}
		})
	}
}
