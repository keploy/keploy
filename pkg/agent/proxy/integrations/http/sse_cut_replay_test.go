//go:build linux

package http

import (
	"bufio"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// eitherSwitch runs a row named name with the proven switch off, then on: the
// package's TestMain clears the switch the test process inherited, so a row
// reads only the one it sets.
func eitherSwitch(t *testing.T, name string, run func(t *testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		for _, env := range []string{"", "1"} {
			mode := "switch-off"
			if env != "" {
				mode = "switch-on"
			}
			t.Run(mode, func(t *testing.T) {
				t.Setenv(provenswitch.Env, env)
				run(t)
			})
		}
	})
}

// sseDataDispatched is the data of each event an SSE client dispatches from
// body, as the WHATWG event-stream parse reads it: an event goes out at the
// blank line that ends it, and the one a stream ends inside is dropped.
func sseDataDispatched(body string) []string {
	var events, data []string
	for {
		end := strings.IndexAny(body, "\r\n")
		if end < 0 {
			return events // a line with no end: its event is dropped
		}
		line := body[:end]
		if body[end] == '\r' && end+1 < len(body) && body[end+1] == '\n' {
			end++
		}
		body = body[end+1:]
		if line == "" {
			if data != nil {
				events = append(events, strings.Join(data, "\n"))
			}
			data = nil
			continue
		}
		if name, value, _ := strings.Cut(line, ":"); name == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
}

// A mock of an SSE answer, written to mocks.yaml and read back, is served so
// the application's client reads the events it read at record. One whose
// stream ended inside its last event (a close-delimited body the upstream
// closed mid-event, a server that never ended its last event) is served as
// its bytes, and the client drops that event as it did at record, never gets
// it ended; one whose stream ended its last event is kept as its frames and
// served from them, with every event ended. Under either switch.
func TestReplay_AnSSEAnswerIsServedAsItsClientReadItAtRecord(t *testing.T) {
	for _, c := range []struct {
		name, wire string
		events     []string
		served     string // the body served: the recorded bytes, or the frames read back
	}{
		{"cut inside its last event", "data: a\n\ndata: b", []string{"a"}, "data: a\n\ndata: b"},
		{"cut after its last event's line", "data: a\n\ndata: b\n", []string{"a"}, "data: a\n\ndata: b\n"},
		{"cut before its [DONE] ended", "data: a\n\ndata: [DONE]", []string{"a"}, "data: a\n\ndata: [DONE]"},
		{"its last event ended", "data: a\n\ndata: [DONE]\n\n", []string{"a", "[DONE]"}, "data:a\n\ndata:[DONE]\n\n"},
	} {
		eitherSwitch(t, c.name, func(t *testing.T) {
			if got := sseDataDispatched(c.wire); !reflect.DeepEqual(got, c.events) {
				t.Fatalf("the row's recorded stream dispatches %q; want %q", got, c.events)
			}
			m := throughDisk(t, faultMock(nil, okHead(c.wire, map[string]string{
				"Content-Type":   "text/event-stream",
				"Content-Length": strconv.Itoa(len(c.wire)),
			})))
			app, _ := replaying(t, &HTTP{Logger: zap.NewNop()}, m)
			if err := app.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(app), nil)
			if err != nil {
				t.Fatalf("the application read no answer: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("the application read the body %q, then %v", body, err)
			}
			if got := sseDataDispatched(string(body)); !reflect.DeepEqual(got, c.events) {
				t.Errorf("the application's client dispatched %q from %q; want %q, as at record", got, body, c.events)
			}
			if string(body) != c.served {
				t.Errorf("the application read the body %q; want %q", body, c.served)
			}
		})
	}
}
