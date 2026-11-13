package kubeconfig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// ca is a CA certificate (PEM) and a server leaf it signed for 127.0.0.1.
type ca struct {
	pem  []byte
	leaf *x509.Certificate
}

func newCA(t *testing.T, name string) ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(der)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kube-apiserver"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"kubernetes"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	return ca{pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), leaf: leaf}
}

// trusts reports whether a client trusting caPEM alone (as kubectl trusts its
// cluster's certificate-authority-data) accepts leaf.
func trusts(caPEM []byte, leaf *x509.Certificate) bool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return false
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "kubernetes"})
	return err == nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// parsed is a rewritten kubeconfig read back as kubectl's fields.
type parsed struct {
	Clusters []struct {
		Name    string                 `yaml:"name"`
		Cluster map[string]interface{} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string                 `yaml:"name"`
		User map[string]interface{} `yaml:"user"`
	} `yaml:"users"`
	Contexts       []map[string]interface{} `yaml:"contexts"`
	CurrentContext string                   `yaml:"current-context"`
	Preferences    map[string]interface{}   `yaml:"preferences"`
}

func parse(t *testing.T, b []byte) parsed {
	t.Helper()
	var p parsed
	if err := yaml.Unmarshal(b, &p); err != nil {
		t.Fatalf("the copy is not YAML: %v\n%s", err, b)
	}
	return p
}

func (p parsed) cluster(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	for _, c := range p.Clusters {
		if c.Name == name {
			return c.Cluster
		}
	}
	t.Fatalf("the copy has no cluster %q", name)
	return nil
}

func (p parsed) user(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	for _, u := range p.Users {
		if u.Name == name {
			return u.User
		}
	}
	t.Fatalf("the copy has no user %q", name)
	return nil
}

// caData is a cluster's certificate-authority-data, decoded.
func caData(t *testing.T, c map[string]interface{}) []byte {
	t.Helper()
	s, _ := c["certificate-authority-data"].(string)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || s == "" {
		t.Fatalf("certificate-authority-data %q is not base64 (%v)", s, err)
	}
	return b
}

// The copy of a kubeconfig keploy points kubectl at trusts keploy's CA beside
// each cluster's own, whether the cluster names it inline or by a file path
// (relative to the kubeconfig's directory, as kubectl reads it); a cluster
// with insecure-skip-tls-verify is left as it is (kubectl checks nothing, and
// refuses a CA beside it); one with no CA gets the roots keploy reads with its
// CA. Every relative path kubectl resolves from the file's directory is made
// absolute; exec credentials, auth providers and everything else are kept.
func TestRewriteTrustsKeploysCABesideEachClustersOwn(t *testing.T) {
	dir := t.TempDir()
	kind, filed, keploy, roots := newCA(t, "kind"), newCA(t, "filed"), newCA(t, "keploy"), newCA(t, "roots")
	// A CA file with no newline at its end: keploy's CA still starts a line.
	if err := os.WriteFile(filepath.Join(dir, "filed-ca.crt"), bytes.TrimRight(filed.pem, "\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `apiVersion: v1
kind: Config
# a comment kept as written
preferences:
  colors: true
x-shared: &shared
  server: https://10.0.0.3:6443
  certificate-authority-data: ` + b64(kind.pem) + `
current-context: kind-kp
clusters:
- name: kind-kp
  cluster:
    server: https://127.0.0.1:36205
    certificate-authority-data: ` + b64(kind.pem) + `
- name: filed
  cluster:
    server: https://10.0.0.1:6443
    certificate-authority: filed-ca.crt
- name: insecure
  cluster:
    server: https://10.0.0.2:6443
    insecure-skip-tls-verify: true
- name: public
  cluster:
    server: https://k8s.example.com
- name: aliased
  cluster: *shared
- name: both
  cluster:
    certificate-authority-data: ` + b64(kind.pem) + `
    certificate-authority: filed-ca.crt
- name: emptydata
  cluster:
    certificate-authority-data: ""
    certificate-authority: filed-ca.crt
- name: emptypath
  cluster:
    certificate-authority-data: ` + b64(kind.pem) + `
    certificate-authority: ""
contexts:
- name: kind-kp
  context:
    cluster: kind-kp
    user: kind-kp
users:
- name: kind-kp
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
- name: files
  user:
    client-certificate: certs/client.crt
    client-key: /abs/client.key
    tokenFile: token
- name: empty
  user:
    tokenFile: ""
- name: eks
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: [eks, get-token, --cluster-name, prod]
      env:
      - name: AWS_PROFILE
        value: prod
- name: local-exec
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: ` + filepath.Join("bin", "cred") + `
- name: gcp
  user:
    auth-provider:
      name: gcp
      config:
        cmd-path: gcloud
`
	path := filepath.Join(dir, "config")
	out, notes, err := Rewrite(path, []byte(src), Trust{CA: keploy.pem, Bundle: roots.pem})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("notes %q; want none", notes)
	}
	p := parse(t, out)

	// kubectl trusts the cluster's own server and keploy's certificate.
	for _, c := range []struct {
		name string
		own  ca
	}{{"kind-kp", kind}, {"filed", filed}} {
		got := caData(t, p.cluster(t, c.name))
		if !trusts(got, c.own.leaf) {
			t.Fatalf("cluster %s: the copy no longer trusts the cluster's own CA", c.name)
		}
		if !trusts(got, keploy.leaf) {
			t.Fatalf("cluster %s: the copy does not trust keploy's CA", c.name)
		}
		if want := append(append([]byte{}, c.own.pem...), keploy.pem...); !bytes.Equal(got, want) {
			t.Fatalf("cluster %s: the copy's CA is\n%s\nwant the cluster's own, then keploy's, each on lines of its own:\n%s", c.name, got, want)
		}
	}
	if _, ok := p.cluster(t, "filed")["certificate-authority"]; ok {
		t.Fatal("the copy names the CA file beside its data")
	}
	insecure := p.cluster(t, "insecure")
	if want := map[string]interface{}{"server": "https://10.0.0.2:6443", "insecure-skip-tls-verify": true}; !reflect.DeepEqual(insecure, want) {
		t.Fatalf("the insecure cluster is %v; want it as written, %v", insecure, want)
	}
	// kubectl opens a CA path beside data (and reads the data): it must
	// still name the file from the copy's place.
	if got := p.cluster(t, "both")["certificate-authority"]; got != filepath.Join(dir, "filed-ca.crt") {
		t.Fatalf("a CA path beside CA data is %v; want it absolute", got)
	}
	if got := p.cluster(t, "emptypath")["certificate-authority"]; got != "" {
		t.Fatalf("an empty CA path beside CA data became %v; want it empty", got)
	}
	if !trusts(caData(t, p.cluster(t, "emptydata")), filed.leaf) || !trusts(caData(t, p.cluster(t, "emptydata")), keploy.leaf) {
		t.Fatal("a cluster with empty CA data and a CA path does not trust its CA file and keploy's")
	}
	if !trusts(caData(t, p.cluster(t, "aliased")), keploy.leaf) {
		t.Fatal("a cluster written as a YAML alias does not trust keploy's CA")
	}
	if got := p.user(t, "empty")["tokenFile"]; got != "" {
		t.Fatalf("an empty tokenFile became %v; want it empty, as kubectl reads it", got)
	}
	public := caData(t, p.cluster(t, "public"))
	if !bytes.Equal(public, roots.pem) {
		t.Fatal("a cluster with no CA does not get the roots keploy reads with its CA")
	}

	if got := p.user(t, "kind-kp"); got["client-key-data"] != "a2V5" || got["client-certificate-data"] != "Y2VydA==" {
		t.Fatalf("the inline client certificate changed: %v", got)
	}
	files := p.user(t, "files")
	for k, want := range map[string]string{
		"client-certificate": filepath.Join(dir, "certs", "client.crt"),
		"client-key":         "/abs/client.key",
		"tokenFile":          filepath.Join(dir, "token"),
	} {
		if files[k] != want {
			t.Fatalf("user files' %s = %v; want %s", k, files[k], want)
		}
	}
	exec := p.user(t, "eks")["exec"].(map[string]interface{})
	if exec["command"] != "aws" {
		t.Fatalf("a command kubectl looks up in PATH became %v", exec["command"])
	}
	if !reflect.DeepEqual(exec["args"], []interface{}{"eks", "get-token", "--cluster-name", "prod"}) ||
		!reflect.DeepEqual(exec["env"], []interface{}{map[string]interface{}{"name": "AWS_PROFILE", "value": "prod"}}) {
		t.Fatalf("the exec credential changed: %v", exec)
	}
	if got := p.user(t, "local-exec")["exec"].(map[string]interface{})["command"]; got != filepath.Join(dir, "bin", "cred") {
		t.Fatalf("an exec command relative to the kubeconfig is %v; want it from the kubeconfig's directory", got)
	}
	ap := p.user(t, "gcp")["auth-provider"].(map[string]interface{})
	if !reflect.DeepEqual(ap, map[string]interface{}{"name": "gcp", "config": map[string]interface{}{"cmd-path": "gcloud"}}) {
		t.Fatalf("the auth provider changed: %v", ap)
	}
	if p.CurrentContext != "kind-kp" || len(p.Contexts) != 1 || p.Preferences["colors"] != true {
		t.Fatalf("the copy lost what kubectl reads besides clusters and users: %+v", p)
	}
	if !strings.Contains(string(out), "# a comment kept as written") {
		t.Fatal("the copy dropped the file's comment")
	}
}

// What keploy cannot read is said per cluster, and the rest of the copy is
// still made; a file that is not YAML is refused whole; an empty file is an
// empty config, copied as it is.
func TestRewriteSaysWhatItCouldNotDo(t *testing.T) {
	dir := t.TempDir()
	keploy := newCA(t, "keploy")
	path := filepath.Join(dir, "config")
	src := "clusters:\n- name: gone\n  cluster:\n    certificate-authority: missing.crt\n- name: bad\n  cluster:\n    certificate-authority-data: '%%%'\n- name: ok\n  cluster:\n    certificate-authority-data: " + b64(newCA(t, "ok").pem) + "\n"
	out, notes, err := Rewrite(path, []byte(src), Trust{CA: keploy.pem})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], `cluster "gone"`) || !strings.Contains(notes[0], filepath.Join(dir, "missing.crt")) ||
		!strings.Contains(notes[1], `cluster "bad"`) || !strings.Contains(notes[1], "not base64") {
		t.Fatalf("notes %q; want one for cluster gone (naming its CA's absolute path) and one for cluster bad", notes)
	}
	p := parse(t, out)
	if got := p.cluster(t, "gone")["certificate-authority"]; got != filepath.Join(dir, "missing.crt") {
		t.Fatalf("an unreadable CA path is %v in the copy; want it absolute", got)
	}
	if got := p.cluster(t, "bad")["certificate-authority-data"]; got != "%%%" {
		t.Fatalf("CA data that is not base64 became %v; want it as written", got)
	}
	if !trusts(caData(t, p.cluster(t, "ok")), keploy.leaf) {
		t.Fatal("a cluster after two keploy could not change does not trust keploy's CA")
	}
	if _, _, err := Rewrite(path, []byte("clusters: [\n"), Trust{CA: keploy.pem}); err == nil {
		t.Fatal("a kubeconfig that is not YAML was copied")
	}
	if _, _, err := Rewrite(path, []byte("- a\n- b\n"), Trust{CA: keploy.pem}); err == nil {
		t.Fatal("a kubeconfig that is no mapping was copied")
	}
	if got, _, err := Rewrite(path, nil, Trust{CA: keploy.pem}); err != nil || len(got) != 0 {
		t.Fatalf("an empty kubeconfig: %q, %v; want it empty", got, err)
	}
	// insecure-skip-tls-verify: false checks the certificate: keploy's CA is added.
	out, _, err = Rewrite(path, []byte("clusters:\n- name: c\n  cluster:\n    insecure-skip-tls-verify: false\n    certificate-authority-data: "+b64(newCA(t, "c").pem)+"\n"), Trust{CA: keploy.pem})
	if err != nil || !trusts(caData(t, parse(t, out).cluster(t, "c")), keploy.leaf) {
		t.Fatalf("a cluster with insecure-skip-tls-verify: false does not trust keploy's CA (%v)", err)
	}
}

// kubectl reads the files KUBECONFIG lists, in order, each once, skipping
// empty entries and files that do not exist; with KUBECONFIG unset or empty,
// its home's .kube/config.
func TestFilesAreTheOnesKubectlReadsInItsOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte("kind: Config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	sep := string(os.PathListSeparator)
	t.Setenv(EnvVar, strings.Join([]string{"b", "", filepath.Join(dir, "missing"), a, b, dir}, sep))
	if got := Files(); !reflect.DeepEqual(got, []string{b, a}) {
		t.Fatalf("Files() = %q; want %q", got, []string{b, a})
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvVar, "")
	if got := Files(); got != nil {
		t.Fatalf("Files() = %q with no kubeconfig in the home; want none", got)
	}
	cfg := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Files(); !reflect.DeepEqual(got, []string{cfg}) {
		t.Fatalf("Files() = %q with KUBECONFIG empty; want the home's %q", got, cfg)
	}
	os.Unsetenv(EnvVar)
	if got := Files(); !reflect.DeepEqual(got, []string{cfg}) {
		t.Fatalf("Files() = %q with KUBECONFIG unset; want the home's %q", got, cfg)
	}
}

// On Windows kubectl's home is the first of %HOME%, %HOMEDRIVE%%HOMEPATH%
// and %USERPROFILE% that holds .kube\config; elsewhere $HOME.
func TestHomeFileIsInTheHomeKubectlUses(t *testing.T) {
	empty, drived, profile := t.TempDir(), t.TempDir(), t.TempDir()
	for _, h := range []string{drived, profile} {
		if err := os.MkdirAll(filepath.Join(h, ".kube"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, ".kube", "config"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	vol := filepath.VolumeName(drived) // "C:" on Windows
	if vol == "" {
		vol = drived[:1] // elsewhere any split of the path stands for drive+path
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		want       string
	}{
		{"windows: HOMEDRIVE+HOMEPATH before USERPROFILE", "windows",
			map[string]string{"HOME": empty, "HOMEDRIVE": vol, "HOMEPATH": strings.TrimPrefix(drived, vol), "USERPROFILE": profile}, filepath.Join(drived, ".kube", "config")},
		{"windows: HOMEDRIVE without HOMEPATH is no home", "windows",
			map[string]string{"HOME": empty, "HOMEDRIVE": drived, "USERPROFILE": profile}, filepath.Join(profile, ".kube", "config")},
		{"windows: USERPROFILE last", "windows", map[string]string{"HOME": empty, "USERPROFILE": profile}, filepath.Join(profile, ".kube", "config")},
		{"windows: HOME first", "windows", map[string]string{"HOME": profile, "USERPROFILE": drived}, filepath.Join(profile, ".kube", "config")},
		{"linux: HOME only", "linux", map[string]string{"HOME": empty, "USERPROFILE": profile}, ""},
		{"darwin: HOME", "darwin", map[string]string{"HOME": drived}, filepath.Join(drived, ".kube", "config")},
		{"no home is no home: not the working directory's .kube", "linux", map[string]string{"HOME": ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(drived) // holds .kube/config
			if got := homeFile(tc.goos, env(tc.env)); got != tc.want {
				t.Fatalf("homeFile = %q; want %q", got, tc.want)
			}
		})
	}
}

// Once keploy points KUBECONFIG at copies, the user's files are still what
// keploy reads (a later run in the same process copies them, not the copies
// of a run that ended), until the variable is set again by someone else.
func TestUserEnvIsTheUsersAfterKeployPointsAtCopies(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user")
	if err := os.WriteFile(user, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, user)
	for _, copies := range [][]string{{filepath.Join(dir, "copy-1")}, {filepath.Join(dir, "copy-2"), filepath.Join(dir, "copy-3")}} {
		if err := Point(copies); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv(EnvVar); got != strings.Join(copies, string(os.PathListSeparator)) {
			t.Fatalf("KUBECONFIG = %q; want the copies %q", got, copies)
		}
		if v := UserEnv(); v != user {
			t.Fatalf("UserEnv() = %q; want the user's %q", v, user)
		}
		if got := Files(); !reflect.DeepEqual(got, []string{user}) {
			t.Fatalf("Files() = %q; want the user's file", got)
		}
	}
	other := filepath.Join(dir, "other")
	t.Setenv(EnvVar, other)
	if v := UserEnv(); v != other {
		t.Fatalf("UserEnv() = %q after the variable was set again; want %q", v, other)
	}
	os.Unsetenv(EnvVar)
	if err := Point([]string{filepath.Join(dir, "copy-4")}); err != nil {
		t.Fatal(err)
	}
	if v := UserEnv(); v != "" {
		t.Fatalf("UserEnv() = %q; want it empty, as the user had it (unset)", v)
	}
}

// keploy reads each user's client key, inline or from the file client-key
// names (relative to the kubeconfig), in memory, for its agent to present
// with the certificate kubectl sends; what it cannot read is said without
// what any key holds, and the keys it read are still handed over.
func TestClientKeysAreEachUsersKey(t *testing.T) {
	dir := t.TempDir()
	inline, filed := "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n", "-----BEGIN RSA PRIVATE KEY-----\nBBBB\n-----END RSA PRIVATE KEY-----"
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "filed.key"), []byte(filed), 0o600); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("users:\n- name: inline\n  user:\n    client-key-data: "+b64([]byte(inline))+
		"\n- name: filed\n  user:\n    client-key: "+filepath.Join("keys", "filed.key")+
		"\n- name: gone\n  user:\n    client-key: gone.key\n- name: bad\n  user:\n    client-key-data: '!!'\n- name: token\n  user:\n    token: abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notYAML, empty := filepath.Join(dir, "not-yaml"), filepath.Join(dir, "empty")
	if err := os.WriteFile(notYAML, []byte("users: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, notes := ClientKeys([]string{a, filepath.Join(dir, "missing"), notYAML, empty})
	if want := inline + filed + "\n"; keys != want {
		t.Fatalf("keys = %q; want %q", keys, want)
	}
	if len(notes) != 4 || !strings.Contains(notes[0], `user "gone"`) || !strings.Contains(notes[1], `user "bad"`) || !strings.Contains(notes[2], "missing") ||
		!strings.Contains(notes[3], "not-yaml") {
		t.Fatalf("notes = %q; want user gone, user bad, the missing file and the file that is not YAML", notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "AAAA") || strings.Contains(n, "BBBB") {
			t.Fatalf("a note shows a key: %q", n)
		}
	}

	t.Setenv(EnvVar, a)
	if got := WithClientKeys(zap.NewNop(), "CONFIGURED", false); got != "CONFIGURED\n"+inline+filed+"\n" {
		t.Fatalf("WithClientKeys = %q; want record.tlsPrivateKeyPath's key, then the kubeconfig's", got)
	}
	if got := WithClientKeys(zap.NewNop(), "CONFIGURED", true); got != "CONFIGURED" {
		t.Fatalf("WithClientKeys = %q for an app in a container; want record.tlsPrivateKeyPath's key alone", got)
	}
	t.Setenv(EnvVar, filepath.Join(dir, "missing"))
	if got := WithClientKeys(zap.NewNop(), "CONFIGURED", false); got != "CONFIGURED" {
		t.Fatalf("WithClientKeys = %q with no kubeconfig; want record.tlsPrivateKeyPath's key alone", got)
	}
}
