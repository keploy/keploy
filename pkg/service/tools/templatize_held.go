package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/templates"
	yamlLib "gopkg.in/yaml.v3"
)

// heldText is template text a test set holds already, and where.
type heldText struct {
	// Text is the placeholder as the file writes it.
	Text string
	// Where names its place: a test case file and line, or a key of
	// config.yaml's template.
	Where string
}

// heldIn is the first template text the test set holds: in a test case file,
// anywhere in it, or in config.yaml's template (template, as read), a key or
// a value. nil for a plain recording, the only set templatize writes into.
//
// One rule, and no place is parsed for it: a file is scanned as it is written
// (templates.FirstHeld over its bytes, so a placeholder in a cookie's
// attribute, a header's name, a comment or a field keploy does not know
// counts as any other) and as it is read (every scalar of its YAML or JSON
// document, a key or a value, so an escape of the file's own syntax hides
// none). A secret keploy sanitize wrote is the literal it redacts, no
// template text.
func heldIn(ctx context.Context, db TestSetFiles, testSetID string, template map[string]interface{}) (*heldText, error) {
	files, err := db.TestCaseFiles(ctx, testSetID)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		held, err := heldInFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s of %s: %w", filepath.Base(f.Path), testSetID, err)
		}
		if held != nil {
			return held, nil
		}
	}
	var held *heldText
	templates.EachString(template, func(path, text string) {
		if held != nil {
			return
		}
		if found, _, ok := templates.FirstHeld(text); ok {
			held = &heldText{Text: found, Where: "config.yaml template." + path}
		}
	})
	return held, nil
}

// heldInFile is the first template text of one test case file: as written,
// else as read.
func heldInFile(f setfile.File) (*heldText, error) {
	name := filepath.Base(f.Path)
	at := func(line int) string { return fmt.Sprintf("%s line %d", name, line) }
	if text, start, ok := templates.FirstHeld(string(f.Data)); ok {
		return &heldText{Text: text, Where: at(1 + bytes.Count(f.Data[:start], []byte("\n")))}, nil
	}
	var text string
	var line int
	var err error
	if strings.EqualFold(filepath.Ext(name), ".json") {
		text, line, err = heldInJSON(f.Data)
	} else {
		text, line, err = heldInYAML(f.Data)
	}
	if err != nil || text == "" {
		return nil, err
	}
	return &heldText{Text: text, Where: at(line)}, nil
}

// heldInYAML is the first template text among the scalars of a YAML file's
// documents, keys and values, each as the file's syntax reads it (an escape
// decoded, a folded line joined, a !!binary value decoded), with its line.
func heldInYAML(data []byte) (string, int, error) {
	dec := yamlLib.NewDecoder(bytes.NewReader(data))
	for {
		var doc yamlLib.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, nil
			}
			return "", 0, err
		}
		if text, line, ok := heldInNode(&doc); ok {
			return text, line, nil
		}
	}
}

func heldInNode(n *yamlLib.Node) (string, int, bool) {
	if n.Kind == yamlLib.ScalarNode {
		value := n.Value
		if n.Tag == "!!binary" {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), "")); err == nil {
				value = string(raw)
			}
		}
		if text, _, ok := templates.FirstHeld(value); ok {
			return text, n.Line, true
		}
	}
	for _, child := range n.Content {
		if text, line, ok := heldInNode(child); ok {
			return text, line, true
		}
	}
	return "", 0, false
}

// heldInJSON is the first template text among the strings of a JSON file,
// keys and values, each decoded, with the line it ends on.
func heldInJSON(data []byte) (string, int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, nil
			}
			return "", 0, err
		}
		s, isString := tok.(string)
		if !isString {
			continue
		}
		if text, _, ok := templates.FirstHeld(s); ok {
			return text, 1 + bytes.Count(data[:dec.InputOffset()], []byte("\n")), nil
		}
	}
}

// setFiles is the test store as files, which templatize reads a set and
// replaces its files through (TestSetFiles, ConfigFiles). The error names a
// store that keeps none: templatize then changes nothing.
func (t *Tools) setFiles() (TestSetFiles, ConfigFiles, error) {
	db, ok := t.testDB.(TestSetFiles)
	if !ok {
		return nil, nil, fmt.Errorf("keploy templatize reads a test set's files and replaces them together, and this test store (%T) keeps no files", t.testDB)
	}
	conf, ok := t.testSetConf.(ConfigFiles)
	if !ok {
		return nil, nil, fmt.Errorf("keploy templatize replaces config.yaml together with the test cases, and this config store (%T) keeps no file", t.testSetConf)
	}
	return db, conf, nil
}

// stagedConfig is the test-set config store templatize writes config.yaml
// through. It reads as the store does; its Write, which
// testset.WriteTemplatedConfig calls with the config as read and the new
// template, hands the file it would write (file, the store's ConfigFile) to
// add in place of writing it, so config.yaml is replaced together with the
// test cases (TestSetFiles.ReplaceFiles) or not at all.
type stagedConfig struct {
	TestSetConfig
	file ConfigFiles
	add  func(setfile.File)
}

func (s stagedConfig) Write(ctx context.Context, testSetID string, testSet *models.TestSet) error {
	f, err := s.file.ConfigFile(ctx, testSetID, testSet)
	if err != nil {
		return err
	}
	s.add(f)
	return nil
}

// editable is a copy of tc whose request and response texts can be edited
// without changing tc: its own header maps, URL params and form values.
func editable(tc *models.TestCase) *models.TestCase {
	cp := *tc
	cp.HTTPReq.Header = copyStrings(tc.HTTPReq.Header)
	cp.HTTPReq.URLParams = copyStrings(tc.HTTPReq.URLParams)
	cp.HTTPResp.Header = copyStrings(tc.HTTPResp.Header)
	if tc.HTTPReq.Form != nil {
		cp.HTTPReq.Form = make([]models.FormData, len(tc.HTTPReq.Form))
		for i, fd := range tc.HTTPReq.Form {
			fd.Values = append([]string(nil), fd.Values...)
			cp.HTTPReq.Form[i] = fd
		}
	}
	return &cp
}

func copyStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
