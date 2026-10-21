// Package logurl is a URL as a log line may carry it under utils/logcontent's
// switch: whole with the switch on, and with it off by its shape
// (models.URLShape). That shape writes a path and a query much as the lines
// a default run prints for a call keploy could not answer write them
// (models.CallShape), keeps the scheme, host and port of an absolute URL
// when they read as such, and reads a string with no '/' before its query
// only as a host, a method or "*"; models.URLShape says where it differs
// from CallShape's.
//
// A URL is content. Its query's values are the application's, and so may be
// its path's segments: a token in /reset/<token>, an email address in
// /users/<email>. Its shape writes {} for every query value, and for every
// path segment and query name that does not read as a name (models.PathShape
// says which do): a number, a UUID, a hash, an email address, a token or an
// ID with a digit in it. A segment that reads as a name is kept, so a value
// that reads as one too (a user name or a token in lower-case letters, a
// slug, a code like USD) is still logged: keploy cannot tell it from a
// route's by looking at it. An absolute URL keeps its scheme when it reads as
// one, and its host and port when they read as a host (a name that is a
// customer's, as a tenant's subdomain can be, is kept with them), and drops
// its credentials and its fragment. An '@' after its host, which can end a
// password that holds a '/', a '?' or a '#', drops its host and whatever
// precedes that '@' too, path segments, query names and values alike, so a
// URL that names an email address after its host loses its host as well. An
// '@' in a path after a "//" or a ':', as a URL the path holds (a proxy's
// target, a redirect's ?next=) or a user:password has, drops whatever
// precedes it the same way (models.URLShape). A target that names its host
// first ("pay.internal:8443/cards/42") is shaped as the URL with a scheme is,
// as a call's destination is.
//
// It is a package of its own because the shape is pkg/models', and
// logcontent depends on nothing of keploy's.
package logurl

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// Field is zap.String(key, u) with the switch on, and u's shape with it off.
// u is a URL ("http://pay.internal/cards/4111111111111111?cvv=123"), a
// request target or a path ("/users/alice@example.com/orders"), a call's
// summary ("GET /users/42"), or a gRPC method ("/pkg.v1.Users/Get"). A
// target with no '/' before its query is read only as a host and port
// ("CONNECT pay.internal:443") or "*", and is {} otherwise, whatever its look
// ("hunterpassword", "user:hunter"), after the method before it, if any:
// "GET {}". A method is kept when it is one of HTTP's or gRPC, and is {}
// otherwise ("ABCD-EFGH"). A u the shape has no reading of is {}, and an
// empty u is "". A value that is no URL but reads as a host and port
// ("alice:4242", "user.secret") is kept as one: Field is for URLs, and a
// value goes to logcontent's helpers.
//
// The shape is computed when a core encodes the entry, not here, so a Debug
// line on a hot path whose level is off shapes nothing. Its off field costs a
// disabled line one small allocation, for the string it holds, that
// zap.String does not.
func Field(key, u string) zap.Field {
	if logcontent.On() {
		return zap.String(key, u)
	}
	return zap.Stringer(key, shaped(u))
}

// URL is u as a log line's message may carry it: u with the switch on, and
// its shape with it off, as Field logs it. Unlike Field's, the shape is
// computed when it is called, whether or not the line is written. It is for
// a log line alone: an error, a report or a test's result, and anything
// keploy compares, stores or sends, carries u whole, so that nothing keploy
// does but what it logs depends on the switch.
func URL(u string) string {
	if logcontent.On() {
		return u
	}
	return shape(u)
}

// Error is err as a log line may carry it, for an error whose text names a
// URL: err itself with the switch on. With it off, a *url.Error in it (what
// client.Do and url.Parse return, however wrapped) is written as its op, its
// URL's shape and its cause's kind (logcontent.ErrorKind), `Get
// "http://app:8080/reset/{}": syscall.Errno(connection refused)`, since its
// cause can quote what the server answered (net/http's "malformed HTTP
// response") or a piece of the URL (url.EscapeError); and each of urls,
// which its text names in its own words, is written by its shape. The
// result unwraps to err, so errors.Is and errors.As find what they find in
// err, and it is err itself when its text names none of them.
//
// An error in err's chain that names URLs of its own (URLNamer: a stalled
// answer names its request's target) has them written by their shape too, as
// urls are.
//
// It is for a line only. An error a caller returns, or a report keeps, is
// left whole: the switch decides what a log line carries, not what keploy
// reports.
func Error(err error, urls ...string) error {
	if err == nil || logcontent.On() {
		return err
	}
	var named URLNamer
	if errors.As(err, &named) {
		urls = append(urls[:len(urls):len(urls)], named.LoggedURLs()...)
	}
	text := err.Error()
	logged := text
	var uerr *url.Error
	if errors.As(err, &uerr) {
		logged = strings.ReplaceAll(logged, uerr.Error(),
			fmt.Sprintf("%s %q: %s", uerr.Op, shape(uerr.URL), logcontent.ErrorKind(uerr.Err)))
	}
	for _, u := range urls {
		if u == "" {
			continue
		}
		l := shape(u)
		// Quoted first, as fmt's %q writes it, then as it is.
		logged = strings.ReplaceAll(logged, strconv.Quote(u), strconv.Quote(l))
		logged = strings.ReplaceAll(logged, u, l)
	}
	if logged == text {
		return err
	}
	return loggedError{text: logged, err: err}
}

// URLNamer is an error whose text names URLs of its own, in its own words:
// Error writes each by its shape, as it does the urls it is given.
type URLNamer interface {
	error
	LoggedURLs() []string
}

// loggedError is an error as a line may carry it (Error): its own text, and
// the error it stands for underneath.
type loggedError struct {
	text string
	err  error
}

func (e loggedError) Error() string { return e.text }

func (e loggedError) Unwrap() error { return e.err }

// shaped is a URL whose shape a core writes when it encodes the entry.
type shaped string

func (s shaped) String() string { return shape(string(s)) }

// shape is u's shape (models.URLShape), and {} for a u it has none for. A
// variable so a test can count the shapes a line costs.
var shape = func(u string) string {
	if s := models.URLShape(u); s != "" || u == "" {
		return s
	}
	return "{}"
}
