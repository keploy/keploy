package util

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/logcontent"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in a request's header, as an Authorization value would be.
const canary = "kp-canary-proxy-util-2d9b"

// A header section read whole, or in parts, is logged by its size, not its
// lines, unless KEPLOY_LOG_CONTENT is set.
func TestReadHTTPHeadersUntilEnd_TheHeadersAreLoggedWithoutTheirValues(t *testing.T) {
	head := "GET /profile HTTP/1.1\r\nHost: api.test\r\nAuthorization: Bearer " + canary + "\r\n\r\n"
	for name, tc := range map[string]struct {
		parts []string
		want  string
	}{
		"in one read":  {[]string{head}, "received complete HTTP headers in initial buffer"},
		"in two reads": {[]string{head[:40], head[40:]}, "received complete HTTP headers"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				client, server := net.Pipe()
				t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
				go func() {
					for _, p := range tc.parts {
						_, _ = client.Write([]byte(p))
						time.Sleep(20 * time.Millisecond)
					}
				}()
				logger, logs := logtest.Observe()
				if _, err := ReadHTTPHeadersUntilEnd(context.Background(), logger, server); err != nil {
					t.Fatal(err)
				}
				logtest.Check(t, logs, canary, on, tc.want)
			})
		})
	}
}

// failingConn reads its bytes once, and then fails, as a connection reset
// mid-message does.
type failingConn struct {
	net.Conn
	data []byte
}

func (c *failingConn) Read(b []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(b, c.data)
	c.data = c.data[n:]
	return n, errors.New("connection reset by peer")
}

// The bytes a failed read had read are logged by their size, unless
// KEPLOY_LOG_CONTENT is set.
func TestReadBuffConn_AFailedReadsBytesAreLoggedWithoutThem(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		logger, logs := logtest.Observe()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		errs := make(chan error, 1)
		data := []byte("GET /?ref=" + canary)
		go ReadBuffConn(ctx, logger, &failingConn{Conn: server, data: data}, make(chan []byte, 1), errs, false)
		select {
		case <-errs:
		case <-time.After(5 * time.Second):
			t.Fatal("ReadBuffConn did not fail the read")
		}
		logtest.Check(t, logs, canary, on, "Failed to read buffer")
		// The bytes are named as a logcontent.Bytes line names them, not by
		// the fingerprint of their base64.
		got := logs.FilterMessage("Failed to read buffer").All()[0].ContextMap()["base64_encoded"]
		if want := map[string]interface{}{"len": len(data), "fp": logcontent.Fingerprint(data)}; !on && !reflect.DeepEqual(got, want) {
			t.Fatalf("the bytes are named %v, want %v", got, want)
		}
	})
}
