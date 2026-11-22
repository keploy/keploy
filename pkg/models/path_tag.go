package models

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
)

// A path tag lets a reader of keploy's log find the HTTP call a line names.
// A log line names a call's path only by its shape, every segment {} (a
// segment can hold a user's name, a reset link's token, a session id), so
// the agent's log alone cannot tell GET /events from GET /poll. Each such
// line also carries the call's path tag, and keploy's console and its report,
// which name the call whole for the user's own view, print the same tag
// beside it: the user finds the line's call there by its tag.
//
// The tag is LogKey of the call's method and its path (escaped, as the
// request line carries it), without its query and fragment: "#" and the
// first 12 hex digits of their HMAC-SHA256 under the session's key
// (LogKeySession), the form enterprise's agent names a key by. So:
//   - a tag names no value: without the key, no one can compute the tag of a
//     path they guess, so a log store's reader cannot confirm a guess at a
//     token in a path by its tag. The key is never logged, written or sent;
//   - a tag is stable for one method and path within a session, and the same
//     path has another tag in another session, so tags from two sessions say
//     nothing of each other, even of two sessions one process serves at once.
//     Two lines of one session with one tag named calls of one method and
//     path. A session is a recording or a replay, and its key rides in the
//     context.Context of the work it does (WithLogKeySession): each line
//     that names a call takes the key from the context it was logged in. The
//     agent starts one as each session it serves begins (a recording's, or a
//     replay's for one test set), and keploy's CLI as each keploy record,
//     keploy test, keploy mock record or keploy mock replay begins. A program
//     that embeds keploy and serves several sessions at once (one recording
//     per application) starts one for each (NewLogKeySession) and carries it
//     in the context of each connection and request of that session: that is
//     the embedder's job. A line logged under a context that carries no
//     session names under the process's key, drawn once at its first use and
//     kept for its life (a nil *LogKeySession): stable across every session
//     that process serves without one;
//   - the query is left out: the log names the query by its parameter
//     names, its values change from call to call of one endpoint (a page, a
//     time, a cache-buster) and can be tokens, so a tag of them would tell a
//     reader which calls sent the same token and would split one endpoint's
//     calls over many tags. The console and the report print the whole
//     target beside the tag.
//
// A tag is made once, in the session that logs the line, and travels with the
// call it names (MockMismatchReport.PathTag, UnmatchedCall.PathTag, a
// not-recorded call's notice): the agent and the CLI are two processes, with
// two keys, and a place that prints a call another session logged prints the
// tag that session made, never one of its own. So a new session's key changes
// no tag a console or a report prints: a miss the agent logged in one session
// and the CLI reads in the next carries the tag its line named.

// logKeyDigits is how many hex digits of the HMAC LogKey keeps: 12 (48
// bits), so a session's keys are told apart (two of a million distinct keys
// share a name with a chance near 1 in 500) and a name is short to search
// for.
const logKeyDigits = 12

// LogKeySession is one session's key for LogKey and PathTag: 32 bytes from
// crypto/rand, drawn as the session begins (NewLogKeySession), and carried in
// the context.Context of the work the session does (WithLogKeySession), so
// every line logged in that work names a key or a path under it, and no line
// of another session, at the same time or later, names one alike. Never
// logged, written or sent. A nil *LogKeySession is the process's key
// (processLogKey): what a line logged under a context with no session names
// under.
type LogKeySession struct{ key []byte }

// NewLogKeySession begins a session for LogKey: a key drawn now, which no
// other session's names agree with. A program that embeds keploy calls it as
// each session it serves begins (enterprise's node agent, for each
// application's recording) and carries it in that session's contexts
// (WithLogKeySession).
func NewLogKeySession() *LogKeySession {
	return &LogKeySession{key: drawLogKey()}
}

// logKeySessionKey is the context key WithLogKeySession stores a session
// under.
type logKeySessionKey struct{}

// WithLogKeySession is ctx carrying s: every line logged under it, or a
// context made from it, names keys and paths under s's key (LogKeys). ctx
// as it is for a nil s.
func WithLogKeySession(ctx context.Context, s *LogKeySession) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, logKeySessionKey{}, s)
}

// StartLogKeySession is ctx carrying a new session (NewLogKeySession).
func StartLogKeySession(ctx context.Context) context.Context {
	return WithLogKeySession(ctx, NewLogKeySession())
}

// EnsureLogKeySession is ctx carrying a session: the one it carries, or a new
// one (StartLogKeySession) when it carries none. What keploy's commands call
// as their run begins (keploy record, keploy test, keploy mock record and
// keploy mock replay): a run started under no session is a session of its
// own, and a program that embeds one and started a session for it (to name
// the run's calls in its own lines by the same tags) keeps that one.
func EnsureLogKeySession(ctx context.Context) context.Context {
	if LogKeys(ctx) != nil {
		return ctx
	}
	return StartLogKeySession(ctx)
}

// LogKeys is the session ctx carries (WithLogKeySession), or nil, the
// process's key, when it carries none.
func LogKeys(ctx context.Context) *LogKeySession {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(logKeySessionKey{}).(*LogKeySession)
	return s
}

// processLogKey is the key a line with no session in its context names
// under: drawn at the process's first use, kept for its life.
var processLogKey = sync.OnceValue(drawLogKey)

// secret is s's key, or the process's for a nil s.
func (s *LogKeySession) secret() []byte {
	if s == nil {
		return processLogKey()
	}
	return s.key
}

// drawLogKey is a new key for LogKey. crypto/rand.Read does not fail (the
// program stops if the system cannot give it randomness).
func drawLogKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("keploy: no randomness for LogKey: " + err.Error())
	}
	return k
}

// LogKey is key as a log line logged under ctx may name it: "#" and the
// first 12 hex digits of its HMAC-SHA256 under the key of the session ctx
// carries (LogKeys; the process's when it carries none). Within a session
// the same key is named the same, so two lines about it can be matched up; no
// line tells what the key is, and no one without the secret can tell whether
// a key they guess is the one named. It is enterprise's agent's LogKey (an
// application's keys, a session id among them), in the same form, so one
// name style runs through both. "" names "".
func LogKey(ctx context.Context, key string) string {
	return LogKeys(ctx).LogKey(key)
}

// LogKey is LogKey under s's key (the process's for a nil s).
func (s *LogKeySession) LogKey(key string) string {
	return logKeyUnder(s.secret(), key)
}

// logKeyUnder is LogKey of key under salt.
func logKeyUnder(salt []byte, key string) string {
	if key == "" {
		return ""
	}
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(key))
	return "#" + hex.EncodeToString(m.Sum(nil))[:logKeyDigits]
}

// PathTag is the tag of an HTTP call of method to target (a request target,
// "/a/b?q=1", or a URL, "http://host/a/b#f") in the session ctx carries (see
// above): of its method and its escaped path, its query (and a URL's
// fragment) left out. A target that does not parse is taken up to its
// query. "" for method "": a tag names a call, and the console and the
// report print a call's tag beside its method and path, so a tag of a path
// alone (a recording's path in a matcher's line, a path keploy's advice
// quotes about no call) is one no reader could find there.
func PathTag(ctx context.Context, method, target string) string {
	return LogKeys(ctx).PathTag(method, target)
}

// PathTag is PathTag under s's key (the process's for a nil s).
func (s *LogKeySession) PathTag(method, target string) string {
	return s.pathTagOf(method, escapedPathOf(target))
}

// URLPathTag is PathTag of a parsed URL: of method and u's escaped path. ""
// for a nil u, or method "".
func URLPathTag(ctx context.Context, method string, u *url.URL) string {
	return LogKeys(ctx).URLPathTag(method, u)
}

// URLPathTag is URLPathTag under s's key (the process's for a nil s).
func (s *LogKeySession) URLPathTag(method string, u *url.URL) string {
	if u == nil {
		return ""
	}
	return s.pathTagOf(method, u.EscapedPath())
}

// pathTagOf is the tag of method and the escaped path: LogKey of the two,
// apart by a space. A method has no space (RFC 9110's token), so method and
// path are read apart. "" for method "" (PathTag).
func (s *LogKeySession) pathTagOf(method, escapedPath string) string {
	if method == "" {
		return ""
	}
	return s.LogKey(method + " " + escapedPath)
}

// escapedPathOf is the escaped path of target, a request target or a URL,
// as the request line carries it: so a summary that names the path decoded
// ("/café") and a request whose URL holds it escaped ("/caf%C3%A9") have one
// tag. A target that does not parse is taken up to its query or fragment.
func escapedPathOf(target string) string {
	if u, err := ParseRequestURL(target); err == nil {
		return u.EscapedPath()
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		return target[:i]
	}
	return target
}

// CallPathTag is the path tag, in the session ctx carries, of a call of
// protocol whose summary is "METHOD target", as an HTTP miss's is: "" for a
// call of another protocol, or a summary that is not of that form.
func CallPathTag(ctx context.Context, protocol, summary string) string {
	return LogKeys(ctx).CallPathTag(protocol, summary)
}

// CallPathTag is CallPathTag under s's key (the process's for a nil s).
func (s *LogKeySession) CallPathTag(protocol, summary string) string {
	if !isHTTPProtocol(protocol) {
		return ""
	}
	method, target, _ := strings.Cut(strings.TrimSpace(summary), " ")
	target = strings.TrimSpace(target)
	if !isMethodWord(method) || target == "" {
		return ""
	}
	return s.PathTag(method, target)
}

// WithPathTag is text, a line's name for a call (its shape in a log line, its
// summary on the console and in a report), followed by the call's path tag:
// "POST /{}/{} [path tag #1f2e3d4c5b6a]". text alone when tag is "".
func WithPathTag(text, tag string) string {
	if tag == "" {
		return text
	}
	return text + " " + PathTagLabel(tag)
}

// PathTagLabel is how a line writes a path tag: "[path tag #1f2e3d4c5b6a]".
func PathTagLabel(tag string) string {
	return "[path tag " + tag + "]"
}
