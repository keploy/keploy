package kubeconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The copy keploy wrote for a user with one kind cluster: the cluster's CA
// followed by keploy's (Rewrite), a context and a client-key user.
func wroteCopy(clusterCA, keployCA []byte) string {
	return "apiVersion: v1\nkind: Config\ncurrent-context: kind-a\nclusters:\n" +
		"- name: kind-a\n  cluster:\n    server: https://127.0.0.1:40000\n    certificate-authority-data: " + b64(withPEM(clusterCA, keployCA)) + "\n" +
		"    insecure-skip-tls-verify: false\n    extensions: []\n" +
		"contexts:\n- name: kind-a\n  context:\n    cluster: kind-a\n    user: kind-a\n" +
		"users:\n- name: kind-a\n  user:\n    client-key-data: c2VjcmV0\n" +
		"- name: gcp\n  user:\n    auth-provider:\n      name: gcp\n      config: {}\n" +
		"- name: oidc\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: kubelogin\n      args: [get-token]\n"
}

// Diff names, by name, each cluster, context and user the test added,
// changed or removed in keploy's copy of its kubeconfig, and a switch of its
// current context; and each cluster it added or changed whose CA leaves
// keploy's out. kubectl writing the file again as it was is no change.
func TestDiffNamesWhatTheTestWroteToTheCopy(t *testing.T) {
	keploy, cluster, other := newCA(t, "keploy"), newCA(t, "kind-a"), newCA(t, "kind-b")
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig-0")
	wrote := wroteCopy(cluster.pem, keploy.pem)
	if err := os.WriteFile(filepath.Join(dir, "own-ca.crt"), other.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "with-keploy.crt"), withPEM(other.pem, keploy.pem), 0o600); err != nil {
		t.Fatal(err)
	}
	// kubectl's own writing of the same config: its field order, no
	// insecure-skip-tls-verify: false, empty extensions or auth-provider
	// config, args, env, provideClusterInfo and interactiveMode written
	// though empty, preferences: {}, other indentation.
	again := "apiVersion: v1\nclusters:\n" +
		"  - cluster:\n      certificate-authority-data: " + b64(withPEM(cluster.pem, keploy.pem)) + "\n      server: https://127.0.0.1:40000\n    name: kind-a\n" +
		"contexts:\n  - context:\n      cluster: kind-a\n      user: kind-a\n    name: kind-a\n" +
		"current-context: kind-a\nkind: Config\npreferences: {}\n" +
		"users:\n  - name: kind-a\n    user:\n      client-key-data: c2VjcmV0\n" +
		"  - name: gcp\n    user:\n      auth-provider:\n        name: gcp\n" +
		"  - name: oidc\n    user:\n      exec:\n        apiVersion: client.authentication.k8s.io/v1\n        args:\n          - get-token\n        command: kubelogin\n        env: null\n        provideClusterInfo: false\n        interactiveMode: \"\"\n"
	addCluster := func(name, body string) string {
		return strings.Replace(wrote, "contexts:\n", "- name: "+name+"\n  cluster:\n    server: https://127.0.0.1:40001\n"+body+"contexts:\n", 1)
	}
	kindB := strings.Replace(addCluster("kind-b", "    certificate-authority-data: "+b64(other.pem)+"\n"),
		"users:\n", "- name: kind-b\n  context:\n    cluster: kind-b\n    user: kind-b\nusers:\n- name: kind-b\n  user:\n    client-key-data: b3RoZXI=\n", 1)
	kindB = strings.Replace(kindB, "current-context: kind-a", "current-context: kind-b", 1)
	rows := []struct {
		name string
		now  string
		want Changes
	}{
		{"unchanged", wrote, Changes{}},
		{"kubectl wrote it again as it was", again, Changes{}},
		{"kind create cluster: a cluster with its own CA, its context and user, made current", kindB, Changes{
			Added:          []string{"cluster kind-b", "context kind-b", "user kind-b"},
			CurrentContext: `"kind-a" -> "kind-b"`,
			Untrusted:      []string{"kind-b (https://127.0.0.1:40001)"},
		}},
		{"kubectl config set-context --current --namespace", strings.Replace(wrote, "    user: kind-a\n", "    user: kind-a\n    namespace: web\n", 1),
			Changes{Changed: []string{"context kind-a"}}},
		{"kubectl config use-context of no context", strings.Replace(wrote, "current-context: kind-a\n", "", 1),
			Changes{CurrentContext: `"kind-a" -> ""`}},
		{"kind delete cluster", "apiVersion: v1\nkind: Config\nusers:\n- name: gcp\n  user:\n    auth-provider:\n      name: gcp\n- name: oidc\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: kubelogin\n      args: [get-token]\n",
			Changes{Removed: []string{"cluster kind-a", "context kind-a", "user kind-a"}, CurrentContext: `"kind-a" -> ""`}},
		{"kubectl config delete-user", strings.Replace(wrote, "- name: gcp\n  user:\n    auth-provider:\n      name: gcp\n      config: {}\n", "", 1),
			Changes{Removed: []string{"user gcp"}}},
		{"emptied", "", Changes{
			Removed:        []string{"cluster kind-a", "context kind-a", "user gcp", "user kind-a", "user oidc"},
			CurrentContext: `"kind-a" -> ""`,
		}},
		{"its cluster's CA replaced by one without keploy's", strings.Replace(wrote, b64(withPEM(cluster.pem, keploy.pem)), b64(other.pem), 1),
			Changes{Changed: []string{"cluster kind-a"}, Untrusted: []string{"kind-a (https://127.0.0.1:40000)"}}},
		{"its cluster's server moved, keploy's CA kept", strings.Replace(wrote, "40000", "40002", 1),
			Changes{Changed: []string{"cluster kind-a"}}},
		{"a cluster that checks no certificate", addCluster("x", "    insecure-skip-tls-verify: true\n    certificate-authority-data: "+b64(other.pem)+"\n"),
			Changes{Added: []string{"cluster x"}}},
		{"a cluster whose CA holds keploy's", addCluster("x", "    certificate-authority-data: "+b64(withPEM(other.pem, keploy.pem))+"\n"),
			Changes{Added: []string{"cluster x"}}},
		{"a cluster whose CA is no base64", addCluster("x", "    certificate-authority-data: '%%%'\n"),
			Changes{Added: []string{"cluster x"}, Untrusted: []string{"x (https://127.0.0.1:40001)"}}},
		{"a cluster whose CA holds keploy's, then what is no base64", addCluster("x", "    certificate-authority-data: '"+b64(withPEM(other.pem, keploy.pem))+"%%%'\n"),
			Changes{Added: []string{"cluster x"}, Untrusted: []string{"x (https://127.0.0.1:40001)"}}},
		{"a cluster whose CA holds keploy's without its last newline", addCluster("x", "    certificate-authority-data: "+b64(bytes.TrimRight(withPEM(other.pem, keploy.pem), "\n"))+"\n"),
			Changes{Added: []string{"cluster x"}}},
		{"a cluster whose CA file, beside the copy, leaves keploy's out", addCluster("x", "    certificate-authority: own-ca.crt\n"),
			Changes{Added: []string{"cluster x"}, Untrusted: []string{"x (https://127.0.0.1:40001)"}}},
		{"a cluster whose CA file holds keploy's", addCluster("x", "    certificate-authority: with-keploy.crt\n"),
			Changes{Added: []string{"cluster x"}}},
		{"a cluster whose CA data leaves keploy's out, beside a CA file that holds it", addCluster("x", "    certificate-authority: with-keploy.crt\n    certificate-authority-data: "+b64(other.pem)+"\n"),
			Changes{Added: []string{"cluster x"}, Untrusted: []string{"x (https://127.0.0.1:40001)"}}},
		{"a cluster whose CA file cannot be read", addCluster("x", "    certificate-authority: gone.crt\n"),
			Changes{Added: []string{"cluster x"}, Untrusted: []string{"x (https://127.0.0.1:40001)"}}},
		{"a cluster that names no CA (the system's roots)", addCluster("x", ""),
			Changes{Added: []string{"cluster x"}}},
		{"a second item of a name: kubectl reads the first", addCluster("kind-a", "    certificate-authority-data: "+b64(other.pem)+"\n"), Changes{}},
		{"an item that is no mapping", strings.Replace(wrote, "contexts:\n", "- just-a-name\ncontexts:\n", 1), Changes{}},
	}
	for _, r := range rows {
		got, err := Diff(path, []byte(wrote), []byte(r.now), keploy.pem)
		if err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		if !reflect.DeepEqual(got, r.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", r.name, got, r.want)
		}
		if got.Empty() != (len(r.want.Added)+len(r.want.Changed)+len(r.want.Removed) == 0 && r.want.CurrentContext == "") {
			t.Errorf("%s: Empty() is %v for %+v", r.name, got.Empty(), got)
		}
	}
}

// A copy that is no kubeconfig YAML, before or after, is an error naming it.
func TestDiffRefusesWhatIsNoKubeconfig(t *testing.T) {
	keploy, cluster := newCA(t, "keploy"), newCA(t, "kind-a")
	wrote := wroteCopy(cluster.pem, keploy.pem)
	for _, r := range []struct{ name, wrote, now, want string }{
		{"left as no YAML", wrote, "clusters: [\n", "the kubeconfig the test left at /k: not YAML"},
		{"left as a scalar", wrote, "just text\n", "the kubeconfig the test left at /k: not a YAML mapping"},
		{"left as a list", wrote, "- a\n- b\n", "the kubeconfig the test left at /k: not a YAML mapping"},
		{"written as no YAML", "clusters: [\n", wrote, "the copy of the kubeconfig keploy wrote to /k: not YAML"},
	} {
		_, err := Diff("/k", []byte(r.wrote), []byte(r.now), keploy.pem)
		if err == nil || !strings.HasPrefix(err.Error(), r.want) {
			t.Errorf("%s: err %v; want one that begins %q", r.name, err, r.want)
		}
	}
}
