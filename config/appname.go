package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	yamlLib "gopkg.in/yaml.v3"
)

// unnamedAppLine is a top-level appName line that names no app: the key, in
// any case (viper reads keys so) and quoted or not, a colon, and an empty or
// null value or none, then perhaps a comment. keploy.yml files written before
// keploy named apps hold `appName: ""`.
var unnamedAppLine = regexp.MustCompile(`^(\s*)((?i:appname)|"(?i:appname)"|'(?i:appname)')\s*:\s*(""|''|~|null|Null|NULL)?\s*(#.*)?$`)

// WithAppName is the keploy.yml doc, which names no app, naming the app name:
// doc with the line `appName: "<name>"` put before its first key, or in place
// of an appName line that names no app (unnamedAppLine), whose comment it
// keeps. Every other byte of doc is kept. A doc with no keys (empty, or
// comments only) gets the line last.
//
// It fails, and doc is not to be changed, unless the result reads as doc with
// appName set to name and nothing else changed: a doc that names an app
// already, is not a block mapping of keys ({path: x}), or names the app on a
// line of its own shape.
func WithAppName(doc []byte, name string) ([]byte, error) {
	var root yamlLib.Node
	if err := yamlLib.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	nl := "\n"
	if strings.Contains(string(doc), "\r\n") {
		nl = "\r\n"
	}
	line := "appName: " + strconv.Quote(name)
	key := "appName"
	// No document at all (an empty file, or comments only) reads as null.
	m := &yamlLib.Node{Tag: "!!null"}
	if len(root.Content) > 0 {
		m = root.Content[0]
	}
	var out string
	if m.Tag == "!!null" {
		out = string(doc)
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += nl
		}
		out += line + nl
	} else {
		if m.Kind != yamlLib.MappingNode {
			return nil, errors.New("it is not a mapping of keys")
		}
		var named *yamlLib.Node
		for i := 0; i < len(m.Content); i += 2 {
			if strings.EqualFold(m.Content[i].Value, "appName") {
				if named != nil {
					return nil, errors.New("it has more than one appName key")
				}
				named = m.Content[i]
			}
		}
		lines := strings.SplitAfter(string(doc), "\n")
		if named == nil {
			// A block mapping starts at its first key.
			lines = slices.Insert(lines, m.Line-1, strings.Repeat(" ", m.Column-1)+line+nl)
		} else {
			key = named.Value
			text, ending := cutLineEnding(lines[named.Line-1])
			g := unnamedAppLine.FindStringSubmatch(text)
			if g == nil {
				return nil, fmt.Errorf("its line %d is not an appName line keploy rewrites", named.Line)
			}
			text = g[1] + g[2] + ": " + strconv.Quote(name)
			if g[4] != "" {
				text += " " + g[4]
			}
			lines[named.Line-1] = text + ending
		}
		out = strings.Join(lines, "")
	}

	// What the line does to the file, read back: doc's keys, appName set.
	var before, after map[string]any
	if err := yamlLib.Unmarshal(doc, &before); err != nil {
		return nil, fmt.Errorf("it does not read as settings: %w", err)
	}
	if err := yamlLib.Unmarshal([]byte(out), &after); err != nil {
		return nil, fmt.Errorf("with appName added it would not read as YAML: %w", err)
	}
	want := map[string]any{key: name}
	for k, v := range before {
		if k != key {
			want[k] = v
		}
	}
	if !reflect.DeepEqual(after, want) {
		return nil, errors.New("with appName added it would read as other settings too")
	}
	return []byte(out), nil
}

// cutLineEnding splits a line of a file into its text and its ending: "\n",
// "\r\n", or "" for a last line with none.
func cutLineEnding(l string) (text, ending string) {
	text = strings.TrimSuffix(l, "\n")
	if len(text) < len(l) {
		ending = "\n"
		if t := strings.TrimSuffix(text, "\r"); len(t) < len(text) {
			text, ending = t, "\r\n"
		}
	}
	return text, ending
}
