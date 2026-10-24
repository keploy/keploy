package learn

import (
	"encoding/base64"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The base64 runs a text holds are read byte by byte, exactly as the
// pattern [A-Za-z0-9+/_-]+={0,6} reads them: as long as the alphabet goes,
// then at most six '='.
func TestB64RunSpansReadAsThePattern(t *testing.T) {
	pattern := regexp.MustCompile(`[A-Za-z0-9+/_-]+={0,6}`)
	for _, s := range []string{
		"", "abc", "a=b", "QUJD=======x", "x QUJDRA== y", "é_-+/9=", "==a==", "a.b/c", "éabcé", "a\nb=\r\n=c",
	} {
		var want [][2]int
		for _, ix := range pattern.FindAllStringIndex(s, -1) {
			want = append(want, [2]int{ix[0], ix[1]})
		}
		if got := b64RunSpans(s); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: spans %v, want %v", s, got, want)
		}
		if s != "" && isB64Run(s) != (pattern.FindString(s) == s) {
			t.Errorf("%q: whole run %v", s, isB64Run(s))
		}
	}
}

// A text holding no '%' and no '\\' reads as itself; one whose percent
// escape decodes to a backslash reads that backslash's escape too.
func TestUnescapedSkipsAPassWithNoLeadByte(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":       "plain text",
		"a%20b":            "a b",
		`a%5Cnb`:           "a b",
		`xéy`:              "xéy",
		`q\"r`:             "q r",
		`%255C`:            `\`,
		`😀 ok`:             "\U0001F600 ok",
		`%zz and \q stays`: `%zz and \q stays`,
	} {
		if got := unescaped(in); got != want {
			t.Errorf("unescaped(%q) = %q, want %q", in, got, want)
		}
	}
}

// A text's words, by reading, are the words of each of its forms, whether
// read with its forms (rawTokens) or not (rawExtra first).
func TestTextCacheReadsWordsOfEveryForm(t *testing.T) {
	text := `{"token":"` + base64.StdEncoding.EncodeToString([]byte("user-42 Hello")) + `","q":"a%20b"}`
	c := newTextCache()
	if c.extraOf(text) != strings.Join(textForms(text)[1:], "\n") {
		t.Fatalf("extra %q", c.extraOf(text))
	}
	for i, word := range readings {
		want := map[string]bool{}
		for _, f := range textForms(text) {
			for w := range tokensBy(f, word) {
				want[w] = true
			}
		}
		if got := c.tokensOf(text)[i]; !reflect.DeepEqual(got, want) {
			t.Fatalf("reading %d: words %v, want %v", i, got, want)
		}
	}
	if !c.tokensOf(text)[0]["Hello"] {
		t.Fatal("the decoded form's words are missing")
	}
}

// Warm reads a staged set's recordings into the set's text cache in the
// background: what a learn step later reads of them is already there, and
// is what it would have read.
func TestTrackerWarmsTheSetsTextCache(t *testing.T) {
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Body: `{"q":"a%20b"}`},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"data":"` + base64.StdEncoding.EncodeToString([]byte("Hello there")) + `"}`},
	}}
	tr := NewTracker()
	tr.Warm([]*models.Mock{m}, nil)
	text := RawOf(m, false).Text
	deadline := time.Now().Add(5 * time.Second)
	for {
		tr.texts.mu.Lock()
		_, extra := tr.texts.extra[text]
		_, toks := tr.texts.tokens[text]
		tr.texts.mu.Unlock()
		if extra && toks {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the staged recording was not read into the set's cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fresh := newTextCache()
	if tr.texts.extraOf(text) != fresh.extraOf(text) || !reflect.DeepEqual(tr.texts.tokensOf(text), fresh.tokensOf(text)) {
		t.Fatal("the warmed reading differs from a fresh one")
	}
}
