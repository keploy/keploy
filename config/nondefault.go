package config

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	yamlLib "gopkg.in/yaml.v3"
)

// A generated keploy.yml was 251 lines, of which two were the developer's:
// the test command and the app name. Everything else was Keploy's own
// defaults, written out in full — so the one file a human is meant to read
// and edit buried its own content, and every default became a value the
// repository appeared to have chosen. A default that is copied into a
// repository also stops being a default: the next release cannot change it.
//
// So Keploy writes what DIFFERS, and nothing else. The complete set is a
// command away (`keploy config defaults`), never a file nobody edited.

// DefaultsYAML is Keploy's complete default configuration as a user could
// write it: every setting the binary applies, carrying the documentation that
// explains the ones worth explaining, minus `agent`, whose settings Keploy
// manages itself and which therefore has no business in a repository's file.
// This is what `keploy config defaults` prints, and the document every
// generated config is stripped against — one source, so the two can never
// disagree about what a default is.
//
// Two documents go into it, because neither is enough alone. The struct is
// COMPLETE: marshalling New() lists every setting, including the ones nobody
// has written a line about. The hand-written default document is DOCUMENTED:
// sixty lines explaining strictMockWindow, upstreamTls, the record buffer and
// the MySQL knobs -- written precisely because those settings are not
// self-evident, and, once the generated config stopped carrying them, at risk
// of being explained nowhere the user can reach. So: the struct supplies the
// settings, and the comments are transplanted onto them.
func DefaultsYAML() (string, error) {
	doc, err := yamlLib.Marshal(New())
	if err != nil {
		return "", fmt.Errorf("failed to assemble the defaults: %w", err)
	}
	documented, err := Merge(GetDefaultConfig(), InternalConfig)
	if err != nil {
		return "", fmt.Errorf("failed to assemble the documented defaults: %w", err)
	}
	withComments, err := transplantComments(string(doc), documented)
	if err != nil {
		return "", err
	}
	return withoutKey(withComments, "agent")
}

// transplantComments copies every comment from `from` onto the matching key of
// `onto`, matched by path. Values are never taken: the struct is the authority
// on what a default IS, the document only on how it is explained.
func transplantComments(onto, from string) (string, error) {
	var target, source yamlLib.Node
	if err := yamlLib.Unmarshal([]byte(onto), &target); err != nil {
		return "", fmt.Errorf("failed to parse the defaults: %w", err)
	}
	if err := yamlLib.Unmarshal([]byte(from), &source); err != nil {
		return "", fmt.Errorf("failed to parse the documented defaults: %w", err)
	}
	if len(target.Content) == 0 || len(source.Content) == 0 {
		return onto, nil
	}
	copyComments(target.Content[0], source.Content[0])
	out, err := yamlLib.Marshal(target.Content[0])
	if err != nil {
		return "", fmt.Errorf("failed to write the defaults: %w", err)
	}
	return string(out), nil
}

func copyComments(dst, src *yamlLib.Node) {
	if dst == nil || src == nil {
		return
	}
	if dst.HeadComment == "" {
		dst.HeadComment = src.HeadComment
	}
	if dst.LineComment == "" {
		dst.LineComment = src.LineComment
	}
	if dst.FootComment == "" {
		dst.FootComment = src.FootComment
	}
	if dst.Kind != yamlLib.MappingNode || src.Kind != yamlLib.MappingNode {
		return
	}
	for i := 0; i+1 < len(dst.Content); i += 2 {
		key, val := dst.Content[i], dst.Content[i+1]
		for j := 0; j+1 < len(src.Content); j += 2 {
			if src.Content[j].Value != key.Value {
				continue
			}
			copyComments(key, src.Content[j])
			copyComments(val, src.Content[j+1])
			break
		}
	}
}

// WithoutKey drops one top-level key from a config document, preserving the
// order of the rest. Used for `agent`, whose settings Keploy resolves per run
// and which therefore has no business in a repository's file.
func WithoutKey(doc, key string) (string, error) { return withoutKey(doc, key) }

// withoutKey drops one top-level key, preserving the order of the rest.
func withoutKey(doc, key string) (string, error) {
	if err := singleDocument(doc); err != nil {
		return "", err
	}
	var node yamlLib.Node
	if err := yamlLib.Unmarshal([]byte(doc), &node); err != nil {
		return "", fmt.Errorf("failed to parse the config: %w", err)
	}
	if len(node.Content) == 0 {
		return doc, nil
	}
	root := node.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			break
		}
	}
	out, err := yamlLib.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("failed to write the config: %w", err)
	}
	return string(out), nil
}

// pinned are the settings that describe artifacts ALREADY ON DISK. They are
// written even when they equal today's default, because the file they
// describe outlives the default: a release that changed storageFormat would
// otherwise orphan every recording in a repository whose keploy.yml never
// mentioned it, and nothing in the repo would have changed. Everything else
// is safe to omit -- a changed default is a changed behaviour, which is what
// a default is for.
var pinned = map[string]bool{
	"storageFormat":         true, // the on-disk format of every test set and mock
	"mock.name":             true, // which recorded set replay looks for
	"record.testCaseNaming": true, // what recorded test-case FILES are called
}

// StripDefaults returns cfgYAML with every key whose value already equals
// Keploy's default removed. Key order is preserved (it is the order of the
// default document, which is the struct's order), and a mapping left empty by
// the strip is dropped with it. Anything the defaults do not mention is kept.
//
// Defaults are supplied rather than read from the package so a caller can
// strip against the exact document it would otherwise have written.
func StripDefaults(cfgYAML, defaultsYAML string) (string, error) {
	// Both helpers rewrite ONE document. A second one in the same file would
	// be silently dropped on the way out -- the caller would get a config
	// shorter than the one it handed in, with no error to say so.
	if err := singleDocument(cfgYAML); err != nil {
		return "", err
	}
	var cfg, def yamlLib.Node
	if err := yamlLib.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		return "", fmt.Errorf("failed to parse the config: %w", err)
	}
	if err := yamlLib.Unmarshal([]byte(defaultsYAML), &def); err != nil {
		return "", fmt.Errorf("failed to parse the defaults: %w", err)
	}
	if len(cfg.Content) == 0 {
		return cfgYAML, nil
	}
	var defRoot *yamlLib.Node
	if len(def.Content) > 0 {
		defRoot = def.Content[0]
	}
	kept := stripNode(cfg.Content[0], defRoot)
	if kept == nil || len(kept.Content) == 0 {
		return "", nil
	}
	out, err := yamlLib.Marshal(kept)
	if err != nil {
		return "", fmt.Errorf("failed to write the config: %w", err)
	}
	return string(out), nil
}

// stripNode returns the part of node that differs from def, or nil when there
// is nothing left to say.
func stripNode(node, def *yamlLib.Node) *yamlLib.Node {
	return stripAt(node, def, "")
}

func stripAt(node, def *yamlLib.Node, path string) *yamlLib.Node {
	if def == nil {
		return node
	}
	if node.Kind != yamlLib.MappingNode || def.Kind != yamlLib.MappingNode {
		if sameNode(node, def) && !pinned[path] {
			return nil
		}
		return node
	}
	out := *node
	out.Content = nil
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		defVal := mapValue(def, key.Value)
		if defVal == nil {
			out.Content = append(out.Content, key, val)
			continue
		}
		// A nested mapping keeps only its own differing keys, so one
		// changed timeout does not drag its eleven siblings in with it.
		child := key.Value
		if path != "" {
			child = path + "." + key.Value
		}
		if kept := stripAt(val, defVal, child); kept != nil {
			out.Content = append(out.Content, key, kept)
		}
	}
	if len(out.Content) == 0 {
		return nil
	}
	return &out
}

// singleDocument refuses a multi-document stream, which these helpers cannot
// rewrite without losing everything after the first `---`.
func singleDocument(doc string) error {
	dec := yamlLib.NewDecoder(strings.NewReader(doc))
	seen := 0
	for {
		var n yamlLib.Node
		err := dec.Decode(&n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to parse the config: %w", err)
		}
		// A trailing `---` opens a document with nothing in it. That is a
		// separator, not a second config, and refusing it would reject a file
		// Keploy reads without complaint. yaml.v3 hands it back as a document
		// node carrying a single null scalar, not as an empty one -- so the
		// obvious test for "no content" never fired.
		if len(n.Content) == 0 || n.Content[0].Kind == 0 || n.Content[0].Tag == "!!null" {
			continue
		}
		seen++
		if seen > 1 {
			return fmt.Errorf("this config holds more than one YAML document; Keploy reads one")
		}
	}
	return nil
}

func mapValue(mapping *yamlLib.Node, key string) *yamlLib.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// sameNode compares what two subtrees MEAN, not how they were written: 0x0
// and 0 are the same port, "localhost" and localhost the same host, and a
// comment is not a difference. Comparing the marshalled YAML instead kept
// every default a developer had merely restyled.
func sameNode(a, b *yamlLib.Node) bool {
	var av, bv interface{}
	if err := a.Decode(&av); err != nil {
		return false
	}
	if err := b.Decode(&bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
