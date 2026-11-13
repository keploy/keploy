// Package kubeconfig makes the kubeconfig a test command keploy starts reads
// trust keploy's CA, and reads the client keys its users hold.
//
// kubectl and helm check the API server's certificate against the CA their
// kubeconfig names (certificate-authority or certificate-authority-data), not
// against the trust store the environment names (SSL_CERT_FILE and its kind,
// which keploy points at its CA for the app). So every call they made through
// keploy's proxy failed its handshake with keploy's certificate. The app is
// pointed (KUBECONFIG) at copies, one per file kubectl reads and in kubectl's
// order, so kubectl merges the copies as it merges the files themselves. As
// the run ends, what the test changed in the copies (a cluster kind made, a
// context it set) is written back to the user's files (WriteBack), never
// what keploy itself set in them.
//
// A user that authenticates with a client certificate sends it to keploy's
// proxy, which presents it to the real API server at record with that
// certificate's private key. The keys are read from the same files
// (ClientKeys) and handed to keploy's agent in memory, never written anywhere.
// Exec and auth-provider credentials (tokens) pass through unchanged.
package kubeconfig

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// EnvVar is the variable kubectl and helm read their kubeconfig files from.
const EnvVar = "KUBECONFIG"

// pointed is what keploy last set KUBECONFIG to (Point), and the user's
// KUBECONFIG it replaced, so a later run in the same process reads the
// user's files, not the copies of a run that has ended. kubectl reads an
// unset KUBECONFIG as an empty one.
var pointed struct {
	mu    sync.Mutex
	value string // what Point set; "" before the first Point
	user  string
}

// UserEnv is KUBECONFIG as the user set it: what it held before keploy
// pointed it at copies (Point) while it still holds keploy's value, else what
// it holds.
func UserEnv() string {
	pointed.mu.Lock()
	defer pointed.mu.Unlock()
	return userEnvLocked()
}

func userEnvLocked() string {
	if cur := os.Getenv(EnvVar); cur != pointed.value {
		return cur
	}
	return pointed.user
}

// Point sets KUBECONFIG, for the processes keploy starts, to copies, in
// order, keeping the user's value for UserEnv.
func Point(copies []string) error {
	pointed.mu.Lock()
	defer pointed.mu.Unlock()
	pointed.user = userEnvLocked()
	pointed.value = strings.Join(copies, string(os.PathListSeparator))
	return os.Setenv(EnvVar, pointed.value)
}

// Files is the kubeconfig files kubectl reads, in its order: the files
// KUBECONFIG lists as the user set it (UserEnv), each once, leaving out empty
// entries and files that do not exist, as kubectl does; when KUBECONFIG is
// unset or empty, the .kube/config of the home kubectl uses, when it exists.
// A relative entry is made absolute from the working directory.
func Files() []string {
	if v := UserEnv(); v != "" {
		return listed(filepath.SplitList(v))
	}
	if f := homeFile(runtime.GOOS, os.Getenv); f != "" {
		return []string{f}
	}
	return nil
}

// listed is the regular files of entries, each once, in order, made absolute.
func listed(entries []string) []string {
	var files []string
	seen := map[string]bool{}
	for _, e := range entries {
		// An empty entry is the working directory: no regular file.
		abs, err := filepath.Abs(e)
		if err != nil || seen[abs] || !regular(abs) {
			continue
		}
		seen[abs] = true
		files = append(files, abs)
	}
	return files
}

// homeFile is .kube/config in the home kubectl uses on goos, when it is a
// file: $HOME; on Windows the first of %HOME%, %HOMEDRIVE%%HOMEPATH% and
// %USERPROFILE% that holds one (client-go's homedir.HomeDir).
func homeFile(goos string, getenv func(string) string) string {
	homes := []string{getenv("HOME")}
	if goos == "windows" {
		if drive, path := getenv("HOMEDRIVE"), getenv("HOMEPATH"); drive != "" && path != "" {
			homes = append(homes, drive+path)
		}
		homes = append(homes, getenv("USERPROFILE"))
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		if f := filepath.Join(h, ".kube", "config"); regular(f) {
			return f
		}
	}
	return ""
}

func regular(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// Trust is what a copy's clusters trust: CA, keploy's CA certificate, added to
// each cluster's own CA; Bundle, the roots keploy reads with its CA, for a
// cluster that names no CA (kubectl checks it against the system's roots).
type Trust struct {
	CA     []byte
	Bundle []byte
}

// Rewrite is data, the kubeconfig file at path, as the copy keploy points the
// app at: each cluster that checks its server's certificate trusts keploy's
// CA too (certificate-authority-data: its own CA, read from the file a
// certificate-authority path names, then keploy's), a cluster with
// insecure-skip-tls-verify is left as it is, and every relative path kubectl
// resolves from the file's directory (certificate-authority, client-certificate,
// client-key, tokenFile, an exec command holding a path separator) is made
// absolute, since the copy lives elsewhere. Everything else is kept as written.
// notes say what keploy could not do, per cluster, for the run's log.
func Rewrite(path string, data []byte, trust Trust) (out []byte, notes []string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("the kubeconfig %s is not YAML kubectl reads: %w", path, err)
	}
	if len(doc.Content) == 0 {
		// An empty file: kubectl reads it as an empty config.
		return data, nil, nil
	}
	root := deref(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("the kubeconfig %s is not a YAML mapping", path)
	}
	dir := filepath.Dir(path)
	for _, c := range entries(root, "clusters", "cluster") {
		if note := trustKeploy(c, dir, trust); note != "" {
			notes = append(notes, fmt.Sprintf("kubeconfig %s, cluster %q: %s", path, c.name, note))
		}
	}
	for _, u := range entries(root, "users", "user") {
		for _, key := range []string{"client-certificate", "client-key", "tokenFile"} {
			if n := get(u.body, key); n.Value != "" {
				n.Value = resolve(dir, n.Value)
			}
		}
		// kubectl resolves an exec command from the file's directory only
		// when it holds a path separator; a bare name is looked up in PATH.
		if c := get(get(u.body, "exec"), "command"); strings.ContainsRune(c.Value, filepath.Separator) {
			c.Value = resolve(dir, c.Value)
		}
	}
	out, err = yaml.Marshal(&doc)
	if err != nil {
		return nil, nil, fmt.Errorf("could not write the copy of the kubeconfig %s: %w", path, err)
	}
	return out, notes, nil
}

// trustKeploy makes cluster c trust keploy's CA beside its own. It returns why
// it could not, or "".
func trustKeploy(c entry, dir string, trust Trust) string {
	// A value that is no bool leaves skip false: kubectl checks the
	// certificate (it would refuse such a file anyway).
	var skip bool
	_ = get(c.body, "insecure-skip-tls-verify").Decode(&skip)
	if skip {
		// kubectl checks no certificate, and refuses a CA beside this.
		return ""
	}
	if d := get(c.body, "certificate-authority-data"); d.Value != "" {
		own, err := base64.StdEncoding.DecodeString(d.Value)
		if err != nil {
			return "its certificate-authority-data is not base64, so keploy left it as it is; kubectl will not trust keploy's CA for it"
		}
		setString(d, base64.StdEncoding.EncodeToString(withPEM(own, trust.CA)))
		if p := get(c.body, "certificate-authority"); p.Value != "" {
			// kubectl reads the data and ignores the path beside it.
			p.Value = resolve(dir, p.Value)
		}
		return ""
	}
	if p := get(c.body, "certificate-authority"); p.Value != "" {
		p.Value = resolve(dir, p.Value)
		own, err := os.ReadFile(p.Value)
		if err != nil {
			return fmt.Sprintf("keploy could not read its certificate-authority %s (%v), so kubectl will not trust keploy's CA for it", p.Value, err)
		}
		del(c.body, "certificate-authority")
		set(c.body, "certificate-authority-data", base64.StdEncoding.EncodeToString(withPEM(own, trust.CA)))
		return ""
	}
	// No CA: kubectl checks the server against the system's roots. The copy
	// names the roots keploy reads, with its CA, as SSL_CERT_FILE does for
	// the app: Go reads that variable on Linux only, not on macOS or Windows.
	set(c.body, "certificate-authority-data", base64.StdEncoding.EncodeToString(trust.Bundle))
	return ""
}

// withPEM is own followed by add, on a line of its own.
func withPEM(own, add []byte) []byte {
	out := append(bytes.TrimRight(own, "\r\n"), '\n')
	return append(out, add...)
}

// resolve is p made absolute from dir, as kubectl resolves a path written in
// a kubeconfig file.
func resolve(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// ClientKeys is the private key, as PEM, of each user of files that holds a
// client certificate's key (client-key-data, or the file client-key names),
// for keploy's agent to present with the certificate the client sends.
// notes say which keys keploy could not read, never what a key holds.
func ClientKeys(files []string) (keys string, notes []string) {
	var b strings.Builder
	add := func(k []byte) {
		b.Write(bytes.TrimRight(k, "\r\n"))
		b.WriteByte('\n')
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			notes = append(notes, fmt.Sprintf("keploy could not read the kubeconfig %s: %v", f, err))
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			notes = append(notes, fmt.Sprintf("the kubeconfig %s is not YAML kubectl reads: %v", f, err))
			continue
		}
		// A document holds its one root; an empty file, none.
		for _, u := range usersOf(doc.Content) {
			if d := get(u.body, "client-key-data"); d.Value != "" {
				k, err := base64.StdEncoding.DecodeString(d.Value)
				if err != nil {
					notes = append(notes, fmt.Sprintf("kubeconfig %s, user %q: its client-key-data is not base64", f, u.name))
					continue
				}
				add(k)
				continue
			}
			if p := get(u.body, "client-key"); p.Value != "" {
				path := resolve(filepath.Dir(f), p.Value)
				k, err := os.ReadFile(path)
				if err != nil {
					notes = append(notes, fmt.Sprintf("kubeconfig %s, user %q: keploy could not read its client-key %s: %v", f, u.name, path, err))
					continue
				}
				add(k)
			}
		}
	}
	return b.String(), notes
}

// WithClientKeys is key, the private key keploy was given
// (record.tlsPrivateKeyPath), followed by the client keys of the kubeconfig
// files kubectl reads (ClientKeys): the keys keploy's agent may present with
// a certificate a client sends. An app in a container reads its own
// kubeconfig, not this machine's: it gets key alone. What keploy could not
// read is logged.
func WithClientKeys(logger *zap.Logger, key string, inContainer bool) string {
	if inContainer {
		return key
	}
	kube, notes := ClientKeys(Files())
	for _, n := range notes {
		logger.Warn(n+"; a call made with that user's client certificate cannot be recorded",
			zap.String("next_step", "make the key readable to keploy, or set record.tlsPrivateKeyPath to it"))
	}
	if kube == "" {
		return key
	}
	return strings.TrimRight(key, "\r\n") + "\n" + kube
}

// usersOf is the users of each of roots that is a mapping.
func usersOf(roots []*yaml.Node) []entry {
	var out []entry
	for _, root := range roots {
		out = append(out, entries(deref(root), "users", "user")...)
	}
	return out
}

// entry is one named item of a kubeconfig list (clusters, users): its name
// and the mapping under its field (cluster, user).
type entry struct {
	name string
	body *yaml.Node
}

// entries is each item of root's list whose field is a mapping.
func entries(root *yaml.Node, list, field string) []entry {
	var out []entry
	// An item with no field mapping gets a body in no document: what is done
	// to it changes nothing.
	for _, item := range get(root, list).Content {
		item = deref(item)
		out = append(out, entry{name: get(item, "name").Value, body: get(item, field)})
	}
	return out
}

// deref is the node alias n stands for (an anchor's), else n.
func deref(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode {
		return n.Alias
	}
	return n
}

// lookup is the value of key in mapping m, and whether m holds key. A
// scalar holds no pairs; kubectl reads no file where a mapping is a sequence.
func lookup(m *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return deref(m.Content[i+1]), true
		}
	}
	return nil, false
}

// get is the value of key in mapping m, or a new empty node, which is in
// no document, when m holds no such key or is no mapping.
func get(m *yaml.Node, key string) *yaml.Node {
	if n, ok := lookup(m, key); ok {
		return n
	}
	return &yaml.Node{}
}

// set gives key in mapping m the string value v, adding the key when absent.
func set(m *yaml.Node, key, v string) {
	if n, ok := lookup(m, key); ok {
		setString(n, v)
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
}

func setString(n *yaml.Node, v string) {
	n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, "!!str", v, 0
}

// del removes key from mapping m.
func del(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
