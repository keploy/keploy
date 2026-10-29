package docker

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap/zapcore"
)

// childDoc is a compose document with a service compose names
// <service>-<number> (app) and one that declares its container_name (db).
const childDoc = `name: fromdoc
services:
  app:
    image: app
  db:
    image: db
    container_name: my-db
`

func startChild(t *testing.T, project string) *ComposeChildOutput {
	t.Helper()
	unsetEnv(t, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
	o, err := StartComposeChildOutput(context.Background(), []byte(childDoc), project, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartComposeChildOutput: %v", err)
	}
	return o
}

func write(t *testing.T, w io.Writer, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write(%q) = %d, %v; want every byte taken", p, n, err)
		}
	}
}

func tailOf(project, name string) (AppOutput, bool) {
	all, _ := AppOutputTails()
	for _, o := range all {
		if o.Project == project && o.Name == name {
			return o, true
		}
	}
	return AppOutput{}, false
}

// What a `docker compose up` child prints for each container, in compose's
// `<name><padding> | <line>` form, is kept in that container's tail under the
// project, with its service: a container_name's and a <service>-<number>'s,
// from its stdout and its stderr, a line split across writes, an empty line,
// and a line compose coloured. compose's own lines (attaching, progress, an
// exit) are not kept.
func TestComposeChildOutputKeepsEachContainersLines(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	o := startChild(t, "childproj")
	if o.Project() != "childproj" {
		t.Fatalf("Project() = %q, want the given childproj", o.Project())
	}
	out, errOut := o.Stream(), o.Stream()
	write(t, out,
		"Attaching to app-1, my-db\n",
		"my-db  | ready\n",
		"app-1  | listening on :8080\n",
		"app-1  | pan", "ic: bo", "om\n",
		"app-1  | \n",
		"\x1b[36mapp-1  | \x1b[0mcoloured\n",
		"app-1 exited with code 1\n",
		"app-1  |no space after the bar is not compose's\n",
	)
	write(t, errOut,
		" Container childproj-app-1  Started\n",
		"app-1  | to stderr\n",
		"time=\"x\" level=warning msg=\"a | b\"\n",
	)

	if got, want := string(ServiceOutputTail("childproj", "app")), "listening on :8080\npanic: boom\n\ncoloured\nto stderr\n"; got != want {
		t.Fatalf("app's tail = %q, want %q", got, want)
	}
	db, ok := tailOf("childproj", "my-db")
	if !ok || string(db.Text) != "ready\n" || db.Service != "db" {
		t.Fatalf("my-db's tail = %+v (held %v); want \"ready\\n\" of service db", db, ok)
	}
	if all, _ := AppOutputTails(); len(all) != 2 {
		t.Fatalf("tails held: %+v; want only app-1's and my-db's", all)
	}
}

// Each StartComposeChildOutput is one Up: the project's tails then hold that
// Up's output alone. A set whose app printed nothing reads nothing, and a line
// the earlier Up's child still prints is not kept.
func TestComposeChildOutputStartsANewRunAtEachUp(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	first := startChild(t, "upproj")
	firstOut := first.Stream()
	write(t, firstOut, "app-1  | set 1\n")
	if got := string(ServiceOutputTail("upproj", "app")); got != "set 1\n" {
		t.Fatalf("set 1's tail = %q", got)
	}

	second := startChild(t, "upproj")
	if got := ServiceOutputTail("upproj", "app"); got != nil {
		t.Fatalf("set 2 printed nothing, but its tail = %q", got)
	}
	write(t, firstOut, "app-1  | late line of set 1\n")
	if got := ServiceOutputTail("upproj", "app"); got != nil {
		t.Fatalf("a line set 1's child printed after set 2's Up was kept: %q", got)
	}
	write(t, second.Stream(), "app-1  | panic: set 2\n")
	if got := string(ServiceOutputTail("upproj", "app")); got != "panic: set 2\n" {
		t.Fatalf("set 2's tail = %q, want its one panic line", got)
	}
}

// The project is the one `docker compose` takes: the name given (the
// command's -p), else COMPOSE_PROJECT_NAME, else the document's name:, else
// the project directory's name, normalized.
func TestComposeChildOutputProjectFollowsComposePrecedence(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	unsetEnv(t, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
	dir := filepath.Join(t.TempDir(), "My_Dir")
	noName := []byte("services: {app: {image: app}}")
	for _, tc := range []struct {
		name, given, env string
		doc              []byte
		want             string
	}{
		{"given", "given", "fromenv", []byte(childDoc), "given"},
		{"env", "", "fromenv", []byte(childDoc), "fromenv"},
		{"document", "", "", []byte(childDoc), "fromdoc"},
		{"directory", "", "", noName, "my_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(composeProjectNameEnv, tc.env)
			o, err := StartComposeChildOutput(context.Background(), tc.doc, tc.given, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if o.Project() != tc.want {
				t.Fatalf("Project() = %q, want %q", o.Project(), tc.want)
			}
		})
	}
	if _, err := StartComposeChildOutput(context.Background(), nil, "x", dir, nil); err == nil {
		t.Fatal("no document: want an error")
	}
	var none *ComposeChildOutput
	if none.Project() != "" {
		t.Fatal("a nil ComposeChildOutput has no project")
	}
}

// A line longer than a tail keeps is held only to its last composeLineMax
// bytes while it comes, in a buffer of bounded size, and kept by its end, the
// rest counted dropped; once it ends its buffer is let go. A line with no
// prefix in its first composePrefixMax bytes holds nothing.
func TestComposeChildOutputBoundsALongLine(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	o := startChild(t, "longproj")
	w := o.Stream().(*composeChildStream)
	write(t, w, "app-1  | ")
	piece := strings.Repeat("x", 32<<10)
	total := 3*composeLineMax + 12345
	for n := 0; n < total; n += len(piece) {
		p := piece
		if total-n < len(p) {
			p = p[:total-n]
		}
		write(t, w, p)
		if c := cap(w.text); c > composeLineBuf {
			t.Fatalf("after %d bytes of one line the stream holds a %d-byte buffer; want at most %d", n+len(p), c, composeLineBuf)
		}
	}
	// One write longer than the whole bound keeps its own end.
	write(t, w, strings.Repeat("z", composeLineBuf+7))
	if c := cap(w.text); c > composeLineBuf || len(w.text) != composeLineMax {
		t.Fatalf("after one write past the bound the stream holds %d bytes in a %d-byte buffer; want %d in at most %d", len(w.text), c, composeLineMax, composeLineBuf)
	}
	total += composeLineBuf + 7
	write(t, w, "END\n")
	if w.text != nil {
		t.Fatalf("the long line's %d-byte buffer is still held after it ended", cap(w.text))
	}
	tail, ok := tailOf("longproj", "app-1")
	if !ok || !bytes.HasSuffix(tail.Text, []byte("zzzEND\n")) {
		t.Fatalf("the long line's end was not kept (held %v)", ok)
	}
	if got, want := int64(len(tail.Text)-1)+tail.Dropped, int64(total+len("END")); got != want {
		t.Fatalf("kept %d + dropped %d = %d bytes, want the line's %d", len(tail.Text)-1, tail.Dropped, got, want)
	}

	write(t, w, strings.Repeat("y", 4*composePrefixMax))
	if len(w.head) != 0 || len(w.text) != 0 || !w.skip {
		t.Fatalf("a line with no prefix holds %d+%d bytes (skip %v); want none", len(w.head), len(w.text), w.skip)
	}
	write(t, w, "y | not a prefix this far in\n", "app-1  | after\n")
	if tail, _ := tailOf("longproj", "app-1"); !bytes.HasSuffix(tail.Text, []byte("\nafter\n")) && string(tail.Text) != "after\n" || bytes.Contains(tail.Text, []byte("y")) {
		t.Fatalf("the prefix-less line reached the tail, or the next line did not (%d bytes held, ends %q)", len(tail.Text), tail.Text[max(0, len(tail.Text)-20):])
	}
}

// Each write is weighed against the room the line has left, at each edge of
// the three ways a piece is taken: into the buffer's free room, into a buffer
// grown to fit (twice its size, or what the line needs, never past
// composeLineBuf), or past the bound, where only the line's last
// composeLineMax bytes are kept, from the text and b or from b alone, and the
// rest is counted dropped. The bytes held are always the line's last ones.
// Each case starts from a state a stream reaches; in one, a buffer of
// composeLineBuf always holds more than half of it.
func TestComposeChildOutputTakeWeighsEachWriteAgainstTheRoomLeft(t *testing.T) {
	const bound, most = composeLineBuf, composeLineMax
	pattern := func(from, n int) []byte {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte((from + i) % 251)
		}
		return p
	}
	for _, c := range []struct {
		name              string
		held, capacity, n int
		wantLen, wantCap  int
		wantLost          int64
	}{
		{"an empty write", 0, 0, 0, 0, 0, 0},
		{"a write that fills the free room", 10, 16, 6, 16, 16, 0},
		{"one byte past the free room doubles the buffer", 10, 16, 7, 17, 32, 0},
		{"a write the doubled buffer cannot hold grows it to just what the line needs", 1000, 1024, 40000, 41000, 41000, 0},
		{"a write one short of the most kept grows a small buffer to just what the line needs", 10, 16, most - 1, most + 9, most + 9, 0},
		{"one byte past the free room of a buffer over half the bound grows it to the bound", most, most, 1, most + 1, bound, 0},
		{"a write that fills the bound grows the buffer to it, not past", bound - 100, bound - 100, 100, bound, bound, 0},
		{"a write that fills the bound from the buffer's free room", bound - 100, bound, 100, bound, bound, 0},
		{"a write that fills the bound after a line of the most kept", most, most, bound - most, bound, bound, 0},
		{"a first write as large as the bound", 0, 0, bound, bound, bound, 0},
		{"b one short of the most kept fills the bound", bound - most + 1, bound - most + 1, most - 1, bound, bound, 0},
		{"one byte past the bound keeps the text's end and b", bound - 100, bound - 100, 101, most, bound, int64(bound + 1 - most)},
		{"one byte to a full buffer", bound, bound, 1, most, bound, int64(bound + 1 - most)},
		{"past the bound, b one short of the most kept keeps one byte of the text", bound - most + 2, bound - most + 2, most - 1, most, bound, int64(bound - most + 1)},
		{"past the bound, b of the most kept is kept alone", bound - most + 1, bound - most + 1, most, most, bound, int64(bound - most + 1)},
		{"past the bound, b over the most kept keeps its own end", bound - most + 1, bound - most + 1, most + 1, most, bound, int64(bound - most + 2)},
		{"a first write past the bound keeps its own end", 0, 0, bound + 1, most, bound, int64(bound + 1 - most)},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := make([]byte, c.held, c.capacity)
			copy(text, pattern(0, c.held))
			w := &composeChildStream{name: "app-1", text: text}
			w.take(pattern(c.held, c.n))
			if len(w.text) != c.wantLen || cap(w.text) != c.wantCap || w.lost != c.wantLost {
				t.Fatalf("holds %d bytes in a %d-byte buffer, %d dropped; want %d in %d, %d dropped", len(w.text), cap(w.text), w.lost, c.wantLen, c.wantCap, c.wantLost)
			}
			if cap(w.text) > bound {
				t.Fatalf("a %d-byte buffer is past the bound %d", cap(w.text), bound)
			}
			total := c.held + c.n
			if int64(len(w.text))+w.lost != int64(total) {
				t.Fatalf("held %d + dropped %d is not the line's %d bytes", len(w.text), w.lost, total)
			}
			if want := pattern(total-len(w.text), len(w.text)); !bytes.Equal(w.text, want) {
				t.Fatalf("the %d bytes held are not the line's last ones", len(w.text))
			}
		})
	}
}

const plantedChildValue = "planted-child-s3cr3t"

type childCutter struct{ cut string }

func (childCutter) RedactEntry(*zapcore.Entry)       {}
func (childCutter) RedactField(*zapcore.Field)       {}
func (childCutter) RedactEncoded(text string) string { return text }
func (childCutter) CutsChildOutput() bool            { return true }
func (childCutter) HoldChildOutput(string) int       { return 0 }
func (c childCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, c.cut, "***")
}

// The stream keeps a line cut of the values registered when it ends, even one
// registered after the line was printed (as handed over, uncut), and a read
// cuts it again.
func TestComposeChildOutputKeepsOnlyTheCutLine(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	keployLog.SetRedactor(nil)
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	o := startChild(t, "cutproj")
	w := o.Stream()
	write(t, w, "app-1  | DB_PASSWORD="+plantedChildValue)
	keployLog.SetRedactor(childCutter{cut: plantedChildValue})
	write(t, w, "\n")
	keployLog.SetRedactor(nil)
	if got := string(ServiceOutputTail("cutproj", "app")); got != "DB_PASSWORD=***\n" {
		t.Fatalf("held %q, want the cut line", got)
	}
}
