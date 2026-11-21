package logleak

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Every way keploy's code writes bytes into a log line is found, whole or a
// Window-long part of it, wherever the secret sits in a longer buffer; and a
// failing line names where it was written, never the secret.
func TestFindSeesTheSecretInEveryFormItIsWrittenIn(t *testing.T) {
	for _, secret := range []string{Canary, `kp/Log+Can ary"7Qx&3vZ=9mW?2`} {
		for _, pad := range []string{"", "a", "ab", "abc"} {
			buf := []byte("prefix:" + pad + secret + ":suffix")
			for _, tc := range []struct {
				name string
				log  func(*zap.Logger)
			}{
				{"zap.String", func(l *zap.Logger) { l.Debug("m", zap.String("buf", string(buf))) }},
				{"zap.ByteString", func(l *zap.Logger) { l.Debug("m", zap.ByteString("buf", buf)) }},
				{"zap.Binary", func(l *zap.Logger) { l.Debug("m", zap.Binary("buf", buf)) }},
				{"zap.Any of []byte", func(l *zap.Logger) { l.Debug("m", zap.Any("buf", buf)) }},
				{"zap.Any of a struct with []byte", func(l *zap.Logger) { l.Debug("m", zap.Any("v", struct{ B []byte }{buf})) }},
				{"in the message", func(l *zap.Logger) { l.Debug("got " + string(buf)) }},
				{"%x", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%x", buf)) }},
				{"%X", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%X", buf)) }},
				{"% x", func(l *zap.Logger) { l.Debug(fmt.Sprintf("% x", buf)) }},
				{"%q", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%q", buf)) }},
				{"%v of []byte", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%v", buf)) }},
				{"hex.EncodeToString", func(l *zap.Logger) { l.Debug("m", zap.String("hex", hex.EncodeToString(buf))) }},
				{"hex.Dump", func(l *zap.Logger) { l.Debug("m", zap.String("dump", hex.Dump(buf))) }},
				{"base64 std", func(l *zap.Logger) { l.Debug("m", zap.String("b64", base64.StdEncoding.EncodeToString(buf))) }},
				{"base64 url", func(l *zap.Logger) { l.Debug("m", zap.String("b64", base64.URLEncoding.EncodeToString(buf))) }},
				{"url.QueryEscape", func(l *zap.Logger) { l.Debug("m", zap.String("q", url.QueryEscape(string(buf)))) }},
				{"url.PathEscape", func(l *zap.Logger) { l.Debug("m", zap.String("p", url.PathEscape(string(buf)))) }},
				{"json.Marshal", func(l *zap.Logger) {
					b, _ := json.Marshal(map[string]string{"v": string(buf)})
					l.Debug("m", zap.ByteString("json", b))
				}},
				{"zap.Error", func(l *zap.Logger) { l.Debug("m", zap.Error(errors.New("bad input "+string(buf)))) }},
				{"the first 16 bytes, hex", func(l *zap.Logger) {
					l.Debug("m", zap.String("first", fmt.Sprintf("%x", buf[len("prefix:")+len(pad)+2:][:16])))
				}},
				{"a 10-byte preview", func(l *zap.Logger) { l.Debug("m", zap.ByteString("preview", buf[len("prefix:")+len(pad):][:10])) }},
				{"a With field", func(l *zap.Logger) { l.With(zap.String("ctx", string(buf))).Debug("m") }},
				{"upper case", func(l *zap.Logger) { l.Debug("m", zap.String("v", strings.ToUpper(string(buf)))) }},
				{"spaced out", func(l *zap.Logger) { l.Debug("m", zap.String("v", strings.Join(strings.Split(string(buf), ""), " "))) }},
				{"hex with colons", func(l *zap.Logger) {
					h := hex.EncodeToString(buf)
					var parts []string
					for i := 0; i < len(h); i += 2 {
						parts = append(parts, h[i:i+2])
					}
					l.Debug("m", zap.String("v", strings.Join(parts, ":")))
				}},
				{"base64 wrapped over lines", func(l *zap.Logger) {
					b := base64.StdEncoding.EncodeToString(buf)
					var lines []string
					for i := 0; i < len(b); i += 7 {
						lines = append(lines, b[i:min(i+7, len(b))])
					}
					l.Debug("m", zap.String("v", strings.Join(lines, "\r\n")))
				}},
				{"hex of a %q", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%x", fmt.Sprintf("%q", buf))) }},
				{"JSON of a []int", func(l *zap.Logger) {
					ints := make([]int, len(buf))
					for i, c := range buf {
						ints[i] = int(c)
					}
					l.Debug("m", zap.Any("v", ints))
				}},
				{"%#v of a []byte", func(l *zap.Logger) { l.Debug(fmt.Sprintf("%#v", buf)) }},
				{"percent-encoded bytes", func(l *zap.Logger) {
					var b strings.Builder
					for _, c := range buf {
						fmt.Fprintf(&b, "%%%02X", c)
					}
					l.Debug("m", zap.String("v", b.String()))
				}},
				{"UTF-16LE text", func(l *zap.Logger) {
					u := make([]byte, 0, 2*len(buf))
					for _, c := range buf {
						u = append(u, c, 0)
					}
					l.Debug("m", zap.ByteString("v", u))
				}},
				{"a gzip'd body, base64", func(l *zap.Logger) {
					var z bytes.Buffer
					w := gzip.NewWriter(&z)
					_, _ = w.Write(buf)
					_ = w.Close()
					l.Debug("m", zap.Binary("body", z.Bytes()))
				}},
				{"base64 of base64", func(l *zap.Logger) {
					inner := base64.StdEncoding.EncodeToString(buf)
					l.Debug("m", zap.String("v", base64.StdEncoding.EncodeToString([]byte("x:"+inner))))
				}},
				{"zap.Any of a [N]byte", func(l *zap.Logger) {
					var a [64]byte
					copy(a[:], buf)
					l.Debug("m", zap.Any("v", a))
				}},
			} {
				t.Run(fmt.Sprintf("%s/pad%d/%s", strings.ReplaceAll(secret[:5], "/", "_"), len(pad), tc.name), func(t *testing.T) {
					logger, logs := Observe()
					tc.log(logger)
					found := Find(logs.All(), secret)
					if len(found) != 1 {
						t.Fatalf("Find = %d entries, want 1", len(found))
					}
					for _, f := range WindowForms(secret) {
						if strings.Contains(found[0], f) {
							t.Fatalf("Find's line holds part of the secret: it must say where, not what")
						}
					}
				})
			}
		}
	}
}

// Facts about the traffic are not the traffic: sizes, names, counts and
// shapes pass, and so do fewer than Window bytes of the secret.
func TestFindPassesFactsAboutTheTraffic(t *testing.T) {
	logger, logs := Observe()
	logger.Debug("read a request", zap.Int("bytes", 4096), zap.String("method", "POST"),
		zap.String("path", "/{}/{}"), zap.String("query keys", "access_token"), zap.Strings("headers", []string{"Authorization", "Cookie"}))
	logger.Info("sent a short part", zap.String("part", Canary[:Window-1]))
	if found := Find(logs.All(), Canary); len(found) != 0 {
		t.Fatalf("Find = %v, want none", found)
	}
}

// Holds sees the secret in a text the caller logs: an error, a report.
func TestHoldsSeesTheSecretInAnyForm(t *testing.T) {
	for _, text := range []string{
		"miss: GET /v1/me?token=" + Canary,
		"payload " + base64.StdEncoding.EncodeToString([]byte("x"+Canary)),
		strings.ToUpper(Canary[3:12]),
	} {
		if !Holds(text, Canary) {
			t.Errorf("Holds missed the secret in a text of %d bytes", len(text))
		}
	}
	if Holds("miss: GET /{}/{}?{token}", Canary) {
		t.Error("Holds found the secret in a text without it")
	}
}
