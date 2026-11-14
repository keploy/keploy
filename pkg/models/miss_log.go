package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Stated limits of this file's shaping of keploy's own words (a next step, a
// why) for a log line. Each is a form no producer of keploy's advice writes
// today; one that starts to must write its value as the rules here read one:
//   - a string quoted after "header " or written after "header." is kept,
//     read as a header's name (the answer's header "<name>" is "{}"; a
//     refusal's place mock-1 header.<name>), whatever it holds;
//   - a string inside a placeholder's braces is kept ("{{<key>}}"), read as
//     a template key;
//   - a value in single quotes or bare passes through (the value '<v>'
//     changed): only a double-quoted or backquoted string is read as one;
//   - a worker-mock rule's problem quotes its expression in backquotes
//     (regexp's error), and utils.WithholdQuoted withholds backquoted
//     strings in pairs: an expression with an odd number of backquotes in
//     it leaves its text after the last one.

// MissLogShape is how a log line names the call r missed: never a value the
// call sent. An HTTP call is named by its method and its path's shape, every
// segment written {} (a segment of any look can hold a value: a user's name,
// a reset link's token, a session id), without its query; any other protocol
// by its CallShape (a gRPC method, a SQL statement with its literals written
// ?, a Mongo command and its namespace's shape), or by the command its
// parser read (CommandName). The report keeps the call's summary for the
// user's own view of the miss; a log line reaches a log store. "" for a nil
// r, or a call it can name none of.
func MissLogShape(r *MockMismatchReport) string {
	if r == nil {
		return ""
	}
	if isHTTPProtocol(r.Protocol) {
		// With the label of the operation it carries (a GraphQL query's
		// name): a label validOperationLabel took, a name, not a value.
		return WithHTTPOperation(httpCallLogShape(r.ActualSummary), SanitizedOperation(r.Operation))
	}
	if s := CallShape(r.Protocol, r.ActualSummary); s != "" {
		return s
	}
	return CommandName(r.Command)
}

// MissLogNextStep is r's next step as a log line carries it: keploy's own
// advice, with every field path it names in its shape (the rule of
// UnmatchedCall.SanitizedNextStep: a map's key can be a value, a session id
// or an email), and every string it quotes in its shape (quotedLogShape): a
// URL path with each segment {}, a field path, a method and a sessionCalls
// host kept, anything else {}. A refused word's line (learn.RefusalLine,
// which a learn round's miss carries) quotes the recorded value and round
// F's, and its why can quote one: the line is logged by its where, its kind,
// its places and its edits' kinds (refusalLogForm). A next step whose quotes
// do not pair, so that a value may stand outside them, is logged by its
// size, and so is one that holds a backquote: an error it quotes (YAML's)
// quotes a value so. The report keeps the advice whole for the user's own
// view.
func MissLogNextStep(r *MockMismatchReport) string {
	if r == nil || r.NextSteps == "" {
		return ""
	}
	return adviceShape(r.NextSteps, r.FieldDiffs)
}

// adviceShape is text, keploy's advice about a call whose fields diffs are,
// with no value of the call: MissLogNextStep's shaping, which evidence's next
// step (UnmatchedCall.SanitizedNextStep) shares.
func adviceShape(text string, diffs []MockFieldDiff) string {
	next := refusedWordsInLogForm(text)
	next = UnmatchedCall{NextSteps: next, FieldDiffs: diffs}.shapedText(next)
	shaped, ok := quotedInLogForm(next)
	if !ok || strings.ContainsRune(next, '`') {
		return fmt.Sprintf("(a next step of %d bytes, withheld: its quotes do not pair, or it holds a backquote; the miss report has it)", len(text))
	}
	return shaped
}

// AdviceLogText is keploy's own words about a call or a test case (a next
// step, a why) as a log line carries them, shaped as MissLogNextStep shapes
// a miss's next step: every field path and quoted URL path in its shape,
// every quoted value {}, a refused word's line without its values, and the
// whole withheld where its quotes do not pair. The report keeps the words
// whole for the user's own view.
func AdviceLogText(text string) string {
	return MissLogNextStep(&MockMismatchReport{NextSteps: text})
}

// refusedValues begin a refused word's line after its where: its two
// values, each Go-quoted, and its kind's clause (learn.RefusalLine:
// `<where>: recorded "<r>", round F "<f>": refused (<kind>): <why>...`),
// matched by the line's own marks, so no quote elsewhere moves them.
var refusedValues = regexp.MustCompile(`: recorded "(?:[^"\\]|\\.)*", round F "(?:[^"\\]|\\.)*": refused \(`)

// refusalTail begins a refused word's line's machine-read tail: a JSON
// object, the line's end.
const refusalTail = " refusal="

// refusedWordsInLogForm is text with each refused word's line from its
// values on in its log form (refusalLogForm), read from the line's tail; a
// line with no tail is logged without its values, why and edits. The where
// before the values is left for the rest of MissLogNextStep's shaping.
func refusedWordsInLogForm(text string) string {
	var b strings.Builder
	for {
		loc := refusedValues.FindStringIndex(text)
		if loc == nil {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:loc[0]])
		rest := text[loc[1]:]
		// The line's own tail lies before the next refused word's line.
		upTo := len(rest)
		if next := refusedValues.FindStringIndex(rest); next != nil {
			upTo = next[0]
		}
		tail, after, ok := cutRefusalTail(rest[:upTo])
		if !ok {
			end := strings.IndexByte(rest, '\n')
			if end < 0 {
				end = len(rest)
			}
			b.WriteString(": refused; its values, why and edits withheld")
			text = rest[end:]
			continue
		}
		form := tail.logForm()
		if strings.HasPrefix(after, ".") {
			form = strings.TrimSuffix(form, ".")
		}
		b.WriteString(form)
		text = after + rest[upTo:]
	}
}

// refusalTailFields are what a refused word's log line takes from its
// line's tail: none of them a value (the tail leaves out the values and the
// why).
type refusalTailFields struct {
	Kind   string   `json:"kind"`
	Places []string `json:"places"`
	Edits  []struct {
		Kind string `json:"kind"`
	} `json:"edits"`
}

// cutRefusalTail reads the last machine-read tail in s (refusalTail, then a
// JSON object with a kind), as learn.ParseRefusal reads a line's, and
// returns it and what follows it: one the why holds is earlier, and is
// taken with the why.
func cutRefusalTail(s string) (refusalTailFields, string, bool) {
	for end := len(s); end > 0; {
		i := strings.LastIndex(s[:end], refusalTail)
		if i < 0 {
			break
		}
		at := s[i+len(refusalTail):]
		dec := json.NewDecoder(strings.NewReader(at))
		var t refusalTailFields
		if strings.HasPrefix(at, "{") && dec.Decode(&t) == nil && t.Kind != "" {
			return t, at[dec.InputOffset():], true
		}
		end = i
	}
	return refusalTailFields{}, s, false
}

// logForm is a refused word's line from its values on, as a log line has
// it: its kind, its places with every field in its shape, and its edits'
// kinds; a kind that is not a word is {}.
func (t refusalTailFields) logForm() string {
	s := ": refused (" + refusalWord(t.Kind) + "), its recorded and round F values withheld"
	for i, p := range t.Places {
		if i == 0 {
			s += ". Places: "
		} else {
			s += ", "
		}
		s += placeLogShape(p)
	}
	for i, e := range t.Edits {
		if i == 0 {
			s += ". Edits: "
		} else {
			s += "; or "
		}
		s += refusalWord(e.Kind)
	}
	return s + "."
}

// placeLogShape is a refused word's place ("<mock> <field>") as a log line
// names it: the mock by its name and the field in its shape (FieldShape),
// read from the place as it is; {} for one that is not of that form, or
// whose mock is not a name.
func placeLogShape(p string) string {
	mock, field, ok := strings.Cut(p, " ")
	if !ok || strings.ContainsAny(p, "\"`'") || !mockName.MatchString(mock) {
		return pathHole
	}
	return mock + " " + FieldShape(field)
}

// mockName is a mock's name as keploy names one (mock-3, a test's own).
var mockName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// refusalWord is a refusal's or an edit's kind as a log line names it:
// itself when it is a word of letters and '-', else {}.
func refusalWord(k string) string {
	if refusalKindWord.MatchString(k) {
		return k
	}
	return pathHole
}

var refusalKindWord = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)

// quotedInLogForm is text with every Go-quoted string in it in its shape
// (quotedLogShape); false where its quotes do not pair: a quote that opens
// no string that closes, or a string that does not stand as a word (each
// quote beside a space, a bracket or a separator), so that what is taken
// as quoted may not be, and a value may stand outside.
func quotedInLogForm(text string) (string, bool) {
	var b strings.Builder
	for i := 0; ; {
		j := strings.IndexByte(text[i:], '"')
		if j < 0 {
			b.WriteString(text[i:])
			return b.String(), true
		}
		j += i
		b.WriteString(text[:j][i:])
		q, err := strconv.QuotedPrefix(text[j:])
		if err != nil || !quoteEdge(text, j-1) || !quoteEdge(text, j+len(q)) {
			return "", false
		}
		b.WriteString(quotedLogShape(q, text[:j]))
		i = j + len(q)
	}
}

// quoteEdge reports whether text's byte at i may stand beside a quoted
// string: none (the text's start or end), a space, a bracket, a separator
// or an English sentence's mark ("x"'s, "x"!).
func quoteEdge(text string, i int) bool {
	return i < 0 || i >= len(text) || strings.IndexByte(" \t\n([{}]),.;:=!?'", text[i]) >= 0
}

// quotedLogShape is a quoted string of advice as a log line writes it, by
// the text before it: a URL path with each segment {}, a field path in its
// shape (FieldShape), an HTTP method, keploy's placeholder <regex>, a
// template placeholder ("{{capture string .id}}"), a header
// named as one (header "Content-Encoding"), or the host a sessionCalls entry
// names ({host: "api.example.com"}); anything else, a value a line may
// quote, {}.
func quotedLogShape(q, before string) string {
	u, err := strconv.Unquote(q)
	switch {
	case err != nil:
	case strings.HasPrefix(u, "/"):
		return strconv.Quote(urlPathLogShape(u))
	case fieldRoot.MatchString(u):
		return strconv.Quote(FieldShape(u))
	case httpMethods[u] || u == "<regex>" || placeholderText.MatchString(u):
		return q
	case strings.HasSuffix(before, "header ") && headerName.MatchString(u):
		return q
	case strings.HasSuffix(before, "{host: ") && hostName.MatchString(u):
		return q
	}
	return `"` + pathHole + `"`
}

// placeholderText is a template placeholder as advice quotes one, for the
// edit it names ("{{capture string .id}}"): keploy's syntax and a key.
var placeholderText = regexp.MustCompile(`^\{\{[A-Za-z0-9_ .]{1,128}\}\}$`)

// httpMethods are the methods advice may quote (a sessionCalls entry's).
var httpMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true, "CONNECT": true, "TRACE": true}

// headerName is an HTTP header's name: a token (RFC 9110's tchar).
var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")

// fieldRoot begins a compared field's path, as keploy names one: a body's,
// a header's, a URL's or a query's, or an SQL call's place.
var fieldRoot = regexp.MustCompile(`^(?:body|header|url|query|bind|literal)(?:[.\[]|$)`)

// hostName is a DNS name, with a port or none.
var hostName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(?::[0-9]{1,5})?$`)

// MissLogDiffers are the fields r says the call differs in, as a log line
// names them: the strict matcher's (Differs) and every field diff's path,
// each in its shape (FieldShape), each once.
func MissLogDiffers(r *MockMismatchReport) []string {
	if r == nil {
		return nil
	}
	var out []string
	add := func(p string) {
		if p = FieldShape(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range r.Differs {
		add(p)
	}
	for _, d := range r.FieldDiffs {
		add(d.Path)
	}
	return out
}

// httpCallLogShape is an HTTP summary ("METHOD target") as MissLogShape
// names it: its method, and its target's path with every segment {}.
func httpCallLogShape(summary string) string {
	method, target, _ := strings.Cut(strings.TrimSpace(summary), " ")
	if !isMethodWord(method) {
		return ""
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return method
	}
	if _, err := ParseRequestURL(target); err != nil {
		return method
	}
	return method + " " + urlPathLogShape(target)
}

// urlPathLogShape is the path of the URL raw with every segment {}, without
// its query; "" for a URL that does not parse.
func urlPathLogShape(raw string) string {
	u, err := ParseRequestURL(raw)
	if err != nil {
		return ""
	}
	escaped := u.EscapedPath()
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	for i, s := range segs {
		if s != "" {
			segs[i] = pathHole
		}
	}
	path := strings.Join(segs, "/")
	if strings.HasPrefix(escaped, "/") {
		path = "/" + path
	}
	return path
}

// UnmatchedCallLogShape is MissLogShape of the call m: how a log line names a
// call no mock answered. Never "", as a sentence names it: a call whose shape
// it cannot tell is "a <protocol> call", or "a call" where its protocol is
// not a name either.
func UnmatchedCallLogShape(m UnmatchedCall) string {
	if s := MissLogShape(&MockMismatchReport{Protocol: m.Protocol, ActualSummary: m.ActualSummary, Command: m.Command, Operation: m.Operation}); s != "" {
		return s
	}
	if p := m.Protocol; p != "" && KeyShape(p) == p {
		return "a " + p + " call"
	}
	return "a call"
}

// UnmatchedCallLogText is text, keploy's own words about the call m (its
// next step, its reason), as a log line carries it: MissLogNextStep's
// shaping, by m's field diffs.
func UnmatchedCallLogText(m UnmatchedCall, text string) string {
	return MissLogNextStep(&MockMismatchReport{NextSteps: text, FieldDiffs: m.FieldDiffs})
}

// FieldPathsInTextShaped is each of texts, keploy's words naming fields
// ("mock-3 body.sessions.u_8f2c.user (noise)", "variable id bound at
// body.id"), with every word that is a dotted field path in its shape
// (FieldShape): a map's key in a path can be a value. Other words (a mock's
// name, a kind) stay.
func FieldPathsInTextShaped(texts []string) []string {
	out := make([]string, len(texts))
	for i, t := range texts {
		words := strings.Split(t, " ")
		for j, w := range words {
			if strings.Contains(w, ".") && !strings.ContainsAny(w, "()") {
				words[j] = FieldShape(w)
			}
		}
		out[i] = strings.Join(words, " ")
	}
	return out
}
