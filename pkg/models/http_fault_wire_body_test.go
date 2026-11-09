package models

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlLib "gopkg.in/yaml.v3"
)

// schemaThroughYAML is s written as a mock's spec is written and read back.
func schemaThroughYAML(t *testing.T, s HTTPSchema) (HTTPSchema, string) {
	t.Helper()
	out, err := yamlLib.Marshal(s)
	require.NoError(t, err)
	var got HTTPSchema
	require.NoError(t, yamlLib.Unmarshal(out, &got), "%s", out)
	return got, string(out)
}

// sseSchema is the spec of a call to an SSE stream whose body came as wire,
// as the recorder keeps it: the bytes the upstream sent.
func sseSchema(wire string, fault *Fault) HTTPSchema {
	return HTTPSchema{
		Request: HTTPReq{Method: "GET", URL: "http://llm.example/stream", ProtoMajor: 1, ProtoMinor: 1},
		Response: HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "text/event-stream"},
			Body:       wire,
			Timestamp:  time.Unix(1700000000, 0).UTC(),
		},
		Fault: fault,
	}
}

// A fault that sends the head has replay send BodyBytes of the body as the
// upstream sent them, so a recording keeps that body as those bytes, never as
// a stream's frames: the frames of "data: a\n\ndata: b\n\n" read back as
// "data:a\n\ndata:b\n\n", 2 bytes short of the 18 replay sends, and a stream
// cut inside its second event, "data: a\n\ndata: b", would read back with
// both events ended, one more than the client dispatched at record. A fault
// that sends no head, and a delay (the whole answer, served as any answer
// is), keep a stream that ended its last event as frames, as a call with no
// fault does.
func TestAFaultsBodyIsKeptAsTheBytesTheUpstreamSent(t *testing.T) {
	const ended = "data: a\n\ndata: b\n\n"
	const cut = "data: a\n\ndata: b"
	for _, c := range []struct {
		name   string
		wire   string
		fault  *Fault
		asWire bool
		events []string // dispatched before the fault, at record
	}{
		{"a reset after the head", ended, &Fault{Kind: FaultReset, AfterHead: true, BodyBytes: int64(len(ended))}, true, []string{"a", "b"}},
		{"a truncate", ended, &Fault{Kind: FaultTruncate, BodyBytes: int64(len(ended))}, true, []string{"a", "b"}},
		{"a timeout after the head", ended, &Fault{Kind: FaultTimeout, AfterHead: true, BodyBytes: int64(len(ended))}, true, []string{"a", "b"}},
		{"a reset inside the second event", cut, &Fault{Kind: FaultReset, AfterHead: true, BodyBytes: int64(len(cut))}, true, []string{"a"}},
		{"a reset before the head", ended, &Fault{Kind: FaultReset}, false, nil},
		{"a delay", ended, &Fault{Kind: FaultDelay, Delay: time.Second}, false, nil},
		{"no fault", ended, nil, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := sseSchema(c.wire, c.fault)
			got, out := schemaThroughYAML(t, s)
			if c.asWire {
				assert.Equal(t, c.wire, got.Response.Body, "the bytes replay sends before the fault:\n%s", out)
				assert.Empty(t, got.Response.StreamBody, "a fault's body was kept as frames:\n%s", out)
				assert.Equal(t, c.events, sseEventsDispatched(got.Response.Body[:c.fault.BodyBytes]),
					"the events the client dispatches before the fault")
			} else {
				assert.NotEmpty(t, got.Response.StreamBody, "a stream's body was not kept as its frames:\n%s", out)
				assert.Equal(t, "data:a\n\ndata:b\n\n", got.Response.Body)
			}
		})
	}
}

// A fault's body read back from frames (StreamBody: an earlier keploy wrote
// it so) stays frames when it is written again, never rewritten as the bytes
// the frames read back as: those are not the bytes the upstream sent, and
// replay refuses the fault of a body kept as frames, which a rewrite to bytes
// would hide.
func TestAFaultsBodyKeptAsFramesStaysFrames(t *testing.T) {
	framed, _ := schemaThroughYAML(t, sseSchema("data: a\n\ndata: b\n\n", nil))
	require.NotEmpty(t, framed.Response.StreamBody)
	framed.Fault = &Fault{Kind: FaultReset, AfterHead: true, BodyBytes: 18}
	got, out := schemaThroughYAML(t, framed)
	assert.Equal(t, framesData(framed.Response.StreamBody), framesData(got.Response.StreamBody),
		"a fault's body kept as frames was not written as them:\n%s", out)
	require.NotNil(t, got.Fault)
	assert.Equal(t, int64(18), got.Fault.BodyBytes)
}
