package models

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	yamlLib "gopkg.in/yaml.v3"
)

// HTTPStreamOf is a stream on a stream's Content-Type, or on frames the
// recording kept for a Content-Type keploy reads frames of, and never on how
// the body's length was framed (which it is not even given).
func TestHTTPStreamOf(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	for _, c := range []struct {
		contentType string
		framed      bool
		want        HTTPStreamFormat
	}{
		{"text/event-stream", false, HTTPStreamSSE},
		{"Text/Event-Stream; charset=UTF-8", false, HTTPStreamSSE},
		// A value mime does not parse whole is no stream, as replay has
		// always read it: a parameter it cannot read, or a Content-Type
		// sent on two lines, which is recorded joined by a comma.
		{"text/event-stream;;", false, HTTPStreamNone},
		{"text/event-stream,text/event-stream", false, HTTPStreamNone},
		{"text/event-stream,text/event-stream", true, HTTPStreamNone},
		{"application/x-ndjson, application/json", false, HTTPStreamNone},
		{"application/json; profile=text/event-stream", false, HTTPStreamNone},
		{"application/x-ndjson", false, HTTPStreamNDJSON},
		{"application/ndjson; charset=utf-8", false, HTTPStreamNDJSON},
		{"multipart/x-mixed-replace; boundary=frame", false, HTTPStreamMultipart},
		{"multipart/mixed; boundary=part", false, HTTPStreamMultipart},
		{"text/plain; charset=utf-8", false, HTTPStreamNone},
		{"text/plain; charset=utf-8", true, HTTPStreamText},
		{"application/octet-stream", false, HTTPStreamNone},
		{"application/octet-stream", true, HTTPStreamBinary},
		{"application/json", false, HTTPStreamNone},
		{"application/json", true, HTTPStreamNone},
		// Without the proven switch stream+json is read whole, as it
		// always has been, frames or not.
		{"application/stream+json", false, HTTPStreamNone},
		{"application/stream+json", true, HTTPStreamNone},
		{"multipart/form-data; boundary=x", false, HTTPStreamNone},
		{"", false, HTTPStreamNone},
		{"", true, HTTPStreamNone},
	} {
		if got := HTTPStreamOf(c.contentType, c.framed); got != c.want {
			t.Errorf("HTTPStreamOf(%q, framed=%v) = %q; want %q", c.contentType, c.framed, got, c.want)
		}
	}
	// Under the switch stream+json is a stream by its Content-Type; an octet
	// stream is a stream on the frame a recording made without the switch
	// kept.
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		contentType string
		framed      bool
		want        HTTPStreamFormat
	}{
		{"application/stream+json", false, HTTPStreamNDJSON},
		{"application/stream+json; charset=utf-8", true, HTTPStreamNDJSON},
		{"application/octet-stream", false, HTTPStreamNone},
		{"application/octet-stream", true, HTTPStreamBinary},
		{"text/plain", false, HTTPStreamNone},
	} {
		if got := HTTPStreamOf(c.contentType, c.framed); got != c.want {
			t.Errorf("under the proven switch HTTPStreamOf(%q, framed=%v) = %q; want %q", c.contentType, c.framed, got, c.want)
		}
	}
}

// StreamChunksOf splits a stream's body by the format of its Content-Type,
// whatever its length framing: a text stream the recording kept as frames is
// split by lines, as its frames were, Content-Length or not.
func TestStreamChunksOfSplitsAStreamByItsFormat(t *testing.T) {
	for _, c := range []struct {
		header map[string]string
		body   string
		want   int
	}{
		{map[string]string{"Content-Type": "text/plain", "Content-Length": "12"}, "one\ntwo\nsix\n", 3},
		{map[string]string{"Content-Type": "text/plain"}, "one\ntwo\n", 2},
		{map[string]string{"Content-Type": "text/event-stream"}, "data: a\n\ndata: b\n\n", 2},
		{map[string]string{"Content-Type": "application/x-ndjson"}, "{}\n\n{}\n", 2},
		{map[string]string{"Content-Type": "application/octet-stream"}, "abcdef", 1},
		{map[string]string{"Content-Type": "multipart/x-mixed-replace; boundary=b"}, "--b\r\n\r\none\r\n--b--\r\n", 0},
		{map[string]string{"Content-Type": "application/json"}, `{"a":1}`, 0},
	} {
		if got := len(StreamChunksOf(c.header, c.body)); got != c.want {
			t.Errorf("StreamChunksOf(%v) split %q into %d frames; want %d", c.header, c.body, got, c.want)
		}
	}
}

// Under the proven switch an octet stream is kept whole, as the rule says (its
// Content-Type is no evidence of a stream); without it a recording keeps it as
// one frame, as before. application/stream+json is kept whole, byte for byte,
// either way: under the switch it is a stream by its Content-Type, and a mock
// of it is served as the dependency sent it.
func TestHTTPResp_MarshalYAML_TheProvenSwitchKeepsTheRuleWhole(t *testing.T) {
	framesOf := func(header map[string]string, body string) int {
		t.Helper()
		out, err := yamlLib.Marshal(HTTPResp{StatusCode: 200, Header: header, Body: body, Timestamp: time.Unix(1700000000, 0).UTC()})
		if err != nil {
			t.Fatal(err)
		}
		var got HTTPResp
		if err := yamlLib.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.Body != body && len(got.StreamBody) == 0 {
			t.Fatalf("a %v body kept whole did not round-trip: %q", header, got.Body)
		}
		return len(got.StreamBody)
	}
	octet := map[string]string{"Content-Type": "application/octet-stream", "Content-Length": "11"}
	flux := map[string]string{"Content-Type": "application/stream+json"}
	const fluxBody = "{\"n\":1}\r\n\r\n{\"n\":2}\r\n"

	t.Setenv(provenswitch.Env, "0")
	if n := framesOf(octet, "hello world"); n != 1 {
		t.Errorf("without the proven switch an octet stream is kept as %d frames; want 1, as before", n)
	}
	if n := framesOf(flux, fluxBody); n != 0 {
		t.Errorf("without the proven switch application/stream+json is kept as %d frames; want whole, as before", n)
	}

	t.Setenv(provenswitch.Env, "1")
	if n := framesOf(octet, "hello world"); n != 0 {
		t.Errorf("under the proven switch an octet stream is kept as %d frames; want whole", n)
	}
	if n := framesOf(flux, fluxBody); n != 0 {
		t.Errorf("under the proven switch application/stream+json is kept as %d frames; want whole, byte for byte", n)
	}
}

// A Content-Type the app sent on two lines is recorded with its lines joined
// by a comma (pkg.ToYamlHTTPHeader). Such a response is no stream, as replay
// has always read it, so a recording keeps its body whole. One a keploy older
// than the rule kept as SSE events (it framed any Content-Type naming
// text/event-stream) is still read back as the SSE it was.
func TestHTTPResp_YAML_AContentTypeSentTwice(t *testing.T) {
	header := map[string]string{"Content-Type": "text/event-stream,text/event-stream"}
	body := "data: one\n\ndata: two\n\n"
	out, err := yamlLib.Marshal(HTTPResp{StatusCode: 200, Header: header, Body: body, Timestamp: time.Unix(1700000000, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var got HTTPResp
	if err := yamlLib.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.StreamBody) != 0 || got.Body != body {
		t.Errorf("a recording kept the body as %d frames (body %q); want it whole, %q", len(got.StreamBody), got.Body, body)
	}

	for _, contentType := range []string{"text/event-stream,text/event-stream", "application/json,text/event-stream"} {
		older := HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": contentType}, Timestamp: time.Unix(1700000000, 0).UTC(),
			StreamBody: parseSSEBodyToChunks(body, time.Unix(1700000000, 0).UTC())}
		out, err := yamlLib.Marshal(older)
		if err != nil {
			t.Fatal(err)
		}
		var back HTTPResp
		if err := yamlLib.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		if want := "data:one\n\ndata:two"; back.Body != want {
			t.Errorf("an older recording's SSE events under %q were read back as %q; want the SSE they were, %q", contentType, back.Body, want)
		}
	}
}
