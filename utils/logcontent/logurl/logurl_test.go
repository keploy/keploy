package logurl

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
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
