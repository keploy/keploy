package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// pathTagForm is a path tag: LogKey's form, "#" and 12 lower-case hex digits.
var pathTagForm = regexp.MustCompile(`^#[0-9a-f]{12}$`)

// A call's path tag is stable within a run: one tag for one method and path,
// however the path is given (a request target with or without its query, a
// URL with or without a fragment, a parsed URL, the path escaped or decoded,
// a miss's summary), and
// another for another path or another method.
func TestPathTagIsStableWithinARun(t *testing.T) {
	events := PathTag(context.Background(), "GET", "/events")
	if !pathTagForm.MatchString(events) {
		t.Fatalf("PathTag = %q, want # and %d hex digits", events, logKeyDigits)
	}
	same := map[string]string{
		"again":                    PathTag(context.Background(), "GET", "/events"),
		"with a query":             PathTag(context.Background(), "GET", "/events?since=42&token=t0k"),
		"as a URL":                 PathTag(context.Background(), "GET", "http://localhost:18996/events?x=1"),
		"as a URL with a fragment": PathTag(context.Background(), "GET", "http://localhost:18996/events#top"),
		"parsed":                   URLPathTag(context.Background(), "GET", &url.URL{Path: "/events", RawQuery: "x=1"}),
		"from an HTTP miss's line": CallPathTag(context.Background(), "HTTP", "GET /events?since=42"),
		"from an HTTP/2 miss":      CallPathTag(context.Background(), "HTTP/2", "GET /events"),
	}
	for how, got := range same {
		if got != events {
			t.Errorf("%s: %q, want %q", how, got, events)
		}
	}
	if PathTag(context.Background(), "GET", "/caf%C3%A9") != PathTag(context.Background(), "GET", "/café") || URLPathTag(context.Background(), "GET", &url.URL{Path: "/café"}) != PathTag(context.Background(), "GET", "/café") {
		t.Error("the escaped and the decoded path have two tags")
	}
	for how, got := range map[string]string{
		"another path":         PathTag(context.Background(), "GET", "/poll"),
		"another method":       PathTag(context.Background(), "POST", "/events"),
		"a path one byte more": PathTag(context.Background(), "GET", "/events/"),
	} {
		if got == events {
			t.Errorf("%s has the tag of GET /events", how)
		}
	}
	for how, got := range map[string]string{
		"a MySQL call":         CallPathTag(context.Background(), "MySQL", "COM_QUERY SELECT 1"),
		"a summary of no form": CallPathTag(context.Background(), "HTTP", "connection phase"),
		"no URL":               URLPathTag(context.Background(), "GET", nil),
		// A path alone: no console or report line prints its tag.
		"no method":        PathTag(context.Background(), "", "/events"),
		"a URL, no method": URLPathTag(context.Background(), "", &url.URL{Path: "/events"}),
	} {
		if got != "" {
			t.Errorf("%s: %q, want no tag", how, got)
		}
	}
}

// Keys apart, tags apart: a tag is LogKey's keyed hash, so another key
// (another process's) gives the same method and path another tag, and no one
// can compute a path's tag without the key: it is neither the path's unkeyed
// hash nor its hash under a key of zeros. It is LogKey of the method and the
// path, in LogKey's form, and "" names "".
func TestPathTagsDifferUnderAnotherKey(t *testing.T) {
	k1, k2 := make([]byte, 32), make([]byte, 32)
	k2[0] = 1
	if logKeyUnder(k1, "GET /events") == logKeyUnder(k2, "GET /events") {
		t.Fatal("two keys give GET /events one tag")
	}
	key := processLogKey()
	if len(key) != 32 || string(key) == string(k1) {
		t.Fatalf("the process's key is %d bytes, or all zeros", len(key))
	}
	sum := sha256.Sum256([]byte("GET /events"))
	if got := PathTag(context.Background(), "GET", "/events"); got == "#"+hex.EncodeToString(sum[:])[:logKeyDigits] || got == logKeyUnder(k1, "GET /events") {
		t.Fatalf("PathTag = %q: a hash anyone can compute", got)
	}
	if PathTag(context.Background(), "GET", "/events") != LogKey(context.Background(), "GET /events") || LogKey(context.Background(), "") != "" || !pathTagForm.MatchString(LogKey(context.Background(), "PHPREDIS_SESSION:abc")) {
		t.Fatal("PathTag is not LogKey of the method and path, or LogKey's form differs")
	}
}

// pathTagHelperEnv runs TestPathTagHelperProcess as a helper: it prints the
// tag of GET /events in its own process.
const pathTagHelperEnv = "KEPLOY_PATH_TAG_HELPER"

// TestPathTagHelperProcess is the helper process of
// TestEachProcessMakesItsOwnKey; a test run does nothing in it.
func TestPathTagHelperProcess(t *testing.T) {
	if os.Getenv(pathTagHelperEnv) != "1" {
		t.Skip("a helper process only")
	}
	if _, err := os.Stdout.WriteString("tag=" + PathTag(context.Background(), "GET", "/events") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Each process draws its own key: two processes give one method and path two
// tags, and neither is this process's, so tags of two runs say nothing of
// each other.
func TestEachProcessMakesItsOwnKey(t *testing.T) {
	tagIn := func() string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPathTagHelperProcess$")
		cmd.Env = append(os.Environ(), pathTagHelperEnv+"=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the helper process: %v (%s)", err, out)
		}
		m := regexp.MustCompile(`(?m)^tag=(#[0-9a-f]{12})$`).FindStringSubmatch(string(out))
		if m == nil {
			t.Fatalf("the helper process printed no tag: %q", out)
		}
		return m[1]
	}
	a, b, here := tagIn(), tagIn(), PathTag(context.Background(), "GET", "/events")
	if a == b || a == here || b == here {
		t.Fatalf("GET /events: tags %s and %s in two processes and %s here: want three keys", a, b, here)
	}
}

// The miss line names an HTTP call by its shape and the call's path tag,
// never by its path; keploy's advice in it names a path it quotes (a
// sessionCalls entry's pathPrefix) by where the console's Hint and the report
// name it, by the same tag. The tag is the one the report carries, made in
// the session that logged the miss; a report that carries none makes none
// here, where no session's context is (its advice names the path by its
// shape). The report's call carries that tag beside the summary whole, and a
// line of another process about the call names it by that tag, never one of
// its own.
func TestTheMissLineCarriesTheTagAndNoPathText(t *testing.T) {
	next := `Each test is served the HTTP calls it recorded. Declare it in keploy.yaml (sessionCalls: [{host: "auth.example.com", method: "POST", pathPrefix: "/oauth/token"}]), or record again.`
	noTag := &MockMismatchReport{Protocol: "HTTP", ActualSummary: "POST /oauth/token?client=web", NextSteps: next}
	if noTag.LogPathTag() != "" || !strings.Contains(MissLogNextStep(noTag), `pathPrefix: "/{}/{}"`) {
		t.Fatalf("a report with no tag is named by %q, next step %q: want no tag, made by no session", noTag.LogPathTag(), MissLogNextStep(noTag))
	}
	session := StartLogKeySession(context.Background())
	r := &MockMismatchReport{
		Protocol: "HTTP", ActualSummary: "POST /oauth/token?client=web", NextSteps: next,
		PathTag: CallPathTag(session, "HTTP", "POST /oauth/token?client=web"),
	}
	tag := r.LogPathTag()
	if tag != PathTag(session, "POST", "/oauth/token") || tag == PathTag(context.Background(), "POST", "/oauth/token") {
		t.Fatalf("LogPathTag = %q, want the tag of POST /oauth/token in the session that logged the miss", tag)
	}
	shape, next := MissLogShape(r), MissLogNextStep(r)
	if shape != "POST /{}/{}" {
		t.Errorf("MissLogShape = %q, want the call's shape", shape)
	}
	wantNext := `Each test is served the HTTP calls it recorded. Declare it in keploy.yaml (sessionCalls: [{host: <the host keploy's console Hint and report name for [path tag ` + tag + `]>, method: "POST", pathPrefix: <the path keploy's console Hint and report name for [path tag ` + tag + `]>}]), or record again.`
	if next != wantNext {
		t.Errorf("MissLogNextStep =\n%q, want\n%q", next, wantNext)
	}
	for _, line := range []string{shape, next} {
		if strings.Contains(line, "oauth") || strings.Contains(line, "token") || strings.Contains(line, "client") || strings.Contains(line, "example") {
			t.Errorf("the line %q names the call's path", line)
		}
	}

	// A tag the report carries is the one its lines name: the agent's, set
	// where the miss is logged, and kept whatever this process would make
	// of the summary (one whose path a recording redacted, here).
	r.PathTag, r.ActualSummary = "#0badc0de0bad", "POST /oauth/{}"
	if r.LogPathTag() != "#0badc0de0bad" || !strings.Contains(MissLogNextStep(r), "[path tag #0badc0de0bad]") {
		t.Errorf("the line names the call by %q, want the tag its report carries", r.LogPathTag())
	}

	// The report's call: the summary whole, beside the tag.
	m := UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /oauth/token?client=web", PathTag: tag, NextSteps: r.NextSteps}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// YAML quotes a value that begins with "#" (a comment's mark).
	var back UnmatchedCall
	if err := yaml.Unmarshal(out, &back); err != nil || back.ActualSummary != m.ActualSummary || back.PathTag != tag ||
		!strings.Contains(string(out), "actual_summary: POST /oauth/token?client=web\n") || !strings.Contains(string(out), "path_tag: '"+tag+"'\n") {
		t.Errorf("the report's call is\n%s\nwant its summary whole and path_tag %s (read back: %+v, %v)", out, tag, back, err)
	}
	// The CLI's lines about it: its shape and the tag it carries.
	if got := UnmatchedCallLogName(m); got != "POST /{}/{} [path tag "+tag+"]" {
		t.Errorf("UnmatchedCallLogName = %q", got)
	}
	if got := UnmatchedCallLogText(m, m.NextSteps); got != wantNext {
		t.Errorf("UnmatchedCallLogText =\n%q, want\n%q", got, wantNext)
	}
	// A call that carries no tag (an agent that tags none reported it) is
	// named by its shape alone: this process makes none for it, which no
	// line of the agent's would name.
	untagged := UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /oauth/token"}
	if got := UnmatchedCallLogName(untagged); got != "POST /{}/{}" {
		t.Errorf("UnmatchedCallLogName of an untagged call = %q, want its shape alone", got)
	}
	// Evidence outlives the run: it carries no tag, and its next step
	// names a quoted path by its shape.
	if ev := m.Sanitized(); ev.PathTag != "" || strings.Contains(ev.NextSteps, "path tag") || !strings.Contains(ev.NextSteps, `pathPrefix: "/{}/{}"`) {
		t.Errorf("evidence is %+v, want no tag and the path's shape", ev)
	}
}

// Advice about no call (a template check's why) names a URL path it quotes
// by its shape alone: a tag of a path with no method is one no console or
// report line prints, and the line names the test the report keeps the advice
// whole for.
func TestAdviceAboutNoCallNamesAQuotedPathByItsShape(t *testing.T) {
	got := AdviceLogText(`the request "/v1/reset/abc123" names the page`)
	if want := `the request "/{}/{}/{}" names the page`; got != want {
		t.Errorf("AdviceLogText = %q, want %q", got, want)
	}
}

// Each session draws its own key (NewLogKeySession), carried in the context
// of its work: two sessions one process serves at once (enterprise's node
// agent records several applications at a time) give one method and path two
// tags, each the same all through its session and in every context made from
// its own, and neither is the tag a line with no session in its context
// gives (the process's key), which is the same all through the process. A
// session begun later changes no tag of one begun before, and a call that
// carries a tag keeps it.
func TestEachSessionTagsPathsUnderAKeyOfItsOwn(t *testing.T) {
	base := context.Background()
	a, b := StartLogKeySession(base), StartLogKeySession(base)
	if LogKeys(a) == nil || LogKeys(a) == LogKeys(b) {
		t.Fatal("two sessions share a key, or a session carries none")
	}
	// The two sessions tag at once, over and over, as two recordings'
	// connections do.
	const n = 200
	tags := [2][]string{make([]string, n), make([]string, n)}
	var wg sync.WaitGroup
	for i, ctx := range []context.Context{a, b} {
		wg.Add(1)
		go func(i int, ctx context.Context) {
			defer wg.Done()
			for j := range n {
				// A context made from the session's, as a connection's
				// and a request's are.
				conn, cancel := context.WithCancel(context.WithValue(ctx, struct{ n int }{j}, j))
				tags[i][j] = URLPathTag(conn, "GET", &url.URL{Path: "/v1/users/42/reset/t0k"})
				cancel()
			}
		}(i, ctx)
	}
	wg.Wait()
	for i := range tags {
		for j, tag := range tags[i] {
			if tag != tags[i][0] || !pathTagForm.MatchString(tag) {
				t.Fatalf("session %d gave GET /v1/users/42/reset/t0k tag %q at call %d and %q at its first", i, tag, j, tags[i][0])
			}
		}
	}
	if tags[0][0] == tags[1][0] {
		t.Fatalf("two sessions at once gave GET /v1/users/42/reset/t0k one tag, %s", tags[0][0])
	}
	if got := LogKeys(a).PathTag("GET", "/v1/users/42/reset/t0k"); got != tags[0][0] {
		t.Fatalf("the session's own PathTag is %s, its context's %s", got, tags[0][0])
	}
	process := PathTag(base, "GET", "/v1/users/42/reset/t0k")
	if process == tags[0][0] || process == tags[1][0] || process != PathTag(context.TODO(), "GET", "/v1/users/42/reset/t0k") ||
		process != (*LogKeySession)(nil).PathTag("GET", "/v1/users/42/reset/t0k") {
		t.Fatalf("a line with no session tags %s, its sessions %s and %s: want the process's key, apart from both", process, tags[0][0], tags[1][0])
	}
	carried := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/users/42/reset/t0k", PathTag: tags[0][0]}
	later := StartLogKeySession(a)
	if PathTag(later, "GET", "/v1/users/42/reset/t0k") == tags[0][0] || PathTag(a, "GET", "/v1/users/42/reset/t0k") != tags[0][0] {
		t.Fatal("a session begun inside another kept its key, or changed the first's")
	}
	if got := UnmatchedCallLogName(carried); got != "GET /{}/{}/{}/{}/{} [path tag "+tags[0][0]+"]" {
		t.Fatalf("a carried call is named %q, want the tag its line named, %s", got, tags[0][0])
	}
	if WithLogKeySession(base, nil) != base || LogKeys(nil) != nil || LogKeys(base) != nil { //nolint:staticcheck // a nil context names no session
		t.Fatal("no session is not the process's key")
	}
}

// WithPathTag writes a tag after a call's name, and nothing for no tag.
func TestWithPathTag(t *testing.T) {
	if got := WithPathTag("GET /{}", "#1f2e3d4c5b6a"); got != "GET /{} [path tag #1f2e3d4c5b6a]" {
		t.Errorf("WithPathTag = %q", got)
	}
	if got := WithPathTag("GET /{}", ""); got != "GET /{}" {
		t.Errorf("WithPathTag with no tag = %q", got)
	}
}
