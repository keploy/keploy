package realdeps

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// relayLeakToken is the planted value: net/http's parse error quotes the line
// it is in, and a chunk-size line that is not hex is the body's own bytes.
const relayLeakToken = "sk_live_9fQ2xLp7Rw"

// The mock.real relay checks every request head on a connection. One that
// net/http cannot parse is refused, and the refusal, which the proxy logs at
// Warn and charges to the test, names the head by its length, never by
// net/http's text, which quotes the header line.
func TestRelayRefusesAnUnparsedHeadByItsLength(t *testing.T) {
	r := newRig(t, checkFor(t, 8443, "auth.example.com:8443"))
	first := "GET /a HTTP/1.1\r\nHost: auth.example.com:8443\r\n\r\n"
	second := "GET /b HTTP/1.1\r\nHost: auth.example.com:8443\r\nAuthorization Bearer " + relayLeakToken + "\r\n\r\n"
	_, err := r.app.Write([]byte(first + second))
	require.NoError(t, err)
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, first, string(got))
	<-r.done
	refused := r.refusals()
	require.Len(t, refused, 1)
	require.Contains(t, refused[0], "a request head (")
	require.Contains(t, refused[0], " bytes) does not parse")
	require.NotContains(t, refused[0], relayLeakToken)
}

// A chunked body whose chunk-size line is not hex ends the relay with an error
// that names the line by its length, never its bytes, the body's own.
func TestRelayEndsOnAnUnframedChunkByItsLength(t *testing.T) {
	r := newRig(t, checkFor(t, 8443, "auth.example.com:8443"))
	head := "POST /a HTTP/1.1\r\nHost: auth.example.com:8443\r\nTransfer-Encoding: chunked\r\n\r\n"
	body := "{\"token\":\"" + relayLeakToken + "\"}\r\n0\r\n\r\n"
	_, err := r.app.Write([]byte(head + body))
	require.NoError(t, err)
	err = <-r.done
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "-byte chunk-size line that is not a hex size"), "%v", err)
	require.NotContains(t, err.Error(), relayLeakToken)
}
