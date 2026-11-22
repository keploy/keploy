package containerkey

import (
	"reflect"
	"strings"
	"testing"
)

// RedactArgs is a command line as keploy may log or store it: each secret's
// value says RedactedText, never the fingerprint's internal marker.
func TestRedactArgsSaysRedactedText(t *testing.T) {
	got := RedactArgs([]string{"./app", "--password=hunter2", "--db", "postgres://u:hunter2@db/x", "--token", "abc", "plain"})
	want := []string{"./app", "--password=" + RedactedText, "--db", RedactedText, "--token", RedactedText, "plain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactArgs = %q; want %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, redacted) || strings.Contains(a, "hunter2") {
			t.Fatalf("RedactArgs left %q", a)
		}
	}
	if RedactArgs(nil) != nil {
		t.Fatal("RedactArgs(nil) is not nil")
	}
}
