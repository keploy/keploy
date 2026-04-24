package kpcap

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewOnlyEnablesCaptureForDebugRecordOrTest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mode    string
		debug   bool
		enabled bool
	}{
		{name: "record debug", mode: "record", debug: true, enabled: true},
		{name: "test debug", mode: "test", debug: true, enabled: true},
		{name: "record no debug", mode: "record", debug: false, enabled: false},
		{name: "agent debug", mode: "agent", debug: true, enabled: false},
	}

	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			capture := New(nil, tt.mode, t.TempDir(), tt.debug, "outgoing")
			defer func() { _ = capture.Close() }()
			if capture.Enabled() != tt.enabled {
				t.Fatalf("Enabled() = %v, want %v", capture.Enabled(), tt.enabled)
			}
			if tt.enabled && filepath.Ext(capture.Path()) != ".kpcap" {
				t.Fatalf("Path() = %q, want .kpcap extension", capture.Path())
			}
		})
	}
}

func TestCaptureWritesJSONLineEvents(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	capture := New(nil, "record", baseDir, true, "outgoing")
	if !capture.Enabled() {
		t.Fatal("capture was not enabled")
	}

	payload := []byte("hello kpcap")
	capture.RecordChunk(PacketContext{Flow: "outgoing", ConnID: "1", PeerConnID: "2"}, DirectionAppToProxy, payload)
	if err := capture.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if !strings.HasPrefix(capture.Path(), filepath.Join(baseDir, "debug")) {
		t.Fatalf("capture path %q is not under debug dir %q", capture.Path(), filepath.Join(baseDir, "debug"))
	}

	events := readKpcapEvents(t, capture.Path())
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	if events[0]["type"] != "capture-start" {
		t.Fatalf("first event type = %v, want capture-start", events[0]["type"])
	}
	if events[1]["type"] != "chunk" {
		t.Fatalf("second event type = %v, want chunk", events[1]["type"])
	}
	if events[2]["type"] != "capture-end" {
		t.Fatalf("third event type = %v, want capture-end", events[2]["type"])
	}
	if events[1]["direction"] != DirectionAppToProxy {
		t.Fatalf("chunk direction = %v, want %s", events[1]["direction"], DirectionAppToProxy)
	}
	encoded, ok := events[1]["payload_b64"].(string)
	if !ok || encoded == "" {
		t.Fatalf("payload_b64 = %v, want non-empty string", events[1]["payload_b64"])
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("payload_b64 decode error = %v", err)
	}
	if string(decoded) != string(payload) {
		t.Fatalf("decoded payload = %q, want %q", decoded, payload)
	}
}

func readKpcapEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
		}
		events = append(events, event)
	}
	return events
}
