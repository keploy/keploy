package kubeconfig

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"gopkg.in/yaml.v3"
)

// Changes is what a test wrote, while it ran, to a copy of the kubeconfig
// keploy pointed it at (Rewrite). kubectl config, kind create cluster and helm
// write the first file KUBECONFIG names, which is keploy's copy; the copy goes
// with the run, and keploy never writes the user's file, so the user's file
// does not get what the test wrote. keploy says so at the run's end, naming
// what was lost.
type Changes struct {
	// Added, Changed and Removed are the clusters, contexts and users the test
	// added, changed or removed, as "cluster NAME", "context NAME" and
	// "user NAME": clusters first, then contexts, then users, each by name.
	Added, Changed, Removed []string
	// CurrentContext is `"FROM" -> "TO"` when the test switched the current
	// context, else "".
	CurrentContext string
	// Untrusted is each cluster the test added or changed whose own CA leaves
	// keploy's out, as "NAME (SERVER)": kubectl and helm refuse keploy's
	// certificate for it, so their calls to it through keploy are neither
	// recorded nor mocked. A cluster that names no CA is not in it: kubectl
	// checks its server against the system's roots, which keploy points
	// SSL_CERT_FILE at with its CA.
	Untrusted []string
}

// Empty is whether the test changed none of the kubeconfig's clusters,
// contexts and users, nor its current context.
func (c Changes) Empty() bool {
	return len(c.Added) == 0 && len(c.Changed) == 0 && len(c.Removed) == 0 && c.CurrentContext == ""
}

// Diff is what now, the copy at path as the run ends, holds that wrote, the
// copy as keploy wrote it, did not. ca is keploy's CA certificate (PEM). An
// entry is compared by what it holds, not how it is written: kubectl writes
// the whole file again to change one entry, in its own order and leaving out
// fields that hold nothing (false, "", empty lists and mappings), and adds
// some that hold nothing. A file that holds nothing reads as an empty config.
func Diff(path string, wrote, now, ca []byte) (Changes, error) {
	before, err := configOf(wrote)
	if err != nil {
		return Changes{}, fmt.Errorf("the copy of the kubeconfig keploy wrote to %s: %w", path, err)
	}
	after, err := configOf(now)
	if err != nil {
		return Changes{}, fmt.Errorf("the kubeconfig the test left at %s: %w", path, err)
	}
	var c Changes
	for _, l := range []struct{ list, field, kind string }{
		{"clusters", "cluster", "cluster"},
		{"contexts", "context", "context"},
		{"users", "user", "user"},
	} {
		was, is := named(before, l.list), named(after, l.list)
		for _, name := range sortedNames(is) {
			old, ok := was[name]
			switch {
			case !ok:
				c.Added = append(c.Added, l.kind+" "+name)
			case !reflect.DeepEqual(old, is[name]):
				c.Changed = append(c.Changed, l.kind+" "+name)
			default:
				continue
			}
			if l.kind == "cluster" {
				body, _ := is[name][l.field].(map[string]any)
				if !trustsCA(body, filepath.Dir(path), ca) {
					server, _ := body["server"].(string)
					c.Untrusted = append(c.Untrusted, fmt.Sprintf("%s (%s)", name, server))
				}
			}
		}
		for _, name := range sortedNames(was) {
			if _, ok := is[name]; !ok {
				c.Removed = append(c.Removed, l.kind+" "+name)
			}
		}
	}
	from, _ := before["current-context"].(string)
	to, _ := after["current-context"].(string)
	if from != to {
		c.CurrentContext = fmt.Sprintf("%q -> %q", from, to)
	}
	return c, nil
}

// configOf is the kubeconfig data holds, with every field that holds nothing
// left out (pruned).
func configOf(data []byte) (map[string]any, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("not YAML kubectl reads: %w", err)
	}
	m, ok := pruned(v).(map[string]any)
	if !ok && v != nil {
		return nil, fmt.Errorf("not a YAML mapping")
	}
	return m, nil
}

// named is each item of cfg's list by its name; of items that share a name
// (kubectl refuses such a file), the first.
func named(cfg map[string]any, list string) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := cfg[list].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		name, _ := m["name"].(string)
		if _, ok := out[name]; !ok && m != nil {
			out[name] = m
		}
	}
	return out
}

func sortedNames(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// pruned is v without the fields, list items aside, that hold nothing.
func pruned(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			if e = pruned(e); !empty(e) {
				out[k] = e
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = pruned(e)
		}
		return out
	}
	return v
}

// empty is whether v holds nothing: nil, false, "", or an empty list or
// mapping. kubectl leaves such a field out, or writes one it reads as unset.
func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// trustsCA is whether kubectl, reading cluster body from a file in dir,
// accepts a certificate keploy's CA (ca, PEM) signed: it checks none
// (insecure-skip-tls-verify), its CA holds keploy's, or it names no CA.
func trustsCA(body map[string]any, dir string, ca []byte) bool {
	if skip, _ := body["insecure-skip-tls-verify"].(bool); skip {
		return true
	}
	ca = bytes.TrimSpace(ca)
	// kubectl reads certificate-authority-data before a path beside it.
	if d, ok := body["certificate-authority-data"].(string); ok {
		// What decodes before a byte that is no base64 is no CA: kubectl
		// refuses the whole value.
		own, err := base64.StdEncoding.DecodeString(d)
		return err == nil && bytes.Contains(own, ca)
	}
	if p, ok := body["certificate-authority"].(string); ok {
		// A file keploy cannot read holds no CA (kubectl refuses it).
		own, _ := os.ReadFile(resolve(dir, p))
		return bytes.Contains(own, ca)
	}
	return true
}
