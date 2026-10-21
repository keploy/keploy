// Package templates holds the one meaning keploy gives a placeholder in a test
// case, and the per-run values those placeholders take.
//
// The rules, all of them:
//
//  1. A value written out is compared as written. Nothing is matched, or let
//     off, because of what its value is.
//  2. {{string .k}}, {{int .k}} and {{float .k}} are this run's value of k: sent
//     in a request, and asserted (it must be equal) in an expected response.
//  3. {{capture string .k}}, {{capture int .k}} and {{capture float .k}}, in an
//     expected response only, say "what the app answers here is k from now
//     on". The app's answer there must be present, of that JSON type and not
//     empty; that is all that is asserted about it.
//  4. For each test case: its request is sent with the current values; then
//     its response's captures take the app's answer; then its response's other
//     placeholders take the current values; then the response is compared as
//     written, with noise as configured.
//  5. Every pass over a test set starts each key at its config.yaml value (the
//     value recorded). A run never writes config.yaml.
//  6. A placeholder whose key config.yaml does not define is literal text,
//     and so is template text outside the grammar: placeholder-shaped text
//     whose key is not one name ({{.a.b}}, {{string .user-id}}), and an
//     expression ({{ .id | print }}), which an earlier keploy rendered. Each
//     is named once when the set loads.
//  7. A variable place, a capture or a use of a defined key, is asserted on
//     its own, even where noise or a body the comparison does not read
//     leaves the rest unchecked: noise says what is not compared, a variable
//     what is. A capture that finds no value fails its test case. A test
//     case that failed on a variable place stays FAILED: a replay never
//     demotes it to OBSOLETE, whatever its mock set did.
//  8. {{string .secret.K}}, which keploy sanitize writes over a value the
//     recording held, is the literal it redacts, not a variable: sent exactly
//     as the recording held it (in a URL or a form body the text sanitize
//     kept, never escaped again; inside a JSON string JSON-escaped, as
//     sanitize kept it decoded), and in an expected response compared as the
//     literal would be, noise and all.
//  9. A shape in which a variable cannot be asserted exactly at its place is
//     refused when the set loads (Run.Check, and the replay for what depends
//     on noise, --schema-match and a test case's assertions), naming the test
//     case, the place and the edit to make with the placeholder's own key;
//     the test case fails unsent. A variable in a name (a header's, a form
//     field's), in a gRPC trailer, in a part of a Set-Cookie value that is no
//     cookie (no name=) or in a test case's assertions (which are read as
//     written), and a use inside an array under test.ignoreOrdering that a
//     noise entry, --schema-match or assertions comparing no body unties
//     from its element, are such shapes. Only a placeholder of a key the set
//     defines is ever refused.
package templates

import (
	"net/url"
	"regexp"
	"strings"

	"go.keploy.io/server/v3/utils/logcontent"
)

// Types a placeholder can name.
const (
	TypeString = "string"
	TypeInt    = "int"
	TypeFloat  = "float"
)

// secretPrefix is the key prefix under which a placeholder names a value in
// the test set's secret.yaml (written by keploy sanitize) rather than in its
// config.yaml template.
const secretPrefix = "secret."

// keyRe is one key: a letter or '_', then letters, decimal digits and '_',
// letters and digits of any script. It is the identifier of text/template,
// and every key keploy templatize and keploy sanitize write is one
// (normalizeBaseKey, keyToSecretKey): writer and reader share this grammar.
const keyRe = `[\p{L}_][\p{L}\p{Nd}_]*`

// placeholderRe is the whole grammar: {{ [capture] [string|int|float] .key }}.
// The key is one identifier, or ".secret.K" for a secret. Any other key
// ({{.a.b}}, {{string .user-id}}) is not in the grammar, so such text stays
// as written (Check names it). A capture without a type matches here so that
// it can be refused by name instead of being read as text.
var placeholderRe = regexp.MustCompile(`\{\{\s*(capture\s+)?(?:(string|int|float)\s+)?\.((?:secret\.)?` + keyRe + `)\s*\}\}`)

// heldRe is all template text a set can hold: "{{", an optional capture, an
// optional string, int or float, then a '.' and any text without a brace, up
// to "}}". It takes in the grammar (placeholderRe), placeholder-shaped text
// whose key is outside it ({{.a.b}}, {{string .user-id}}), and every
// expression the replay of an earlier keploy rendered, its pattern being this
// one without the capture ({{ .x }}, {{ .id | print }}, {{.}}). That replay
// rendered the test case as JSON, where a double quote is written \", so an
// expression holding one ({{ .id | printf "%s" }}) did not parse and was text
// to it too; it is template text here all the same, being what a person meant
// as one. Text with no '.' after the "{{" and the type ({{name}}, LaTeX's
// {{\pi}}) is no template text: no keploy renders it. The groups are the
// capture, the type, and the text after the '.'.
var heldRe = regexp.MustCompile(`\{\{\s*(capture\s+)?(?:(string|int|float)\s+)?\.([^{}]*?)\}\}`)

// keyOnlyRe is one key of the grammar, a secret's included, and nothing else.
var keyOnlyRe = regexp.MustCompile(`^(?:secret\.)?` + keyRe + `$`)

// IsKey reports whether key is a key of the grammar: one name of letters,
// decimal digits and '_' that starts with a letter or '_' (or "secret." and
// one such name). A config.yaml key that is not one can never be named by a
// placeholder.
func IsKey(key string) bool { return keyOnlyRe.MatchString(key) }

// anchoredRe is placeholderRe matching only at the start of its input.
var anchoredRe = regexp.MustCompile(`^` + placeholderRe.String())

// Placeholder is one placeholder found in a piece of text.
type Placeholder struct {
	// Start and End are its byte span in the text it was found in.
	Start, End int
	// Capture marks {{capture ...}}.
	Capture bool
	// Type is "", "string", "int" or "float".
	Type string
	// Key is the key after the dot: "id", or "secret.apiKey".
	Key string
	// Text is the placeholder as written (percent-decoded when it was found
	// percent-encoded in a URL).
	Text string
}

// Secret reports whether the placeholder names a secret, and which one.
func (p Placeholder) Secret() (string, bool) {
	if strings.HasPrefix(p.Key, secretPrefix) {
		return strings.TrimPrefix(p.Key, secretPrefix), true
	}
	return "", false
}

// Format writes a placeholder: Format(true, "int", "orderId") is
// {{capture int .orderId}}.
func Format(capture bool, typ, key string) string {
	var b strings.Builder
	b.WriteString("{{")
	if capture {
		b.WriteString("capture ")
	}
	if typ != "" {
		b.WriteString(typ)
		b.WriteByte(' ')
	}
	b.WriteByte('.')
	b.WriteString(key)
	b.WriteString("}}")
	return b.String()
}

// Contains reports whether s holds a placeholder in any form Find knows.
func Contains(s string) bool {
	if !strings.Contains(s, "{{") && !hasEncodedOpen(s) {
		return false
	}
	return len(Find(s)) > 0
}

// FindOutsideGrammar returns the template text in s (heldRe) that is no
// placeholder of the grammar, in order: placeholder-shaped text whose key is
// not a key (IsKey), such as a dotted key ({{.a.b}}, {{string .user.id}}), a
// key with a '-' ({{string .user-id}}) or one that starts with a digit; and
// any other expression an earlier keploy rendered ({{ .id | print }}, {{.}}).
// Such text is not a placeholder, so it stays as written; its Key is the text
// after the '.', trimmed. A secret ({{string .secret.K}}) is in the grammar
// and is not returned.
func FindOutsideGrammar(s string) []Held {
	if !strings.Contains(s, "{{") {
		return nil
	}
	var out []Held
	for _, m := range heldRe.FindAllStringSubmatchIndex(s, -1) {
		if g := anchoredRe.FindStringIndex(s[m[0]:m[1]]); g != nil && g[1] == m[1]-m[0] {
			continue
		}
		out = append(out, heldAt(s, m))
	}
	return out
}

// Held is template text outside the grammar that a text holds
// (FindOutsideGrammar), with a Placeholder's fields: its span, whether it
// captures, its type, the text after its '.', trimmed, as its key, and its
// text. It is no placeholder: its key can be any text without a brace, as an
// application's own value can be, so it is a type of its own.
type Held struct {
	Start, End int
	Capture    bool
	Type       string
	Key        string
	Text       string
}

// heldAt is the template text heldRe matched at m in s.
func heldAt(s string, m []int) Held {
	h := Held{Start: m[0], End: m[1], Text: s[m[0]:m[1]], Capture: m[2] >= 0}
	if m[4] >= 0 {
		h.Type = s[m[4]:m[5]]
	}
	h.Key = strings.TrimSpace(s[m[6]:m[7]])
	return h
}

// FirstHeld returns the first template text in s and where it starts; ok is
// false when s holds none. Template text is a match of heldRe, or a
// placeholder of the grammar written percent-encoded (Find). A secret keploy
// sanitize wrote ({{string .secret.K}}, percent-encoded too) is the literal
// it redacts, no template text; a capture of a secret, which sanitize never
// writes, is. s is any text: one value, or a whole file.
func FirstHeld(s string) (text string, at int, ok bool) {
	if !strings.Contains(s, "{{") && !hasEncodedOpen(s) {
		return "", 0, false
	}
	at = -1
	take := func(start int, t string) {
		if at < 0 || start < at {
			at, text = start, t
		}
	}
	secret := map[int]bool{}
	for _, p := range Find(s) {
		if _, is := p.Secret(); is && !p.Capture {
			secret[p.Start] = true
			continue
		}
		take(p.Start, p.Text)
	}
	for _, m := range heldRe.FindAllStringIndex(s, -1) {
		if !secret[m[0]] {
			take(m[0], s[m[0]:m[1]])
		}
	}
	if at < 0 {
		return "", 0, false
	}
	return text, at, true
}

// Find returns the placeholders in s, in order. It also finds a placeholder
// written percent-encoded (%7B%7Bstring%20.id%7D%7D), as a URL holds one once
// something has escaped it.
func Find(s string) []Placeholder {
	var out []Placeholder
	if strings.Contains(s, "{{") {
		for _, m := range placeholderRe.FindAllStringSubmatchIndex(s, -1) {
			out = append(out, placeholderAt(s, m))
		}
	}
	if hasEncodedOpen(s) {
		out = append(out, findEncoded(s)...)
		sortByStart(out)
	}
	return out
}

// placeholderAt is the placeholder of the grammar placeholderRe matched at m
// in s: its whole text, whether it captures, its type and its key, each a
// match of the grammar's own groups ({{ [capture] [string|int|float]
// .key }}, a key of letters, digits and '_'), whatever else s holds.
func placeholderAt(s string, m []int) Placeholder {
	p := Placeholder{Start: m[0], End: m[1], Text: s[m[0]:m[1]]}
	p.Capture = m[2] >= 0
	if m[4] >= 0 {
		p.Type = s[m[4]:m[5]]
	}
	p.Key = s[m[6]:m[7]]
	return p
}

// findEncoded finds %7B%7B ... %7D%7D spans whose percent-decoded text is a
// whole placeholder.
func findEncoded(s string) []Placeholder {
	var out []Placeholder
	lower := strings.ToLower(s)
	from := 0
	for {
		i := strings.Index(lower[from:], "%7b%7b")
		if i < 0 {
			return out
		}
		start := from + i
		j := strings.Index(lower[start:], "%7d%7d")
		if j < 0 {
			return out
		}
		end := start + j + len("%7d%7d")
		decoded, err := url.PathUnescape(s[start:end])
		if err == nil {
			if m := anchoredRe.FindStringSubmatchIndex(decoded); m != nil && m[1] == len(decoded) {
				p := placeholderAt(decoded, m)
				p.Start, p.End = start, end
				out = append(out, p)
				from = end
				continue
			}
		}
		from = start + len("%7b%7b")
	}
}

// hasEncodedOpen reports whether s holds "%7B%7B" in any letter case. It looks
// for '%' first, so a body without one costs a single scan and no copy.
func hasEncodedOpen(s string) bool {
	const want = "%7b%7b"
	for i := strings.IndexByte(s, '%'); i >= 0 && i+len(want) <= len(s); {
		if strings.EqualFold(s[i:i+len(want)], want) {
			return true
		}
		next := strings.IndexByte(s[i+1:], '%')
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

func sortByStart(ps []Placeholder) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Start < ps[j-1].Start; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

// Shape is text, a placeholder or template text a test case holds, as a line
// logs it: whole when it is one placeholder of the grammar ({{string .id}}),
// whose words are a template variable's name, or when KEPLOY_LOG_CONTENT is
// set (logcontent.On); otherwise "{{.{}}}". Template text outside the
// grammar ({{.a.b}}, {{ .id | print }}) is any text without a brace after
// its '.', as an application's own value can be, so the line names where it
// is instead of what it says.
func Shape(text string) string {
	if logcontent.On() {
		return text
	}
	if g := anchoredRe.FindStringIndex(text); g != nil && g[1] == len(text) {
		return text
	}
	return "{{.{}}}"
}

// KeyShape is key, a template variable's key, as a line logs it: whole when
// it is a key of the grammar (IsKey), one name of letters, digits and '_', or
// when KEPLOY_LOG_CONTENT is set; otherwise "{}", as Shape writes template
// text outside the grammar.
func KeyShape(key string) string {
	if logcontent.On() || IsKey(key) {
		return key
	}
	return "{}"
}
