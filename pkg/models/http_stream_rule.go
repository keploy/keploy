package models

import (
	"mime"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// HTTPStreamFormat is how a streamed HTTP response frames its body: by SSE
// events, by lines (NDJSON, text), by multipart parts, or whole (binary).
// HTTPStreamNone is a response that is no stream, read and compared whole.
type HTTPStreamFormat string

const (
	HTTPStreamNone      HTTPStreamFormat = ""
	HTTPStreamSSE       HTTPStreamFormat = "sse"
	HTTPStreamNDJSON    HTTPStreamFormat = "ndjson"
	HTTPStreamMultipart HTTPStreamFormat = "multipart"
	HTTPStreamText      HTTPStreamFormat = "plain-text"
	HTTPStreamBinary    HTTPStreamFormat = "binary"
)

// HTTPStreamOf is keploy's one rule for whether an HTTP response is a stream,
// and in which format. Record and replay both read it: the recording keeps a
// stream's body as its frames (recordedFramesOf), and the replay runs a
// stream's test case as one (pkg.DetectHTTPStreamConfig: after the set's
// other test cases, with a stream's mock window, its answer read frame by
// frame).
//
// A response is a stream only on evidence of one:
//
//   - its Content-Type is a stream's (streamOf): text/event-stream
//     (SSE), application/x-ndjson or application/ndjson (and, under the proven
//     switch, application/stream+json, NDJSON by its older name: read whole
//     without it, as it always has been), multipart/x-mixed-replace or
//     multipart/mixed (whose parts pkg.DetectHTTPStreamConfig reads by the
//     boundary of the answer's or the recorded Content-Type: one with none in
//     either is read whole);
//   - or the recording kept its body as frames (framed: HTTPResp.StreamBody),
//     for a Content-Type keploy reads frames of: text/plain (by lines; only a
//     keploy older than this rule kept one so) and application/octet-stream
//     (whole).
//
// How the transport framed the body's length is no evidence:
// Transfer-Encoding: chunked, or no Content-Length, is how any body of a size
// not known up front is sent. Go's JSON written with no Content-Type goes out
// as text/plain, chunked once it is over 2 KB, and is one document.
func HTTPStreamOf(contentType string, framed bool) HTTPStreamFormat {
	return streamOf(mediaTypeOf(contentType), framed)
}

// streamOf is HTTPStreamOf of a parsed media type. A format that frames a
// body by its own syntax (SSE events, NDJSON lines, multipart parts) is a
// stream on its Content-Type; text and binary only on a recording's frames.
func streamOf(mediaType string, framed bool) HTTPStreamFormat {
	switch f := streamFormatOfMediaType(mediaType); f {
	case HTTPStreamSSE, HTTPStreamNDJSON, HTTPStreamMultipart:
		return f
	case HTTPStreamText, HTTPStreamBinary:
		if framed {
			return f
		}
	}
	return HTTPStreamNone
}

// httpStreamFormatOf is the format a body of this Content-Type has when it is
// a stream: the frames it splits into. It says nothing of whether it is one
// (HTTPStreamOf).
func httpStreamFormatOf(contentType string) HTTPStreamFormat {
	return streamFormatOfMediaType(mediaTypeOf(contentType))
}

// streamFormatOfMediaType is httpStreamFormatOf of a parsed media type.
func streamFormatOfMediaType(mediaType string) HTTPStreamFormat {
	switch mediaType {
	case "text/event-stream":
		return HTTPStreamSSE
	case "application/x-ndjson", "application/ndjson":
		return HTTPStreamNDJSON
	case "application/stream+json":
		// NDJSON by its older name (Spring WebFlux): a stream under the
		// proven switch, and read whole without it, as it always has been.
		if provenswitch.On() {
			return HTTPStreamNDJSON
		}
	case "multipart/x-mixed-replace", "multipart/mixed":
		return HTTPStreamMultipart
	case "text/plain":
		return HTTPStreamText
	case "application/octet-stream":
		return HTTPStreamBinary
	}
	return HTTPStreamNone
}

// mediaTypeOf is a Content-Type's media type, in lower case, without its
// parameters, and "" for a value mime does not parse whole: one with a
// parameter it cannot read, or one the app sent on more than one line (which
// is recorded with its lines joined by a comma, pkg.ToYamlHTTPHeader). Such a
// response is no stream, as replay has always read it.
func mediaTypeOf(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return mediaType
}

// splitStreamBody splits a stream's body into the frames of its format: SSE
// events, NDJSON lines (blank ones are keepalives, not frames), text lines,
// or a binary body whole. nil for a format that does not split (multipart, or
// no stream), and for a body with no frame in it.
func splitStreamBody(format HTTPStreamFormat, body string, ts time.Time) []HTTPStreamChunk {
	switch format {
	case HTTPStreamSSE:
		return parseSSEBodyToChunks(body, ts)
	case HTTPStreamNDJSON:
		return parseRawBodyToChunks(body, ts, true)
	case HTTPStreamText:
		return parseRawBodyToChunks(body, ts, false)
	case HTTPStreamBinary:
		if strings.TrimSpace(body) == "" {
			return nil
		}
		return []HTTPStreamChunk{{TS: ts, Data: []HTTPStreamDataField{{Key: "raw", Value: body}}}}
	}
	return nil
}

// recordedFramesOf is the format in which a recording keeps the body of a
// response of this Content-Type as frames (HTTPResp.MarshalYAML), or
// HTTPStreamNone for a body it keeps whole: an SSE or an NDJSON stream as its
// events or lines (a multipart one is kept whole, and read by its boundary).
// A text/plain body is kept whole: its Content-Type is no evidence of a
// stream. So is an application/stream+json body, byte for byte, with or
// without the proven switch: under the switch it is a stream by its
// Content-Type, compared by its body split into lines (pkg's expected stream
// frames), so a recording made with either switch reads the same under both.
//
// With the proven switch off an application/octet-stream body is kept as one
// frame, as it always has been, which makes it a stream at replay
// (HTTPStreamOf, framed): the one exception to the rule, which goes at the
// flip. Under the switch a recording keeps it whole, as the rule says. One
// recorded as a frame before is still a stream, on that frame.
func recordedFramesOf(contentType string) HTTPStreamFormat {
	mediaType := mediaTypeOf(contentType)
	switch f := streamFormatOfMediaType(mediaType); {
	case mediaType == "application/stream+json":
		return HTTPStreamNone
	case f == HTTPStreamSSE, f == HTTPStreamNDJSON:
		return f
	case f == HTTPStreamBinary && !provenswitch.On():
		return f
	}
	return HTTPStreamNone
}
