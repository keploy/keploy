//go:build ignore

// Command learnwords writes, for the word-drift e2e
// (../proven-word-drift-linux.sh), what record's learn step learns of each
// recorded HTTP call whose request body holds a text one word of which the
// application makes fresh on every call (a UUID), and writes it onto the
// set's mocks with the code that writes what record's full round learned
// (mockdb.MockYaml.PersistLearned). The call is decided by the HTTP parser
// itself (MockOutgoing under OutgoingOptions.Learn, its learn step), against
// the agent's own mock manager, so a change to what the learn step learns of
// such a text, or to how it is written, changes what the lane replays.
//
// OSS keploy has no learn round (record's full round F, which replays every
// test right after its recording, is enterprise's), so this command plays
// F's run of each HTTP mock's call:
//
//   - the set's mocks are read by keploy's mock reader
//     (mockdb.MockYaml.GetTestSetMocks) and handed to a mock manager
//     (proxy.NewMockManager, SetMocksWithWindow) whose window holds the
//     set's per-test HTTP mocks;
//   - each recorded call is sent again, as F's run of the app sends it: its
//     method, URL and headers as recorded, its body with the -word'th word
//     (counted from 0, words split at spaces as the app writes them) of the
//     JSON string at -place, which must be a UUID, made fresh;
//   - the HTTP parser decides it with its learn step (MockOutgoing, under
//     KEPLOY_PROVEN=1, with OutgoingOptions.Learn and a learn tracker, as the
//     agent's proxy runs it in F), and must answer it with the recorded
//     answer: a refusal fails the command, with the reason the parser gave;
//   - the consumed mocks' states (MockManager.GetConsumedMocks), what the
//     agent sends the replay service, are written by PersistLearned, as the
//     replay service writes record's full round's learning, which prints
//     what it added to each mock.
//
// What it does NOT run of F: the app itself (the call is the recorded one
// with its UUID made fresh, as the app makes it; the app writes nothing else
// that changes), the test's own request and answer, the proxy's connection
// handling and the replay service's filters of unread learning
// (learnedElsewhere: here no call is made in another window).
//
// Run it from the keploy module's root, as one file (it is no package of the
// lane's own module, which does not require keploy):
//
//	KEPLOY_PROVEN=1 go run ./.github/workflows/test_workflow_scripts/golang/word-drift/learnwords/main.go \
//	    -path keploy -set test-set-0 -place body.label -word 1
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func main() {
	path := flag.String("path", "keploy", "the recording's keploy directory")
	set := flag.String("set", "test-set-0", "the test set")
	place := flag.String("place", "", "body.<key>: a JSON string of the request body holding the fresh word")
	word := flag.Int("word", -1, "the word of that text, counted from 0, that the app makes fresh on every call (a UUID)")
	flag.Parse()
	if *place == "" || *word < 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*path, *set, *place, *word); err != nil {
		fmt.Fprintln(os.Stderr, "learnwords:", err)
		os.Exit(1)
	}
}

func run(path, set, place string, word int) error {
	if !provenswitch.On() {
		return errors.New("run it under KEPLOY_PROVEN=1: record's learn step runs only under the proven switch")
	}
	ctx := context.Background()
	logger := zap.NewNop()
	db := mockdb.New(logger, path, "")
	read, err := db.GetTestSetMocks(ctx, set, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		return fmt.Errorf("read the mocks of %s: %w", set, err)
	}
	var recs []*models.Mock
	for _, m := range read.AllPerTest {
		if m.Kind == models.HTTP {
			recs = append(recs, m)
		}
	}
	if len(recs) == 0 {
		return fmt.Errorf("%s holds no HTTP mock of a test", set)
	}
	states := map[string]models.MockState{}
	for _, rec := range recs {
		st, err := learnMock(ctx, logger, read.AllPerTest, read.AllSession, rec, place, word)
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		states[rec.Name] = st
	}
	added, err := db.PersistLearned(ctx, set, states)
	if err != nil {
		return fmt.Errorf("write what was learned onto %s's mocks: %w", set, err)
	}
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if len(added[name]) == 0 {
			return fmt.Errorf("%s: the writer added nothing, want what the learn step learned at %s", name, place)
		}
		fmt.Printf("%s: %s\n", name, strings.Join(added[name], ", "))
	}
	return nil
}

// learnMock sends rec's call again, its UUID word made fresh, through the
// HTTP parser in record's full round, and returns the state the agent sends
// of rec once the call consumed it.
func learnMock(ctx context.Context, logger *zap.Logger, perTest, session []*models.Mock, rec *models.Mock, place string, word int) (models.MockState, error) {
	if rec.Spec.HTTPReq == nil || rec.Spec.HTTPResp == nil {
		return models.MockState{}, errors.New("no request or answer")
	}
	body, err := freshBody(rec.Spec.HTTPReq.Body, place, word)
	if err != nil {
		return models.MockState{}, err
	}
	request, err := rawRequest(rec.Spec.HTTPReq, body)
	if err != nil {
		return models.MockState{}, err
	}

	// The agent's proxy's learn tracker, wired to the manager as the proxy
	// installs a manager under the switch (Proxy.setMockManager), and the
	// test begun, as the replay service begins each test of the round.
	tr := learn.NewTracker()
	mm := proxy.NewMockManager(nil, nil, logger)
	defer mm.Close()
	mm.SetServedObserver(tr.ServedMock)
	mm.SetServedInObserver(tr.ServedMockIn)
	mm.SetOpenTestEpoch(tr.OpenEpoch)
	mm.SetTrackerSetAt(tr.SetAt)
	tr.SetServeOwnerOf(mm.ServeOwnerOf)
	at := rec.Spec.ReqTimestampMock
	mm.SetMocksWithWindow(perTest, session, at.Add(-time.Minute), at.Add(time.Minute))
	tr.Begin()
	var misses []string
	ctx = models.WithMockMismatchReporter(ctx, func(err error) { misses = append(misses, err.Error()) })
	ctx = learn.WithTracker(ctx, tr)
	app, agentEnd := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		err := httpint.New(logger).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{Learn: true})
		_ = agentEnd.Close()
		done <- err
	}()
	if _, err := app.Write(request); err != nil {
		return models.MockState{}, fmt.Errorf("send the call: %w (the parser: %v; misses: %v)", err, <-done, misses)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		_ = app.Close()
		return models.MockState{}, fmt.Errorf("read the answer: %w (the parser: %v; misses: %v)", err, <-done, misses)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	if resp.StatusCode != rec.Spec.HTTPResp.StatusCode || string(got) != rec.Spec.HTTPResp.Body {
		return models.MockState{}, fmt.Errorf("the learn step answered %d %q to the call %s, want the recorded %d %q (misses: %v)",
			resp.StatusCode, got, body, rec.Spec.HTTPResp.StatusCode, rec.Spec.HTTPResp.Body, misses)
	}
	for _, st := range mm.GetConsumedMocks() {
		if st.Name == rec.Name {
			return st, nil
		}
	}
	return models.MockState{}, fmt.Errorf("the learn step answered the call but consumed no %s", rec.Name)
}

// freshBody is body with the word-th word of the JSON string at place, a
// UUID, made fresh: the recorded word's one occurrence in body replaced.
func freshBody(body, place string, word int) (string, error) {
	v, ok := learn.DecodeJSON([]byte(body))
	if !ok {
		return "", fmt.Errorf("the request body is no JSON: %q", body)
	}
	leaves := map[string]learn.Leaf{}
	learn.JSONLeaves("body", v, leaves)
	l, ok := leaves[place]
	if !ok || l.Type != learn.String {
		return "", fmt.Errorf("the request body holds no string at %s: %q", place, body)
	}
	words := strings.Split(l.Value, " ")
	if word >= len(words) || !uuidRE.MatchString(words[word]) {
		return "", fmt.Errorf("the recorded %s %q holds no UUID at word %d", place, l.Value, word)
	}
	if strings.Count(body, words[word]) != 1 {
		return "", fmt.Errorf("the recorded UUID %s is not once in the body %q", words[word], body)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	fresh := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	return strings.Replace(body, words[word], fresh, 1), nil
}

// rawRequest is the recorded call as the app sends it, its body body.
func rawRequest(r *models.HTTPReq, body string) ([]byte, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("the recorded URL %q: %w", r.URL, err)
	}
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		if !strings.EqualFold(k, "Content-Length") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", r.Method, u.RequestURI())
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", k, r.Header[k])
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	return []byte(b.String()), nil
}
