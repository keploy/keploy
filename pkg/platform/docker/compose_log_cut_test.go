package docker

import (
	"bytes"
	"strings"
	"testing"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap/zapcore"
)

// plantedComposeValue is what the test cutter cuts; never a real secret.
const plantedComposeValue = "planted-compose-s3cr3t"

type composeCutter struct{}

func (composeCutter) RedactEntry(*zapcore.Entry)       {}
func (composeCutter) RedactField(*zapcore.Field)       {}
func (composeCutter) RedactEncoded(text string) string { return text }
func (composeCutter) CutsChildOutput() bool            { return true }
func (composeCutter) HoldChildOutput(string) int       { return 0 }
func (composeCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, plantedComposeValue, "***")
}

// A line a container prints reaches keploy's console through the compose
// library's log consumer, and is cut there as a child process's line is.
func TestComposeLogConsumerCutsWhatAContainerPrints(t *testing.T) {
	keployLog.SetRedactor(composeCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	var out, errOut bytes.Buffer
	c := composeLogConsumer{out: &out, err: &errOut}
	c.Log("app-1", "DB_PASSWORD="+plantedComposeValue)
	c.Err("keploy-agent-1", "dial with "+plantedComposeValue+" failed")
	if got, want := out.String(), "app-1  | DB_PASSWORD=***\n"; got != want {
		t.Fatalf("stdout %q, want %q", got, want)
	}
	if got, want := errOut.String(), "keploy-agent-1  | dial with *** failed\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}

	keployLog.SetRedactor(nil)
	out.Reset()
	c.Log("app-1", "DB_PASSWORD="+plantedComposeValue)
	if got, want := out.String(), "app-1  | DB_PASSWORD="+plantedComposeValue+"\n"; got != want {
		t.Fatalf("with no redactor: stdout %q, want the line as it came, %q", got, want)
	}
}
