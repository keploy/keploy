package logcontent_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/logcontent"
)

// The line reaches agent-debug.log through the chain the agent builds
// (utils/log.New, then AddDebugFileSink, as main's maybeAttachDebugFileSink
// does) without the content when the switch is off, and with it when it is
// on. The caller overwrites its buffer as soon as the Debug call returns, as
// a parser reusing its read buffer does: the line still names what the
// buffer held at the call, which holds only if the debug file's core encodes
// the entry before the call returns, as Bytes' lazy digest relies on.
func TestTheAgentDebugLogCarriesContentOnlyWhenOptedIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
	}{{"off", false}, {"on", true}} {
		on := tc.on
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(logcontent.Set(on))
			t.Chdir(t.TempDir()) // New opens keploy-logs.txt in the working directory.
			t.Cleanup(log.TestOnlyResetSink)
			logger, logFile, err := log.New()
			if err != nil {
				t.Fatalf("log.New: %v", err)
			}
			if logFile != nil {
				t.Cleanup(func() { _ = logFile.Close() })
			}
			f, err := os.Create(filepath.Join(t.TempDir(), "agent-debug.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			logger, sink := log.AddDebugFileSink(logger, f, 0)

			const canary = "kp-canary-0b7d2a-customer-row"
			buf := []byte("DataRow " + canary)
			fp := logcontent.Fingerprint(buf)
			logger.Debug("frame", logcontent.Bytes("payload", buf))
			copy(buf, bytes.Repeat([]byte{'X'}, len(buf)))

			if err := sink.Flush(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			line := string(got)
			if !strings.Contains(line, "frame") {
				t.Fatalf("the Debug line did not reach agent-debug.log: %q", line)
			}
			if strings.Contains(line, "XXXX") {
				t.Fatalf("the line was encoded after the call returned, from the reused buffer: %q", line)
			}
			if on {
				if !strings.Contains(line, canary) {
					t.Fatalf("with the switch on, the line does not carry the payload: %q", line)
				}
				return
			}
			if strings.Contains(line, canary) {
				t.Fatalf("with the switch off, the line carries the payload: %q", line)
			}
			if !strings.Contains(line, fp) {
				t.Fatalf("with the switch off, the line does not name the payload by %s: %q", fp, line)
			}
		})
	}
}
