package logurl

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The values each URL below holds that no line may carry with the switch off.
var values = []string{"kq7Zt0x9resettoken", "alice.smith@example.com", "admin", "hunter", "4111111111111111", "123", "frag", "12345", "items", "opaque", "password", "SECRETKEY", "2024", "secret"}

// urls are what call sites log: a path, a URL, a call's summary, a target
// that names its host, a gRPC method, and what is none of those.
var urls = []struct{ u, shape string }{
	{"/reset/kq7Zt0x9resettoken", "/reset/{}"},
	{"/users/alice.smith@example.com/orders", "/users/{}/orders"},
	{"http://admin:hunter2@pay.internal:8443/cards/4111111111111111?cvv=123#frag", "http://pay.internal:8443/cards/{}?cvv={}"},
	{"GET https://api.example.com/reset/kq7Zt0x9resettoken", "GET https://api.example.com/reset/{}"},
	{"GET /orders/12345?expand=items", "GET /orders/{}?expand={}"},
	{"pay.internal:8443/cards/4111111111111111", "pay.internal:8443/cards/{}"},
	// A password with a '/' in it ends the authority before its '@'.
	{"http://admin:2024/hunter/b@pay.internal/cards", "http://{}/{}/{}/cards"},
	// An '@' after the host may end credentials, so it takes the host, and
	// whatever precedes it, with it.
	{"https://api.example.com/users?email=alice.smith@example.com", "https://{}/{}?{}={}"},
	// So does an '@' after a host a target names first, or after "//".
	{"admin:hunter/pw@db/orders", "{}/{}/orders"},
	{"GET admin:hunter/pw@db/orders", "GET {}/{}/orders"},
	{"root:secret/x@tcp(db:3306)/app", "{}/{}/app"},
	{"//admin:hunter/pw@db/x", "//{}/{}/x"},
	// A word in capitals before a target is no method.
	{"SECRETKEY /reset/kq7Zt0x9resettoken", "{} /reset/{}"},
	{"/pkg.v1.Users/GetUser", "/pkg.v1.Users/GetUser"},
	// A value that reads as a name is kept: the shape cannot tell it from a
	// route's segment.
	{"/users/alice", "/users/alice"},
	{"not a request 4111/1111", "{}"},
	// A string with no '/' in it is read only as a host, or a method and a
	// host: whatever else its look, none of it is kept.
	{"hunterpassword", "{}"},
	{"user:hunter", "{}"},
	{"opaque_abcdefghijklmnopqrstuvwx", "{}"},
	{"GET hunterpassword", "GET {}"},
	{"CONNECT pay.internal:443", "CONNECT pay.internal:443"},
	{"", ""},
}

// observed logs through a Debug-level observer core and returns each entry as
// a console encoder writes it.
func observed(t *testing.T, logLines func(*zap.Logger)) []string {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logLines(zap.New(core))
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.TimeKey = ""
	enc := zapcore.NewConsoleEncoder(cfg)
	var out []string
	for _, e := range logs.All() {
		buf, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			t.Fatalf("encode %q: %v", e.Message, err)
		}
		out = append(out, fmt.Sprintf("%v %s", e.ContextMap(), buf.String()))
		buf.Free()
	}
	return out
}

func TestALineNamesAURLByItsShapeWithTheSwitchOff(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	lines := observed(t, func(l *zap.Logger) {
		for _, c := range urls {
			l.Debug("call", Field("url", c.u))
		}
	})
	if len(lines) != len(urls) {
		t.Fatalf("logged %d lines, want %d", len(lines), len(urls))
	}
	for i, c := range urls {
		if want := fmt.Sprintf(`{"url": %q}`, c.shape); !strings.Contains(lines[i], want) {
			t.Errorf("%q is not logged as %s: %s", c.u, want, lines[i])
		}
		if got := URL(c.u); got != c.shape {
			t.Errorf("URL(%q) = %q, want %q", c.u, got, c.shape)
		}
		if c.shape != "{}" && c.u != "" {
			if got := models.URLShape(c.u); got != c.shape {
				t.Errorf("%q is logged as %q, but its shape is %q", c.u, c.shape, got)
			}
		}
		for _, v := range values {
			if strings.Contains(c.shape, v) {
				t.Fatalf("the test's own shape %q holds %q", c.shape, v)
			}
			if strings.Contains(lines[i], v) {
				t.Errorf("the line for %q carries %q: %s", c.u, v, lines[i])
			}
		}
	}
}

func TestALineCarriesTheURLWholeWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	for _, c := range urls {
		got := observed(t, func(l *zap.Logger) { l.Debug("call", Field("url", c.u)) })
		want := observed(t, func(l *zap.Logger) { l.Debug("call", zap.String("url", c.u)) })
		if len(got) != 1 || len(want) != 1 || got[0] != want[0] {
			t.Errorf("Field(%q) logs %q, zap.String %q", c.u, got, want)
		}
		if URL(c.u) != c.u {
			t.Errorf("URL(%q) = %q, want it whole", c.u, URL(c.u))
		}
	}
}

// A Debug line on a hot path whose level is off shapes nothing: the shape is
// computed when a core encodes the entry.
func TestADisabledLineShapesNothing(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	var n atomic.Int64
	orig := shape
	shape = func(u string) string {
		n.Add(1)
		return orig(u)
	}
	t.Cleanup(func() { shape = orig })

	var out bytes.Buffer
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&out), zapcore.InfoLevel))
	for range 100 {
		logger.Debug("call", Field("url", urls[0].u))
	}
	if got := n.Load(); got != 0 || out.Len() != 0 {
		t.Fatalf("100 disabled lines shaped %d times and wrote %q, want nothing", got, out.String())
	}
	logger.Info("call", Field("url", urls[0].u))
	if got := n.Load(); got != 1 {
		t.Fatalf("an encoded line shaped %d times, want 1", got)
	}
	if !strings.Contains(out.String(), urls[0].shape) {
		t.Fatalf("the written line does not name the URL by its shape: %s", out.String())
	}
}

// namer is an error that names a URL of its own (URLNamer), as a stalled
// answer names its request's.
type namer struct{ target string }

func (n namer) Error() string        { return "stalled for " + n.target }
func (n namer) LoggedURLs() []string { return []string{n.target} }

// An error that names a URL, client.Do's and url.Parse's however wrapped, one
// that names it in its own words, and one that names URLs of its own
// (URLNamer), is logged naming it by its shape and a *url.Error's cause by
// its kind, and still reads as the error it stands for; with the switch on,
// or naming no URL, it is the error itself.
func TestErrorNamesTheURLByItsShape(t *testing.T) {
	const token = "kq7Zt0x9resettoken"
	refused := &url.Error{Op: "Get", URL: "http://app:8080/reset/" + token + "?code=123456",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	// net/http quotes what the server answered in a transport's error.
	answered := &url.Error{Op: "Get", URL: "http://app:8080/orders",
		Err: fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", fmt.Errorf("malformed HTTP response %q", token))}
	_, unparsed := url.Parse("http://app:8080/reset/" + token + "\x7f")
	_, escaped := url.Parse("http://app:8080/reset/%z" + token)
	own := "/reset/" + token + "?next=/x"
	refusedKind := "syscall.Errno(" + syscall.ECONNREFUSED.Error() + ")"
	for name, c := range map[string]struct {
		err  error
		urls []string
		want string
	}{
		"client.Do's":                 {refused, nil, `Get "http://app:8080/reset/{}?code={}": ` + refusedKind},
		"client.Do's, wrapped":        {fmt.Errorf("the app's port cannot be reached: %w", refused), nil, `the app's port cannot be reached: Get "http://app:8080/reset/{}?code={}": ` + refusedKind},
		"client.Do's, with an answer": {answered, nil, `Get "http://app:8080/orders": *fmt.wrapError`},
		"url.Parse's, which escapes":  {unparsed, nil, `parse "http://app:8080/reset/{}": *errors.errorString`},
		"url.Parse's, which quotes":   {escaped, nil, `parse "http://app:8080/reset/{}": url.EscapeError`},
		"one in its own words":        {fmt.Errorf("the test's URL %q has no host", own), []string{own}, `the test's URL "/reset/{}?next={}" has no host`},
		"one an error names":          {fmt.Errorf("not judged: %w", namer{own}), nil, `not judged: stalled for /reset/{}?next={}`},
	} {
		t.Run(name, func(t *testing.T) {
			func() {
				defer logcontent.Set(false)()
				got := Error(c.err, c.urls...)
				if got.Error() != c.want {
					t.Fatalf("Error = %q, want %q", got, c.want)
				}
				if strings.Contains(got.Error(), token) {
					t.Fatalf("Error carries the token: %q", got)
				}
				if !errors.Is(got, c.err) {
					t.Fatalf("Error(%v) does not unwrap to it", c.err)
				}
				var uerr *url.Error
				if errors.As(c.err, &uerr) && (!errors.As(got, &uerr) || !strings.Contains(uerr.URL+fmt.Sprint(uerr.Err), token)) {
					t.Fatalf("Error(%v) hides the *url.Error it holds, or changed it", c.err)
				}
			}()
			defer logcontent.Set(true)()
			if got := Error(c.err, c.urls...); got != c.err {
				t.Fatalf("with the switch on, Error = %v, want the error itself", got)
			}
		})
	}
	defer logcontent.Set(false)()
	if plain := errors.New("connection refused"); Error(plain) != plain {
		t.Fatal("an error that names no URL is not logged as itself")
	}
	if got := Error(nil); got != nil {
		t.Fatalf("Error(nil) = %v", got)
	}
}

// A request's method and a place in it are logged by their shapes with the
// switch off, a token or a key a client made up written {}, and whole with it
// on, as zap's constructors log them.
func TestAMethodAndAPlaceAreLoggedByTheirShapes(t *testing.T) {
	const method, place = "SECRETKEY", "body.users.alice@example.com.sessions.kq7Zt0x9resettoken"
	places := []string{place, "url.query.4111111111111111"}
	logLines := func(l *zap.Logger) {
		l.Debug("call", MethodField("method", method), PlaceField("where", place), PlacesField("paths", places))
		l.Debug("call " + Method(method) + " at " + Place(place))
	}
	t.Cleanup(logcontent.Set(false))
	off := observed(t, logLines)
	for _, want := range []string{
		`"method": "{}"`, `"where": "body.users.{}.com.sessions.{}"`, `"paths": ["body.users.{}.com.sessions.{}", "url.query.{}"]`,
		"call {} at body.users.{}.com.sessions.{}",
	} {
		if !strings.Contains(strings.Join(off, "\n"), want) {
			t.Errorf("no line carries %s: %v", want, off)
		}
	}
	for _, v := range []string{"SECRETKEY", "alice", "kq7Zt0x9resettoken", "4111111111111111"} {
		if strings.Contains(strings.Join(off, "\n"), v) {
			t.Errorf("a line carries %q with the switch off: %v", v, off)
		}
	}
	if got := observed(t, func(l *zap.Logger) { l.Debug("call", MethodField("method", "PROPFIND")) }); !strings.Contains(got[0], `"method": "PROPFIND"`) {
		t.Errorf("HTTP's method is not kept: %v", got)
	}

	logcontent.Set(true)
	on := observed(t, logLines)
	want := observed(t, func(l *zap.Logger) {
		l.Debug("call", zap.String("method", method), zap.String("where", place), zap.Strings("paths", places))
		l.Debug("call " + method + " at " + place)
	})
	if strings.Join(on, "\n") != strings.Join(want, "\n") {
		t.Errorf("with the switch on:\n got %v\nwant %v", on, want)
	}
}
